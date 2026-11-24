package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-azure-devops/internal/azure"
)

const hostActionResponseLimit = 1 << 20 // 1 MiB enforced by Kandev action_handlers.go

func TestLargeProjectRepositoriesKeepEachActionResponseBelowOneMiB(t *testing.T) {
	h := newHarness(t)
	h.connect("ws-1")

	repos := make([]azure.Repository, 0, 5500)
	for i := range 5500 {
		repos = append(repos, azure.Repository{
			ID:            fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
			Name:          fmt.Sprintf("Azure-Maps-Fabric-UX-Extension-%05d", i),
			ProjectID:     "p1",
			ProjectName:   "One",
			DefaultBranch: "refs/heads/main",
			WebURL:        fmt.Sprintf("https://dev.azure.com/acme/One/_git/Azure-Maps-Fabric-UX-Extension-%05d", i),
		})
	}
	h.azure.repositories = repos

	var collected int
	cursor := ""
	seen := map[string]bool{}
	for {
		bodyBytes, _ := json.Marshal(map[string]any{"projectId": "p1", "cursor": cursor})
		resp, err := h.runtime.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
			ActionKey: "projects.repositories",
			Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
			Body:      bodyBytes,
		})
		if err != nil {
			t.Fatalf("HandleAction error: %v", err)
		}
		if len(resp.Body) > hostActionResponseLimit {
			t.Fatalf("projects.repositories response = %d bytes, exceeds 1 MiB host limit (%d)", len(resp.Body), hostActionResponseLimit)
		}
		var page struct {
			Repositories []azure.Repository `json:"repositories"`
			NextCursor   string             `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		collected += len(page.Repositories)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			t.Fatalf("cursor loop on %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if collected != 5500 {
		t.Fatalf("collected = %d, want 5500", collected)
	}
}

func TestRepositoriesListResolvesPastedVisualStudioAndDevAzureURLs(t *testing.T) {
	h := newHarness(t)
	h.connect("ws-1")

	for _, query := range []string{
		"https://acme.visualstudio.com/Platform%20Team/_git/api",
		"https://dev.azure.com/acme/Platform%20Team/_git/api",
	} {
		status, body := h.action("ws-1", "repositories.list", map[string]any{"query": query, "limit": 50})
		if status != 200 {
			t.Fatalf("query %q status = %d body = %v", query, status, body)
		}
		got := repositoryNames(body)
		if len(got) != 1 || got[0] != "Platform Team/api" {
			t.Fatalf("query %q returned %v, want [Platform Team/api]", query, got)
		}
	}
}

func TestWorkItemsSearchSupportsDoneAndToDoStateFilteringAndPresets(t *testing.T) {
	presets := DefaultWorkItemQueryPresets()
	var activeWIQL, doneWIQL string
	for _, p := range presets {
		if p.ID == "active" {
			activeWIQL, _ = p.Filters["wiql"].(string)
		}
		if p.ID == "done" {
			doneWIQL, _ = p.Filters["wiql"].(string)
		}
	}
	if !strings.Contains(activeWIQL, "Removed") {
		t.Fatalf("active preset WIQL does not exclude Removed items: %q", activeWIQL)
	}
	if doneWIQL == "" {
		t.Fatal("missing 'done' preset in DefaultWorkItemQueryPresets")
	}

	h := newHarness(t)
	h.connect("ws-1")
	status, _ := h.action("ws-1", "work_items.search", map[string]any{
		"projectId":     "p1",
		"wiql":          "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project ORDER BY [System.ChangedDate] DESC",
		"stateCategory": "todo",
		"top":           50,
	})
	if status != 200 {
		t.Fatalf("work_items.search status = %d", status)
	}
	if !strings.Contains(h.azure.browse.wiql, "To Do") {
		t.Fatalf("expected WIQL to filter To Do states, got %q", h.azure.browse.wiql)
	}
}
