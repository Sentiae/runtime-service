package config

import (
	"strings"
	"testing"
)

// withOwnerCredentials sets the owner credentials every Load that migrates
// needs. They have NO default and NO fallback to the app credentials (D-490): a
// config-name miss must refuse the boot rather than silently run DDL as the
// serving role.
func withOwnerCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("APP_DATABASE_MIGRATE_USER", "runtime_service_owner")
	t.Setenv("APP_DATABASE_MIGRATE_PASSWORD", "owner-pw")
}

// TestLoad_OwnerCredentialsRequiredExactlyWhenMigrating proves the owner
// credentials are required config with no fallback — but only on a process that
// migrates at boot. APP_DATABASE_MIGRATIONS_ENABLED=false (the fleet host, which
// must never hold the owner credential) loads without them, and the flag reaches
// the struct the boot reads.
//
// Control (the refusal): drop the ValidateMigrateCredentials call from Load, or
// give database.postgres.migrate_user a default ⇒ the two refusal rows load.
// Control (the condition): make the check unconditional ⇒ the "disabled" row
// refuses, i.e. the fleet host stops booting.
func TestLoad_OwnerCredentialsRequiredExactlyWhenMigrating(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantErr     string
		wantEnabled bool
	}{
		{
			name:    "migrating by default without an owner user refuses",
			env:     map[string]string{"APP_DATABASE_MIGRATE_PASSWORD": "owner-pw"},
			wantErr: "load config: database migrate user is required (APP_DATABASE_MIGRATE_USER)",
		},
		{
			name: "migrating without an owner password refuses",
			env: map[string]string{
				"APP_DATABASE_MIGRATIONS_ENABLED": "true",
				"APP_DATABASE_MIGRATE_USER":       "runtime_service_owner",
			},
			wantErr: "load config: database migrate password is required (APP_DATABASE_MIGRATE_PASSWORD)",
		},
		{
			name: "migrating with both owner credentials loads",
			env: map[string]string{
				"APP_DATABASE_MIGRATE_USER":     "runtime_service_owner",
				"APP_DATABASE_MIGRATE_PASSWORD": "owner-pw",
			},
			wantEnabled: true,
		},
		{
			name:        "migrations disabled loads without owner credentials",
			env:         map[string]string{"APP_DATABASE_MIGRATIONS_ENABLED": "false"},
			wantEnabled: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", nodeRunnerHost)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() succeeded; want refusal %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("Load() error = %q, want exactly %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg.Database.Postgres.Migrations.Enabled != tt.wantEnabled {
				t.Fatalf("Migrations.Enabled = %v, want %v", cfg.Database.Postgres.Migrations.Enabled, tt.wantEnabled)
			}
		})
	}
}

// TestValidateMigrateCredentials_IgnoresTheBootFlag proves the check the
// `migrate` command runs is unconditional: a migrations-disabled config without
// owner credentials — exactly a fleet host's — cannot migrate.
//
// Control: gate ValidateMigrateCredentials on Migrations.Enabled ⇒ it returns
// nil here and the command would dial the database as user "".
func TestValidateMigrateCredentials_IgnoresTheBootFlag(t *testing.T) {
	t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", nodeRunnerHost)
	t.Setenv("APP_DATABASE_MIGRATIONS_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	err = cfg.ValidateMigrateCredentials()
	want := "load config: database migrate user is required (APP_DATABASE_MIGRATE_USER)"
	if err == nil || err.Error() != want {
		t.Fatalf("ValidateMigrateCredentials() = %v, want %q", err, want)
	}
}

// TestMigrateDatabaseDSN_UsesTheOwnerCredentials proves the owner DSN is built
// from the migrate credentials and never from the app ones, and the serving DSN
// the other way round.
//
// Control: build MigrateDatabaseDSN from Database.Postgres.User/Password ⇒ red.
func TestMigrateDatabaseDSN_UsesTheOwnerCredentials(t *testing.T) {
	t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", nodeRunnerHost)
	t.Setenv("APP_DATABASE_USER", "runtime_service_app")
	t.Setenv("APP_DATABASE_PASSWORD", "app-pw")
	t.Setenv("APP_DATABASE_NAME", "runtime_service_fc")
	withOwnerCredentials(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	owner, app := cfg.MigrateDatabaseDSN(), cfg.DatabaseDSN()
	for _, want := range []string{"user=runtime_service_owner", "password=owner-pw", "dbname=runtime_service_fc"} {
		if !strings.Contains(owner, want) {
			t.Errorf("owner DSN = %q, want it to contain %q", owner, want)
		}
	}
	for _, notWant := range []string{"user=runtime_service_app", "password=app-pw"} {
		if strings.Contains(owner, notWant) {
			t.Errorf("owner DSN = %q, want NOT %q", owner, notWant)
		}
	}
	for _, want := range []string{"user=runtime_service_app", "password=app-pw", "dbname=runtime_service_fc"} {
		if !strings.Contains(app, want) {
			t.Errorf("app DSN = %q, want it to contain %q", app, want)
		}
	}
	for _, notWant := range []string{"user=runtime_service_owner", "password=owner-pw"} {
		if strings.Contains(app, notWant) {
			t.Errorf("app DSN = %q, want NOT %q", app, notWant)
		}
	}
}
