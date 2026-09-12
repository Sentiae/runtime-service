package postgres

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sentiae/platform-kit/gormlog"
	"gorm.io/gorm"
)

// Config holds database configuration
type Config struct {
	// DSN is the libpq keyword/value connection string. The caller builds it
	// from config.DatabaseDSN (the serving app role) or config.MigrateDatabaseDSN
	// (the owner role) — which role a pool authenticates as is decided there,
	// never here (D-490).
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// LogLevel is the configured `database.postgres.log_level` name, passed
	// through to gormlog.New unparsed: gormlog.ParseLevel is the single
	// authority over the four accepted names and is fail-closed, so an
	// unrecognised value refuses the connection rather than picking a default.
	LogLevel string
	// LogWriter is where the ORM writes. nil means os.Stdout, which is what the
	// service runs with; a test sets it to observe exactly what an operator
	// reading `docker logs` would see.
	LogWriter io.Writer
}

// NewDB creates a new database connection with proper configuration
func NewDB(cfg Config) (*gorm.DB, error) {
	// gormlog.New is the fleet's ONLY approved ORM logger (D-400). It sets
	// ParameterizedQueries — the security-relevant flag gorm applies inside the
	// `fc()` closure that renders the statement, so bound values become `$N` on
	// ALL THREE Trace paths: every statement at Info, a slow statement at Warn,
	// and a FAILED statement at Error. Without it, `node_executions` /
	// `graph_trace_node_snapshots` INSERTs echo their full jsonb `input`/`output`,
	// which is how a sealed tenant secret reached the runtime's container log
	// (D-396). It also overrides gorm's package-global RecorderParamsFilter,
	// closing the (*gorm.DB).Scan bypass this service's own logger could not
	// reach. Statement shape, SQLSTATE, row count, duration and caller file:line
	// all stay visible.
	gl, err := gormlog.New(cfg.LogWriter, cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("build gorm logger: %w", err)
	}
	// ParameterizedQueries only takes effect through gorm.ParamsFilter, and that
	// assertion (callbacks.go:143) is OPTIONAL: a logger failing it leaves
	// stmt.Vars populated and Explain inlines every bound value. Refuse to boot
	// rather than run a logger that would echo tenant data into the log (D-396).
	if _, ok := gl.(gorm.ParamsFilter); !ok {
		return nil, fmt.Errorf("gorm logger %T does not implement ParamsFilter; bound values would echo into logs (D-396)", gl)
	}

	db, err := gorm.Open(newSanitizingDialector(cfg.DSN), &gorm.Config{
		Logger: gl,
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
		PrepareStmt:                              true,
		DisableForeignKeyConstraintWhenMigrating: false,
		// TranslateError routes every driver error through the dialector's
		// Translate, which is this package's sanitizer: a Postgres error carries
		// the offending value in Message and the WHOLE failing row in Detail, and
		// that object survives %w into any log line (D-396).
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	return db, nil
}

// Close closes the database connection
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// HealthCheck checks if the database connection is alive
func HealthCheck(ctx context.Context, db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}
