//go:build unit

package di

import (
	"testing"

	"github.com/sentiae/runtime-service/pkg/config"
)

// TestMigrateDatabase_RefusesWithoutOwnerCredentials proves the one migrating
// path — boot with migrations enabled and `runtime-service migrate` — refuses
// before dialling when the owner credential is absent, whatever the boot flag
// says. The config here is a fleet host's: migrations disabled, app credentials
// only. The host points at a port nothing listens on, so a refusal that came
// from dialling would read as a connection error, not this message.
//
// Control: drop the ValidateMigrateCredentials call from MigrateDatabase ⇒ the
// error becomes a connection failure (or, against a real server, an attempt to
// migrate as user "") and the exact-message assertion fails.
func TestMigrateDatabase_RefusesWithoutOwnerCredentials(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.Postgres = config.PostgresConfig{
		Host:       "127.0.0.1",
		Port:       "1",
		User:       "runtime_service_fc_app",
		Password:   "app-pw",
		Database:   "runtime_service_fc",
		SSLMode:    "disable",
		LogLevel:   "warn",
		Migrations: config.MigrationsConfig{Enabled: false},
	}

	_, _, err := MigrateDatabase(cfg)
	want := "load config: database migrate user is required (APP_DATABASE_MIGRATE_USER)"
	if err == nil || err.Error() != want {
		t.Fatalf("MigrateDatabase() error = %v, want exactly %q", err, want)
	}
}
