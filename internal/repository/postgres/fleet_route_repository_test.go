package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// A route host collision must be recognised from the DRIVER error (SQLSTATE +
// constraint name), never from message text (§30.5/§16.5) — and it must not
// swallow every other unique violation on the table.
func TestIsRouteHostConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "host_pattern unique violation",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: routeHostUniqueIndex},
			want: true,
		},
		{
			name: "wrapped host_pattern unique violation",
			err:  fmt.Errorf("create route: %w", &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: routeHostUniqueIndex}),
			want: true,
		},
		{
			name: "unique violation on another constraint",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "fleet_routes_pkey"},
			want: false,
		},
		{
			name: "different SQLSTATE on the same constraint",
			err:  &pgconn.PgError{Code: "23503", ConstraintName: routeHostUniqueIndex},
			want: false,
		},
		{
			// The text alone must NOT be enough — string-matching is what this replaces.
			name: "driver-shaped message with no pg error",
			err:  errors.New(`ERROR: duplicate key value violates unique constraint "fleet_routes_host_pattern_key"`),
			want: false,
		},
		{
			name: "unrelated gorm error",
			err:  gorm.ErrRecordNotFound,
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRouteHostConflict(tt.err); got != tt.want {
				t.Fatalf("isRouteHostConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
