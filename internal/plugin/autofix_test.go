package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"

	"kandev-plugin-azure-devops/internal/azure"
)

func setupLinkedPRForAutoFix(t *testing.T) (*harness, pluginsdk.VerifiedActionContext) {
	t.Helper()
	h := newHarness(t)
	h.host.AddWorkspace("ws-1")
	h.host.AddRepository(pluginsdk.Repository{
		ID: "host-repo-1", WorkspaceID: "ws-1", Name: "api", ProviderID: ProviderID,
		RemoteURL: "https://dev.azure.com/acme/Platform%20Team/_git/api",
	})
	h.host.AddTask(pluginsdk.Task{
		ID: "task-1", WorkspaceID: "ws-1", Title: "Fix auth token flow", State: "IN_PROGRESS",
		Repositories: []pluginsdk.TaskRepository{{RepositoryID: "host-repo-1", BaseBranch: "main"}},
	})
	h.host.AddSession(pluginsdk.Session{
		ID: "session-1", TaskID: "task-1", State: "IDLE",
	})
	h.connect("ws-1")

	h.azure.mu.Lock()
	h.azure.pullRequests = map[int]azure.PullRequest{
		42: {
			ID: 42, Title: "Fix auth token flow", Status: "active",
			SourceBranch: "feature/auth", TargetBranch: "main",
			ProjectID: "p1", ProjectName: "Platform Team",
			RepositoryID: "guid-api", RepositoryName: "api",
			WebURL: "https://dev.azure.com/acme/Platform%20Team/_git/api/pullrequest/42",
		},
	}
	h.azure.prPolicies = map[int][]azure.PolicyEvaluation{}
	h.azure.prThreads = map[int][]azure.Thread{}
	h.azure.mu.Unlock()

	taskCtx := pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1"}
	status, _ := h.actionWith(taskCtx, "pull_requests.link", map[string]any{
		"projectId": "p1", "repositoryId": "guid-api", "pullRequestId": 42,
	})
	require.Equal(t, 200, status)
	return h, taskCtx
}

func TestAutoFixDefaultsToEnabledWithHumanApprovalAndSettlesPendingChecks(t *testing.T) {
	h, taskCtx := setupLinkedPRForAutoFix(t)

	status, body := h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{
		"repositoryId": "guid-api", "pullRequestId": 42,
	})
	require.Equal(t, 200, status)
	require.Equal(t, true, field(body, "state", "enabled"))
	require.Equal(t, true, field(body, "state", "requireApproval"))
	require.Equal(t, "idle", field(body, "state", "status"))

	// Pending policy prevents premature dispatch even when a comment exists.
	h.azure.mu.Lock()
	h.azure.prPolicies[42] = []azure.PolicyEvaluation{
		{ID: "eval-1", Name: "Unit Tests", Status: "running", IsBlocking: true},
	}
	h.azure.prThreads[42] = []azure.Thread{{
		ID: 7, Status: "active", FilePath: "src/auth.ts", Line: 29,
		Comments: []azure.Comment{{
			ID: 1, Content: "Check nil token", CommentType: "text",
			Author: azure.Identity{ID: "reviewer-1", DisplayName: "Grace"},
		}},
	}}
	h.azure.mu.Unlock()

	services, err := h.runtime.ready()
	require.NoError(t, err)
	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Empty(t, h.host.SentMessages(), "must wait for pending policy checks to settle")

	// Once the build policy settles as rejected, auto-fix dispatches exactly once.
	h.azure.mu.Lock()
	h.azure.prPolicies[42] = []azure.PolicyEvaluation{{
		ID: "eval-1", Name: "Unit Tests", Status: "rejected", IsBlocking: true,
		BuildID: 777, BuildURL: "https://dev.azure.com/acme/p1/_build/results?buildId=777",
		Output: "[Run vitest] FAIL src/auth.test.ts",
	}}
	h.azure.mu.Unlock()

	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	messages := h.host.SentMessages()
	require.Len(t, messages, 1)
	require.Equal(t, "task-1", messages[0].TaskID)
	require.Contains(t, messages[0].Text, "Unit Tests")
	require.Contains(t, messages[0].Text, "[Run vitest] FAIL src/auth.test.ts")
	require.Contains(t, messages[0].Text, "src/auth.ts:29")
	require.Contains(t, messages[0].Text, "Check nil token")
	require.Contains(t, messages[0].Text, SaveRemediationDraftTool)
	require.Contains(t, messages[0].Text, "Do NOT run `git commit`")

	// Repeated poll on identical PR state must not re-dispatch.
	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Len(t, h.host.SentMessages(), 1)
}

func TestAutoFixHumanReviewLoopReviseApproveAndKeepActiveWithoutRetriggering(t *testing.T) {
	h, taskCtx := setupLinkedPRForAutoFix(t)
	h.azure.mu.Lock()
	h.azure.prPolicies[42] = []azure.PolicyEvaluation{
		{ID: "eval-ok", Name: "Build", Status: "approved", IsBlocking: true},
	}
	h.azure.prThreads[42] = []azure.Thread{{
		ID: 7, Status: "active", FilePath: "src/auth.ts", Line: 29,
		Comments: []azure.Comment{{
			ID: 1, Content: "Please guard empty bearer header", CommentType: "text",
			Author: azure.Identity{ID: "reviewer-1", DisplayName: "Grace"},
		}},
	}}
	h.azure.mu.Unlock()

	services, err := h.runtime.ready()
	require.NoError(t, err)
	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Len(t, h.host.SentMessages(), 1)

	// Hard backend gate: direct reply tool is blocked while requireApproval is true.
	blocked, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    ReplyPullRequestThreadTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"pullRequestId": 42, "threadId": 7, "content": "Tried to reply without human approval",
		},
	})
	require.NoError(t, err)
	require.True(t, blocked.IsError)
	require.Empty(t, h.azure.threadReplies)

	// Agent submits remediation draft via save_pr_remediation_draft.
	draftRes, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    SaveRemediationDraftTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"pullRequestId": 42,
			"summary":       "Added empty-bearer guard in src/auth.ts and unit test.",
			"hasChanges":    true,
			"commitMessage": "fix: guard empty bearer header",
			"replies": []map[string]any{
				{"threadId": 7, "content": "Added an empty bearer check and unit test."},
			},
		},
	})
	require.NoError(t, err)
	require.False(t, draftRes.IsError, draftRes.Text)

	status, body := h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{
		"repositoryId": "guid-api", "pullRequestId": 42,
	})
	require.Equal(t, 200, status)
	require.Equal(t, "awaiting_approval", field(body, "state", "status"))
	draftID, _ := field(body, "state", "pendingDraft", "draftId").(string)
	require.NotEmpty(t, draftID)
	replies, _ := field(body, "state", "pendingDraft", "replies").([]any)
	require.Len(t, replies, 1)
	firstReply := replies[0].(map[string]any)
	require.Equal(t, false, firstReply["resolveThread"], "resolveThread must default to false (keep_active)")
	require.Equal(t, "src/auth.ts", firstReply["filePath"])

	// While awaiting human approval, even if new comments arrive, poller must not overwrite the draft.
	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Len(t, h.host.SentMessages(), 1)

	// Human requests revisions with feedback -> sends prompt to agent and clears pending draft.
	status, _ = h.actionWith(taskCtx, "pull_requests.autofix.revise", map[string]any{
		"repositoryId":  "guid-api",
		"pullRequestId": 42,
		"draftId":       draftID,
		"feedback":      "Use fix(auth): prefix in the commit title and mention the test file in the reply.",
	})
	require.Equal(t, 200, status)
	require.Len(t, h.host.SentMessages(), 2)
	require.Contains(t, h.host.SentMessages()[1].Text, "fix(auth): prefix")

	// Agent submits revised draft.
	draftRes2, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    SaveRemediationDraftTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"summary":       "Updated draft per review.",
			"hasChanges":    true,
			"commitMessage": "fix(auth): guard empty bearer header",
			"replies": []map[string]any{
				{"threadId": 7, "content": "Guarded empty bearer header and covered in src/auth.test.ts."},
			},
		},
	})
	require.NoError(t, err)
	require.False(t, draftRes2.IsError)

	_, body = h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{
		"repositoryId": "guid-api", "pullRequestId": 42,
	})
	draftID2, _ := field(body, "state", "pendingDraft", "draftId").(string)
	replyID2, _ := field(body, "state", "pendingDraft", "replies").([]any)[0].(map[string]any)["id"].(string)

	// Human edits wording and approves (leaving resolveThread = false / keep_active).
	status, _ = h.actionWith(taskCtx, "pull_requests.autofix.approve", map[string]any{
		"repositoryId":  "guid-api",
		"pullRequestId": 42,
		"draftId":       draftID2,
		"commitPush":    true,
		"commitMessage": "fix(auth): reject blank bearer tokens",
		"replies": []map[string]any{{
			"id":            replyID2,
			"threadId":      7,
			"send":          true,
			"content":       "Fixed! Blank bearer tokens are now rejected; see src/auth.test.ts.",
			"resolveThread": false,
		}},
	})
	require.Equal(t, 200, status)

	// Verify ADO received the human-edited reply, thread stayed active (no status patch),
	// and agent received the exact commit+push prompt.
	require.Len(t, h.azure.threadReplies, 1)
	require.Equal(t, "Fixed! Blank bearer tokens are now rejected; see src/auth.test.ts.", h.azure.threadReplies[0].Content)
	require.Empty(t, h.azure.threadStatus, "keep_active must not resolve thread when resolveThread is false")
	require.Len(t, h.host.SentMessages(), 3)
	commitPrompt := h.host.SentMessages()[2].Text
	require.True(t, strings.Contains(commitPrompt, "fix(auth): reject blank bearer tokens"))
	require.True(t, strings.Contains(commitPrompt, "feature/auth"))

	// CRITICAL: Thread #7 is still active on ADO, with Ada's reply as latest comment.
	// Polling again MUST NOT re-trigger on our own reply!
	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Len(t, h.host.SentMessages(), 3)

	// When reviewer Grace replies with a new follow-up comment, Round 2 triggers!
	h.azure.mu.Lock()
	threads := h.azure.prThreads[42]
	threads[0].Comments = append(threads[0].Comments, azure.Comment{
		ID: 999, Content: "Also trim whitespace before checking empty", CommentType: "text",
		Author: azure.Identity{ID: "reviewer-1", DisplayName: "Grace"},
	})
	h.azure.prThreads[42] = threads
	h.azure.mu.Unlock()

	require.NoError(t, services.autoFix.PollOnce(context.Background()))
	require.Len(t, h.host.SentMessages(), 4)
	require.Contains(t, h.host.SentMessages()[3].Text, "Also trim whitespace")
}

func TestAutoFixAllowsDirectReplyWhenRequireApprovalIsToggledOff(t *testing.T) {
	h, taskCtx := setupLinkedPRForAutoFix(t)
	h.azure.mu.Lock()
	h.azure.prPolicies[42] = []azure.PolicyEvaluation{}
	h.azure.prThreads[42] = []azure.Thread{{
		ID: 7, Status: "active",
		Comments: []azure.Comment{{
			ID: 1, Content: "Nit: typo", CommentType: "text",
			Author: azure.Identity{ID: "reviewer-1", DisplayName: "Grace"},
		}},
	}}
	h.azure.mu.Unlock()

	status, body := h.actionWith(taskCtx, "pull_requests.autofix.update", map[string]any{
		"repositoryId":    "guid-api",
		"pullRequestId":   42,
		"requireApproval": false,
	})
	require.Equal(t, 200, status)
	require.Equal(t, false, field(body, "state", "requireApproval"))

	res, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    ReplyPullRequestThreadTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"threadId": 7, "content": "Fixed typo in latest commit.",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Text)
	require.Len(t, h.azure.threadReplies, 1)
	require.Empty(t, h.azure.threadStatus, "default resolveThread remains false")
}

func TestAutoFixOptionalCommitOrReplyOnlyAndExplicitResolveAndRoundCap(t *testing.T) {
	h, taskCtx := setupLinkedPRForAutoFix(t)

	// 1. Reply-only draft (no code changes needed -> no commit/push prompt dispatched, explicit resolveThread=true works).
	h.azure.mu.Lock()
	h.azure.prPolicies[42] = []azure.PolicyEvaluation{}
	h.azure.prThreads[42] = []azure.Thread{{
		ID: 11, Status: "active",
		Comments: []azure.Comment{{ID: 1, Content: "Why use mutex here?", CommentType: "text", Author: azure.Identity{ID: "r1", DisplayName: "Grace"}}},
	}}
	h.azure.mu.Unlock()

	draftRes, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    SaveRemediationDraftTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"summary":    "Question answered; no code change needed.",
			"hasChanges": false,
			"replies":    []map[string]any{{"threadId": 11, "content": "Serializes concurrent refresh calls."}},
		},
	})
	require.NoError(t, err)
	require.False(t, draftRes.IsError)

	_, body := h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{"repositoryId": "guid-api", "pullRequestId": 42})
	draftID := field(body, "state", "pendingDraft", "draftId").(string)
	replyID := field(body, "state", "pendingDraft", "replies").([]any)[0].(map[string]any)["id"].(string)

	status, _ := h.actionWith(taskCtx, "pull_requests.autofix.approve", map[string]any{
		"repositoryId": "guid-api", "pullRequestId": 42, "draftId": draftID,
		"commitPush": false,
		"replies": []map[string]any{{
			"id": replyID, "threadId": 11, "send": true,
			"content": "Serializes concurrent refresh calls.", "resolveThread": true,
		}},
	})
	require.Equal(t, 200, status)
	require.Empty(t, h.host.SentMessages(), "reply-only approval must not dispatch a commit/push prompt")
	require.Len(t, h.azure.threadReplies, 1)
	require.Len(t, h.azure.threadStatus, 1)
	require.Equal(t, "fixed", h.azure.threadStatus[0].Status)

	// 2. CI-only draft (commit/push needed, zero replies -> no ADO reply calls).
	draftRes2, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name:    SaveRemediationDraftTool,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1"},
		Arguments: map[string]any{
			"summary":       "Fixed lint error in build.",
			"hasChanges":    true,
			"commitMessage": "fix: remove unused import",
		},
	})
	require.NoError(t, err)
	require.False(t, draftRes2.IsError)

	_, body = h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{"repositoryId": "guid-api", "pullRequestId": 42})
	draftID2 := field(body, "state", "pendingDraft", "draftId").(string)

	status, _ = h.actionWith(taskCtx, "pull_requests.autofix.approve", map[string]any{
		"repositoryId": "guid-api", "pullRequestId": 42, "draftId": draftID2,
		"commitPush": true, "commitMessage": "fix: remove unused import",
	})
	require.Equal(t, 200, status)
	require.Len(t, h.azure.threadReplies, 1, "commit-only approval must not post PR comments")
	require.Len(t, h.host.SentMessages(), 1)

	// 3. Round cap exhaustion after 5 rounds, and manual trigger resets & dispatches.
	services, err := h.runtime.ready()
	require.NoError(t, err)
	for round := 1; round <= 6; round++ {
		h.azure.mu.Lock()
		h.azure.prPolicies[42] = []azure.PolicyEvaluation{{
			ID: "eval-ci", Name: "CI", Status: "rejected", IsBlocking: true,
			Output: "failure round " + strings.Repeat("x", round),
		}}
		h.azure.mu.Unlock()
		require.NoError(t, services.autoFix.PollOnce(context.Background()))
	}
	require.Len(t, h.host.SentMessages(), 1+defaultAutoFixMaxRounds)
	_, body = h.actionWith(taskCtx, "pull_requests.autofix.get", map[string]any{"repositoryId": "guid-api", "pullRequestId": 42})
	require.Equal(t, "exhausted", field(body, "state", "status"))

	status, body = h.actionWith(taskCtx, "pull_requests.autofix.trigger", map[string]any{"repositoryId": "guid-api", "pullRequestId": 42})
	require.Equal(t, 200, status)
	require.Equal(t, "running", field(body, "state", "status"))
	require.Len(t, h.host.SentMessages(), 2+defaultAutoFixMaxRounds)
}
