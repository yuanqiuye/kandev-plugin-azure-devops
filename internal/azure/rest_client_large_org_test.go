package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListTeamsUsesThousandItemPagesForLargeProjects(t *testing.T) {
	const totalTeams = 6714
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acme/_apis/projects/One/teams" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&requests, 1)
		top, skip := 100, 0
		if v := r.URL.Query().Get("$top"); v != "" {
			top, _ = strconv.Atoi(v)
		}
		if v := r.URL.Query().Get("$skip"); v != "" {
			skip, _ = strconv.Atoi(v)
		}
		top = min(top, 1000)
		values := make([]map[string]string, 0, min(top, max(0, totalTeams-skip)))
		for i := skip; i < totalTeams && len(values) < top; i++ {
			values = append(values, map[string]string{
				"id":   fmt.Sprintf("team-%04d", i),
				"name": fmt.Sprintf("Team %04d", i),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(values), "value": values})
	}))
	t.Cleanup(server.Close)

	client := newTestRESTClient(t, server, "pat")
	teams, err := client.ListTeams(context.Background(), "One")
	if err != nil {
		t.Fatalf("ListTeams failed on 6,714-team project: %v", err)
	}
	if len(teams) != totalTeams {
		t.Fatalf("len(teams) = %d, want %d", len(teams), totalTeams)
	}
	if got := atomic.LoadInt32(&requests); got > 12 {
		t.Fatalf("ListTeams made %d HTTP requests for 6,714 teams, want <= 12 using $top=1000", got)
	}
}

func TestListAllRepositoriesAcceptsPayloadsLargerThanSixteenMiB(t *testing.T) {
	padding := strings.Repeat("x", 700)
	var buf bytes.Buffer
	buf.WriteString(`{"value":[`)
	const repoCount = 19500
	for i := range repoCount {
		if i > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf,
			`{"id":"00000000-0000-0000-0000-%012d","name":"Azure-Repo-%05d","defaultBranch":"refs/heads/main","webUrl":"https://dev.azure.com/msazure/One/_git/Azure-Repo-%05d","project":{"id":"b32aa71e-8ed2-41b2-9d77-5bc261222004","name":"One","description":"%s"}}`,
			i, i, i, padding,
		)
	}
	buf.WriteString(`]}`)
	payload := buf.Bytes()
	if len(payload) <= 16<<20 {
		t.Fatalf("test payload = %d bytes, want > 16 MiB", len(payload))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	client := newTestRESTClient(t, server, "pat")
	repos, err := client.ListAllRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListAllRepositories failed on %.2f MiB payload: %v", float64(len(payload))/(1<<20), err)
	}
	if len(repos) != repoCount {
		t.Fatalf("len(repos) = %d, want %d", len(repos), repoCount)
	}
}

func TestQueryWIQLPopulatesStateCategoryFromProjectMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/acme/One/_apis/wit/wiql":
			_, _ = w.Write([]byte(`{"workItems":[{"id":1},{"id":2},{"id":3}]}`))
		case "/acme/One/_apis/wit/workitemsbatch":
			_, _ = w.Write([]byte(`{"value":[
				{"id":1,"rev":1,"fields":{"System.Title":"First","System.WorkItemType":"Task","System.State":"To Do","System.TeamProject":"One"}},
				{"id":2,"rev":1,"fields":{"System.Title":"Second","System.WorkItemType":"Task","System.State":"3.3 Mitigate - Done","System.TeamProject":"One"}},
				{"id":3,"rev":1,"fields":{"System.Title":"Third","System.WorkItemType":"Task","System.State":"In Review","System.TeamProject":"One"}}
			]}`))
		case "/acme/One/_apis/wit/workitemtypes":
			_, _ = w.Write([]byte(`{"value":[{"name":"Task","states":[
				{"name":"To Do","category":"Proposed"},
				{"name":"In Review","category":"InProgress"},
				{"name":"3.3 Mitigate - Done","category":"Completed"}
			]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestRESTClient(t, server, "pat")
	res, err := client.QueryWIQL(context.Background(), "One", "SELECT [System.Id] FROM WorkItems", 10)
	if err != nil {
		t.Fatalf("QueryWIQL: %v", err)
	}
	if len(res.Items) != 3 ||
		res.Items[0].StateCategory != "Proposed" ||
		res.Items[1].StateCategory != "Completed" ||
		res.Items[2].StateCategory != "InProgress" {
		t.Fatalf("items = %+v", res.Items)
	}
}
