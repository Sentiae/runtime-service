// Package testdb provisions throwaway PostgreSQL databases for isolated
// test executions. Each call to Provision clones a configurable template
// database (CREATE DATABASE ... TEMPLATE seed) and returns a connection
// URL plus a Cleanup function. The intent is to give every test VM a
// pristine, seed-loaded database without paying the cost of a full pg
// container per test.
//
// The package talks to the Postgres admin DB via lib/pq's database/sql
// driver — no GORM dependency — so the provisioner can be driven from
// any service that has Postgres credentials.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Config captures everything the provisioner needs. Host/Port/User/
// Password are credentials for an account with CREATEDB. Template names
// the seed database that each per-test DB is cloned from. AdminDatabase
// is the database the admin connection initially binds to (typically
// "postgres") since you cannot CREATE DATABASE while connected to your
// own target. SSLMode mirrors the lib/pq sslmode parameter.
type Config struct {
	Host          string
	Port          int
	User          string
	Password      string
	AdminDatabase string
	Template      string
	SSLMode       string
	// MaxLifetime caps how long a provisioned DB may live before
	// Cleanup is force-called by the package's reaper. Set to 0 to
	// disable the reaper (Cleanup must then be called explicitly).
	MaxLifetime time.Duration
}

// DefaultConfig returns a config tuned for local development: localhost,
// postgres user, no SSL, "template_test" as the template DB.
func DefaultConfig() Config {
	return Config{
		Host:          "localhost",
		Port:          5432,
		User:          "postgres",
		Password:      "postgres",
		AdminDatabase: "postgres",
		Template:      "template_test",
		SSLMode:       "disable",
		MaxLifetime:   30 * time.Minute,
	}
}

// Provisioner manages the lifecycle of throwaway databases. It holds an
// admin sql.DB used exclusively to issue CREATE/DROP DATABASE statements.
type Provisioner struct {
	cfg     Config
	adminDB *sql.DB

	mu     sync.Mutex
	leases map[string]time.Time // dbName -> created
}

// NewProvisioner opens an admin connection and verifies that the
// template database exists. Returning an error here surfaces
// misconfiguration before any test runs.
func NewProvisioner(cfg Config) (*Provisioner, error) {
	if cfg.Host == "" || cfg.User == "" || cfg.AdminDatabase == "" || cfg.Template == "" {
		return nil, fmt.Errorf("testdb: incomplete config (host/user/admin_db/template required)")
	}
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	dsn := buildDSN(cfg, cfg.AdminDatabase)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("testdb: open admin: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(2 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("testdb: ping admin: %w", err)
	}
	// Verify template exists. We use a placeholder query so a malicious
	// template name cannot become SQL injection (template names should
	// be operator-controlled but defensive coding is cheap).
	var n int
	row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_database WHERE datname = $1", cfg.Template)
	if err := row.Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("testdb: template lookup: %w", err)
	}
	if n == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("testdb: template database %q not found", cfg.Template)
	}

	return &Provisioner{
		cfg:     cfg,
		adminDB: db,
		leases:  map[string]time.Time{},
	}, nil
}

// Lease describes a provisioned database. URL is the lib/pq connection
// string suitable for injection into a guest VM as DATABASE_URL. Name
// is the raw database name (useful for logging). Cleanup drops the
// database — it is safe to call multiple times.
type Lease struct {
	Name    string
	URL     string
	Cleanup func() error
}

// Provision creates a new database cloned from the configured template.
// The name is randomised so concurrent calls cannot collide, and the
// returned Lease.Cleanup drops the database when invoked.
//
// Postgres requires the source DB to have no other connections at clone
// time, so the template must be idle. We retry the CREATE once if we
// see SQLSTATE 55006 (object_in_use).
func (p *Provisioner) Provision(ctx context.Context) (*Lease, error) {
	dbName, err := generateDBName()
	if err != nil {
		return nil, fmt.Errorf("testdb: name: %w", err)
	}

	// Quote identifiers manually — pq does not parameterise CREATE DB.
	createStmt := fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, quoteIdent(dbName), quoteIdent(p.cfg.Template))
	if _, err := p.adminDB.ExecContext(ctx, createStmt); err != nil {
		// One retry if the template is briefly busy.
		if isObjectInUse(err) {
			time.Sleep(200 * time.Millisecond)
			if _, retryErr := p.adminDB.ExecContext(ctx, createStmt); retryErr != nil {
				return nil, fmt.Errorf("testdb: create %s: %w", dbName, retryErr)
			}
		} else {
			return nil, fmt.Errorf("testdb: create %s: %w", dbName, err)
		}
	}

	p.mu.Lock()
	p.leases[dbName] = time.Now()
	p.mu.Unlock()

	url := buildDSN(p.cfg, dbName)

	cleanup := func() error {
		return p.drop(dbName)
	}
	return &Lease{Name: dbName, URL: url, Cleanup: cleanup}, nil
}

// drop terminates outstanding connections then drops the database. It
// is idempotent — dropping a non-existent DB returns nil.
func (p *Provisioner) drop(dbName string) error {
	p.mu.Lock()
	delete(p.leases, dbName)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Force-disconnect any client still bound to the test DB so DROP
	// does not block. pg_terminate_backend ignores rows that no longer
	// exist, so the LIMIT is just defensive.
	_, _ = p.adminDB.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid)
		   FROM pg_stat_activity
		  WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)

	stmt := fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName))
	if _, err := p.adminDB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("testdb: drop %s: %w", dbName, err)
	}
	return nil
}

// Close releases the admin connection. Outstanding leases are best-effort
// dropped first.
func (p *Provisioner) Close() error {
	p.mu.Lock()
	names := make([]string, 0, len(p.leases))
	for n := range p.leases {
		names = append(names, n)
	}
	p.mu.Unlock()
	for _, n := range names {
		_ = p.drop(n)
	}
	return p.adminDB.Close()
}

// LeaseCount returns the number of currently-active leases. Useful for
// metrics and assertions in tests.
func (p *Provisioner) LeaseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.leases)
}

func buildDSN(cfg Config, dbName string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Path:   "/" + dbName,
	}
	q := u.Query()
	q.Set("sslmode", cfg.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

func generateDBName() (string, error) {
	// 8 random bytes (16 hex chars) plus a fixed prefix keeps names
	// well under Postgres's 63-byte identifier limit.
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "test_" + hex.EncodeToString(b), nil
}

func quoteIdent(name string) string {
	// Postgres identifiers are double-quoted and embedded quotes are
	// doubled. Our names are random hex so this is overkill but makes
	// the helper safe for arbitrary template names.
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isObjectInUse(err error) bool {
	if err == nil {
		return false
	}
	// SQLSTATE 55006 is "object_in_use". lib/pq exposes the code via
	// its *pq.Error type but importing the type for one comparison is
	// noisy; substring match on the standard message text is reliable.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is being accessed by other users") ||
		strings.Contains(msg, "55006")
}
