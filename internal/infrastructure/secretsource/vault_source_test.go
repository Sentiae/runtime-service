//go:build unit

package secretsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
)

// recordingResolver is the P14 seam under test: it captures the ref and the
// principal the source built, and answers whatever the row wants.
type recordingResolver struct {
	refs       []string
	principals []secret.Principal
	value      secret.SecretValue
	err        error
}

func (r *recordingResolver) Resolve(_ context.Context, ref string, p secret.Principal) (secret.SecretValue, error) {
	r.refs = append(r.refs, ref)
	r.principals = append(r.principals, p)
	return r.value, r.err
}

// staticGetter is the minimal Vault KV surface secret.NewVaultResolver needs.
type staticGetter struct{ value string }

func (g staticGetter) GetSecret(context.Context, string, string) (string, error) {
	return g.value, nil
}

// mustSecretValue mints a real secret.SecretValue. The type's field is
// unexported, so the only way to obtain a non-empty one outside platform-kit is
// through a real resolver — which is exactly what the source consumes.
func mustSecretValue(t *testing.T, value string) secret.SecretValue {
	t.Helper()
	org := uuid.New()
	v, err := secret.NewVaultResolver(staticGetter{value: value}).Resolve(
		context.Background(),
		secret.TenantRef(org, "flows/staging/mint", "value"),
		secret.Principal{Service: "test", OrgID: org.String()},
	)
	if err != nil {
		t.Fatalf("mint secret value: %v", err)
	}
	return v
}

// T3.1 — the ref grammar and the principal are runtime's, not the caller's: the
// source builds tenants/<org>/flows/<environment>/<name>#value and resolves it
// as service "runtime" for the ref's own org, bearing the token handed with the
// run. An absent secret is (found=false, err=nil); every other failure is an
// error, so a broken Vault can never read as an unset secret.
//
// Control: build the ref without its field (drop "#value") ⇒ every ref row
// fails. Second control: map secret.ErrSecretNotFound to (found=true) ⇒ the
// absent row fails.
func TestVaultSource_RefAndPrincipal(t *testing.T) {
	org := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	tests := []struct {
		name        string
		environment string
		secretName  string
		resolverErr error
		wantRef     string
		wantValue   string
		wantFound   bool
		wantErr     error
	}{
		{
			name: "staging", environment: "staging", secretName: "greeting_suffix",
			wantRef:   "tenants/11111111-2222-3333-4444-555555555555/flows/staging/greeting_suffix#value",
			wantValue: "s3cr3t", wantFound: true,
		},
		{
			name: "dev", environment: "dev", secretName: "api_key",
			wantRef:   "tenants/11111111-2222-3333-4444-555555555555/flows/dev/api_key#value",
			wantValue: "s3cr3t", wantFound: true,
		},
		{
			name: "prod", environment: "prod", secretName: "api_key",
			wantRef:   "tenants/11111111-2222-3333-4444-555555555555/flows/prod/api_key#value",
			wantValue: "s3cr3t", wantFound: true,
		},
		{
			name: "absent secret is not an error", environment: "staging", secretName: "greeting_suffix",
			resolverErr: fmt.Errorf("%w: some ref", secret.ErrSecretNotFound),
			wantRef:     "tenants/11111111-2222-3333-4444-555555555555/flows/staging/greeting_suffix#value",
		},
		{
			name: "vault failure is an error", environment: "staging", secretName: "greeting_suffix",
			resolverErr: secret.ErrVaultUnavailable,
			wantRef:     "tenants/11111111-2222-3333-4444-555555555555/flows/staging/greeting_suffix#value",
			wantErr:     secret.ErrVaultUnavailable,
		},
		{
			name: "no handed token is an error", environment: "staging", secretName: "greeting_suffix",
			resolverErr: secret.ErrNoHandedToken,
			wantRef:     "tenants/11111111-2222-3333-4444-555555555555/flows/staging/greeting_suffix#value",
			wantErr:     secret.ErrNoHandedToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &recordingResolver{value: mustSecretValue(t, "s3cr3t"), err: tt.resolverErr}
			src := New(resolver, &recordingRevoker{})

			value, found, err := src.Resolve(context.Background(), org, "handed-token", tt.environment, tt.secretName)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Resolve error = %v, want nil", err)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if value != tt.wantValue {
				t.Fatalf("value = %q, want %q", value, tt.wantValue)
			}

			if len(resolver.refs) != 1 {
				t.Fatalf("resolver called %d time(s), want 1", len(resolver.refs))
			}
			if resolver.refs[0] != tt.wantRef {
				t.Fatalf("ref = %q, want %q", resolver.refs[0], tt.wantRef)
			}
			want := secret.Principal{Service: "runtime", OrgID: org.String(), Token: "handed-token"}
			if resolver.principals[0] != want {
				t.Fatalf("principal = %#v, want %#v", resolver.principals[0], want)
			}
		})
	}
}

// A failed resolve is reported by reference, never by value: the error a node
// failure is built from must not carry the plaintext the source was reaching
// for. Anchor: the successful call DOES return that plaintext, so this is not
// asserting on a source that resolves nothing.
//
// Control: wrap the failure as fmt.Errorf("resolve %s: %w", value, err) ⇒ the
// canary appears in the error text and this fails.
func TestVaultSource_ErrorCarriesNoValue(t *testing.T) {
	const canary = "p4-canary-value"
	org := uuid.New()

	ok := &recordingResolver{value: mustSecretValue(t, canary)}
	value, found, err := New(ok, &recordingRevoker{}).Resolve(context.Background(), org, "handed-token", "staging", "greeting_suffix")
	if err != nil || !found || value != canary {
		t.Fatalf("anchor: Resolve = (%q, %v, %v), want (%q, true, nil)", value, found, err, canary)
	}

	failing := &recordingResolver{value: mustSecretValue(t, canary), err: secret.ErrVaultUnavailable}
	_, _, err = New(failing, &recordingRevoker{}).Resolve(context.Background(), org, "handed-token", "staging", "greeting_suffix")
	if err == nil {
		t.Fatal("Resolve error = nil, want the vault failure")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("the error text carries the secret value: %q", err.Error())
	}
	if strings.Contains(err.Error(), "handed-token") {
		t.Fatalf("the error text carries the handed token: %q", err.Error())
	}
}

type recordingRevoker struct {
	tokens []string
	err    error
}

func (r *recordingRevoker) Revoke(_ context.Context, token string) error {
	r.tokens = append(r.tokens, token)
	return r.err
}

// Revoke hands the run's own token back — the exact token, unmodified, so
// revoke-self runs under the credential the run was given (D-125).
//
// Control: pass a constant or the source's own credential instead of the handed
// token ⇒ the token assertion fails.
func TestVaultSource_RevokeHandsBackTheRunToken(t *testing.T) {
	revoker := &recordingRevoker{}
	src := New(&recordingResolver{}, revoker)

	if err := src.Revoke(context.Background(), "handed-token"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(revoker.tokens) != 1 || revoker.tokens[0] != "handed-token" {
		t.Fatalf("revoked %v, want [handed-token]", revoker.tokens)
	}

	// A refused revocation must reach the caller with its CAUSE intact, not
	// merely as "some error": the engine's terminal cleanup is the only place
	// that learns the per-run credential outlived its run, and D-7 was exactly a
	// revoke-self 403 whose signal nothing could act on. `err != nil` alone would
	// stay green if the %w in VaultSource.Revoke became a %v, so the chain is
	// what is asserted.
	//
	// Control: change `revoke handed token: %w` to `%v` in vault_source.go ⇒ the
	// errors.Is check fails.
	cause := errors.New("revoke-self: 403")
	err := New(&recordingResolver{}, &recordingRevoker{err: cause}).Revoke(context.Background(), "handed-token")
	if err == nil {
		t.Fatal("Revoke error = nil, want the revoker's failure")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Revoke error = %v, want it to wrap the revoker's cause", err)
	}
}

// Without Vault the source must REFUSE, not answer "absent": a node that
// declares a secret and runs without it is the failure this fails closed to
// prevent.
//
// Control: return ("", false, nil) from Unavailable.Resolve ⇒ both rows fail.
func TestUnavailable_FailsClosed(t *testing.T) {
	if _, _, err := (Unavailable{}).Resolve(context.Background(), uuid.New(), "t", "staging", "greeting_suffix"); !errors.Is(err, domain.ErrNodeRunnerNotReady) {
		t.Fatalf("Resolve error = %v, want ErrNodeRunnerNotReady", err)
	}
	if err := (Unavailable{}).Revoke(context.Background(), "t"); !errors.Is(err, domain.ErrNodeRunnerNotReady) {
		t.Fatalf("Revoke error = %v, want ErrNodeRunnerNotReady", err)
	}
}
