package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"

	"kandev-plugin-azure-devops/internal/auth"
	"kandev-plugin-azure-devops/internal/store"
)

type fakeAzureCLI struct {
	mu         sync.Mutex
	azure      *fakeAzure
	issued     int
	ttl        time.Duration
	err        error
	fixedToken string
	tenants    []string
	gate       chan struct{}
}

func newFakeAzureCLI(azure *fakeAzure) *fakeAzureCLI {
	return &fakeAzureCLI{azure: azure, ttl: time.Hour}
}

func (f *fakeAzureCLI) AccessToken(_ context.Context, tenant string) (*auth.AzCLIToken, error) {
	f.mu.Lock()
	f.tenants = append(f.tenants, tenant)
	gate, err, fixed, ttl := f.gate, f.err, f.fixedToken, f.ttl
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	if fixed != "" {
		return &auth.AzCLIToken{AccessToken: fixed, ExpiresAt: time.Now().UTC().Add(ttl)}, nil
	}
	f.mu.Lock()
	f.issued++
	n := f.issued
	f.mu.Unlock()
	token := fmt.Sprintf("az-access-%d", n)
	f.azure.mu.Lock()
	f.azure.validBearers[token] = true
	f.azure.mu.Unlock()
	return &auth.AzCLIToken{AccessToken: token, ExpiresAt: time.Now().UTC().Add(ttl)}, nil
}

func (f *fakeAzureCLI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tenants)
}

func (h *harness) connectAzureCLI(workspaceID string) map[string]any {
	h.t.Helper()
	status, body := h.action(workspaceID, "connection.save", map[string]any{
		"organizationUrl":    "https://dev.azure.com/acme/",
		"defaultProjectName": "Platform Team",
		"authMethod":         "azure_cli",
	})
	if status != 200 {
		h.t.Fatalf("connection.save azure_cli status = %d body = %v", status, body)
	}
	return body
}

func TestSaveAzureCLIProbesWithoutStoringSecretsAndUpdatesProjectInPlace(t *testing.T) {
	h := newHarness(t)
	body := h.connectAzureCLI("ws-1")
	connection := field(body, "connection")
	if field(connection, "authMethod") != "azure_cli" || field(connection, "organizationUrl") != "https://dev.azure.com/acme" ||
		field(connection, "hasSecret") != true || field(connection, "reauthRequired") != false ||
		field(connection, "lastOk") != true || field(connection, "defaultProjectName") != "Platform Team" {
		t.Fatalf("connection = %v", connection)
	}
	if strings.Contains(flatten(body), "az-access-") {
		t.Fatalf("response leaked Azure CLI token: %v", body)
	}
	for _, key := range []string{store.PATKey("ws-1"), store.OAuthRefreshKey("ws-1"), store.OAuthAccessKey("ws-1")} {
		if _, found := h.host.Secret(key); found {
			t.Fatalf("unexpected stored secret %s", key)
		}
	}

	before := mustConnection(t, h, "ws-1")
	callsBefore := h.azCLI.callCount()
	status, updated := h.action("ws-1", "connection.save", map[string]any{
		"organizationUrl":    "https://dev.azure.com/acme",
		"defaultProjectName": "Ops",
		"authMethod":         "azure_cli",
	})
	if status != 200 || field(updated, "connection", "defaultProjectName") != "Ops" {
		t.Fatalf("project update status = %d body = %v", status, updated)
	}
	after := mustConnection(t, h, "ws-1")
	if after.Generation != before.Generation || h.azCLI.callCount() != callsBefore {
		t.Fatalf("generation %d -> %d, az calls %d -> %d", before.Generation, after.Generation, callsBefore, h.azCLI.callCount())
	}
}

func TestSaveAzureCLIFailurePreservesExistingPATAndOAuthConnections(t *testing.T) {
	h := newHarness(t)
	h.connect("ws-pat")
	h.signIn("ws-oauth")

	h.azCLI.err = &auth.AzCLIError{Kind: auth.AzCLINotSignedIn}
	for _, ws := range []string{"ws-pat", "ws-oauth"} {
		status, body := h.action(ws, "connection.save", map[string]any{
			"organizationUrl": "https://dev.azure.com/acme",
			"authMethod":      "azure_cli",
		})
		if status != 400 || !strings.Contains(field(body, "error").(string), "az login") {
			t.Fatalf("%s failed save status = %d body = %v", ws, status, body)
		}
	}
	if pat, ok := h.host.Secret(store.PATKey("ws-pat")); !ok || pat != "good-pat" {
		t.Fatalf("PAT lost after failed switch: %q, %v", pat, ok)
	}
	if refresh, ok := h.host.Secret(store.OAuthRefreshKey("ws-oauth")); !ok || refresh == "" {
		t.Fatalf("OAuth refresh token lost after failed switch: %q, %v", refresh, ok)
	}

	// Once Azure CLI is signed in, switching removes replaced secrets.
	h.azCLI.err = nil
	for _, ws := range []string{"ws-pat", "ws-oauth"} {
		h.connectAzureCLI(ws)
	}
	if _, found := h.host.Secret(store.PATKey("ws-pat")); found {
		t.Fatal("PAT survived switch to Azure CLI")
	}
	if _, found := h.host.Secret(store.OAuthRefreshKey("ws-oauth")); found {
		t.Fatal("OAuth refresh token survived switch to Azure CLI")
	}

	// Switching from Azure CLI back to PAT requires a PAT.
	if status, _ := h.action("ws-pat", "connection.save", map[string]any{
		"organizationUrl": "https://dev.azure.com/acme",
		"authMethod":      "pat",
	}); status != 400 {
		t.Fatalf("switch to PAT without token status = %d, want 400", status)
	}
}

func TestTestAzureCLIProbesWithoutPersistingAndReportsErrors(t *testing.T) {
	h := newHarness(t)
	status, body := h.action("ws-1", "connection.test", map[string]any{
		"organizationUrl": "https://dev.azure.com/acme",
		"authMethod":      "azure_cli",
	})
	if status != 200 || field(body, "result", "ok") != true || field(body, "result", "displayName") != "Ada" {
		t.Fatalf("status = %d body = %v", status, body)
	}
	if _, found, _ := store.New(h.host).Connection(t.Context(), "ws-1"); found {
		t.Fatal("connection.test persisted an Azure CLI connection")
	}

	h.azCLI.err = &auth.AzCLIError{Kind: auth.AzCLINotInstalled}
	status, body = h.action("ws-1", "connection.test", map[string]any{
		"organizationUrl": "https://dev.azure.com/acme",
		"authMethod":      "azure_cli",
	})
	if status != 200 || field(body, "result", "ok") != false || !strings.Contains(field(body, "result", "error").(string), "not installed") {
		t.Fatalf("missing az test = %d %v", status, body)
	}
}

func TestAzureCLICachesTokensRenewsOnRejectionAndCoalescesConcurrentCalls(t *testing.T) {
	h := newHarness(t)
	h.connectAzureCLI("ws-1")
	initialCalls := h.azCLI.callCount()

	// Cached token serves multiple requests without shelling out again.
	for range 3 {
		if status, _ := h.action("ws-1", "projects.list", nil); status != 200 {
			t.Fatalf("projects.list status = %d", status)
		}
	}
	if h.azCLI.callCount() != initialCalls {
		t.Fatalf("cached token invoked az again: %d -> %d", initialCalls, h.azCLI.callCount())
	}

	// Revoking the current bearer triggers a single renewal and retries.
	h.azure.mu.Lock()
	delete(h.azure.validBearers, "az-access-1")
	h.azure.mu.Unlock()
	if status, body := h.action("ws-1", "projects.list", nil); status != 200 {
		t.Fatalf("retry after 401 status = %d body = %v", status, body)
	}
	if h.azCLI.callCount() != initialCalls+1 {
		t.Fatalf("renewal calls = %d, want %d", h.azCLI.callCount(), initialCalls+1)
	}

	// Concurrent requests when near expiry share one Azure CLI call.
	services, _ := h.runtime.ready()
	services.connections.setAzCLIToken("ws-1", cachedAzToken{
		generation:      mustConnection(t, h, "ws-1").Generation,
		organizationURL: "https://dev.azure.com/acme",
		token:           "az-access-2",
		expiresAt:       time.Now().UTC().Add(30 * time.Second),
	})
	gate := make(chan struct{})
	h.azCLI.mu.Lock()
	h.azCLI.gate = gate
	h.azCLI.mu.Unlock()
	beforeConcurrent := h.azCLI.callCount()
	results := make(chan string, 4)
	bound := mustConnection(t, h, "ws-1")
	for range 4 {
		go func() {
			tok, _, err := services.connections.boundAzureCLIToken(context.Background(), "ws-1", bound)
			if err != nil {
				tok = "err:" + err.Error()
			}
			results <- tok
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	h.azCLI.mu.Lock()
	h.azCLI.gate = nil
	h.azCLI.mu.Unlock()
	for range 4 {
		if got := <-results; got != "az-access-3" {
			t.Fatalf("concurrent token = %q, want az-access-3", got)
		}
	}
	if h.azCLI.callCount() != beforeConcurrent+1 {
		t.Fatalf("concurrent calls = %d, want %d", h.azCLI.callCount(), beforeConcurrent+1)
	}

	// If az returns the same rejected token, Renew stops without looping.
	h.azCLI.fixedToken = "unaccepted-token"
	services.connections.clearAzCLIToken("ws-1")
	if status, _ := h.action("ws-1", "projects.list", nil); status != 409 {
		t.Fatalf("unaccepted fixed token status = %d, want 409", status)
	}
}

func TestAzureCLIGitCredentialsCopyHealthAndRedaction(t *testing.T) {
	h := newHarness(t)
	h.azCLI.ttl = 4 * time.Minute
	h.connectAzureCLI("ws-source")
	h.connect("ws-target")

	// Copy replaces target PAT with Azure CLI without requiring a PAT entry.
	status, copied := h.action("ws-target", "connection.copy", map[string]any{"sourceWorkspaceId": "ws-source"})
	if status != 200 || field(copied, "connection", "authMethod") != "azure_cli" ||
		field(copied, "connection", "reauthRequired") != false || field(copied, "connection", "lastOk") != true {
		t.Fatalf("copied = %d %v", status, copied)
	}
	if _, found := h.host.Secret(store.PATKey("ws-target")); found {
		t.Fatal("target PAT survived copying an Azure CLI connection")
	}

	// Git credentials issue the short-lived Azure CLI bearer capped at its expiry.
	h.host.AddRepository(pluginsdk.Repository{
		ID: "repo-1", WorkspaceID: "ws-source", ProviderID: "azure_devops",
		RemoteURL: "https://dev.azure.com/acme/Platform%20Team/_git/api",
	})
	req := credentialRequest()
	req.WorkspaceID = "ws-source"
	cred, err := h.runtime.ResolveGitCredential(context.Background(), req)
	if err != nil || cred.Username != "kandev" || !strings.HasPrefix(cred.Secret, "az-access-") {
		t.Fatalf("credential = %+v, err = %v", cred, err)
	}
	expiresAt, _ := time.Parse(time.RFC3339, cred.ExpiresAt)
	if expiresAt.After(time.Now().Add(4*time.Minute + 5*time.Second)) {
		t.Fatalf("credential expiry %v exceeded token TTL", expiresAt)
	}

	// Redact scrubs both the last sent token and any rotated cached token.
	services, _ := h.runtime.ready()
	bound := mustConnection(t, h, "ws-source")
	authz := &azCLIAuthorizer{connections: services.connections, workspaceID: "ws-source", bound: bound}
	header, err := authz.Header(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTok := strings.TrimPrefix(header, "Bearer ")
	services.connections.setAzCLIToken("ws-source", cachedAzToken{
		generation: bound.Generation, organizationURL: bound.OrganizationURL,
		token: "rotated-tok", expiresAt: time.Now().UTC().Add(time.Hour),
	})
	redacted := authz.Redact("leaked " + firstTok + " and rotated-tok")
	if strings.Contains(redacted, firstTok) || strings.Contains(redacted, "rotated-tok") {
		t.Fatalf("redacted = %q", redacted)
	}

	// When az signs out later, health records the failure and recovers once signed back in.
	h.host.AddWorkspace("ws-source")
	services.connections.clearAzCLIToken("ws-source")
	h.azCLI.err = &auth.AzCLIError{Kind: auth.AzCLINotSignedIn}
	if status, body := h.action("ws-source", "projects.list", nil); status != 409 || !strings.Contains(field(body, "error").(string), "az login") {
		t.Fatalf("projects.list while signed out = %d %v", status, body)
	}
	NewHealthPoller(services.connections, h.host.Workspaces()).PollOnce(t.Context())
	health, _, _ := store.New(h.host).Health(t.Context(), "ws-source")
	if health.OK || !strings.Contains(health.Error, "az login") {
		t.Fatalf("health while signed out = %+v", health)
	}
	h.azCLI.err = nil
	NewHealthPoller(services.connections, h.host.Workspaces()).PollOnce(t.Context())
	health, _, _ = store.New(h.host).Health(t.Context(), "ws-source")
	if !health.OK || health.Error != "" {
		t.Fatalf("health after az login = %+v", health)
	}
}
