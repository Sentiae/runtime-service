package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/migrations"
)

// ErrSchemaNotCurrent refuses a boot that does not migrate (migrations
// disabled) against a schema other than the exact, clean version this binary
// embeds. Such a process can neither fix the schema nor safely run on it.
var ErrSchemaNotCurrent = errors.New("database schema is not at the version this binary expects")

// RunMigrations applies the embedded golang-migrate SQL migrations against the
// connected database (CLAUDE.md §24) and then CLOSES db. Returns the schema
// version now current and whether anything was applied this run.
//
// db must be the OWNER connection (D-490): it exists only to migrate, so this
// closes it on every return path — including the one connection golang-migrate
// pins for its advisory lock, which sql.DB.Close alone leaves open until the
// process exits (an active *sql.Conn is only closed when it is released). The
// serving pool is a different connection, opened afterwards as the app role.
func RunMigrations(db *gorm.DB) (version uint, applied bool, err error) {
	sqlDB, err := db.DB()
	if err != nil {
		return 0, false, fmt.Errorf("migrate: unwrap sql.DB: %w", err)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		_ = sqlDB.Close() // reason: already failing; the open error is the one to report
		return 0, false, fmt.Errorf("migrate: open embedded source: %w", err)
	}
	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		_ = src.Close()   // reason: already failing; the driver error is the one to report
		_ = sqlDB.Close() // reason: as above
		return 0, false, fmt.Errorf("migrate: init postgres driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		_ = src.Close()    // reason: already failing; the init error is the one to report
		_ = driver.Close() // reason: as above — closes the pinned conn and sqlDB
		return 0, false, fmt.Errorf("migrate: init: %w", err)
	}
	// m.Close closes the source and the driver; the driver closes its pinned
	// connection and sqlDB. A close failure on an otherwise-successful run is
	// still returned: nothing else proves the owner session is gone.
	defer func() {
		srcErr, dbErr := m.Close()
		if err == nil && (srcErr != nil || dbErr != nil) {
			err = fmt.Errorf("migrate: close owner connection: %w", errors.Join(srcErr, dbErr))
		}
	}()

	applied = true
	if err := m.Up(); err != nil {
		if !errors.Is(err, migrate.ErrNoChange) {
			return 0, false, fmt.Errorf("migrate: up: %w", err)
		}
		applied = false
	}
	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return 0, applied, fmt.Errorf("migrate: read version: %w", err)
	}
	if dirty {
		return version, applied, fmt.Errorf("migrate: schema version %d is dirty — manual repair required", version)
	}
	return version, applied, nil
}

// LatestMigrationVersion is the newest version in the embedded migration set —
// the schema this binary was built against.
func LatestMigrationVersion() (uint, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, fmt.Errorf("open embedded migration source: %w", err)
	}
	defer func() { _ = src.Close() }() // reason: embed.FS has nothing to release

	latest, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("read first embedded migration: %w", err)
	}
	for {
		next, err := src.Next(latest)
		if errors.Is(err, fs.ErrNotExist) {
			return latest, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read embedded migration after %d: %w", latest, err)
		}
		latest = next
	}
}

// AssertSchemaCurrent is the boot check of a process that does NOT migrate. It
// only reads: one SELECT of schema_migrations inside a READ ONLY transaction,
// so it cannot create the version table, take golang-migrate's lock or change
// anything. It returns the verified version, or ErrSchemaNotCurrent naming the
// database's version and the one this binary expects.
func AssertSchemaCurrent(ctx context.Context, db *gorm.DB) (uint, error) {
	want, err := LatestMigrationVersion()
	if err != nil {
		return 0, err
	}

	var rows []struct {
		Version int64
		Dirty   bool
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Raw("SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&rows).Error
	}, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations (this binary expects version %d): %w", want, err)
	}

	if len(rows) == 0 || rows[0].Version < 0 {
		return 0, fmt.Errorf("%w: schema_migrations records no version; this binary expects version %d", ErrSchemaNotCurrent, want)
	}
	got := uint(rows[0].Version)
	if rows[0].Dirty {
		return got, fmt.Errorf("%w: database schema version %d is dirty; this binary expects version %d, clean", ErrSchemaNotCurrent, got, want)
	}
	if got != want {
		return got, fmt.Errorf("%w: database schema is at version %d; this binary expects version %d", ErrSchemaNotCurrent, got, want)
	}
	return got, nil
}
