package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"kandev-plugin-azure-devops/internal/auth"
	"kandev-plugin-azure-devops/internal/azure"
	"kandev-plugin-azure-devops/internal/store"
)

const azCLIRenewalMargin = 2 * time.Minute

var errAzureCLINoAccess = errors.New("the Azure CLI account cannot access this organization")

type cachedAzToken struct {
	generation      int64
	organizationURL string
	token           string
	expiresAt       time.Time
}

func (c *Connections) discoverAzCLITenant(ctx context.Context, organizationURL string) string {
	tenant, err := c.entra.DiscoverTenant(ctx, organizationURL)
	if err != nil || !auth.IsGUID(tenant) {
		return ""
	}
	return strings.ToLower(tenant)
}

func (c *Connections) setAzCLIToken(workspaceID string, entry cachedAzToken) {
	c.azCacheMu.Lock()
	defer c.azCacheMu.Unlock()
	if c.azTokens == nil {
		c.azTokens = make(map[string]cachedAzToken)
	}
	c.azTokens[workspaceID] = entry
}

func (c *Connections) getAzCLIToken(workspaceID string) (cachedAzToken, bool) {
	c.azCacheMu.Lock()
	defer c.azCacheMu.Unlock()
	entry, ok := c.azTokens[workspaceID]
	return entry, ok
}

func (c *Connections) clearAzCLIToken(workspaceID string) {
	c.azCacheMu.Lock()
	defer c.azCacheMu.Unlock()
	delete(c.azTokens, workspaceID)
}

func azCLISaveError(err error) error {
	var azErr *auth.AzCLIError
	if errors.As(err, &azErr) {
		return invalidArgument(azErr)
	}
	return unavailable(err)
}

func (c *Connections) saveAzureCLI(ctx context.Context, workspaceID, organizationURL string, input ConnectionInput) error {
	previous, found, err := c.store.Connection(ctx, workspaceID)
	if err != nil {
		return err
	}
	if found && previous.AuthMethod == store.AuthMethodAzureCLI && previous.OrganizationURL == organizationURL && !previous.ReauthRequired {
		updated, err := c.saveAzureCLIProjectOnly(ctx, workspaceID, organizationURL, input)
		if err != nil || updated {
			return err
		}
	}
	tenant := c.discoverAzCLITenant(ctx, organizationURL)
	tok, err := c.azCLI.AccessToken(ctx, tenant)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return azCLISaveError(err)
	}
	identity, err := c.newClient(organizationURL, azure.NewBearerAuthorizer(tok.AccessToken)).TestAuth(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return invalidArgument(fmt.Errorf("%w: %s", errAzureCLINoAccess, probeError(err)))
	}
	if identity == nil || !identity.OK {
		return invalidArgument(errAzureCLINoAccess)
	}
	unlock := c.locks.lock(workspaceID)
	defer unlock()
	current, _, err := c.store.Connection(ctx, workspaceID)
	if err != nil {
		return err
	}
	now := c.now().UTC()
	next := store.Connection{
		OrganizationURL:    organizationURL,
		AuthMethod:         store.AuthMethodAzureCLI,
		DefaultProjectID:   strings.TrimSpace(input.DefaultProjectID),
		DefaultProjectName: strings.TrimSpace(input.DefaultProjectName),
		OAuthTenantID:      tenant,
		Generation:         current.Generation + 1,
		UpdatedAt:          now,
	}
	if err := c.store.SaveConnection(ctx, workspaceID, next); err != nil {
		return err
	}
	saved, savedFound, err := c.store.Connection(ctx, workspaceID)
	if err != nil {
		return err
	}
	generation := next.Generation
	if savedFound {
		generation = saved.Generation
	}
	_ = c.store.DeletePAT(ctx, workspaceID)
	c.discardOAuthTokens(ctx, workspaceID)
	c.setAzCLIToken(workspaceID, cachedAzToken{
		generation:      generation,
		organizationURL: organizationURL,
		token:           tok.AccessToken,
		expiresAt:       tok.ExpiresAt,
	})
	_ = c.store.SaveHealth(ctx, workspaceID, store.Health{Generation: generation, CheckedAt: now, OK: true})
	return nil
}

func (c *Connections) saveAzureCLIProjectOnly(ctx context.Context, workspaceID, organizationURL string, input ConnectionInput) (bool, error) {
	unlock := c.locks.lock(workspaceID)
	defer unlock()
	current, found, err := c.store.Connection(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	if !found || current.AuthMethod != store.AuthMethodAzureCLI || current.OrganizationURL != organizationURL || current.ReauthRequired {
		return false, nil
	}
	current.DefaultProjectID = strings.TrimSpace(input.DefaultProjectID)
	current.DefaultProjectName = strings.TrimSpace(input.DefaultProjectName)
	current.UpdatedAt = c.now().UTC()
	return true, c.store.SaveConnection(ctx, workspaceID, current)
}

func (c *Connections) testAzureCLI(ctx context.Context, organizationURL string) (*azure.TestConnectionResult, error) {
	tenant := c.discoverAzCLITenant(ctx, organizationURL)
	tok, err := c.azCLI.AccessToken(ctx, tenant)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return &azure.TestConnectionResult{OK: false, Error: probeError(err)}, nil
	}
	result, err := c.newClient(organizationURL, azure.NewBearerAuthorizer(tok.AccessToken)).TestAuth(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return &azure.TestConnectionResult{OK: false, Error: probeError(err)}, nil
	}
	return result, nil
}

type azCLIAuthorizer struct {
	connections *Connections
	workspaceID string
	bound       store.Connection
	mu          sync.Mutex
	lastToken   string
}

func (a *azCLIAuthorizer) Header(ctx context.Context) (string, error) {
	token, _, err := a.connections.boundAzureCLIToken(ctx, a.workspaceID, a.bound)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.lastToken = token
	a.mu.Unlock()
	return azure.BearerPrefix + token, nil
}

func (a *azCLIAuthorizer) Renew(ctx context.Context, rejectedHeader string) (bool, error) {
	return a.connections.renewAzureCLIToken(ctx, a.workspaceID, a.bound, strings.TrimPrefix(rejectedHeader, azure.BearerPrefix))
}

func (a *azCLIAuthorizer) Redact(text string) string {
	a.mu.Lock()
	last := a.lastToken
	a.mu.Unlock()
	text = azure.RedactToken(text, last)
	if cached, ok := a.connections.getAzCLIToken(a.workspaceID); ok {
		text = azure.RedactToken(text, cached.token)
	}
	return text
}

func (c *Connections) boundAzureCLIToken(ctx context.Context, workspaceID string, bound store.Connection) (string, time.Time, error) {
	unlock := c.locks.lock(workspaceID)
	defer unlock()
	connection, err := c.boundConnection(ctx, workspaceID, bound)
	if err != nil {
		return "", time.Time{}, err
	}
	if cached, ok := c.getAzCLIToken(workspaceID); ok &&
		cached.generation == connection.Generation &&
		cached.organizationURL == connection.OrganizationURL &&
		cached.token != "" &&
		cached.expiresAt.Sub(c.now()) > azCLIRenewalMargin {
		return cached.token, cached.expiresAt, nil
	}
	tenant := connection.OAuthTenantID
	if !auth.IsGUID(tenant) {
		tenant = c.discoverAzCLITenant(ctx, connection.OrganizationURL)
	}
	tok, err := c.azCLI.AccessToken(ctx, tenant)
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := c.boundConnection(ctx, workspaceID, bound); err != nil {
		return "", time.Time{}, err
	}
	c.setAzCLIToken(workspaceID, cachedAzToken{
		generation:      connection.Generation,
		organizationURL: connection.OrganizationURL,
		token:           tok.AccessToken,
		expiresAt:       tok.ExpiresAt,
	})
	return tok.AccessToken, tok.ExpiresAt, nil
}

func (c *Connections) renewAzureCLIToken(ctx context.Context, workspaceID string, bound store.Connection, rejected string) (bool, error) {
	unlock := c.locks.lock(workspaceID)
	defer unlock()
	connection, err := c.boundConnection(ctx, workspaceID, bound)
	if err != nil {
		return false, err
	}
	if cached, ok := c.getAzCLIToken(workspaceID); ok &&
		cached.generation == connection.Generation &&
		cached.organizationURL == connection.OrganizationURL &&
		cached.token != "" &&
		rejected != "" &&
		cached.token != rejected {
		return true, nil
	}
	c.clearAzCLIToken(workspaceID)
	tenant := c.discoverAzCLITenant(ctx, connection.OrganizationURL)
	if tenant == "" && auth.IsGUID(connection.OAuthTenantID) {
		tenant = connection.OAuthTenantID
	}
	tok, err := c.azCLI.AccessToken(ctx, tenant)
	if err != nil {
		return false, err
	}
	if rejected != "" && tok.AccessToken == rejected {
		return false, nil
	}
	c.setAzCLIToken(workspaceID, cachedAzToken{
		generation:      connection.Generation,
		organizationURL: connection.OrganizationURL,
		token:           tok.AccessToken,
		expiresAt:       tok.ExpiresAt,
	})
	return true, nil
}
