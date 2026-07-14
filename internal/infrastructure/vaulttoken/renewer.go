// Package vaulttoken renews and revokes a handed per-deployment Vault token
// (D-125) over the Vault API. It implements usecase.DeploymentTokenOps so the
// fleet's in-memory token store can keep a handed token alive for the
// deployment lifetime (renew-self) and revoke it on Decommission (revoke-self)
// WITHOUT the fleet host holding any mint capability. The fleet is a pure bearer.
package vaulttoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	vault "github.com/hashicorp/vault/api"
)

// Renewer renews/revokes handed tokens by cloning a base Vault client (address +
// TLS only) and bearing the handed token on the clone. The base client's own
// token is never used — every operation runs under the handed token, so the
// renew-self / revoke-self ACL is the handed token's, not the fleet SVID's.
type Renewer struct {
	base *vault.Client
}

// New wires the renewer over a base Vault client (typically the fleet's
// svc/runtime SVID client — used only for its address + TLS config; its token is
// overridden per call).
func New(base *vault.Client) *Renewer { return &Renewer{base: base} }

// Renew renews the handed token via auth/token/renew-self and returns the new
// granted TTL so the caller can schedule the next renewal.
func (r *Renewer) Renew(ctx context.Context, token string) (time.Duration, error) {
	c, err := r.client(token)
	if err != nil {
		return 0, err
	}
	sec, err := c.Auth().Token().RenewSelfWithContext(ctx, 0)
	if err != nil {
		return 0, fmt.Errorf("renew-self: %w", err)
	}
	if sec == nil || sec.Auth == nil {
		return 0, errors.New("renew-self: vault returned no auth")
	}
	return time.Duration(sec.Auth.LeaseDuration) * time.Second, nil
}

// Revoke revokes the handed token via auth/token/revoke-self.
func (r *Renewer) Revoke(ctx context.Context, token string) error {
	c, err := r.client(token)
	if err != nil {
		return err
	}
	if err := c.Auth().Token().RevokeSelfWithContext(ctx, token); err != nil {
		return fmt.Errorf("revoke-self: %w", err)
	}
	return nil
}

// client clones the base client and sets the handed token on the clone.
func (r *Renewer) client(token string) (*vault.Client, error) {
	if r.base == nil {
		return nil, errors.New("vaulttoken: no base vault client")
	}
	c, err := r.base.Clone()
	if err != nil {
		return nil, fmt.Errorf("clone vault client: %w", err)
	}
	c.SetToken(token)
	if ns := r.base.Namespace(); ns != "" {
		c.SetNamespace(ns)
	}
	return c, nil
}
