package plugin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kandev-plugin-azure-devops/internal/azure"
	"kandev-plugin-azure-devops/internal/store"
)

const (
	// SaveRemediationDraftTool stores a human-reviewable commit + reply draft.
	SaveRemediationDraftTool = "save_pr_remediation_draft"
	// ReplyPullRequestThreadTool replies directly only when human approval is off.
	ReplyPullRequestThreadTool = "reply_pull_request_thread"

	defaultAutoFixMaxRounds = 5
	maxDraftReplies         = 50
	maxThreadExcerptChars   = 280
)

// SessionLister is the subset of Host.Sessions() used by PR auto-fix.
type SessionLister interface {
	List(ctx context.Context, filter pluginsdk.SessionFilter, page pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error)
}

// MessageSender is the subset of Host.Messages() used by PR auto-fix.
type MessageSender interface {
	Send(ctx context.Context, taskID, sessionID, text string) (*pluginsdk.MessageDispatch, error)
}

// DraftReply is one proposed PR comment reply awaiting human review.
type DraftReply struct {
	ID            string `json:"id"`
	ThreadID      int    `json:"threadId"`
	Author        string `json:"author,omitempty"`
	FilePath      string `json:"filePath,omitempty"`
	Line          int    `json:"line,omitempty"`
	ThreadExcerpt string `json:"threadExcerpt,omitempty"`
	Content       string `json:"content"`
	ResolveThread bool   `json:"resolveThread"`
}

// RemediationDraft holds the agent's proposed commit message and thread replies
// until a human approves, requests changes, or dismisses it.
type RemediationDraft struct {
	DraftID       string       `json:"draftId"`
	Summary       string       `json:"summary"`
	HasChanges    bool         `json:"hasChanges"`
	CommitMessage string       `json:"commitMessage,omitempty"`
	Replies       []DraftReply `json:"replies"`
	CreatedAt     time.Time    `json:"createdAt"`
}

// PRAutoFixStateView is the UI projection of one linked pull request's auto-fix state.
type PRAutoFixStateView struct {
	RepositoryID     string            `json:"repositoryId"`
	PullRequestID    int               `json:"pullRequestId"`
	Enabled          bool              `json:"enabled"`
	RequireApproval  bool              `json:"requireApproval"`
	Status           string            `json:"status"`
	RoundCount       int               `json:"roundCount"`
	MaxRounds        int               `json:"maxRounds"`
	LastDispatchedAt *time.Time        `json:"lastDispatchedAt,omitempty"`
	LastError        string            `json:"lastError,omitempty"`
	LastErrorAt      *time.Time        `json:"lastErrorAt,omitempty"`
	PendingDraft     *RemediationDraft `json:"pendingDraft,omitempty"`
}

type prAutoFixEntry struct {
	Enabled                 *bool             `json:"enabled,omitempty"`
	RequireApproval         *bool             `json:"requireApproval,omitempty"`
	Status                  string            `json:"status,omitempty"`
	RoundCount              int               `json:"roundCount,omitempty"`
	LastDispatchedSignature string            `json:"lastDispatchedSignature,omitempty"`
	LastDispatchedAt        *time.Time        `json:"lastDispatchedAt,omitempty"`
	LastSessionID           string            `json:"lastSessionId,omitempty"`
	LastError               string            `json:"lastError,omitempty"`
	LastErrorAt             *time.Time        `json:"lastErrorAt,omitempty"`
	RepliedComments         map[int]int       `json:"repliedComments,omitempty"`
	AcknowledgedComments    map[int]int       `json:"acknowledgedComments,omitempty"`
	PendingDraft            *RemediationDraft `json:"pendingDraft,omitempty"`
}

type taskPRAutoFixRecord struct {
	PullRequests map[string]prAutoFixEntry `json:"pullRequests"`
}

// AutoFix manages linked pull request auto-remediation loops and human review gates.
type AutoFix struct {
	store       *store.Store
	connections *Connections
	links       *Links
	tasks       func() TaskReader
	sessions    func() SessionLister
	messages    func() MessageSender
	workspaces  WorkspaceLister
	locks       workspaceLocks
	now         func() time.Time
}

// NewAutoFix constructs the linked pull request auto-fix service.
func NewAutoFix(
	stateStore *store.Store,
	connections *Connections,
	links *Links,
	tasks func() TaskReader,
	sessions func() SessionLister,
	messages func() MessageSender,
	workspaces WorkspaceLister,
) *AutoFix {
	return &AutoFix{
		store:       stateStore,
		connections: connections,
		links:       links,
		tasks:       tasks,
		sessions:    sessions,
		messages:    messages,
		workspaces:  workspaces,
		now:         time.Now,
	}
}

func prAutoFixStateKey(taskID string) string {
	return taskLinkKeyPrefix + taskID + prAutoFixStateSuffix
}

func prAutoFixEntryKey(repositoryID string, pullRequestID int) string {
	return strings.ToLower(strings.TrimSpace(repositoryID)) + "!" + strconv.Itoa(pullRequestID)
}

// RemovePullRequest drops stored auto-fix state for an unlinked pull request.
func (a *AutoFix) RemovePullRequest(ctx context.Context, workspaceID, taskID, repositoryID string, pullRequestID int) error {
	if a == nil || workspaceID == "" || taskID == "" {
		return nil
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return err
	}
	key := prAutoFixEntryKey(repositoryID, pullRequestID)
	if _, exists := record.PullRequests[key]; !exists {
		return nil
	}
	delete(record.PullRequests, key)
	return a.saveRecord(ctx, workspaceID, taskID, record)
}

func (a *AutoFix) loadRecord(ctx context.Context, workspaceID, taskID string) (taskPRAutoFixRecord, error) {
	var record taskPRAutoFixRecord
	if _, err := a.store.LoadState(ctx, workspaceID, prAutoFixStateKey(taskID), &record); err != nil {
		return taskPRAutoFixRecord{}, err
	}
	if record.PullRequests == nil {
		record.PullRequests = map[string]prAutoFixEntry{}
	}
	return record, nil
}

func (a *AutoFix) saveRecord(ctx context.Context, workspaceID, taskID string, record taskPRAutoFixRecord) error {
	if len(record.PullRequests) == 0 {
		return a.store.DeleteState(ctx, workspaceID, prAutoFixStateKey(taskID))
	}
	return a.store.SaveState(ctx, workspaceID, prAutoFixStateKey(taskID), record)
}

func (a *AutoFix) workspaceDefaults(ctx context.Context, workspaceID string) (enabled bool, requireApproval bool) {
	settings, err := a.connections.Settings(ctx, workspaceID)
	if err != nil {
		return true, true
	}
	return settings.AutoFixEnabledByDefault, settings.AutoFixRequireApprovalByDefault
}

// StatusForLink returns the current auto-fix status string for task_links.get.
func (a *AutoFix) StatusForLink(ctx context.Context, workspaceID, taskID string, link PullRequestLink) string {
	if a == nil {
		return ""
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	unlock()
	if err != nil {
		return ""
	}
	entry := record.PullRequests[prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)]
	return a.toView(ctx, workspaceID, link, entry).Status
}

func (a *AutoFix) toView(ctx context.Context, workspaceID string, link PullRequestLink, entry prAutoFixEntry) PRAutoFixStateView {
	defaultEnabled, defaultApproval := a.workspaceDefaults(ctx, workspaceID)
	enabled := defaultEnabled
	if entry.Enabled != nil {
		enabled = *entry.Enabled
	}
	requireApproval := defaultApproval
	if entry.RequireApproval != nil {
		requireApproval = *entry.RequireApproval
	}
	viewStatus := entry.Status
	switch {
	case entry.PendingDraft != nil:
		viewStatus = "awaiting_approval"
	case !enabled:
		viewStatus = "paused"
	case viewStatus == "" || viewStatus == "paused" || viewStatus == "awaiting_approval":
		viewStatus = "idle"
	}
	var draftCopy *RemediationDraft
	if entry.PendingDraft != nil {
		cloned := *entry.PendingDraft
		cloned.Replies = nonNil(append([]DraftReply(nil), entry.PendingDraft.Replies...))
		draftCopy = &cloned
	}
	return PRAutoFixStateView{
		RepositoryID:     link.RepositoryID,
		PullRequestID:    link.PullRequestID,
		Enabled:          enabled,
		RequireApproval:  requireApproval,
		Status:           viewStatus,
		RoundCount:       entry.RoundCount,
		MaxRounds:        defaultAutoFixMaxRounds,
		LastDispatchedAt: entry.LastDispatchedAt,
		LastError:        entry.LastError,
		LastErrorAt:      entry.LastErrorAt,
		PendingDraft:     draftCopy,
	}
}

type autoFixSelectorInput struct {
	RepositoryID    string              `json:"repositoryId"`
	PullRequestID   int                 `json:"pullRequestId"`
	Enabled         *bool               `json:"enabled"`
	RequireApproval *bool               `json:"requireApproval"`
	DraftID         string              `json:"draftId"`
	CommitPush      bool                `json:"commitPush"`
	CommitMessage   string              `json:"commitMessage"`
	Feedback        string              `json:"feedback"`
	Replies         []approveReplyInput `json:"replies"`
	ThreadID        int                 `json:"threadId"`
	ThreadRef       string              `json:"threadRef"`
	Content         string              `json:"content"`
	ResolveThread   bool                `json:"resolveThread"`
}

type approveReplyInput struct {
	ID            string `json:"id"`
	ThreadID      int    `json:"threadId"`
	Send          bool   `json:"send"`
	Content       string `json:"content"`
	ResolveThread bool   `json:"resolveThread"`
}

// Handle serves task-scoped pull request auto-fix and reply actions.
func (a *AutoFix) Handle(ctx context.Context, verified pluginsdk.VerifiedActionContext, key string, body []byte) (any, bool, error) {
	switch key {
	case "pull_requests.autofix.get",
		"pull_requests.autofix.update",
		"pull_requests.autofix.trigger",
		"pull_requests.autofix.approve",
		"pull_requests.autofix.revise",
		"pull_requests.autofix.dismiss",
		"pull_requests.reply":
	default:
		return nil, false, nil
	}
	if verified.WorkspaceID == "" || verified.TaskID == "" {
		return nil, true, invalidArgument(errors.New("a verified task is required"))
	}
	var input autoFixSelectorInput
	if err := decodeBody(body, &input); err != nil {
		return nil, true, err
	}
	result, err := a.handleAction(ctx, verified.WorkspaceID, verified.TaskID, key, input)
	return result, true, err
}

func (a *AutoFix) handleAction(ctx context.Context, workspaceID, taskID, key string, input autoFixSelectorInput) (any, error) {
	link, err := a.findLinkedPR(ctx, workspaceID, taskID, input.RepositoryID, input.PullRequestID)
	if err != nil {
		return nil, err
	}
	switch key {
	case "pull_requests.autofix.get":
		return a.getState(ctx, workspaceID, taskID, link)
	case "pull_requests.autofix.update":
		return a.updateSettings(ctx, workspaceID, taskID, link, input)
	case "pull_requests.autofix.trigger":
		return a.triggerNow(ctx, workspaceID, taskID, link)
	case "pull_requests.autofix.approve":
		return a.approveDraft(ctx, workspaceID, taskID, link, input)
	case "pull_requests.autofix.revise":
		return a.reviseDraft(ctx, workspaceID, taskID, link, input)
	case "pull_requests.autofix.dismiss":
		return a.dismissDraft(ctx, workspaceID, taskID, link, input.DraftID)
	default: // pull_requests.reply
		return a.manualReply(ctx, workspaceID, taskID, link, input)
	}
}

func (a *AutoFix) findLinkedPR(ctx context.Context, workspaceID, taskID, repositoryID string, pullRequestID int) (PullRequestLink, error) {
	links, err := a.links.PullRequests(ctx, workspaceID, taskID)
	if err != nil {
		return PullRequestLink{}, unavailable(err)
	}
	if len(links) == 0 {
		return PullRequestLink{}, notFound(errors.New("no Azure DevOps pull request is linked to this task"))
	}
	repoTrimmed := strings.TrimSpace(repositoryID)
	if repoTrimmed == "" && pullRequestID <= 0 {
		if len(links) == 1 {
			return links[0], nil
		}
		return PullRequestLink{}, invalidArgument(errors.New("repositoryId and pullRequestId are required when multiple pull requests are linked"))
	}
	var matches []PullRequestLink
	for _, candidate := range links {
		if pullRequestID > 0 && candidate.PullRequestID != pullRequestID {
			continue
		}
		if repoTrimmed != "" && !strings.EqualFold(candidate.RepositoryID, repoTrimmed) &&
			!strings.EqualFold(candidate.RepositoryName, repoTrimmed) &&
			!strings.EqualFold(candidate.ProjectName+"/"+candidate.RepositoryName, repoTrimmed) {
			continue
		}
		matches = append(matches, candidate)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return PullRequestLink{}, invalidArgument(errors.New("specify repositoryId to disambiguate linked pull requests"))
	}
	return PullRequestLink{}, notFound(errors.New("that pull request is not linked to this task"))
}

func (a *AutoFix) getState(ctx context.Context, workspaceID, taskID string, link PullRequestLink) (any, error) {
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	unlock()
	if err != nil {
		return nil, unavailable(err)
	}
	entry := record.PullRequests[prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)]
	return map[string]any{"state": a.toView(ctx, workspaceID, link, entry)}, nil
}

func (a *AutoFix) updateSettings(ctx context.Context, workspaceID, taskID string, link PullRequestLink, input autoFixSelectorInput) (any, error) {
	if input.Enabled == nil && input.RequireApproval == nil {
		return nil, invalidArgument(errors.New("enabled or requireApproval is required"))
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		unlock()
		return nil, unavailable(err)
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	justEnabled := false
	if input.Enabled != nil {
		val := *input.Enabled
		entry.Enabled = &val
		if val {
			justEnabled = true
			if entry.Status == "exhausted" || entry.Status == "paused" {
				entry.Status = "idle"
				entry.RoundCount = 0
			}
			entry.LastError, entry.LastErrorAt = "", nil
		}
	}
	if input.RequireApproval != nil {
		val := *input.RequireApproval
		entry.RequireApproval = &val
	}
	record.PullRequests[entryKey] = entry
	if err := a.saveRecord(ctx, workspaceID, taskID, record); err != nil {
		unlock()
		return nil, unavailable(err)
	}
	view := a.toView(ctx, workspaceID, link, entry)
	unlock()

	if justEnabled && entry.PendingDraft == nil {
		_ = a.pollSinglePR(ctx, workspaceID, taskID, link, false)
		return a.getState(ctx, workspaceID, taskID, link)
	}
	return map[string]any{"state": view}, nil
}

func (a *AutoFix) triggerNow(ctx context.Context, workspaceID, taskID string, link PullRequestLink) (any, error) {
	if err := a.pollSinglePR(ctx, workspaceID, taskID, link, true); err != nil {
		return nil, err
	}
	return a.getState(ctx, workspaceID, taskID, link)
}

func (a *AutoFix) approveDraft(ctx context.Context, workspaceID, taskID string, link PullRequestLink, input autoFixSelectorInput) (any, error) {
	if strings.TrimSpace(input.DraftID) == "" {
		return nil, invalidArgument(errors.New("draftId is required"))
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()

	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return nil, unavailable(err)
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	if entry.PendingDraft == nil || entry.PendingDraft.DraftID != strings.TrimSpace(input.DraftID) {
		return nil, failedPrecondition(errors.New("this remediation draft was already updated or handled; refresh and review again"))
	}

	var client Client
	hasRepliesToSend := false
	for _, reply := range input.Replies {
		if reply.Send && strings.TrimSpace(reply.Content) != "" {
			hasRepliesToSend = true
			break
		}
	}
	if hasRepliesToSend {
		var connErr error
		client, _, connErr = a.connections.Client(ctx, workspaceID)
		if connErr != nil {
			return nil, connErr
		}
	}

	// Fetch current threads once so we know the max comment ID on any replied thread.
	var currentMaxComment map[int]int
	if hasRepliesToSend {
		currentMaxComment = map[int]int{}
		if threads, listErr := client.ListThreads(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID); listErr == nil {
			for _, th := range threads {
				for _, cm := range th.Comments {
					if cm.ID > currentMaxComment[th.ID] {
						currentMaxComment[th.ID] = cm.ID
					}
				}
			}
		}
	}

	if entry.RepliedComments == nil {
		entry.RepliedComments = map[int]int{}
	}
	sentIDs := map[string]bool{}
	var postErr error
	for _, reply := range input.Replies {
		content := strings.TrimSpace(reply.Content)
		if !reply.Send || content == "" {
			sentIDs[reply.ID] = true
			continue
		}
		if reply.ThreadID > 0 {
			posted, replyErr := client.ReplyToThread(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, reply.ThreadID, content)
			if replyErr != nil {
				postErr = browseError(replyErr)
				break
			}
			recordedID := currentMaxComment[reply.ThreadID]
			if posted != nil && posted.ID > recordedID {
				recordedID = posted.ID
			}
			if recordedID > entry.RepliedComments[reply.ThreadID] {
				entry.RepliedComments[reply.ThreadID] = recordedID
			}
			if reply.ResolveThread {
				if statusErr := client.UpdateThreadStatus(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, reply.ThreadID, "fixed"); statusErr != nil {
					sentIDs[reply.ID] = true
					postErr = browseError(statusErr)
					break
				}
			}
		} else {
			created, createErr := client.CreateThread(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, content)
			if createErr != nil {
				postErr = browseError(createErr)
				break
			}
			if created != nil && created.ID > 0 && len(created.Comments) > 0 {
				entry.RepliedComments[created.ID] = created.Comments[len(created.Comments)-1].ID
			}
		}
		sentIDs[reply.ID] = true
	}

	if postErr != nil {
		var remaining []DraftReply
		for _, original := range entry.PendingDraft.Replies {
			if !sentIDs[original.ID] {
				for _, edited := range input.Replies {
					if edited.ID == original.ID {
						original.Content = edited.Content
						original.ResolveThread = edited.ResolveThread
						break
					}
				}
				remaining = append(remaining, original)
			}
		}
		entry.PendingDraft.Replies = remaining
		if input.CommitPush {
			entry.PendingDraft.CommitMessage = input.CommitMessage
		}
		now := a.now().UTC()
		entry.LastError, entry.LastErrorAt = postErr.Error(), &now
		record.PullRequests[entryKey] = entry
		_ = a.saveRecord(ctx, workspaceID, taskID, record)
		return nil, postErr
	}

	// Acknowledge all other active threads from this draft round so keeping threads
	// active or skipping optional replies will not immediately re-trigger on unchanged comments.
	a.acknowledgeCurrentThreads(ctx, workspaceID, link, &entry)

	commitMsg := strings.TrimSpace(input.CommitMessage)
	dispatchedCommit := false
	if input.CommitPush && commitMsg != "" {
		prompt := buildApprovedCommitPushPrompt(link, commitMsg)
		sessionID, sendErr := a.sendToTaskSession(ctx, taskID, entry.LastSessionID, prompt)
		if sendErr != nil {
			entry.PendingDraft.Replies = []DraftReply{}
			entry.PendingDraft.HasChanges = true
			entry.PendingDraft.CommitMessage = commitMsg
			now := a.now().UTC()
			entry.LastError, entry.LastErrorAt = "failed to dispatch commit & push to task session", &now
			record.PullRequests[entryKey] = entry
			_ = a.saveRecord(ctx, workspaceID, taskID, record)
			return nil, unavailable(sendErr)
		}
		if sessionID != "" {
			entry.LastSessionID = sessionID
		}
		dispatchedCommit = true
	}

	entry.PendingDraft = nil
	entry.LastError, entry.LastErrorAt = "", nil
	if dispatchedCommit {
		entry.Status = "applying"
	} else {
		entry.Status = "idle"
	}
	record.PullRequests[entryKey] = entry
	if err := a.saveRecord(ctx, workspaceID, taskID, record); err != nil {
		return nil, unavailable(err)
	}
	return map[string]any{"state": a.toView(ctx, workspaceID, link, entry)}, nil
}

func (a *AutoFix) acknowledgeCurrentThreads(ctx context.Context, workspaceID string, link PullRequestLink, entry *prAutoFixEntry) {
	client, _, err := a.connections.Client(ctx, workspaceID)
	if err != nil {
		return
	}
	threads, err := client.ListThreads(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID)
	if err != nil {
		return
	}
	if entry.AcknowledgedComments == nil {
		entry.AcknowledgedComments = map[int]int{}
	}
	for _, thread := range threads {
		if thread.IsDeleted {
			continue
		}
		maxID := 0
		for _, comment := range thread.Comments {
			if !comment.IsDeleted && !strings.EqualFold(comment.CommentType, "system") && comment.ID > maxID {
				maxID = comment.ID
			}
		}
		if maxID > entry.AcknowledgedComments[thread.ID] {
			entry.AcknowledgedComments[thread.ID] = maxID
		}
	}
}

func (a *AutoFix) reviseDraft(ctx context.Context, workspaceID, taskID string, link PullRequestLink, input autoFixSelectorInput) (any, error) {
	feedback := strings.TrimSpace(input.Feedback)
	if strings.TrimSpace(input.DraftID) == "" || feedback == "" {
		return nil, invalidArgument(errors.New("draftId and non-empty feedback are required"))
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()

	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return nil, unavailable(err)
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	if entry.PendingDraft == nil || entry.PendingDraft.DraftID != strings.TrimSpace(input.DraftID) {
		return nil, failedPrecondition(errors.New("this remediation draft was already updated or handled; refresh and review again"))
	}

	view := a.toView(ctx, workspaceID, link, entry)
	prompt := buildRevisionRequestPrompt(link, *entry.PendingDraft, feedback, view.RequireApproval)
	sessionID, sendErr := a.sendToTaskSession(ctx, taskID, entry.LastSessionID, prompt)
	if sendErr != nil {
		return nil, unavailable(sendErr)
	}
	if sessionID != "" {
		entry.LastSessionID = sessionID
	}
	entry.PendingDraft = nil
	entry.Status = "running"
	entry.LastError, entry.LastErrorAt = "", nil
	record.PullRequests[entryKey] = entry
	if err := a.saveRecord(ctx, workspaceID, taskID, record); err != nil {
		return nil, unavailable(err)
	}
	return map[string]any{"state": a.toView(ctx, workspaceID, link, entry)}, nil
}

func (a *AutoFix) dismissDraft(ctx context.Context, workspaceID, taskID string, link PullRequestLink, draftID string) (any, error) {
	if strings.TrimSpace(draftID) == "" {
		return nil, invalidArgument(errors.New("draftId is required"))
	}
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()

	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return nil, unavailable(err)
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	if entry.PendingDraft != nil && entry.PendingDraft.DraftID == strings.TrimSpace(draftID) {
		entry.PendingDraft = nil
	}
	a.acknowledgeCurrentThreads(ctx, workspaceID, link, &entry)
	entry.Status = "idle"
	entry.LastError, entry.LastErrorAt = "", nil
	record.PullRequests[entryKey] = entry
	if err := a.saveRecord(ctx, workspaceID, taskID, record); err != nil {
		return nil, unavailable(err)
	}
	return map[string]any{"state": a.toView(ctx, workspaceID, link, entry)}, nil
}

func (a *AutoFix) manualReply(ctx context.Context, workspaceID, taskID string, link PullRequestLink, input autoFixSelectorInput) (any, error) {
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return nil, invalidArgument(errors.New("reply content is required"))
	}
	threadID := input.ThreadID
	if threadID <= 0 && strings.TrimSpace(input.ThreadRef) != "" {
		parsed, ok := parseThreadIDPrefix(input.ThreadRef)
		if !ok {
			return nil, invalidArgument(errors.New("thread reference is not valid"))
		}
		threadID = parsed
	}
	client, _, err := a.connections.Client(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	postedCommentID := 0
	if threadID > 0 {
		posted, replyErr := client.ReplyToThread(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, threadID, content)
		if replyErr != nil {
			return nil, browseError(replyErr)
		}
		if posted != nil {
			postedCommentID = posted.ID
		}
		if input.ResolveThread {
			if statusErr := client.UpdateThreadStatus(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, threadID, "fixed"); statusErr != nil {
				return nil, browseError(statusErr)
			}
		}
	} else {
		created, createErr := client.CreateThread(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID, content)
		if createErr != nil {
			return nil, browseError(createErr)
		}
		if created != nil {
			threadID = created.ID
			if len(created.Comments) > 0 {
				postedCommentID = created.Comments[len(created.Comments)-1].ID
			}
		}
	}

	if threadID > 0 {
		unlock := a.locks.lock(workspaceID + "/" + taskID)
		if record, loadErr := a.loadRecord(ctx, workspaceID, taskID); loadErr == nil {
			entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
			entry := record.PullRequests[entryKey]
			if entry.RepliedComments == nil {
				entry.RepliedComments = map[int]int{}
			}
			if postedCommentID > entry.RepliedComments[threadID] {
				entry.RepliedComments[threadID] = postedCommentID
			}
			record.PullRequests[entryKey] = entry
			_ = a.saveRecord(ctx, workspaceID, taskID, record)
		}
		unlock()
	}
	return map[string]any{"threadId": threadID, "commentId": postedCommentID}, nil
}

func parseThreadIDPrefix(ref string) (int, bool) {
	head := strings.TrimSpace(ref)
	if idx := strings.IndexByte(head, '-'); idx > 0 {
		head = head[:idx]
	}
	id, err := strconv.Atoi(head)
	return id, err == nil && id > 0
}

func (a *AutoFix) sendToTaskSession(ctx context.Context, taskID, preferredSessionID, text string) (string, error) {
	sender := a.messages()
	if sender == nil {
		return "", errors.New("task messaging is unavailable")
	}
	if preferredSessionID != "" {
		dispatch, err := sender.Send(ctx, taskID, preferredSessionID, text)
		if err == nil && dispatch != nil {
			return dispatch.SessionID, nil
		}
		if status.Code(err) != codes.NotFound && status.Code(err) != codes.FailedPrecondition {
			return "", err
		}
	}
	dispatch, err := sender.Send(ctx, taskID, "", text)
	if err != nil {
		return "", err
	}
	if dispatch == nil {
		return "", nil
	}
	return dispatch.SessionID, nil
}

// ── Agent Tools ──────────────────────────────────────────────────────────

type saveRemediationDraftArgs struct {
	PullRequestID int    `json:"pullRequestId"`
	Repository    string `json:"repository"`
	Summary       string `json:"summary"`
	HasChanges    bool   `json:"hasChanges"`
	CommitMessage string `json:"commitMessage"`
	Replies       []struct {
		ThreadID int    `json:"threadId"`
		Content  string `json:"content"`
	} `json:"replies"`
}

// InvokeSaveRemediationDraft records a structured commit + reply draft for human approval.
func (a *AutoFix) InvokeSaveRemediationDraft(ctx context.Context, request *pluginsdk.AgentToolRequest) *pluginsdk.AgentToolResult {
	var args saveRemediationDraftArgs
	if err := decodeJSONMap(request.Arguments, &args); err != nil {
		return toolError("the arguments are not valid: %v", err)
	}
	workspaceID, taskID := request.Context.WorkspaceID, request.Context.TaskID
	if workspaceID == "" || taskID == "" {
		return toolError("this tool only works inside a Kandev task")
	}
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		return toolError("summary is required")
	}
	if len(args.Replies) > maxDraftReplies {
		return toolError("at most %d replies can be proposed in one draft", maxDraftReplies)
	}
	link, err := a.findLinkedPR(ctx, workspaceID, taskID, args.Repository, args.PullRequestID)
	if err != nil {
		return toolError("%s", publicMessage(err))
	}

	threadMeta := map[int]azure.Thread{}
	if len(args.Replies) > 0 {
		if client, _, connErr := a.connections.Client(ctx, workspaceID); connErr == nil {
			if threads, listErr := client.ListThreads(ctx, link.ProjectID, link.RepositoryID, link.PullRequestID); listErr == nil {
				for _, th := range threads {
					threadMeta[th.ID] = th
				}
			}
		}
	}

	draftReplies := make([]DraftReply, 0, len(args.Replies))
	for i, raw := range args.Replies {
		content := strings.TrimSpace(raw.Content)
		if content == "" {
			continue
		}
		if raw.ThreadID < 0 {
			return toolError("replies[%d].threadId must be 0 or a positive thread ID", i)
		}
		item := DraftReply{
			ID:            newID(),
			ThreadID:      raw.ThreadID,
			Content:       content,
			ResolveThread: false, // Always default to keep_active for human review.
		}
		if th, found := threadMeta[raw.ThreadID]; found {
			item.FilePath = th.FilePath
			item.Line = th.Line
			if author, excerpt := latestHumanCommentExcerpt(th); excerpt != "" {
				item.Author = author
				item.ThreadExcerpt = excerpt
			}
		}
		draftReplies = append(draftReplies, item)
	}

	commitMsg := strings.TrimSpace(args.CommitMessage)
	hasChanges := args.HasChanges || commitMsg != ""
	draft := &RemediationDraft{
		DraftID:       newID(),
		Summary:       summary,
		HasChanges:    hasChanges,
		CommitMessage: commitMsg,
		Replies:       draftReplies,
		CreatedAt:     a.now().UTC(),
	}

	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, loadErr := a.loadRecord(ctx, workspaceID, taskID)
	if loadErr != nil {
		unlock()
		return toolError("could not load pull request remediation state")
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	entry.PendingDraft = draft
	entry.Status = "awaiting_approval"
	entry.LastError, entry.LastErrorAt = "", nil
	if request.Context.SessionID != "" {
		entry.LastSessionID = request.Context.SessionID
	}
	record.PullRequests[entryKey] = entry
	saveErr := a.saveRecord(ctx, workspaceID, taskID, record)
	unlock()
	if saveErr != nil {
		return toolError("could not save pull request remediation draft")
	}

	return &pluginsdk.AgentToolResult{
		Text: fmt.Sprintf(
			"Saved remediation draft %s for pull request !%d (%d proposed reply/replies, hasChanges=%v). Stop here and wait for human review in Kandev's Azure DevOps PR panel — do NOT run git commit, git push, or post comments.",
			draft.DraftID, link.PullRequestID, len(draftReplies), hasChanges,
		),
		StructuredContent: map[string]any{
			"draftId":       draft.DraftID,
			"pullRequestId": link.PullRequestID,
			"hasChanges":    hasChanges,
			"replyCount":    len(draftReplies),
			"status":        "awaiting_approval",
		},
	}
}

type replyPullRequestThreadArgs struct {
	PullRequestID int    `json:"pullRequestId"`
	Repository    string `json:"repository"`
	ThreadID      int    `json:"threadId"`
	Content       string `json:"content"`
	ResolveThread bool   `json:"resolveThread"`
}

// InvokeReplyPullRequestThread posts a PR comment reply ONLY when human approval is disabled.
func (a *AutoFix) InvokeReplyPullRequestThread(ctx context.Context, request *pluginsdk.AgentToolRequest) *pluginsdk.AgentToolResult {
	var args replyPullRequestThreadArgs
	if err := decodeJSONMap(request.Arguments, &args); err != nil {
		return toolError("the arguments are not valid: %v", err)
	}
	workspaceID, taskID := request.Context.WorkspaceID, request.Context.TaskID
	if workspaceID == "" || taskID == "" {
		return toolError("this tool only works inside a Kandev task")
	}
	content := strings.TrimSpace(args.Content)
	if content == "" {
		return toolError("content is required")
	}
	if args.ThreadID < 0 {
		return toolError("threadId must be 0 or a positive thread ID")
	}
	link, err := a.findLinkedPR(ctx, workspaceID, taskID, args.Repository, args.PullRequestID)
	if err != nil {
		return toolError("%s", publicMessage(err))
	}

	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, loadErr := a.loadRecord(ctx, workspaceID, taskID)
	if loadErr != nil {
		unlock()
		return toolError("could not read pull request approval settings")
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	view := a.toView(ctx, workspaceID, link, entry)
	unlock()

	if view.RequireApproval {
		return toolError(
			"human approval is enabled for pull request !%d: direct replies are blocked. Leave working-tree changes uncommitted and call %s so the human can review and approve.",
			link.PullRequestID, SaveRemediationDraftTool,
		)
	}

	result, replyErr := a.manualReply(ctx, workspaceID, taskID, link, autoFixSelectorInput{
		RepositoryID:  link.RepositoryID,
		PullRequestID: link.PullRequestID,
		ThreadID:      args.ThreadID,
		Content:       content,
		ResolveThread: args.ResolveThread,
	})
	if replyErr != nil {
		return toolError("the pull request reply could not be posted: %s", publicMessage(replyErr))
	}
	out, _ := result.(map[string]any)
	return &pluginsdk.AgentToolResult{
		Text: fmt.Sprintf("Posted reply to pull request !%d (thread %v).", link.PullRequestID, out["threadId"]),
		StructuredContent: map[string]any{
			"pullRequestId": link.PullRequestID,
			"threadId":      out["threadId"],
			"commentId":     out["commentId"],
			"resolved":      args.ResolveThread,
		},
	}
}

func latestHumanCommentExcerpt(thread azure.Thread) (author string, excerpt string) {
	for i := len(thread.Comments) - 1; i >= 0; i-- {
		c := thread.Comments[i]
		if c.IsDeleted || strings.EqualFold(c.CommentType, "system") {
			continue
		}
		text := strings.Join(strings.Fields(c.Content), " ")
		if text == "" {
			continue
		}
		if len(text) > maxThreadExcerptChars {
			text = text[:maxThreadExcerptChars] + "..."
		}
		return strings.TrimSpace(c.Author.DisplayName), text
	}
	return "", ""
}
