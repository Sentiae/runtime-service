package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/sentiae/runtime-service/internal/domain"
)

// logCanary is the value a tenant secret plays in these tests. It must never
// reach the writer the ORM logs to, at any level, on any of gorm's three Trace
// paths (D-396).
const logCanary = "::d396-canary::"

// dryRunDB builds the SAME gorm stack NewDB builds — this package's logger and
// this package's dialector — but in DryRun against an address nothing listens
// on, so statements are rendered by gorm's real pipeline without a server.
func dryRunDB(t *testing.T, w *bytes.Buffer, level logger.LogLevel) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		newSanitizingDialector("host=127.0.0.1 port=1 user=u password=p dbname=d sslmode=disable"),
		&gorm.Config{
			Logger: newGormLogger(w, level),
			DryRun: true,
			// gorm's default create/update transaction would dial a server that is
			// not there; DryRun only skips the statement itself.
			SkipDefaultTransaction: true,
			DisableAutomaticPing:   true,
			PrepareStmt:            true,
			TranslateError:         true,
			NowFunc:                func() time.Time { return time.Now().UTC() },
		},
	)
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

func canaryNodeExecution() *domain.NodeExecution {
	return &domain.NodeExecution{
		ID:               uuid.New(),
		GraphExecutionID: uuid.New(),
		GraphNodeID:      uuid.New(),
		NodeType:         domain.GraphNodeType("code"),
		NodeName:         "greet",
		SequenceNumber:   1,
		Status:           domain.GraphExecutionStatus("completed"),
		Input:            domain.JSONMap{"greeting_suffix": logCanary},
		Output:           domain.JSONMap{"greeting": "hello x " + logCanary},
		CreatedAt:        time.Now().UTC(),
	}
}

func canaryTraceSnapshot() *domain.GraphTraceNodeSnapshot {
	return &domain.GraphTraceNodeSnapshot{
		ID:             uuid.New(),
		TraceID:        uuid.New(),
		GraphNodeID:    uuid.New(),
		NodeName:       "greet",
		NodeType:       "code",
		SequenceNumber: 1,
		Input:          domain.JSONMap{"greeting_suffix": logCanary},
		Output:         domain.JSONMap{"greeting": "hello x " + logCanary},
		Config:         domain.JSONMap{"suffix_ref": logCanary},
		Status:         "completed",
		StartedAt:      time.Now().UTC(),
		CompletedAt:    time.Now().UTC(),
	}
}

// TestNewGormLogger_NeverEchoesBoundValues drives the two body-bearing models
// through gorm's real render path at Info — the level the service is configured
// to run at — and asserts the statement is still there while its bound values
// are not.
//
// Controls: force `LogLevel: logger.Silent` inside newGormLogger and the
// "statement present" leg goes red (the test would otherwise pass on an empty
// buffer); set `ParameterizedQueries: false` and the "canary absent" leg goes
// red.
func TestNewGormLogger_NeverEchoesBoundValues(t *testing.T) {
	cases := []struct {
		name  string
		model any
		table string
	}{
		{"node_executions", canaryNodeExecution(), `INSERT INTO "node_executions"`},
		{"graph_trace_node_snapshots", canaryTraceSnapshot(), `INSERT INTO "graph_trace_node_snapshots"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			db := dryRunDB(t, &buf, logger.Info)

			db.Create(tc.model)

			out := buf.String()
			if !strings.Contains(out, tc.table) {
				t.Fatalf("positive control failed: no %s line was logged at Info, so an absence assertion would prove nothing.\ngot:\n%s", tc.table, out)
			}
			if strings.Contains(out, logCanary) {
				t.Fatalf("bound value echoed to the ORM log:\n%s", out)
			}
		})
	}
}

// TestNewGormLogger_ErrorPathNeverEchoesBoundValues covers the path that makes
// even a `warn`-configured service leak: gorm renders the FULL statement for any
// FAILED statement at Error. The error is forced by a callback so the render
// path is the real one.
//
// Controls: same two — Silent kills the positive control,
// `ParameterizedQueries: false` puts the canary back in the line.
func TestNewGormLogger_ErrorPathNeverEchoesBoundValues(t *testing.T) {
	errForced := errors.New("forced statement failure")

	cases := []struct {
		name  string
		model any
		table string
	}{
		{"node_executions", canaryNodeExecution(), `INSERT INTO "node_executions"`},
		{"graph_trace_node_snapshots", canaryTraceSnapshot(), `INSERT INTO "graph_trace_node_snapshots"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			db := dryRunDB(t, &buf, logger.Error)
			if err := db.Callback().Create().After("gorm:create").Register("d396:force_error", func(tx *gorm.DB) {
				_ = tx.AddError(errForced)
			}); err != nil {
				t.Fatalf("register forcing callback: %v", err)
			}

			if err := db.Create(tc.model).Error; !errors.Is(err, errForced) {
				t.Fatalf("control failed: statement did not fail, so the Error path never ran (err=%v)", err)
			}

			out := buf.String()
			if !strings.Contains(out, tc.table) {
				t.Fatalf("positive control failed: no %s line was logged on the error path.\ngot:\n%s", tc.table, out)
			}
			if strings.Contains(out, logCanary) {
				t.Fatalf("bound value echoed to the ORM log on the error path:\n%s", out)
			}
		})
	}
}

// TestParseLogLevel pins the fail-closed contract: an unrecognised level is an
// error the caller must refuse to boot on, never a default.
//
// Control: return `logger.Warn, nil` from the default branch and the three
// rejection cases go red.
func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    logger.LogLevel
		wantErr bool
	}{
		{"silent", "silent", logger.Silent, false},
		{"error", "error", logger.Error, false},
		{"warn", "warn", logger.Warn, false},
		{"info", "info", logger.Info, false},
		{"normalised", "  INFO  ", logger.Info, false},
		{"empty", "", 0, true},
		{"unknown", "verbose", 0, true},
		{"numeric", "4", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLogLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLogLevel(%q) = %v, nil; want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLogLevel(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseLogLevel(%q) = %v; want %v", tt.in, got, tt.want)
			}
		})
	}
}
