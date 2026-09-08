package container

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sentiae/runtime-service/internal/domain"
)

// The sidecar's event names as its logger writes them (cmd/node-sidecar/control.go).
// TestAudit_MatchesRuntimeDrainContract runs the real sidecar through this parser.
const (
	auditEventBound    = "sidecar_bound"
	auditEventDecision = "egress_decision"
	auditEventCapped   = "egress_audit_capped"
)

// maxSidecarLogLine bounds one line the scanner holds; a decision line is ~300 B.
const maxSidecarLogLine = 64 * 1024

var (
	auditRows = promauto.NewCounter(prometheus.CounterOpts{
		Name: "node_egress_audit_rows_total",
		Help: "Aggregated egress decision rows written to node_egress_decisions.",
	})
	auditFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "node_egress_audit_failures_total",
		Help: "Sidecar audit drains whose record was lost or deferred, by reason.",
	}, []string{"reason"})
)

// auditFailureReasons are pre-registered by NewSidecarManager so every series
// exists at 0: a counter with no observation exports nothing, and "no failures"
// must be distinguishable from "no metric".
var auditFailureReasons = []string{"read", "blind", "gap", "malformed", "write"}

type sidecarLogLine struct {
	Time         time.Time `json:"time"`
	Msg          string    `json:"msg"`
	Decision     string    `json:"decision"`
	Host         string    `json:"host"`
	HostRedacted bool      `json:"host_redacted"`
	Port         int       `json:"port"`
	Reason       string    `json:"reason"`
	Invocation   string    `json:"invocation_id"`
	Node         string    `json:"node"`
	Run          string    `json:"run_id"`
	Seq          uint64    `json:"seq"`
}

// SidecarAudit is everything one sidecar's log said about egress, aggregated per
// (decision, reason, host, port) in first-seen order. Exported for the sidecar's
// contract test only.
type SidecarAudit struct {
	Decisions []domain.EgressDecision
	Capped    bool
}

// ParseSidecarAudit reads a sidecar's stdout. It is strict exactly where a lie
// could hide: a log without the sidecar_bound anchor is blind (nothing proves it
// was readable), a decision sequence with a hole or a repeat is a gap, and a line
// that does not parse is malformed. Every refusal is returned; none is skipped.
func ParseSidecarAudit(stdout []byte) (SidecarAudit, error) {
	type key struct {
		decision, reason, host string
		port                   int
	}
	agg := map[key]*domain.EgressDecision{}
	var order []key
	seen := map[uint64]bool{}
	var maxSeq uint64
	anchored, capped := false, false

	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 4096), maxSidecarLogLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var l sidecarLogLine
		if err := json.Unmarshal(line, &l); err != nil {
			return SidecarAudit{}, fmt.Errorf("%w: not a JSON line", domain.ErrEgressAuditMalformed)
		}
		switch l.Msg {
		case auditEventBound:
			anchored = true
		case auditEventCapped:
			capped = true
		case auditEventDecision:
			if l.Seq == 0 || seen[l.Seq] {
				return SidecarAudit{}, fmt.Errorf("%w: seq %d", domain.ErrEgressAuditGap, l.Seq)
			}
			seen[l.Seq] = true
			if l.Seq > maxSeq {
				maxSeq = l.Seq
			}
			run, err := uuid.Parse(l.Run)
			if err != nil {
				return SidecarAudit{}, fmt.Errorf("%w: run_id: %w", domain.ErrEgressAuditMalformed, err)
			}
			k := key{l.Decision, l.Reason, l.Host, l.Port}
			d, ok := agg[k]
			if !ok {
				d = &domain.EgressDecision{
					RunID: run, InvocationID: l.Invocation, Node: l.Node,
					Decision: domain.EgressVerdict(l.Decision), Reason: l.Reason,
					Host: l.Host, HostRedacted: l.HostRedacted, Port: l.Port,
					FirstAt: l.Time, LastAt: l.Time,
				}
				if err := d.Validate(); err != nil {
					return SidecarAudit{}, fmt.Errorf("%w: %w", domain.ErrEgressAuditMalformed, err)
				}
				agg[k] = d
				order = append(order, k)
			}
			d.Hits++
			if l.Time.Before(d.FirstAt) {
				d.FirstAt = l.Time
			}
			if l.Time.After(d.LastAt) {
				d.LastAt = l.Time
			}
		}
	}
	if err := sc.Err(); err != nil {
		return SidecarAudit{}, fmt.Errorf("%w: %w", domain.ErrEgressAuditMalformed, err)
	}
	if !anchored {
		return SidecarAudit{}, domain.ErrEgressAuditBlind
	}
	if uint64(len(seen)) != maxSeq {
		return SidecarAudit{}, fmt.Errorf("%w: %d of %d decision lines present",
			domain.ErrEgressAuditGap, len(seen), maxSeq)
	}
	out := SidecarAudit{Capped: capped, Decisions: make([]domain.EgressDecision, 0, len(order))}
	for _, k := range order {
		d := agg[k]
		d.Capped = capped
		out.Decisions = append(out.Decisions, *d)
	}
	return out, nil
}
