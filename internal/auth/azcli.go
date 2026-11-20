package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	azCLITimeout         = 15 * time.Second
	azCLIWaitDelay       = 2 * time.Second
	maxAzCLIOutputBytes  = 1 << 20
	defaultAzCLITokenTTL = 5 * time.Minute
)

// AzCLIToken is a short-lived Azure DevOps bearer token acquired from the
// host's Azure CLI session.
type AzCLIToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

// AzureCLIProvider acquires short-lived Azure DevOps access tokens from an
// Azure CLI installation on the Kandev host.
type AzureCLIProvider interface {
	AccessToken(ctx context.Context, tenant string) (*AzCLIToken, error)
}

// AzCLIErrorKind classifies Azure CLI failures without exposing subprocess
// output or token material.
type AzCLIErrorKind string

const (
	AzCLINotInstalled AzCLIErrorKind = "not_installed"
	AzCLINotSignedIn  AzCLIErrorKind = "not_signed_in"
	AzCLIFailed       AzCLIErrorKind = "failed"
)

// AzCLIError is a credential-free failure from the Azure CLI token provider.
type AzCLIError struct {
	Kind AzCLIErrorKind
}

func (e *AzCLIError) Error() string {
	switch e.Kind {
	case AzCLINotInstalled:
		return "Azure CLI (az) is not installed on the Kandev host"
	case AzCLINotSignedIn:
		return "Azure CLI is not signed in; run az login on the Kandev host"
	default:
		return "Azure CLI failed to acquire an Azure DevOps token"
	}
}

type azCLIRunner func(ctx context.Context, args []string) (stdout, stderr []byte, err error)

// AzCLIClient acquires Azure DevOps tokens through `az account get-access-token`.
type AzCLIClient struct {
	run azCLIRunner
	now func() time.Time
}

// NewAzCLIClient returns an Azure CLI token provider backed by `az` on PATH.
func NewAzCLIClient() *AzCLIClient {
	return &AzCLIClient{run: runAzCLI, now: time.Now}
}

func (c *AzCLIClient) AccessToken(ctx context.Context, tenant string) (*AzCLIToken, error) {
	tenant = strings.ToLower(strings.TrimSpace(tenant))
	args := []string{
		"account", "get-access-token",
		"--resource", AzureDevOpsResourceID,
		"--output", "json",
		"--only-show-errors",
	}
	withTenant := IsGUID(tenant)
	runArgs := args
	if withTenant {
		runArgs = append(append([]string{}, args...), "--tenant", tenant)
	}
	stdout, stderr, err := c.run(ctx, runArgs)
	if err != nil && withTenant && tenantUnsupportedByAccount(stderr) {
		stdout, stderr, err = c.run(ctx, args)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var azErr *AzCLIError
		if errors.As(err, &azErr) {
			return nil, azErr
		}
		return nil, classifyAzCLIError(stderr)
	}
	return parseAzCLIToken(stdout, c.now().UTC())
}

func tenantUnsupportedByAccount(stderr []byte) bool {
	lower := strings.ToLower(string(stderr))
	return strings.Contains(lower, "managed identity") ||
		strings.Contains(lower, "cloud shell") ||
		strings.Contains(lower, "tenant shouldn't be specified")
}

func classifyAzCLIError(stderr []byte) *AzCLIError {
	lower := strings.ToLower(string(stderr))
	for _, marker := range []string{
		"az login",
		"not logged in",
		"no subscription found",
		"interaction_required",
		"login_required",
		"consent_required",
		"invalid_grant",
		"aadsts",
		"refresh token",
		"re-authenticate",
	} {
		if strings.Contains(lower, marker) {
			return &AzCLIError{Kind: AzCLINotSignedIn}
		}
	}
	return &AzCLIError{Kind: AzCLIFailed}
}

func parseAzCLIToken(stdout []byte, now time.Time) (*AzCLIToken, error) {
	var raw struct {
		AccessToken  string          `json:"accessToken"`
		TokenType    string          `json:"tokenType"`
		ExpiresOn    json.RawMessage `json:"expires_on"`
		ExpiresOnISO string          `json:"expiresOn"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, &AzCLIError{Kind: AzCLIFailed}
	}
	token := strings.TrimSpace(raw.AccessToken)
	if token == "" || strings.IndexFunc(token, unicode.IsSpace) != -1 {
		return nil, &AzCLIError{Kind: AzCLIFailed}
	}
	if tokenType := strings.TrimSpace(raw.TokenType); tokenType != "" && !strings.EqualFold(tokenType, "Bearer") {
		return nil, &AzCLIError{Kind: AzCLIFailed}
	}
	return &AzCLIToken{
		AccessToken: token,
		ExpiresAt:   parseAzCLIExpiry(raw.ExpiresOn, raw.ExpiresOnISO, now),
	}, nil
}

func parseAzCLIExpiry(expiresOn json.RawMessage, expiresOnISO string, now time.Time) time.Time {
	if unixSeconds, ok := parseUnixSeconds(expiresOn); ok && unixSeconds > 0 {
		return time.Unix(unixSeconds, 0).UTC()
	}
	if trimmed := strings.TrimSpace(expiresOnISO); trimmed != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed.UTC()
			}
		}
	}
	return now.Add(defaultAzCLITokenTTL)
}

func parseUnixSeconds(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	var asInt int64
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return asInt, true
	}
	var asFloat float64
	if err := json.Unmarshal(raw, &asFloat); err == nil {
		return int64(asFloat), true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if parsed, convErr := strconv.ParseFloat(strings.TrimSpace(asString), 64); convErr == nil {
			return int64(parsed), true
		}
	}
	return 0, false
}

func runAzCLI(ctx context.Context, args []string) ([]byte, []byte, error) {
	executable, err := exec.LookPath("az")
	if err != nil {
		return nil, nil, &AzCLIError{Kind: AzCLINotInstalled}
	}
	cctx, cancel := context.WithTimeout(ctx, azCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, executable, args...)
	cmd.WaitDelay = azCLIWaitDelay
	cmd.Env = append(os.Environ(), "AZURE_CORE_NO_COLOR=1", "AZURE_CORE_ONLY_SHOW_ERRORS=1")
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if cctx.Err() != nil && ctx.Err() == nil {
		return nil, nil, &AzCLIError{Kind: AzCLIFailed}
	}
	return stdout.Bytes(), stderr.Bytes(), runErr
}

type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	remaining := maxAzCLIOutputBytes - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return total, nil
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, err := b.buf.Write(p)
	return total, err
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

var _ AzureCLIProvider = (*AzCLIClient)(nil)
