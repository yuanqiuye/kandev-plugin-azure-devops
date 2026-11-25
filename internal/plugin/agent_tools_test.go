package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-azure-devops/internal/azure"
)

func (h *harness) invokeTool(arguments map[string]any) *pluginsdk.AgentToolResult {
	h.t.Helper()
	result, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
		Name: CreatePullRequestTool, Arguments: arguments,
		Context: pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "s-1"},
	})
	if err != nil {
		h.t.Fatalf("InvokeAgentTool error = %v", err)
	}
	return result
}

func TestAgentToolCreatesAndLinksAPullRequestForAPushedBranch(t *testing.T) {
	h := linksHarness(t)
	h.azure.branches["guid-api"] = append(h.azure.branches["guid-api"], azure.Branch{Name: "feature/z"})
	result := h.invokeTool(map[string]any{"title": "Retry webhooks", "description": "Why", "sourceBranch": "refs/heads/feature/z"})
	if result.IsError || result.StructuredContent["linked"] != true || !strings.Contains(result.Text, "pullrequest/") {
		t.Fatalf("result = %+v", result)
	}
	created := h.azure.createdPRs[0]
	if created.TargetBranch != "main" || created.SourceBranch != "feature/z" {
		t.Fatalf("created = %+v", created)
	}
	_, links := h.taskAction("task_links.get", nil)
	if prs, _ := links["pullRequests"].([]any); len(prs) != 1 {
		t.Fatalf("links = %v", links)
	}
}

func TestAgentToolExplainsAnUnpushedBranchAndMissingArguments(t *testing.T) {
	h := linksHarness(t)
	result := h.invokeTool(map[string]any{"title": "X", "sourceBranch": "feature/unpushed"})
	if !result.IsError || !strings.Contains(result.Text, "push it first") {
		t.Fatalf("unpushed result = %+v", result)
	}
	result = h.invokeTool(map[string]any{"title": "X"})
	if !result.IsError || !strings.Contains(result.Text, "sourceBranch is required") {
		t.Fatalf("missing branch result = %+v", result)
	}
	result = h.invokeTool(map[string]any{"sourceBranch": "feature/x"})
	if !result.IsError || len(h.azure.createdPRs) != 0 {
		t.Fatalf("missing title result = %+v", result)
	}
}

func TestAgentToolRequiresAnAzureRepositoryOnTheTask(t *testing.T) {
	h := linksHarness(t)
	h.host.AddTask(pluginsdk.Task{ID: "task-1", WorkspaceID: "ws-1", Repositories: []pluginsdk.TaskRepository{{RepositoryID: "repo-gh"}}})
	result := h.invokeTool(map[string]any{"title": "X", "sourceBranch": "feature/x"})
	if !result.IsError || !strings.Contains(result.Text, "no Azure Repos repository") {
		t.Fatalf("result = %+v", result)
	}
}

func TestSelectRepositoryRequiresAChoiceForSeveralRepositories(t *testing.T) {
	repositories := []azureRepository{{ProjectName: "P", RepositoryName: "api"}, {ProjectName: "P", RepositoryName: "web"}}
	if _, err := selectRepository(repositories, ""); err == nil || !strings.Contains(err.Error(), "P/api, P/web") {
		t.Fatalf("err = %v", err)
	}
	if selected, err := selectRepository(repositories, "P/web"); err != nil || selected.RepositoryName != "web" {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
	if _, err := selectRepository(repositories, "other"); err == nil {
		t.Fatal("expected an unknown repository to fail")
	}
}

func TestSelectRepositoryRejectsANameSharedByTwoProjects(t *testing.T) {
	repositories := []azureRepository{
		{ProjectName: "ProjectA", RepositoryName: "api", RepositoryID: "guid-a"},
		{ProjectName: "ProjectB", RepositoryName: "api", RepositoryID: "guid-b"},
	}
	if _, err := selectRepository(repositories, "api"); err == nil || !strings.Contains(err.Error(), "ProjectA/api, ProjectB/api") {
		t.Fatalf("ambiguous name err = %v", err)
	}
	if selected, err := selectRepository(repositories, "ProjectB/api"); err != nil || selected.RepositoryID != "guid-b" {
		t.Fatalf("qualified name = %+v, %v", selected, err)
	}
	if selected, err := selectRepository(repositories, "GUID-A"); err != nil || selected.RepositoryID != "guid-a" {
		t.Fatalf("repository GUID = %+v, %v", selected, err)
	}
}

func TestUnknownAgentToolIsAnError(t *testing.T) {
	h := linksHarness(t)
	result, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{Name: "other"})
	if err != nil || !result.IsError {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestAgentToolLinkPullRequestByBranchReferenceAndID(t *testing.T) {
	h := linksHarness(t)
	h.azure.pullRequests = map[int]azure.PullRequest{
		77: {
			ID: 77, Title: "External PR via MCP", Status: "active",
			SourceBranch: "feature/ext", TargetBranch: "main",
			ProjectID: "p1", ProjectName: "Platform Team",
			RepositoryID: "guid-api", RepositoryName: "api",
			WebURL: "https://dev.azure.com/acme/Platform%20Team/_git/api/pullrequest/77",
		},
		88: {
			ID: 88, Title: "External PR via URL", Status: "active",
			SourceBranch: "feature/url", TargetBranch: "main",
			ProjectID: "p1", ProjectName: "Platform Team",
			RepositoryID: "guid-api", RepositoryName: "api",
			WebURL: "https://dev.azure.com/acme/Platform%20Team/_git/api/pullrequest/88",
		},
		89: {
			ID: 89, Title: "Backport PR", Status: "active",
			SourceBranch: "feature/ambig", TargetBranch: "main",
			ProjectID: "p1", ProjectName: "Platform Team",
			RepositoryID: "guid-api", RepositoryName: "api",
		},
		90: {
			ID: 90, Title: "Backport PR release", Status: "active",
			SourceBranch: "feature/ambig", TargetBranch: "release",
			ProjectID: "p1", ProjectName: "Platform Team",
			RepositoryID: "guid-api", RepositoryName: "api",
		},
	}

	callLink := func(args map[string]any) *pluginsdk.AgentToolResult {
		t.Helper()
		res, err := h.runtime.InvokeAgentTool(context.Background(), &pluginsdk.AgentToolRequest{
			Name:      LinkPullRequestTool,
			Arguments: args,
			Context:   pluginsdk.AgentToolContext{WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "s-1"},
		})
		if err != nil {
			t.Fatalf("InvokeAgentTool(link_pull_request) error = %v", err)
		}
		return res
	}

	// 1. Missing identifier arguments -> clear error.
	if res := callLink(map[string]any{}); !res.IsError {
		t.Fatalf("expected error for empty args: %+v", res)
	}

	// 2. Active PR discovery by sourceBranch.
	resBranch := callLink(map[string]any{"sourceBranch": "refs/heads/feature/ext"})
	if resBranch.IsError || resBranch.StructuredContent["pullRequestId"] != 77 || resBranch.StructuredContent["linked"] != true {
		t.Fatalf("link by branch = %+v", resBranch)
	}

	// 3. Link by visualstudio.com URL reference.
	resURL := callLink(map[string]any{"reference": "https://acme.visualstudio.com/Platform%20Team/_git/api/pullrequest/88"})
	if resURL.IsError || resURL.StructuredContent["pullRequestId"] != 88 {
		t.Fatalf("link by URL = %+v", resURL)
	}

	// 4. Ambiguous sourceBranch requires targetBranch or pullRequestId, then succeeds with targetBranch.
	if resAmbig := callLink(map[string]any{"sourceBranch": "feature/ambig"}); !resAmbig.IsError || !strings.Contains(resAmbig.Text, "!89") {
		t.Fatalf("ambiguous branch = %+v", resAmbig)
	}
	resDisambig := callLink(map[string]any{"sourceBranch": "feature/ambig", "targetBranch": "release"})
	if resDisambig.IsError || resDisambig.StructuredContent["pullRequestId"] != 90 {
		t.Fatalf("disambiguated branch = %+v", resDisambig)
	}

	// Verify all 3 PRs are linked on task-1.
	_, links := h.taskAction("task_links.get", nil)
	if prs, _ := links["pullRequests"].([]any); len(prs) != 3 {
		t.Fatalf("expected 3 linked PRs, got %v", links)
	}
}
