//go:build integration

package di

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/migrations"
	"github.com/sentiae/runtime-service/pkg/config"
)

// The two login roles D-490 splits runtime onto, created here the way tier 0B
// creates them: neither is a superuser, only the owner may CREATE in public,
// and the app role gets DML on whatever the owner creates — nothing more.
const (
	ownerRole = "rt_owner"
	ownerPW   = "owner-pw"
	appRole   = "rt_app"
	appPW     = "app-pw"
)

// rolePG is a throwaway Postgres cluster plus a superuser handle used ONLY to
// arrange and inspect; the code under test never sees the superuser.
type rolePG struct {
	host, port string
	super      *sql.DB
}

func startRolePG(t *testing.T) *rolePG {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(90*time.Second)),
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
	pg := &rolePG{host: host, port: port.Port()}

	var super *sql.DB
	for i := 0; i < 30; i++ {
		super, err = sql.Open("pgx", pg.dsn("postgres", "postgres", "postgres"))
		if err == nil {
			if err = super.PingContext(ctx); err == nil {
				break
			}
			_ = super.Close() // reason: retrying the open
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("open superuser connection: %v", err)
	}
	t.Cleanup(func() { _ = super.Close() })
	pg.super = super

	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`, ownerRole, ownerPW),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`, appRole, appPW),
	} {
		pg.exec(t, "postgres", stmt)
	}
	return pg
}

func (pg *rolePG) dsn(user, password, db string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		pg.host, pg.port, user, password, db)
}

// newDatabase creates an empty database with tier 0B's grant shape.
func (pg *rolePG) newDatabase(t *testing.T, name string) {
	t.Helper()
	pg.exec(t, "postgres", fmt.Sprintf(`CREATE DATABASE %s`, name))
	for _, stmt := range []string{
		`REVOKE ALL ON SCHEMA public FROM PUBLIC`,
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`, ownerRole),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, appRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s`, ownerRole, appRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s`, ownerRole, appRole),
	} {
		pg.exec(t, name, stmt)
	}
}

// exec runs one statement as the superuser in the named database.
func (pg *rolePG) exec(t *testing.T, db, stmt string) {
	t.Helper()
	conn := pg.open(t, "postgres", "postgres", db)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(stmt); err != nil {
		t.Fatalf("exec %q in %s: %v", stmt, db, err)
	}
}

func (pg *rolePG) open(t *testing.T, user, password, db string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", pg.dsn(user, password, db))
	if err != nil {
		t.Fatalf("open %s@%s: %v", user, db, err)
	}
	return conn
}

// migrateOwnerTo arranges a database at exactly `version` by migrating it as
// the OWNER role, the way the control plane would have left it.
func (pg *rolePG) migrateOwnerTo(t *testing.T, db string, version uint) {
	t.Helper()
	conn := pg.open(t, ownerRole, ownerPW, db)
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open migration source: %v", err)
	}
	driver, err := migratepg.WithInstance(conn, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(version); err != nil {
		t.Fatalf("arrange: migrate %s to %d: %v", db, version, err)
	}
}

// schemaVersion reads schema_migrations as the superuser. ok=false ⇔ the table
// does not exist or holds no row.
func (pg *rolePG) schemaVersion(t *testing.T, db string) (version int64, dirty, ok bool) {
	t.Helper()
	conn := pg.open(t, "postgres", "postgres", db)
	defer func() { _ = conn.Close() }()
	var exists bool
	if err := conn.QueryRow(`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("probe schema_migrations: %v", err)
	}
	if !exists {
		return 0, false, false
	}
	err := conn.QueryRow(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false
	}
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return version, dirty, true
}

func (pg *rolePG) tableOwner(t *testing.T, db, table string) string {
	t.Helper()
	conn := pg.open(t, "postgres", "postgres", db)
	defer func() { _ = conn.Close() }()
	var owner string
	err := conn.QueryRow(`SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, table).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read owner of %s: %v", table, err)
	}
	return owner
}

// waitNoSessions polls until no backend in db is logged in as role. A closed
// client connection's backend exits asynchronously, so a single read can race it.
func (pg *rolePG) waitNoSessions(t *testing.T, db, role string) {
	t.Helper()
	var n int
	for i := 0; i < 50; i++ {
		if err := pg.super.QueryRow(
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, db, role).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%d session(s) as %s still open in %s — the owner connection was not closed", n, role, db)
}

// roleConfig is a runtime config holding BOTH credential pairs, so a code path
// that ignores the migrations flag COULD migrate — a refusal that came from a
// missing credential would prove nothing.
func (pg *rolePG) roleConfig(db string, migrationsEnabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.Database.Postgres = config.PostgresConfig{
		Host:            pg.host,
		Port:            pg.port,
		User:            appRole,
		Password:        appPW,
		MigrateUser:     ownerRole,
		MigratePassword: ownerPW,
		Database:        db,
		SSLMode:         "disable",
		LogLevel:        "warn",
		Pool:            config.PoolConfig{MaxOpenConns: 4, MaxIdleConns: 2},
		Migrations:      config.MigrationsConfig{Enabled: migrationsEnabled},
	}
	return cfg
}

func currentUser(t *testing.T, c *Container) string {
	t.Helper()
	var u string
	if err := c.DB.Raw(`SELECT current_user`).Scan(&u).Error; err != nil {
		t.Fatalf("read current_user on the serving pool: %v", err)
	}
	return u
}

func latestVersion(t *testing.T) uint {
	t.Helper()
	v, err := postgres.LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	return v
}

// TestInitDatabase_MigratesAsOwnerThenServesAsApp drives the boot's database
// step with migrations enabled against a cluster holding only the two
// least-privilege roles.
//
// Control (DSN split): build MigrateDatabaseDSN from the app credentials ⇒ the
// app role has no CREATE on public, the migration fails, boot errors ⇒ red.
// Control (close): drop m.Close from RunMigrations ⇒ golang-migrate's pinned
// owner connection outlives the boot ⇒ waitNoSessions fails.
// Control (serving role): open the serving pool with MigrateDatabaseDSN ⇒
// current_user is rt_owner ⇒ red.
func TestInitDatabase_MigratesAsOwnerThenServesAsApp(t *testing.T) {
	pg := startRolePG(t)
	const db = "runtime_boot"
	pg.newDatabase(t, db)

	c := &Container{}
	if err := c.initDatabase(pg.roleConfig(db, true)); err != nil {
		t.Fatalf("initDatabase (migrations enabled): %v", err)
	}
	t.Cleanup(func() { _ = postgres.Close(c.DB) })

	want := latestVersion(t)
	if v, dirty, ok := pg.schemaVersion(t, db); !ok || uint(v) != want || dirty {
		t.Fatalf("schema_migrations = (%d, dirty=%t, present=%t), want (%d, clean)", v, dirty, ok, want)
	}
	for _, table := range []string{"schema_migrations", "entity_snapshots", "fleet_hosts"} {
		if got := pg.tableOwner(t, db, table); got != ownerRole {
			t.Errorf("owner of %s = %q, want %q (migrations must run as the owner role)", table, got, ownerRole)
		}
	}
	if got := currentUser(t, c); got != appRole {
		t.Errorf("serving pool current_user = %q, want %q", got, appRole)
	}
	pg.waitNoSessions(t, db, ownerRole)

	// The serving role can use the tables the owner created, and nothing more.
	if err := c.DB.Exec(`SELECT count(*) FROM entity_snapshots`).Error; err != nil {
		t.Errorf("serving role cannot read entity_snapshots: %v", err)
	}
	if err := c.DB.Exec(`CREATE TABLE app_role_ddl_probe (id int)`).Error; err == nil {
		t.Errorf("serving role created a table; it must hold no CREATE on public")
	}
}

// TestInitDatabase_MigrationsDisabled_RefusesAnySchemaButTheCurrentOne drives
// the boot's database step with migrations DISABLED (the fleet host). It must
// never migrate: every non-current schema refuses the boot and is left exactly
// as it was found.
//
// Control (flag ignored): run MigrateDatabase regardless of Migrations.Enabled
// ⇒ the stale row boots clean and its version moves to the newest ⇒ red (the
// config carries valid owner credentials precisely so this control CAN fire).
// Control (check dropped): skip AssertSchemaCurrent ⇒ stale and dirty rows boot ⇒ red.
func TestInitDatabase_MigrationsDisabled_RefusesAnySchemaButTheCurrentOne(t *testing.T) {
	pg := startRolePG(t)
	latest := latestVersion(t)
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open migration source: %v", err)
	}
	previous, err := src.Prev(latest)
	if err != nil {
		t.Fatalf("version before %d: %v", latest, err)
	}
	_ = src.Close() // reason: embed.FS has nothing to release

	tests := []struct {
		name        string
		db          string
		arrange     func(t *testing.T, db string)
		wantRefusal bool
		wantMsg     []string
		wantVersion int64 // after the boot attempt; -1 ⇔ no schema_migrations row
		wantDirty   bool
	}{
		{
			name:        "never migrated",
			db:          "fc_empty",
			arrange:     func(t *testing.T, db string) {},
			wantRefusal: true,
			wantMsg:     []string{"schema_migrations", fmt.Sprintf("expects version %d", latest)},
			wantVersion: -1,
		},
		{
			name:        "one migration behind",
			db:          "fc_stale",
			arrange:     func(t *testing.T, db string) { pg.migrateOwnerTo(t, db, previous) },
			wantRefusal: true,
			wantMsg: []string{
				fmt.Sprintf("database schema is at version %d", previous),
				fmt.Sprintf("this binary expects version %d", latest),
			},
			wantVersion: int64(previous),
		},
		{
			name: "dirty at the newest version",
			db:   "fc_dirty",
			arrange: func(t *testing.T, db string) {
				pg.migrateOwnerTo(t, db, latest)
				pg.exec(t, db, `UPDATE schema_migrations SET dirty = true`)
			},
			wantRefusal: true,
			wantMsg: []string{
				fmt.Sprintf("database schema version %d is dirty", latest),
				fmt.Sprintf("this binary expects version %d, clean", latest),
			},
			wantVersion: int64(latest),
			wantDirty:   true,
		},
		{
			name:        "current and clean",
			db:          "fc_current",
			arrange:     func(t *testing.T, db string) { pg.migrateOwnerTo(t, db, latest) },
			wantVersion: int64(latest),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg.newDatabase(t, tt.db)
			tt.arrange(t, tt.db)

			c := &Container{}
			err := c.initDatabase(pg.roleConfig(tt.db, false))
			if tt.wantRefusal {
				if err == nil {
					_ = postgres.Close(c.DB) // reason: the test already failed
					t.Fatalf("initDatabase booted a %s schema with migrations disabled", tt.name)
				}
				if tt.wantVersion >= 0 && !errors.Is(err, postgres.ErrSchemaNotCurrent) {
					t.Errorf("error = %v, want ErrSchemaNotCurrent", err)
				}
				for _, m := range tt.wantMsg {
					if !strings.Contains(err.Error(), m) {
						t.Errorf("error %q does not contain %q", err.Error(), m)
					}
				}
				if c.DB != nil {
					t.Errorf("a refused boot left a serving pool on the container")
				}
			} else {
				if err != nil {
					t.Fatalf("initDatabase (current schema, migrations disabled): %v", err)
				}
				t.Cleanup(func() { _ = postgres.Close(c.DB) })
				if got := currentUser(t, c); got != appRole {
					t.Errorf("serving pool current_user = %q, want %q", got, appRole)
				}
			}

			// Nothing was migrated: the schema is exactly as arranged.
			v, dirty, ok := pg.schemaVersion(t, tt.db)
			if tt.wantVersion < 0 {
				if ok {
					t.Errorf("schema_migrations now holds version %d; a non-migrating boot created or wrote it", v)
				}
			} else if !ok || v != tt.wantVersion || dirty != tt.wantDirty {
				t.Errorf("schema_migrations = (%d, dirty=%t, present=%t) after boot, want (%d, dirty=%t) — the boot changed the schema",
					v, dirty, ok, tt.wantVersion, tt.wantDirty)
			}
			pg.waitNoSessions(t, tt.db, ownerRole)
		})
	}
}

// TestMigrateDatabase_MigratesTheConfiguredDatabaseAsOwner is the migrate-only
// mode (`runtime-service migrate`): pointed at a second database — the fleet
// ledger — with the boot flag off, it migrates that database as the owner role,
// leaves no owner session behind and returns.
//
// Control: gate MigrateDatabase on Migrations.Enabled ⇒ nothing is applied ⇒ red.
func TestMigrateDatabase_MigratesTheConfiguredDatabaseAsOwner(t *testing.T) {
	pg := startRolePG(t)
	const db = "runtime_service_fc"
	pg.newDatabase(t, db)

	version, applied, err := MigrateDatabase(pg.roleConfig(db, false))
	if err != nil {
		t.Fatalf("MigrateDatabase: %v", err)
	}
	want := latestVersion(t)
	if version != want || !applied {
		t.Fatalf("MigrateDatabase = (version %d, applied %t), want (%d, true)", version, applied, want)
	}
	if got := pg.tableOwner(t, db, "entity_snapshots"); got != ownerRole {
		t.Errorf("owner of entity_snapshots = %q, want %q", got, ownerRole)
	}
	pg.waitNoSessions(t, db, ownerRole)

	// Idempotent: a second run applies nothing and still closes.
	if version, applied, err = MigrateDatabase(pg.roleConfig(db, false)); err != nil || version != want || applied {
		t.Fatalf("second MigrateDatabase = (%d, %t, %v), want (%d, false, nil)", version, applied, err, want)
	}
	pg.waitNoSessions(t, db, ownerRole)
}
