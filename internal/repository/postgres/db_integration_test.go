//go:build integration

// External test package (postgres_test), matching this directory's other
// integration tests.
package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/migrations"
)

// pgCanary stands in for a resolved tenant secret. It must not reach the ORM's
// writer, the returned error, or any field of the Postgres error object (D-396).
const pgCanary = "::d396-pg-canary::"

// syncBuf is the ORM's log sink for these tests. gorm writes from whichever
// goroutine ran the statement, so the buffer is mutex-guarded (-race).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// startEchoPG boots a throwaway Postgres and opens it through the REAL NewDB at
// the level the service is configured to run at (`info`), with the ORM's log
// captured so a test can read exactly what an operator would see.
func startEchoPG(t *testing.T) (*gorm.DB, *syncBuf) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("runtime"),
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
	p, _ := strconv.Atoi(port.Port())

	sink := &syncBuf{}
	var db *gorm.DB
	for i := 0; i < 30; i++ {
		db, err = postgres.NewDB(postgres.Config{
			Host: host, Port: p, User: "postgres", Password: "postgres",
			Database: "runtime", SSLMode: "disable",
			LogLevel: "info", LogWriter: sink,
		})
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrap sql.DB: %v", err)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open migration source: %v", err)
	}
	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
	return db, sink
}

// TestNewDB_LogNeverCarriesBoundValues drives the two body-bearing tables
// through the real NewDB against a real Postgres at `info`.
//
// Control: set `ParameterizedQueries: false` in newGormLogger and the canary
// assertions go red — that is the shipped defect reproduced.
func TestNewDB_LogNeverCarriesBoundValues(t *testing.T) {
	db, sink := startEchoPG(t)
	ctx := context.Background()

	exec := &domain.NodeExecution{
		ID:               uuid.New(),
		GraphExecutionID: uuid.New(),
		GraphNodeID:      uuid.New(),
		NodeType:         domain.GraphNodeType("code"),
		NodeName:         "greet",
		SequenceNumber:   1,
		Status:           domain.GraphExecutionStatus("completed"),
		Input:            domain.JSONMap{"greeting_suffix": pgCanary},
		Output:           domain.JSONMap{"greeting": "hello x " + pgCanary},
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.WithContext(ctx).Create(exec).Error; err != nil {
		t.Fatalf("insert node_execution: %v", err)
	}

	snap := &domain.GraphTraceNodeSnapshot{
		ID:             uuid.New(),
		TraceID:        uuid.New(),
		GraphNodeID:    uuid.New(),
		NodeName:       "greet",
		NodeType:       "code",
		SequenceNumber: 1,
		Input:          domain.JSONMap{"greeting_suffix": pgCanary},
		Output:         domain.JSONMap{"greeting": "hello x " + pgCanary},
		Config:         domain.JSONMap{"suffix_ref": pgCanary},
		Status:         "completed",
		StartedAt:      time.Now().UTC(),
		CompletedAt:    time.Now().UTC(),
	}
	if err := db.WithContext(ctx).Create(snap).Error; err != nil {
		t.Fatalf("insert graph_trace_node_snapshot: %v", err)
	}

	out := sink.String()
	for _, table := range []string{
		jsonSQLFragment(`INSERT INTO "node_executions"`),
		jsonSQLFragment(`INSERT INTO "graph_trace_node_snapshots"`),
	} {
		if !strings.Contains(out, table) {
			t.Fatalf("precondition failed: %s never reached the ORM log, so an absence assertion proves nothing.\ngot:\n%s", table, out)
		}
	}
	if strings.Contains(out, pgCanary) {
		t.Fatalf("a bound value reached the ORM log:\n%s", out)
	}
}

// TestNewDB_ErrorsCarryNoRowData covers the OTHER channel: the error object gorm
// returns. Postgres puts the offending value in Message (22P02) and the entire
// failing row in Detail (23502), and that object survives %w into any log line.
//
// Control: delete `TranslateError: true` from NewDB and every canary assertion
// below goes red.
func TestNewDB_ErrorsCarryNoRowData(t *testing.T) {
	db, sink := startEchoPG(t)
	ctx := context.Background()

	t.Run("23502 not-null violation drops the failing row", func(t *testing.T) {
		// graph_execution_id is NOT NULL with no default; the canary rides `input`,
		// which Postgres would otherwise reproduce whole in DETAIL.
		err := db.WithContext(ctx).Exec(
			`INSERT INTO node_executions (id, graph_node_id, node_type, sequence_number, status, input, created_at)
			 VALUES (?, ?, 'code', 1, 'completed', ?, now())`,
			uuid.New(), uuid.New(), `{"greeting_suffix":"`+pgCanary+`"}`,
		).Error
		if err == nil {
			t.Fatal("control failed: the INSERT succeeded, so no error path was exercised")
		}
		assertSanitized(t, err, "23502")

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("not a PgError: %v", err)
		}
		if pgErr.TableName != "node_executions" || pgErr.ColumnName != "graph_execution_id" {
			t.Fatalf("diagnostics lost: table=%q column=%q; want node_executions/graph_execution_id", pgErr.TableName, pgErr.ColumnName)
		}
	})

	t.Run("22P02 invalid uuid drops the offending value", func(t *testing.T) {
		err := db.WithContext(ctx).Exec(
			`SELECT 1 FROM node_executions WHERE id = ?`, pgCanary,
		).Error
		if err == nil {
			t.Fatal("control failed: binding a non-uuid to a uuid column did not fail")
		}
		assertSanitized(t, err, "22P02")
	})

	if out := sink.String(); strings.Contains(out, pgCanary) {
		t.Fatalf("a failed statement echoed the value to the ORM log:\n%s", out)
	}
}

// assertSanitized is the whole INV-LOG-VALUES claim for the error channel: the
// SQLSTATE survives, the row data does not — in the rendered error and in every
// free-text field of the Postgres error object.
func assertSanitized(t *testing.T, err error, wantCode string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError (the repositories match on Code/ConstraintName), got %T: %v", err, err)
	}
	if pgErr.Code != wantCode {
		t.Fatalf("SQLSTATE: got %q, want %q — the diagnostic must survive sanitising", pgErr.Code, wantCode)
	}
	for _, f := range []struct{ name, value string }{
		{"err.Error()", err.Error()},
		{"PgError.Message", pgErr.Message},
		{"PgError.Detail", pgErr.Detail},
		{"PgError.Where", pgErr.Where},
		{"PgError.Hint", pgErr.Hint},
		{"PgError.InternalQuery", pgErr.InternalQuery},
	} {
		if strings.Contains(f.value, pgCanary) {
			t.Fatalf("%s carries row data: %q", f.name, f.value)
		}
	}
	if pgErr.Detail != "" || pgErr.Where != "" || pgErr.Hint != "" || pgErr.InternalQuery != "" {
		t.Fatalf("free-text fields must be dropped, not filtered: detail=%q where=%q hint=%q internal_query=%q",
			pgErr.Detail, pgErr.Where, pgErr.Hint, pgErr.InternalQuery)
	}
}

// randSentinel returns a value that cannot collide with anything else in the
// captured log, so "the sentinel is absent" is a claim about THIS statement's
// bound parameter and nothing else.
func randSentinel(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return "d396-" + hex.EncodeToString(b[:])
}

// TestGormLogger_BoundValuesNeverEchoThroughCallbacks proves the guard on the
// REAL path: gorm's processor renders every logged statement inside the closure
// at callbacks.go:141-146, and that closure only strips bound values if the
// configured logger satisfies the OPTIONAL gorm.ParamsFilter assertion. The
// ParameterizedQueries flag alone is not the control — the interface is.
//
// The control that turns this test red: wrap the value newGormLogger returns in
// `struct{ logger.Interface }`. Embedding an interface promotes only the
// methods that interface declares, so ParamsFilter vanishes from the concrete
// type, the assertion at callbacks.go:143 fails, stmt.Vars stay populated,
// Dialector.Explain inlines them, and the sentinel appears in the output below.
// NewDB refuses to boot on exactly that shape, so reproducing the control also
// requires bypassing that refusal.
func TestGormLogger_BoundValuesNeverEchoThroughCallbacks(t *testing.T) {
	db, sink := startEchoPG(t)
	ctx := context.Background()

	// --- the success path (Info) ---
	sentinel := randSentinel(t)
	var count int64
	if err := db.WithContext(ctx).
		Table("node_executions").
		Where("node_name = ?", sentinel).
		Count(&count).Error; err != nil {
		t.Fatalf("count node_executions by node_name: %v", err)
	}

	out := sink.String()
	// POSITIVE FIRST — a blank page must fail this test. An absence assertion on
	// empty output proves nothing at all.
	if !strings.Contains(out, "SQL executed") {
		t.Fatalf("positive control failed: the ORM logged no statement, so the absence assertion below would prove nothing.\ngot:\n%s", out)
	}
	if !strings.Contains(out, jsonSQLFragment(`"node_executions"`)) {
		t.Fatalf("positive control failed: the counted statement never reached the log.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "$1") {
		t.Fatalf("positive control failed: no placeholder in the rendered statement — the bound value was not parameterised out, it was never bound.\ngot:\n%s", out)
	}
	// ...and only now the claim itself.
	if strings.Contains(out, sentinel) {
		t.Fatalf("a bound value echoed into the ORM log (D-396):\n%s", out)
	}

	// --- the error path (Error): gorm renders the FULL statement for a FAILED
	// one and appends the driver error as its own attribute. Both channels have
	// to be clean. Binding a non-uuid to a uuid column is the cheapest forced
	// failure that still carries a bound value.
	errSentinel := randSentinel(t)
	err := db.WithContext(ctx).Exec(
		`SELECT 1 FROM node_executions WHERE id = ?`, errSentinel,
	).Error
	if err == nil {
		t.Fatal("control failed: binding a non-uuid to a uuid column did not fail, so the Error path never ran")
	}

	out = sink.String()
	// POSITIVE FIRST again.
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("positive control failed: the failed statement was not logged at Error, so nothing below is being checked.\ngot:\n%s", out)
	}
	if !strings.Contains(out, `"error":`) {
		t.Fatalf("positive control failed: the Error record carries no error attribute.\ngot:\n%s", out)
	}
	if strings.Contains(out, errSentinel) {
		t.Fatalf("a bound value echoed into the ORM log on the error path (D-396):\n%s", out)
	}
	if strings.Contains(err.Error(), errSentinel) {
		t.Fatalf("the returned error carries the bound value: %q", err.Error())
	}
}

// jsonSQLFragment renders a SQL fragment the way it appears inside the ORM's
// JSON log line — gorm's identifier quoting is escaped on the way out, so an
// assertion written against the raw form would silently stop checking anything.
func jsonSQLFragment(sqlFragment string) string {
	b, err := json.Marshal(sqlFragment)
	if err != nil {
		panic(err)
	}
	return strings.Trim(string(b), `"`)
}
