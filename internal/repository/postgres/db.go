package postgres

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config holds database configuration
type Config struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	LogLevel        logger.LogLevel
	// LogWriter is where the ORM writes. nil means os.Stdout, which is what the
	// service runs with; a test sets it to observe exactly what an operator
	// reading `docker logs` would see.
	LogWriter io.Writer
}

// gormSlowThreshold matches gorm's own Default logger. A statement slower than
// this is rendered at Warn — one of the three Trace paths that must never carry
// a bound value (D-396).
const gormSlowThreshold = 200 * time.Millisecond

// newGormLogger builds this service's ONLY GORM logger.
//
// ParameterizedQueries is the security-relevant flag: gorm applies it inside the
// `fc()` closure that renders the statement, so bound values become `$N` on ALL
// THREE Trace paths — every statement at Info, a slow statement at Warn, and a
// FAILED statement at Error. Without it, `node_executions` / `graph_trace_node_snapshots`
// INSERTs echo their full jsonb `input`/`output` — which is how a sealed tenant
// secret reached the runtime's container log (D-396). Statement shape, SQLSTATE,
// row count, duration and caller file:line all stay visible.
func newGormLogger(w io.Writer, level logger.LogLevel) logger.Interface {
	if w == nil {
		w = os.Stdout
	}
	return logger.New(log.New(w, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:             gormSlowThreshold,
		LogLevel:                  level,
		IgnoreRecordNotFoundError: false,
		ParameterizedQueries:      true,
		Colorful:                  true,
	})
}

// ParseLogLevel maps the configured `database.postgres.log_level` string onto a
// GORM level. It is fail-closed: an unrecognised value is an error, never a
// silent fallback, because the fallback would decide how much the ORM prints.
// No security-relevant behaviour may key on the environment name (D-396).
func ParseLogLevel(s string) (logger.LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "silent":
		return logger.Silent, nil
	case "error":
		return logger.Error, nil
	case "warn":
		return logger.Warn, nil
	case "info":
		return logger.Info, nil
	default:
		return 0, fmt.Errorf("unknown database log level %q: want one of silent, error, warn, info", s)
	}
}

// NewDB creates a new database connection with proper configuration
func NewDB(cfg Config) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode,
	)

	db, err := gorm.Open(newSanitizingDialector(dsn), &gorm.Config{
		Logger: newGormLogger(cfg.LogWriter, cfg.LogLevel),
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
