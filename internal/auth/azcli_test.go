package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAzCLIClientPassesResourceAndValidGUIDTenant(t *testing.T) {
	fixedNow := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	var recorded [][]string
	client := &AzCLIClient{
		now: func() time.Time { return fixedNow },
		run: func(_ context.Context, args []string) ([]byte, []byte, error) {
			recorded = append(recorded, append([]string{}, args...))
			return []byte(`{"accessToken":"secret-cli-token","expires_on":1795150800,"tokenType":"Bearer"}`), nil, nil
		},
	}

	tok, err := client.AccessToken(context.Background(), "  AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE ")
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	if tok.AccessToken != "secret-cli-token" || !tok.ExpiresAt.Equal(time.Unix(1795150800, 0).UTC()) {
		t.Fatalf("token = %+v", tok)
	}
	wantWithTenant := []string{
		"account", "get-access-token",
		"--resource", AzureDevOpsResourceID,
		"--output", "json",
		"--only-show-errors",
		"--tenant", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	if len(recorded) != 1 || !reflect.DeepEqual(recorded[0], wantWithTenant) {
		t.Fatalf("args = %v, want %v", recorded, wantWithTenant)
	}

	recorded = nil
	for _, nonGUID := range []string{"", "organizations", "tenant-1 & calc.exe"} {
		if _, err := client.AccessToken(context.Background(), nonGUID); err != nil {
			t.Fatalf("non-GUID %q error = %v", nonGUID, err)
		}
	}
	wantWithoutTenant := wantWithTenant[:len(wantWithTenant)-2]
	for i, got := range recorded {
		if !reflect.DeepEqual(got, wantWithoutTenant) {
			t.Fatalf("call %d args = %v, want %v", i, got, wantWithoutTenant)
		}
	}
}

func TestAzCLIClientRetriesWithoutTenantForManagedIdentity(t *testing.T) {
	var calls int
	client := &AzCLIClient{
		now: time.Now,
		run: func(_ context.Context, args []string) ([]byte, []byte, error) {
			calls++
			if len(args) > 7 {
				return nil, []byte("ERROR: --tenant shouldn't be specified for Cloud Shell or Managed Identity account."), errors.New("exit status 1")
			}
			return []byte(`{"accessToken":"msi-token","expires_on":"1795150800"}`), nil, nil
		},
	}
	tok, err := client.AccessToken(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err != nil || tok.AccessToken != "msi-token" || calls != 2 {
		t.Fatalf("tok = %+v, err = %v, calls = %d", tok, err, calls)
	}
}

func TestAzCLIClientParsesExpiryFallbacksAndRejectsMalformedOutput(t *testing.T) {
	fixedNow := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	isoToken, err := parseAzCLIToken([]byte(`{"accessToken":"iso-tok","expiresOn":"2026-11-20T04:30:00Z"}`), fixedNow)
	if err != nil || !isoToken.ExpiresAt.Equal(time.Date(2026, 11, 20, 4, 30, 0, 0, time.UTC)) {
		t.Fatalf("ISO expiry token = %+v, err = %v", isoToken, err)
	}

	fallbackToken, err := parseAzCLIToken([]byte(`{"accessToken":"fallback-tok"}`), fixedNow)
	if err != nil || !fallbackToken.ExpiresAt.Equal(fixedNow.Add(defaultAzCLITokenTTL)) {
		t.Fatalf("fallback expiry token = %+v, err = %v", fallbackToken, err)
	}

	for name, payload := range map[string]string{
		"invalid json":     `not-json-secret-marker`,
		"empty token":      `{"accessToken":"  "}`,
		"whitespace token": `{"accessToken":"secret-marker\nline2"}`,
		"wrong token type": `{"accessToken":"secret-marker","tokenType":"MAC"}`,
	} {
		_, err := parseAzCLIToken([]byte(payload), fixedNow)
		var azErr *AzCLIError
		if !errors.As(err, &azErr) || azErr.Kind != AzCLIFailed {
			t.Fatalf("%s: err = %v, want AzCLIFailed", name, err)
		}
		if strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("%s: error leaked payload: %v", name, err)
		}
	}
}

func TestAzCLIClientClassifiesErrorsWithoutLeakingSubprocessOutput(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		err    error
		want   AzCLIErrorKind
	}{
		{
			name: "not installed",
			err:  &AzCLIError{Kind: AzCLINotInstalled},
			want: AzCLINotInstalled,
		},
		{
			name:   "not logged in",
			stderr: "ERROR: Please run 'az login' to setup account. secret-material",
			err:    errors.New("exit status 1"),
			want:   AzCLINotSignedIn,
		},
		{
			name:   "entra interaction required",
			stderr: "AADSTS50076: Multi-factor authentication required. secret-material",
			err:    errors.New("exit status 1"),
			want:   AzCLINotSignedIn,
		},
		{
			name:   "other failure",
			stderr: "unexpected internal traceback secret-material",
			err:    errors.New("exit status 1"),
			want:   AzCLIFailed,
		},
	}
	for _, tc := range cases {
		client := &AzCLIClient{
			now: time.Now,
			run: func(context.Context, []string) ([]byte, []byte, error) {
				return []byte("secret-stdout"), []byte(tc.stderr), tc.err
			},
		}
		_, err := client.AccessToken(context.Background(), "")
		var azErr *AzCLIError
		if !errors.As(err, &azErr) || azErr.Kind != tc.want {
			t.Fatalf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "secret-") {
			t.Fatalf("%s: error leaked subprocess text: %v", tc.name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledClient := &AzCLIClient{
		now: time.Now,
		run: func(ctx context.Context, _ []string) ([]byte, []byte, error) {
			return nil, nil, ctx.Err()
		},
	}
	if _, err := cancelledClient.AccessToken(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v, want context.Canceled", err)
	}
}
