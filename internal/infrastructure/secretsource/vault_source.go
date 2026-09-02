// Package secretsource resolves a flow node's declared secrets to values, and
// hands the run's minted Vault token back when the run ends.
//
// It is the VALUE half of the node secret path and nothing else: the broker
// that a node redeems its handles against runs inside the per-invocation
// sidecar (platform-kit/nodebroker), never in this process. What this package
// produces is a plaintext string that the invoker puts straight into the
// sidecar binding — runtime memory → the sidecar's attached stdin → sidecar
// memory. It is never written to a file, an argument, an environment variable,
// a log line, or node_executions.input, which is why nothing here logs and
// nothing here formats a value into an error.
package secretsource

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// secretRefField is the ONE field every flow secret is sealed under. A flow
// secret is a single opaque value, so the ref's field is fixed rather than
// caller-supplied: identity's SealTenantSecret chooses the subpath, runtime
// owns the grammar (D-4).
const secretRefField = "value"

// flowSubpathPrefix is the segment that separates flow secrets from every other
// tenant secret under tenants/<org>/.
const flowSubpathPrefix = "flows/"

// TokenRevoker revokes the per-run Vault token delivery minted and handed to
// this run. Implemented by internal/infrastructure/vaulttoken.Renewer, which
// runs revoke-self under the handed token itself — so runtime revokes what it
// was handed and holds no mint or revoke-any capability of its own (D-125).
type TokenRevoker interface {
	Revoke(ctx context.Context, token string) error
}

// VaultSource is the Vault-backed SecretValueSource: it builds the tenant ref
// for a declared secret, resolves it under the token handed with the run, and
// revokes that token at terminal cleanup.
type VaultSource struct {
	resolver secret.Resolver
	revoker  TokenRevoker
}

var _ usecase.SecretValueSource = (*VaultSource)(nil)

// New wires the source over the platform's P14 resolver and the handed-token
// revoker.
func New(resolver secret.Resolver, revoker TokenRevoker) *VaultSource {
	return &VaultSource{resolver: resolver, revoker: revoker}
}

// Resolve answers a node's declared secret for one org, in one flow
// environment, under the token handed with the run.
//
// The ref is tenants/<org>/flows/<environment>/<name>#value (D-4): runtime owns
// this grammar, so delivery never ships refs and a node never names a path.
// An absent secret is (found=false, err=nil) — the caller decides whether the
// spec made it required; every other failure is an error, so a broken Vault can
// never be mistaken for an unset secret.
func (s *VaultSource) Resolve(
	ctx context.Context,
	org uuid.UUID,
	handedToken, environment, name string,
) (string, bool, error) {
	ref := secret.TenantRef(org, flowSubpathPrefix+environment+"/"+name, secretRefField)
	value, err := s.resolver.Resolve(ctx, ref, secret.Principal{
		Service: "runtime",
		OrgID:   org.String(),
		Token:   handedToken,
	})
	if err != nil {
		if errors.Is(err, secret.ErrSecretNotFound) {
			return "", false, nil
		}
		// The ref is a reference, not a secret; the value and the token are
		// neither formatted nor wrapped in here.
		return "", false, fmt.Errorf("secret source: resolve %s: %w", ref, err)
	}
	return value.Reveal(), true, nil
}

// Revoke hands the run's token back. It is called once per run at terminal
// cleanup, whatever the run's outcome.
func (s *VaultSource) Revoke(ctx context.Context, handedToken string) error {
	if err := s.revoker.Revoke(ctx, handedToken); err != nil {
		return fmt.Errorf("secret source: revoke handed token: %w", err)
	}
	return nil
}

// Unavailable is the fail-closed source this service holds when Vault is not
// configured or was unreachable at boot. A node that declares a secret must
// refuse, never run with the secret silently absent — so both methods answer
// ErrNodeRunnerNotReady rather than (found=false).
type Unavailable struct{}

var _ usecase.SecretValueSource = Unavailable{}

// Resolve refuses.
func (Unavailable) Resolve(context.Context, uuid.UUID, string, string, string) (string, bool, error) {
	return "", false, domain.ErrNodeRunnerNotReady
}

// Revoke refuses.
func (Unavailable) Revoke(context.Context, string) error { return domain.ErrNodeRunnerNotReady }
