package usecase

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	// PostgreSQL driver is available via pgx (a direct dependency, also used by
	// gorm). Import the stdlib adapter so database/sql can use the "pgx" driver
	// name. Mirrors canvas-service's database node executor.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sentiae/runtime-service/internal/domain"
)

// dbDriverName maps the user-facing driver name to a database/sql driver.
// v1 supports PostgreSQL only; MySQL/SQLite are future additions that need
// their drivers imported at build time.
func dbDriverName(driver string) (string, error) {
	switch strings.ToLower(driver) {
	case "", "postgres", "postgresql", "pgx":
		return "pgx", nil
	default:
		return "", fmt.Errorf("unsupported database driver %q (v1 supports postgres only)", driver)
	}
}

// dbStatementIsQuery reports whether a SQL statement returns rows (and so must
// run via QueryContext rather than ExecContext). Detection is by leading
// keyword (case-insensitive) plus a RETURNING clause check, so an
// INSERT/UPDATE/DELETE ... RETURNING is correctly treated as a query.
func dbStatementIsQuery(query string) bool {
	trimmed := strings.TrimSpace(query)
	// Strip a leading line comment if present so the keyword check is robust.
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") {
		return true
	}
	// INSERT/UPDATE/DELETE ... RETURNING yields rows. Match on a word-bounded
	// RETURNING to avoid false positives inside identifiers/strings.
	for _, tok := range strings.Fields(upper) {
		if strings.Trim(tok, "(),;") == "RETURNING" {
			return true
		}
	}
	return false
}

// dbBindArgs resolves positional query arguments from the node config and the
// node input. config["params"] is an optional ordered array of input field
// names; each name is looked up in input and bound positionally to $1..$n.
// A missing key binds nil (preserving positional ordering).
func dbBindArgs(config domain.JSONMap, input domain.JSONMap) ([]any, error) {
	raw, ok := config["params"]
	if !ok {
		return nil, nil
	}
	names, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("params must be an array of input field names")
	}
	args := make([]any, 0, len(names))
	for i, n := range names {
		name, ok := n.(string)
		if !ok {
			return nil, fmt.Errorf("params[%d] must be a string field name", i)
		}
		args = append(args, input[name])
	}
	return args, nil
}

// executeDatabaseNode runs a parameterized SQL statement against an external
// database, in-process (runtime-service has network + DB access, like the
// http/transform nodes).
//
// Config (raw-SQL model):
//
//	{
//	  "connection_string": "postgres://user:pass@host:5432/db",  // required
//	  "query": "INSERT INTO links (slug, url) VALUES ($1, $2)",  // required, parameterized
//	  "params": ["slug", "url"],   // optional: input field names, bound positionally to $1..$n
//	  "driver": "postgres"          // optional, default postgres -> pgx
//	}
//
// Output for row-returning statements (SELECT / WITH / ... RETURNING):
//
//	{"rows": [ {col: val, ...}, ... ], "row_count": N}
//
// Output for mutating statements (INSERT / UPDATE / DELETE):
//
//	{"rows_affected": N}
//
// Only parameterized queries are supported — values flow through $1..$n bind
// args, never string concatenation.
func (e *GraphExecutionEngine) executeDatabaseNode(
	ctx context.Context,
	node *domain.GraphNode,
	input domain.JSONMap,
) (domain.JSONMap, error) {
	connStr, _ := node.Config["connection_string"].(string)
	if connStr == "" {
		return nil, fmt.Errorf("database node %q: connection_string is required", node.Name)
	}

	query, _ := node.Config["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("database node %q: query is required", node.Name)
	}

	driverStr, _ := node.Config["driver"].(string)
	driverName, err := dbDriverName(driverStr)
	if err != nil {
		return nil, fmt.Errorf("database node %q: %w", node.Name, err)
	}

	args, err := dbBindArgs(node.Config, input)
	if err != nil {
		return nil, fmt.Errorf("database node %q: %w", node.Name, err)
	}

	// Per-call open is sufficient for v1; a pooled/shared connection keyed by
	// connection string is a later optimization.
	db, err := sql.Open(driverName, connStr)
	if err != nil {
		return nil, fmt.Errorf("database node %q: open database: %w", node.Name, err)
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("database node %q: ping database: %w", node.Name, err)
	}

	if dbStatementIsQuery(query) {
		return dbRunQuery(ctx, db, node.Name, query, args)
	}
	return dbRunExec(ctx, db, node.Name, query, args)
}

// dbRunQuery executes a row-returning statement and collects rows into a
// JSON-friendly shape: {"rows": [...], "row_count": N}.
func dbRunQuery(ctx context.Context, db *sql.DB, nodeName, query string, args []any) (domain.JSONMap, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("database node %q: query failed: %w", nodeName, err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("database node %q: read columns: %w", nodeName, err)
	}

	results := make([]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("database node %q: scan row: %w", nodeName, err)
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			// Convert []byte to string for JSON compatibility.
			if b, ok := values[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = values[i]
			}
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database node %q: row iteration: %w", nodeName, err)
	}

	return domain.JSONMap{
		"rows":      results,
		"row_count": len(results),
	}, nil
}

// dbRunExec executes a mutating statement and returns {"rows_affected": N}.
func dbRunExec(ctx context.Context, db *sql.DB, nodeName, query string, args []any) (domain.JSONMap, error) {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("database node %q: exec failed: %w", nodeName, err)
	}
	affected, _ := res.RowsAffected()
	return domain.JSONMap{
		"rows_affected": affected,
	}, nil
}
