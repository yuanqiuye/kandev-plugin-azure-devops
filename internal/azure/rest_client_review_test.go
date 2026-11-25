package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRESTClientReadsTheCommentContinuationTokenFromTheBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"comments":[{"id":1,"text":"hi"}],"continuationToken":"next-page"}`))
	}))
	t.Cleanup(server.Close)
	page, err := newTestRESTClient(t, server, "pat").ListWorkItemComments(t.Context(), "project-1", 101, "")
	if err != nil || page.ContinuationToken != "next-page" {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestRESTClientBranchExistsUsesAnExactRefFilter(t *testing.T) {
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, r.URL.Query().Get("filter"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[{"name":"refs/heads/feature/x-old"},{"name":"refs/heads/feature/x"}]}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server, "pat")
	found, err := client.BranchExists(t.Context(), "project-1", "repo-1", "feature/x")
	if err != nil || !found {
		t.Fatalf("BranchExists = %v, %v", found, err)
	}
	found, err = client.BranchExists(t.Context(), "project-1", "repo-1", "feature/x-old2")
	if err != nil || found {
		t.Fatalf("BranchExists(missing) = %v, %v", found, err)
	}
	if len(filters) != 2 || filters[0] != "heads/feature/x" || filters[1] != "heads/feature/x-old2" {
		t.Fatalf("filters = %v", filters)
	}
}

func TestRESTClientParsesThreadFileContextAndRepliesAndResolves(t *testing.T) {
	var repliedBody map[string]any
	var patchedStatus map[string]any
	var createdThread map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/acme/project-1/_apis/git/repositories/repo-1/pullrequests/42/threads":
			_, _ = w.Write([]byte(`{"value":[
				{"id":7,"status":1,"threadContext":{"filePath":"/src/auth.ts","rightFileStart":{"line":29,"offset":1}},"comments":[
					{"id":1,"content":"Check nil token","author":{"id":"u2","displayName":"Grace"},"commentType":1},
					{"id":2,"parentCommentId":1,"content":"Deleted note","isDeleted":true,"commentType":"text"}
				]},
				{"id":8,"status":"active","isDeleted":true,"comments":[{"id":1,"content":"Removed thread","commentType":"text"}]}
			]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/acme/project-1/_apis/git/repositories/repo-1/pullrequests/42/threads/7/comments":
			_ = json.NewDecoder(r.Body).Decode(&repliedBody)
			_, _ = w.Write([]byte(`{"id":9,"parentCommentId":1,"content":"Handled nil token in guard.","author":{"id":"u1","displayName":"Ada"},"commentType":1}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/acme/project-1/_apis/git/repositories/repo-1/pullrequests/42/threads/7":
			_ = json.NewDecoder(r.Body).Decode(&patchedStatus)
			_, _ = w.Write([]byte(`{"id":7,"status":"fixed"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/acme/project-1/_apis/git/repositories/repo-1/pullrequests/42/threads":
			_ = json.NewDecoder(r.Body).Decode(&createdThread)
			_, _ = w.Write([]byte(`{"id":12,"status":1,"comments":[{"id":1,"content":"Summary note","commentType":1}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestRESTClient(t, server, "pat")
	threads, err := client.ListThreads(t.Context(), "project-1", "repo-1", 42)
	if err != nil || len(threads) != 2 {
		t.Fatalf("ListThreads = %+v, %v", threads, err)
	}
	if threads[0].Status != "active" || threads[0].FilePath != "src/auth.ts" || threads[0].Line != 29 {
		t.Fatalf("thread[0] = %+v", threads[0])
	}
	if threads[0].Comments[0].CommentType != "text" || !threads[0].Comments[1].IsDeleted || threads[0].Comments[1].ParentID != 1 {
		t.Fatalf("thread[0] comments = %+v", threads[0].Comments)
	}
	if !threads[1].IsDeleted {
		t.Fatalf("thread[1] expected IsDeleted = true: %+v", threads[1])
	}

	reply, err := client.ReplyToThread(t.Context(), "project-1", "repo-1", 42, 7, "Handled nil token in guard.")
	if err != nil || reply.ID != 9 || reply.CommentType != "text" || repliedBody["content"] != "Handled nil token in guard." {
		t.Fatalf("ReplyToThread = %+v, %v (body=%+v)", reply, err, repliedBody)
	}
	if err := client.UpdateThreadStatus(t.Context(), "project-1", "repo-1", 42, 7, "fixed"); err != nil || patchedStatus["status"] != "fixed" {
		t.Fatalf("UpdateThreadStatus = %v (body=%+v)", err, patchedStatus)
	}
	thread, err := client.CreateThread(t.Context(), "project-1", "repo-1", 42, "Summary note")
	if err != nil || thread.ID != 12 || thread.Status != "active" || createdThread["status"] != "active" {
		t.Fatalf("CreateThread = %+v, %v (body=%+v)", thread, err, createdThread)
	}
}

func TestRESTClientEnrichesFailedBuildPoliciesWithTimelineErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/acme/project-1/_apis/policy/evaluations":
			_, _ = w.Write([]byte(`{"value":[
				{"evaluationId":"eval-build","status":"rejected","configuration":{"isBlocking":true,"type":{"displayName":"Build"},"settings":{"displayName":"Unit Tests"}},"context":{"buildId":777}},
				{"evaluationId":"eval-ok","status":"approved","configuration":{"isBlocking":true,"type":{"displayName":"Minimum number of reviewers"}}}
			]}`))
		case "/acme/project-1/_apis/build/builds/777/timeline":
			_, _ = w.Write([]byte(`{"records":[
				{"name":"Run vitest","type":"Task","result":"failed","issues":[{"type":"error","message":"FAIL src/auth.test.ts: expected 200 to be 401"}]},
				{"name":"Checkout","type":"Task","result":"succeeded"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	policies, err := newTestRESTClient(t, server, "pat").ListPolicyEvaluations(t.Context(), "project-1", 42)
	if err != nil || len(policies) != 2 {
		t.Fatalf("ListPolicyEvaluations = %+v, %v", policies, err)
	}
	if policies[0].Name != "Unit Tests" || policies[0].BuildID != 777 || !strings.Contains(policies[0].BuildURL, "buildId=777") {
		t.Fatalf("policies[0] metadata = %+v", policies[0])
	}
	if !strings.Contains(policies[0].Output, "Run vitest") || !strings.Contains(policies[0].Output, "src/auth.test.ts") {
		t.Fatalf("policies[0] output = %q", policies[0].Output)
	}
}
