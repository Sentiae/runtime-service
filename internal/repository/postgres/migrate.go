package postgres

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/migrations"
)

// RunMigrations applies the embedded golang-migrate SQL migrations against the
// connected database (CLAUDE.md §24). Returns the schema version now current and
// whether anything was applied this run.
func RunMigrations(db *gorm.DB) (version uint, applied bool, err error) {
	sqlDB, err := db.DB()
	if err != nil {
		return 0, false, fmt.Errorf("migrate: unwrap sql.DB: %w", err)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, false, fmt.Errorf("migrate: open embedded source: %w", err)
	}
	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		return 0, false, fmt.Errorf("migrate: init postgres driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return 0, false, fmt.Errorf("migrate: init: %w", err)
	}
	// Don't m.Close(): that closes the shared *sql.DB the service keeps using.

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
