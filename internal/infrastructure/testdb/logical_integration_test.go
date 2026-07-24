//go:build integration

package testdb

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPG boots a throwaway Postgres and returns an admin config + a cleanup.
func startPG(t *testing.T) Config {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	p, _ := strconv.Atoi(port.Port())

	// Give the seed template a table + row so we can prove clone contents.
	adminDSN := buildDSN(Config{Host: host, Port: p, User: "postgres", Password: "postgres", AdminDatabase: "postgres", SSLMode: "disable"}, "postgres")
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()
	waitReady(t, admin)
	if _, err := admin.Exec(`CREATE DATABASE tmpl_app`); err != nil {
		t.Fatalf("create template db: %v", err)
	}
	seedDSN := buildDSN(Config{Host: host, Port: p, User: "postgres", Password: "postgres", AdminDatabase: "postgres", SSLMode: "disable"}, "tmpl_app")
	seed, err := sql.Open("pgx", seedDSN)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(`CREATE TABLE widgets (id int PRIMARY KEY); INSERT INTO widgets VALUES (1),(2),(3)`); err != nil {
		_ = seed.Close()
		t.Fatalf("seed template: %v", err)
	}
	_ = seed.Close() // must be idle for TEMPLATE cloning

	return Config{
		Host: host, Port: p, User: "postgres", Password: "postgres",
		AdminDatabase: "postgres", Template: "tmpl_app", SSLMode: "disable",
	}
}

func waitReady(t *testing.T, db *sql.DB) {
	t.Helper()
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("postgres never became ready")
}

func TestProvisionLogical_Integration(t *testing.T) {
	cfg := startPG(t)
	p, err := NewProvisioner(cfg)
	if err != nil {
		t.Fatalf("new provisioner: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	req := usecase.LogicalProvisionRequest{
		DBName:           "res_orders_abc123",
		RoleName:         "r_orders_ff00",
		Password:         "s3cr3t'pw",
		SeedTemplate:     "tmpl_app",
		AllowedTemplates: []string{"tmpl_app"},
	}
	lease, err := p.ProvisionLogical(ctx, req)
	if err != nil {
		t.Fatalf("provision logical: %v", err)
	}
	if lease.DBName != req.DBName || lease.RoleName != req.RoleName {
		t.Fatalf("lease = %+v", lease)
	}

	admin, err := sql.Open("pgx", buildDSN(cfg, cfg.AdminDatabase))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	// Role + database exist.
	if !exists(t, admin, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", req.RoleName) {
		t.Error("role not created")
	}
	if !exists(t, admin, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", req.DBName) {
		t.Error("database not created")
	}

	// PUBLIC connect revoked.
	if exists(t, admin, "SELECT has_database_privilege('public', $1, 'CONNECT')", req.DBName) {
		t.Error("PUBLIC still has CONNECT on the logical database")
	}

	// Template contents present — connect as the owning role and count rows.
	roleCfg := cfg
	roleCfg.User = req.RoleName
	roleCfg.Password = req.Password
	roleDB, err := sql.Open("pgx", buildDSN(roleCfg, req.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer roleDB.Close()
	var n int
	if err := roleDB.QueryRow(`SELECT count(*) FROM widgets`).Scan(&n); err != nil {
		t.Fatalf("owner cannot read cloned template: %v", err)
	}
	if n != 3 {
		t.Errorf("cloned rows = %d, want 3", n)
	}

	// Idempotent: a second provision of the same db name is a no-op success.
	if _, err := p.ProvisionLogical(ctx, req); err != nil {
		t.Errorf("second provision not idempotent: %v", err)
	}

	// DropLogical removes both (what the TTL reaper calls).
	if err := p.DropLogical(ctx, req.DBName, req.RoleName); err != nil {
		t.Fatalf("drop logical: %v", err)
	}
	if exists(t, admin, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", req.DBName) {
		t.Error("database not dropped")
	}
	if exists(t, admin, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", req.RoleName) {
		t.Error("role not dropped")
	}
}

func TestProvisionLogical_SeedNotAllowed(t *testing.T) {
	cfg := startPG(t)
	p, err := NewProvisioner(cfg)
	if err != nil {
		t.Fatalf("new provisioner: %v", err)
	}
	defer p.Close()

	_, err = p.ProvisionLogical(context.Background(), usecase.LogicalProvisionRequest{
		DBName:           "res_evil",
		RoleName:         "r_evil",
		Password:         "pw",
		SeedTemplate:     "tmpl_app",              // real template …
		AllowedTemplates: []string{"tmpl_other"}, // … but NOT allowlisted
	})
	if !errors.Is(err, ErrSeedTemplateNotAllowed) {
		t.Fatalf("got %v, want ErrSeedTemplateNotAllowed", err)
	}
}

func exists(t *testing.T, db *sql.DB, q string, arg string) bool {
	t.Helper()
	var b bool
	if err := db.QueryRow(q, arg).Scan(&b); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return b
}
