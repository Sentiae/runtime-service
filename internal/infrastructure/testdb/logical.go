package testdb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// ErrSeedTemplateNotAllowed is returned when a logical-database provision names
// a seed template outside the operator-controlled allowlist. Seed selection is
// never trusted raw caller input.
var ErrSeedTemplateNotAllowed = errors.New("testdb: seed template not in allowlist")

// Provisioner implements the shared-tier logical-database port (R3).
var _ usecase.LogicalProvisioner = (*Provisioner)(nil)

// ProvisionLogical creates a dedicated LOGIN role and a logical database cloned
// from an allowlisted seed template, owned by that role, with PUBLIC connect
// revoked. It reuses the admin pool. It is idempotent per database name: an
// already-present database is a no-op success. The plaintext password lives only
// in this call frame — it is never returned, logged, or persisted.
func (p *Provisioner) ProvisionLogical(ctx context.Context, in usecase.LogicalProvisionRequest) (usecase.LogicalLease, error) {
	if in.DBName == "" || in.RoleName == "" {
		return usecase.LogicalLease{}, fmt.Errorf("testdb: db name and role name required")
	}
	if in.Password == "" {
		return usecase.LogicalLease{}, fmt.Errorf("testdb: role password required")
	}
	if !seedAllowed(in.SeedTemplate, in.AllowedTemplates) {
		return usecase.LogicalLease{}, fmt.Errorf("%w: %q", ErrSeedTemplateNotAllowed, in.SeedTemplate)
	}

	// Idempotent per db name: if the database already exists, the logical DB was
	// provisioned — return the lease unchanged.
	var dbExists bool
	if err := p.adminDB.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", in.DBName).Scan(&dbExists); err != nil {
		return usecase.LogicalLease{}, fmt.Errorf("testdb: lookup database: %w", err)
	}
	if dbExists {
		return usecase.LogicalLease{DBName: in.DBName, RoleName: in.RoleName}, nil
	}

	// Ensure the owning role. CREATE ROLE has no IF NOT EXISTS, so guard on
	// pg_roles. The password is a DDL string literal (Postgres forbids bind
	// parameters in CREATE ROLE) — quote it safely; NEVER echo it in an error.
	var roleExists bool
	if err := p.adminDB.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", in.RoleName).Scan(&roleExists); err != nil {
		return usecase.LogicalLease{}, fmt.Errorf("testdb: lookup role: %w", err)
	}
	if !roleExists {
		createRole := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s`, quoteIdent(in.RoleName), quoteLiteral(in.Password))
		if _, err := p.adminDB.ExecContext(ctx, createRole); err != nil {
			return usecase.LogicalLease{}, fmt.Errorf("testdb: create role %s: %w", in.RoleName, redactPassword(err, in.Password))
		}
	}

	// Clone the seed template into the new database, owned by the role.
	createDB := fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s OWNER %s`,
		quoteIdent(in.DBName), quoteIdent(in.SeedTemplate), quoteIdent(in.RoleName))
	if _, err := p.adminDB.ExecContext(ctx, createDB); err != nil {
		if isObjectInUse(err) {
			if _, retryErr := p.adminDB.ExecContext(ctx, createDB); retryErr != nil {
				return usecase.LogicalLease{}, fmt.Errorf("testdb: create database %s: %w", in.DBName, retryErr)
			}
		} else {
			return usecase.LogicalLease{}, fmt.Errorf("testdb: create database %s: %w", in.DBName, err)
		}
	}

	// Fail closed on isolation: a database PUBLIC can still connect to is not
	// isolated. If the revoke fails, drop the half-built database and error.
	revoke := fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, quoteIdent(in.DBName))
	if _, err := p.adminDB.ExecContext(ctx, revoke); err != nil {
		if dropErr := p.DropLogical(ctx, in.DBName, ""); dropErr != nil {
			return usecase.LogicalLease{}, fmt.Errorf("testdb: revoke connect on %s (cleanup failed: %v): %w", in.DBName, dropErr, err)
		}
		return usecase.LogicalLease{}, fmt.Errorf("testdb: revoke connect on %s: %w", in.DBName, err)
	}
	return usecase.LogicalLease{DBName: in.DBName, RoleName: in.RoleName}, nil
}

// DropLogical terminates connections to the logical database, drops it, then
// drops the owning role. Both drops use IF EXISTS so a partially-provisioned or
// already-reclaimed resource is idempotent. An empty dbName/roleName skips that
// drop (used by the create path's cleanup, which has no role to remove).
func (p *Provisioner) DropLogical(ctx context.Context, dbName, roleName string) error {
	if dbName != "" {
		_, _ = p.adminDB.ExecContext(ctx,
			`SELECT pg_terminate_backend(pid)
			   FROM pg_stat_activity
			  WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
		if _, err := p.adminDB.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName))); err != nil {
			return fmt.Errorf("testdb: drop database %s: %w", dbName, err)
		}
	}
	if roleName != "" {
		if _, err := p.adminDB.ExecContext(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteIdent(roleName))); err != nil {
			return fmt.Errorf("testdb: drop role %s: %w", roleName, err)
		}
	}
	return nil
}

// seedAllowed reports whether seed is in the operator allowlist. An empty
// allowlist or an empty seed denies (fail closed).
func seedAllowed(seed string, allowed []string) bool {
	if seed == "" {
		return false
	}
	for _, a := range allowed {
		if a == seed {
			return true
		}
	}
	return false
}

// quoteLiteral renders s as a safe Postgres string literal (single quotes
// doubled; backslashes force the E'' escape form). Mirrors lib/pq's own
// quoteLiteral — used for the CREATE ROLE password, which cannot be a bind
// parameter.
func quoteLiteral(s string) string {
	if strings.Contains(s, `\`) {
		return " E'" + strings.NewReplacer(`'`, `''`, `\`, `\\`).Replace(s) + "'"
	}
	return "'" + strings.ReplaceAll(s, `'`, `''`) + "'"
}

// redactPassword strips any occurrence of the plaintext password from an error
// string so a driver error that echoes the failing statement can never leak it.
func redactPassword(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, password) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, password, "[REDACTED]"))
}
