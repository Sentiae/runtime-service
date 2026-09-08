package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// sanitizedPgMessage replaces every Postgres-authored error message. Postgres
// puts the offending VALUE in the message (22P02 renders
// `invalid input syntax for type uuid: "<value>"`), and *pgconn.PgError.Error()
// renders exactly Severity + Message + Code — so the value survives %w into any
// of this service's log lines. The SQLSTATE, table, column and constraint that
// actually identify the fault are preserved on the returned error.
const sanitizedPgMessage = "postgres error (message and detail suppressed; see code/table/column/constraint)"

// sanitizingDialector is the postgres dialector with a Translate that strips
// row data out of driver errors (D-396).
//
// It embeds the concrete postgres.Dialector by value — every one of its methods
// has a value receiver, so Name/Initialize/Migrator/DataTypeOf/BindVarTo/
// QuoteTo/Explain/Apply AND the optional SavePoint/RollbackTo pair are all
// promoted unchanged. Only Translate is ours.
type sanitizingDialector struct {
	postgres.Dialector
}

var _ gorm.ErrorTranslator = sanitizingDialector{}

func newSanitizingDialector(dsn string) gorm.Dialector {
	return sanitizingDialector{postgres.Dialector{Config: &postgres.Config{DSN: dsn}}}
}

// Translate sanitizes a Postgres error instead of mapping it to a gorm sentinel.
//
// It deliberately does NOT delegate to the embedded dialector's Translate: that
// one converts 23505/23503/42703/23514 into gorm.ErrDuplicatedKey and friends,
// and this service's repositories match on *pgconn.PgError + Code +
// ConstraintName (fleet_resource_repo.go, fleet_route_repository.go,
// fleet_net_lease_repo.go, image_workload_repository.go). Collapsing those to a
// sentinel would lose the constraint name they discriminate on.
//
// What is kept: everything that names the FAULT — Severity, Code, SchemaName,
// TableName, ColumnName, DataTypeName, ConstraintName, and Postgres' own source
// File/Line/Routine. What is dropped, by construction (the result is built from
// an explicit allow-list, so any field a future pgx adds is dropped too):
// Message, Detail, Hint, Where, InternalQuery, Position, InternalPosition.
// Detail is the dangerous one — on 23502/23514 Postgres sets it to
// `Failing row contains (…entire row…)`, which for node_executions is the whole
// jsonb input/output.
//
// errors.As rather than a type assertion: a wrapped PgError must be sanitized
// too, and returning the sanitized inner error is the fail-closed answer (an
// outer wrapper's own text would already have rendered the raw message).
func (sanitizingDialector) Translate(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	return &pgconn.PgError{
		Severity:            pgErr.Severity,
		SeverityUnlocalized: pgErr.SeverityUnlocalized,
		Code:                pgErr.Code,
		Message:             sanitizedPgMessage,
		SchemaName:          pgErr.SchemaName,
		TableName:           pgErr.TableName,
		ColumnName:          pgErr.ColumnName,
		DataTypeName:        pgErr.DataTypeName,
		ConstraintName:      pgErr.ConstraintName,
		File:                pgErr.File,
		Line:                pgErr.Line,
		Routine:             pgErr.Routine,
	}
}
