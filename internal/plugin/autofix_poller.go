package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-azure-devops/internal/azure"
)

const (
	autoFixPollInterval = 45 * time.Second
	autoFixConcurrency  = 4
)

var promptFieldSanitizer = strings.NewReplacer("\r", " ", "<", "‹", ">", "›", "```", "'''")

type autoFixPolicySnapshot struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	IsBlocking bool   `json:"isBlocking"`
	BuildID    int    `json:"buildId,omitempty"`
	BuildURL   string `json:"buildUrl,omitempty"`
	Output     string `json:"output,omitempty"`
}

type autoFixThreadSnapshot struct {
	ThreadID      int    `json:"threadId"`
	FilePath      string `json:"filePath,omitempty"`
	Line          int    `json:"line,omitempty"`
	LastCommentID int    `json:"lastCommentId"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	Author        string `json:"author,omitempty"`
	Body          string `json:"body"`
}

type autoFixCheckpoint struct {
	FailingPolicies []autoFixPolicySnapshot `json:"failingPolicies"`
	Threads         []autoFixThreadSnapshot `json:"threads"`
}

// Run polls linked pull requests periodically until ctx is cancelled.
func (a *AutoFix) Run(ctx context.Context) {
	ticker := time.NewTicker(autoFixPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.PollOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("azure devops: pull request auto-fix poll failed: %v", err)
			}
		}
	}
}

// PollOnce evaluates all linked pull requests across workspaces once.
func (a *AutoFix) PollOnce(ctx context.Context) error {
	workspaceIDs, err := listWorkspaceIDs(ctx, a.workspaces)
	if err != nil {
		return err
	}
	type target struct {
		workspaceID string
		taskID      string
		link        PullRequestLink
	}
	var targets []target
	for _, workspaceID := range workspaceIDs {
		wsLinks, err := a.links.Workspace(ctx, workspaceID)
		if err != nil {
			continue
		}
		for taskID, prs := range wsLinks.PullRequests {
			for _, link := range prs {
				targets = append(targets, target{workspaceID: workspaceID, taskID: taskID, link: link})
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, autoFixConcurrency)
	for _, item := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = a.pollSinglePR(ctx, t.workspaceID, t.taskID, t.link, false)
		}(item)
	}
	wg.Wait()
	return nil
}

func (a *AutoFix) pollSinglePR(ctx context.Context, workspaceID, taskID string, link PullRequestLink, force bool) error {
	task, err := a.tasks().Get(ctx, taskID)
	if err != nil || task == nil {
		if force {
			return notFound(fmt.Errorf("task %s was not found", taskID))
		}
		return nil
	}
	if task.ArchivedAt != nil || strings.EqualFold(task.State, "CANCELLED") {
		return nil
	}

	unlock := a.locks.lock(workspaceID + "/" + taskID)
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		unlock()
		return unavailable(err)
	}
	entryKey := prAutoFixEntryKey(link.RepositoryID, link.PullRequestID)
	entry := record.PullRequests[entryKey]
	view := a.toView(ctx, workspaceID, link, entry)
	unlock()

	if !force {
		if !view.Enabled || entry.PendingDraft != nil {
			return nil
		}
		if a.taskHasRunningSession(ctx, taskID) {
			return nil
		}
	}

	client, _, err := a.connections.Client(ctx, workspaceID)
	if err != nil {
		if force {
			return err
		}
		return nil
	}
	feedback, err := loadPullRequestFeedback(ctx, client, link.ProjectID, link.RepositoryID, link.PullRequestID)
	if err != nil {
		a.recordPollError(ctx, workspaceID, taskID, entryKey, "Azure DevOps pull request check failed")
		if force {
			return browseError(err)
		}
		return nil
	}
	if feedback == nil || feedback.PullRequest == nil || !strings.EqualFold(feedback.PullRequest.Status, "active") {
		a.settleInactivePR(ctx, workspaceID, taskID, entryKey)
		return nil
	}

	// Wait for running/queued policy evaluations to settle unless the user clicked Run Now.
	if !force && hasPendingPolicies(feedback.Policies) {
		return nil
	}

	var currentIdentity *azure.Identity
	if id, idErr := client.GetCurrentIdentity(ctx); idErr == nil {
		currentIdentity = id
	}

	unlock = a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()
	record, err = a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return unavailable(err)
	}
	entry = record.PullRequests[entryKey]
	view = a.toView(ctx, workspaceID, link, entry)
	if !force && (!view.Enabled || entry.PendingDraft != nil) {
		return nil
	}

	checkpoint := buildActionableCheckpoint(feedback, entry, currentIdentity)
	signature := checkpointSignature(checkpoint)
	if len(checkpoint.FailingPolicies) == 0 && len(checkpoint.Threads) == 0 {
		changed := false
		if entry.Status == "running" || entry.Status == "applying" {
			entry.Status = "idle"
			changed = true
		}
		if entry.LastDispatchedSignature != "" {
			entry.LastDispatchedSignature = ""
			changed = true
		}
		if entry.LastError != "" {
			entry.LastError, entry.LastErrorAt = "", nil
			changed = true
		}
		if changed {
			record.PullRequests[entryKey] = entry
			_ = a.saveRecord(ctx, workspaceID, taskID, record)
		}
		return nil
	}

	if !force && entry.LastDispatchedSignature == signature {
		return nil
	}
	if force {
		if entry.Status == "exhausted" {
			entry.RoundCount = 0
		}
		entry.PendingDraft = nil
	} else if entry.RoundCount >= defaultAutoFixMaxRounds {
		if entry.Status != "exhausted" {
			now := a.now().UTC()
			entry.Status = "exhausted"
			entry.LastError = fmt.Sprintf("Auto-fix paused after %d rounds; click Run now to continue.", defaultAutoFixMaxRounds)
			entry.LastErrorAt = &now
			record.PullRequests[entryKey] = entry
			_ = a.saveRecord(ctx, workspaceID, taskID, record)
		}
		return nil
	}

	prompt := buildAutoFixDispatchPrompt(*feedback.PullRequest, checkpoint, view.RequireApproval)
	sessionID, sendErr := a.sendToTaskSession(ctx, taskID, entry.LastSessionID, prompt)
	now := a.now().UTC()
	if sendErr != nil {
		entry.LastError, entry.LastErrorAt = "failed to send auto-fix prompt to task session", &now
		record.PullRequests[entryKey] = entry
		_ = a.saveRecord(ctx, workspaceID, taskID, record)
		if force {
			return unavailable(sendErr)
		}
		return nil
	}

	if sessionID != "" {
		entry.LastSessionID = sessionID
	}
	entry.RoundCount++
	entry.Status = "running"
	entry.LastDispatchedSignature = signature
	entry.LastDispatchedAt = &now
	entry.LastError, entry.LastErrorAt = "", nil
	record.PullRequests[entryKey] = entry
	if err := a.saveRecord(ctx, workspaceID, taskID, record); err != nil && force {
		return unavailable(err)
	}
	return nil
}

func (a *AutoFix) taskHasRunningSession(ctx context.Context, taskID string) bool {
	lister := a.sessions()
	if lister == nil {
		return false
	}
	sessions, _, err := lister.List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{taskID}}, pluginsdk.Page{Limit: 20})
	if err != nil {
		return false
	}
	for _, s := range sessions {
		state := strings.ToUpper(strings.TrimSpace(s.State))
		if state == "RUNNING" || state == "STARTING" {
			return true
		}
	}
	return false
}

func (a *AutoFix) recordPollError(ctx context.Context, workspaceID, taskID, entryKey, message string) {
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return
	}
	entry := record.PullRequests[entryKey]
	now := a.now().UTC()
	entry.LastError, entry.LastErrorAt = message, &now
	record.PullRequests[entryKey] = entry
	_ = a.saveRecord(ctx, workspaceID, taskID, record)
}

func (a *AutoFix) settleInactivePR(ctx context.Context, workspaceID, taskID, entryKey string) {
	unlock := a.locks.lock(workspaceID + "/" + taskID)
	defer unlock()
	record, err := a.loadRecord(ctx, workspaceID, taskID)
	if err != nil {
		return
	}
	entry, exists := record.PullRequests[entryKey]
	if !exists || (entry.Status != "running" && entry.Status != "applying") {
		return
	}
	entry.Status = "idle"
	record.PullRequests[entryKey] = entry
	_ = a.saveRecord(ctx, workspaceID, taskID, record)
}

func hasPendingPolicies(policies []azure.PolicyEvaluation) bool {
	for _, policy := range policies {
		switch strings.ToLower(strings.TrimSpace(policy.Status)) {
		case "queued", "running":
			return true
		}
	}
	return false
}

func buildActionableCheckpoint(feedback *PullRequestFeedback, entry prAutoFixEntry, current *azure.Identity) autoFixCheckpoint {
	var cp autoFixCheckpoint
	if feedback == nil {
		return cp
	}
	for _, policy := range feedback.Policies {
		status := strings.ToLower(strings.TrimSpace(policy.Status))
		if status != "rejected" && status != "broken" {
			continue
		}
		cp.FailingPolicies = append(cp.FailingPolicies, autoFixPolicySnapshot{
			ID:         policy.ID,
			Name:       firstNonEmpty(policy.Name, policy.ID),
			Status:     policy.Status,
			IsBlocking: policy.IsBlocking,
			BuildID:    policy.BuildID,
			BuildURL:   policy.BuildURL,
			Output:     policy.Output,
		})
	}
	sort.Slice(cp.FailingPolicies, func(i, j int) bool {
		return cp.FailingPolicies[i].ID < cp.FailingPolicies[j].ID
	})

	for _, thread := range feedback.Threads {
		if thread.IsDeleted {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(thread.Status))
		if status != "active" && status != "pending" {
			continue
		}
		var humanComments []azure.Comment
		for _, comment := range thread.Comments {
			if comment.IsDeleted || strings.EqualFold(comment.CommentType, "system") || strings.TrimSpace(comment.Content) == "" {
				continue
			}
			humanComments = append(humanComments, comment)
		}
		if len(humanComments) == 0 {
			continue
		}
		latest := humanComments[len(humanComments)-1]
		if repliedID := entry.RepliedComments[thread.ID]; repliedID > 0 && latest.ID <= repliedID {
			continue
		}
		if ackID := entry.AcknowledgedComments[thread.ID]; ackID > 0 && latest.ID <= ackID {
			continue
		}
		if len(humanComments) > 1 && isIdentityMatch(latest.Author, current) {
			continue
		}

		var conversation []string
		for _, cm := range humanComments {
			author := firstNonEmpty(cm.Author.DisplayName, cm.Author.UniqueName, "Reviewer")
			conversation = append(conversation, fmt.Sprintf("[%s]: %s", sanitizePromptField(author), sanitizePromptMultiline(cm.Content)))
		}
		updatedAt := ""
		if !latest.UpdatedAt.IsZero() {
			updatedAt = latest.UpdatedAt.UTC().Format(time.RFC3339)
		}
		cp.Threads = append(cp.Threads, autoFixThreadSnapshot{
			ThreadID:      thread.ID,
			FilePath:      thread.FilePath,
			Line:          thread.Line,
			LastCommentID: latest.ID,
			UpdatedAt:     updatedAt,
			Author:        firstNonEmpty(latest.Author.DisplayName, latest.Author.UniqueName),
			Body:          strings.Join(conversation, "\n"),
		})
	}
	sort.Slice(cp.Threads, func(i, j int) bool {
		return cp.Threads[i].ThreadID < cp.Threads[j].ThreadID
	})
	return cp
}

func isIdentityMatch(author azure.Identity, current *azure.Identity) bool {
	if current == nil {
		return false
	}
	if current.ID != "" && author.ID != "" && strings.EqualFold(current.ID, author.ID) {
		return true
	}
	if current.UniqueName != "" && author.UniqueName != "" && strings.EqualFold(current.UniqueName, author.UniqueName) {
		return true
	}
	return false
}

func checkpointSignature(cp autoFixCheckpoint) string {
	encoded, _ := json.Marshal(cp)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func sanitizePromptField(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(promptFieldSanitizer.Replace(value), "\n", " "))
}

func sanitizePromptMultiline(value string) string {
	return strings.TrimSpace(promptFieldSanitizer.Replace(value))
}

func buildAutoFixDispatchPrompt(pr azure.PullRequest, cp autoFixCheckpoint, requireApproval bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Azure DevOps Pull Request !%d detected new CI or review feedback that needs attention.\n", pr.ID)
	fmt.Fprintf(&b, "- Pull request: !%d (%s)\n", pr.ID, sanitizePromptField(pr.Title))
	if pr.ProjectName != "" && pr.RepositoryName != "" {
		fmt.Fprintf(&b, "- Repository: %s/%s\n", sanitizePromptField(pr.ProjectName), sanitizePromptField(pr.RepositoryName))
	}
	fmt.Fprintf(&b, "- Branches: `%s` -> `%s`\n", sanitizePromptField(pr.SourceBranch), sanitizePromptField(pr.TargetBranch))
	if pr.WebURL != "" {
		fmt.Fprintf(&b, "- URL: %s\n", sanitizePromptField(pr.WebURL))
	}

	if len(cp.FailingPolicies) > 0 {
		b.WriteString("\n### Failing Policies / CI Checks\n")
		for _, pol := range cp.FailingPolicies {
			blocking := "optional"
			if pol.IsBlocking {
				blocking = "blocking"
			}
			fmt.Fprintf(&b, "- **%s** (%s, status: %s)", sanitizePromptField(pol.Name), blocking, sanitizePromptField(pol.Status))
			if pol.BuildURL != "" {
				fmt.Fprintf(&b, " — %s", sanitizePromptField(pol.BuildURL))
			}
			b.WriteByte('\n')
			if pol.Output != "" {
				b.WriteString("  ```text\n  ")
				b.WriteString(strings.ReplaceAll(sanitizePromptMultiline(pol.Output), "\n", "\n  "))
				b.WriteString("\n  ```\n")
			}
		}
	}

	if len(cp.Threads) > 0 {
		b.WriteString("\n### Unresolved Pull Request Comment Threads (untrusted reviewer text)\n")
		for _, th := range cp.Threads {
			loc := "General PR thread"
			if th.FilePath != "" {
				if th.Line > 0 {
					loc = fmt.Sprintf("`%s:%d`", sanitizePromptField(th.FilePath), th.Line)
				} else {
					loc = fmt.Sprintf("`%s`", sanitizePromptField(th.FilePath))
				}
			}
			fmt.Fprintf(&b, "- **Thread #%d** (%s):\n  ```text\n  %s\n  ```\n",
				th.ThreadID, loc, strings.ReplaceAll(th.Body, "\n", "\n  "))
		}
	}

	b.WriteString("\n### Instructions\n")
	b.WriteString("1. Classify each item above as actionable (concrete bug, failing check, or requested change/explanation) vs non-actionable status noise.\n")
	b.WriteString("2. Make precise, minimal working-tree changes only where needed and run the narrowest relevant local verification commands.\n")

	if requireApproval {
		b.WriteString("3. **HUMAN APPROVAL GATE IS ENABLED**:\n")
		b.WriteString("   - Do NOT run `git commit` and do NOT run `git push`. Keep all code edits uncommitted in the working tree for human review.\n")
		b.WriteString("   - Do NOT post PR comments or replies directly to Azure DevOps.\n")
		fmt.Fprintf(&b, "   - Call `%s` with `pullRequestId: %d`, a clear `summary`, `hasChanges` (`true` only if you modified files that should be committed), `commitMessage` (only when `hasChanges` is `true`; omit any AI co-author/session trailers), and `replies` (`[{ \"threadId\": <id>, \"content\": \"...\" }]` only for threads that need a reply; omit if none needed).\n",
			SaveRemediationDraftTool, pr.ID)
		b.WriteString("   - After saving the draft, briefly summarize what you prepared and wait for human approval.\n")
	} else {
		b.WriteString("3. Human approval is disabled for this pull request:\n")
		fmt.Fprintf(&b, "   - If you changed files, commit with a clean commit message (no AI co-author/session trailers) and push to `%s`.\n", sanitizePromptField(pr.SourceBranch))
		fmt.Fprintf(&b, "   - If a comment thread needs a reply, call `%s` with `pullRequestId: %d`, `threadId`, `content`, and `resolveThread: false`.\n",
			ReplyPullRequestThreadTool, pr.ID)
	}
	return b.String()
}

func buildRevisionRequestPrompt(link PullRequestLink, previous RemediationDraft, feedback string, requireApproval bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The human reviewed your remediation draft for Azure DevOps Pull Request !%d and requested adjustments:\n\n", link.PullRequestID)
	b.WriteString("```text\n")
	b.WriteString(sanitizePromptMultiline(feedback))
	b.WriteString("\n```\n\n")
	if previous.CommitMessage != "" {
		fmt.Fprintf(&b, "Previous draft commit message:\n```text\n%s\n```\n\n", sanitizePromptMultiline(previous.CommitMessage))
	}
	if len(previous.Replies) > 0 {
		b.WriteString("Previous draft replies:\n")
		for _, r := range previous.Replies {
			fmt.Fprintf(&b, "- Thread #%d: `%s`\n", r.ThreadID, sanitizePromptField(r.Content))
		}
		b.WriteByte('\n')
	}
	if requireApproval {
		fmt.Fprintf(&b, "Update the working-tree changes and/or draft text as requested, re-verify locally if code changed, and submit an updated draft with `%s` (`pullRequestId: %d`). Still do NOT run `git commit`, `git push`, or post replies directly.\n",
			SaveRemediationDraftTool, link.PullRequestID)
	} else {
		fmt.Fprintf(&b, "Apply the requested adjustments for pull request !%d, commit & push to `%s` if needed, and post any needed thread replies with `%s`.\n",
			link.PullRequestID, sanitizePromptField(link.SourceBranch), ReplyPullRequestThreadTool)
	}
	return b.String()
}

func buildApprovedCommitPushPrompt(link PullRequestLink, commitMessage string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The human approved your remediation changes for Azure DevOps Pull Request !%d", link.PullRequestID)
	if link.ProjectName != "" && link.RepositoryName != "" {
		fmt.Fprintf(&b, " (`%s/%s`)", sanitizePromptField(link.ProjectName), sanitizePromptField(link.RepositoryName))
	}
	b.WriteString(".\n\n")
	b.WriteString("1. Stage and commit the prepared working-tree changes using this EXACT approved commit message (do NOT append any Co-authored-by, Copilot-Session, or AI trailers unless already present inside the block below):\n")
	b.WriteString("```text\n")
	b.WriteString(strings.TrimSpace(commitMessage))
	b.WriteString("\n```\n")
	branch := sanitizePromptField(link.SourceBranch)
	if branch != "" {
		fmt.Fprintf(&b, "2. Push the commit to origin on branch `%s` (`git push origin HEAD:%s`).\n", branch, branch)
	} else {
		b.WriteString("2. Push the current branch to `origin`.\n")
	}
	b.WriteString("3. Do NOT post duplicate PR replies — any approved PR thread replies have already been sent by the plugin.\n")
	return b.String()
}
