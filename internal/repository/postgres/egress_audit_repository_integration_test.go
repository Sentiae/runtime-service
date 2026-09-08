//go:build integration

// External test package, matching this directory's other integration tests.
package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// TestEgressAuditRepository_Record drives D-395's durable half against a real
// Postgres: the tuple key really is idempotent (a re-drained sidecar writes
// nothing twice), the FK really is the attribution (a row cannot exist without
// a run, and reading the org means joining the run), and the cascade really is
// the retention rule (the audit cannot outlive what it explains).
func TestEgressAuditRepository_Record(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	org := uuid.New()
	run := seedGraphExecution(t, db, org)
	repo := postgres.NewEgressAuditRepository(db)
	ctx := context.Background()

	invocation := "inv-" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := []domain.EgressDecision{
		{RunID: run, InvocationID: invocation, Node: "echo", Decision: domain.EgressAllow,
			Reason: "manifest_exact", Host: "httpbin.org", Port: 443, Hits: 1,
			FirstAt: now, LastAt: now},
		{RunID: run, InvocationID: invocation, Node: "echo", Decision: domain.EgressDeny,
			Reason: "host_not_declared", Host: "example.com", Port: 443, Hits: 2,
			FirstAt: now, LastAt: now.Add(time.Second)},
	}
	if err := repo.Record(ctx, rows); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := countAudit(t, db); got != 2 {
		t.Fatalf("rows after the first drain: got %d, want 2", got)
	}

	// A second drain of the same sidecar — the sweeper's retry — writes nothing.
	if err := repo.Record(ctx, rows); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if got := countAudit(t, db); got != 2 {
		t.Fatalf("rows after a repeated drain: got %d, want 2", got)
	}

	t.Run("a row without a run is refused", func(t *testing.T) {
		orphan := []domain.EgressDecision{{
			RunID: uuid.New(), InvocationID: "inv-" + uuid.NewString(), Node: "echo",
			Decision: domain.EgressDeny, Reason: "host_not_declared",
			Host: "unknown-run.example", Port: 443, Hits: 1, FirstAt: now, LastAt: now,
		}}
		err := repo.Record(ctx, orphan)
		if err == nil {
			t.Fatal("an audit row must not exist without the run that explains it")
		}
		if strings.Contains(err.Error(), "unknown-run.example") {
			t.Fatalf("a refused INSERT printed the tenant's host: %v", err)
		}
	})

	t.Run("a redaction flag that disagrees with the host is refused", func(t *testing.T) {
		bad := []domain.EgressDecision{{
			RunID: run, InvocationID: "inv-" + uuid.NewString(), Node: "echo",
			Decision: domain.EgressDeny, Reason: "host_not_declared",
			Host: domain.RedactedEgressHost, HostRedacted: false, Port: 443, Hits: 1,
			FirstAt: now, LastAt: now,
		}}
		if err := repo.Record(ctx, bad); err == nil {
			t.Fatal("host_redacted must agree with the host")
		}
	})

	t.Run("the org is read by joining the run", func(t *testing.T) {
		var got string
		err := db.Raw(`SELECT ge.organization_id::text FROM node_egress_decisions d
			JOIN graph_executions ge ON ge.id = d.run_id
			WHERE d.invocation_id = ? LIMIT 1`, invocation).Scan(&got).Error
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if got != org.String() {
			t.Fatalf("organization: got %s, want %s", got, org.String())
		}
	})

	t.Run("deleting the run deletes its audit", func(t *testing.T) {
		if err := db.Exec(`DELETE FROM graph_executions WHERE id = ?`, run).Error; err != nil {
			t.Fatalf("delete run: %v", err)
		}
		if got := countAudit(t, db); got != 0 {
			t.Fatalf("rows after the run was deleted: got %d, want 0", got)
		}
	})
}

func seedGraphExecution(t *testing.T, db *gorm.DB, org uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.Exec(`INSERT INTO graph_executions
		(id, graph_id, organization_id, requested_by, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'running', now(), now())`,
		id, uuid.New(), org, uuid.New()).Error
	if err != nil {
		t.Fatalf("seed graph execution: %v", err)
	}
	return id
}

func countAudit(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT count(*) FROM node_egress_decisions`).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
