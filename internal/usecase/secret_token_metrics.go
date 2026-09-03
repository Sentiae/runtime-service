package usecase

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ─────────────────────────────────────────────────────────────────────
// The per-run secret token's END OF LIFE, made visible.
//
// The handed Vault token is what makes a run's secret access per-RUN: it is
// revoked at terminal cleanup whatever the outcome. When that revocation fails
// the credential survives to its TTL, so the lifetime property the design exists
// to provide is silently gone. D-7 is exactly this: `auth/token/revoke-self`
// answered 403 on EVERY run since the token role was written, the only signal was
// an unwatched WARN line, and nothing went red for the life of the defect.
//
// Instrument on the default registry, like every other counter in this package
// (otelkit.Init bridges it into the OTLP pipeline and /metrics exposes it).
// ─────────────────────────────────────────────────────────────────────

// Handed-token revocation outcome labels.
const (
	revokeOutcomeOK     = "ok"
	revokeOutcomeFailed = "failed"
)

// secretTokenRevocations counts terminal-cleanup revocations of the per-run
// handed token, by outcome.
var secretTokenRevocations = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "sentiae_runtime_secret_token_revocations_total",
	Help: "Terminal-cleanup revocations of the per-run handed Vault token by outcome: ok (the token is dead), failed (the credential outlives its run, up to its TTL). Both series are pre-created at 0 at engine construction, so an absent series means the engine was never built — never 'no failures'.",
}, []string{"outcome"})

// precreateSecretTokenRevocationSeries publishes both series at 0.
//
// ⚠ THIS IS THE POINT OF THE FILE. A promauto CounterVec with no observation
// exports NO series at all, so a dashboard or an alert reading
// `...{outcome="failed"}` gets *nothing back* — indistinguishable from a healthy
// zero, and unmatchable by any `> 0` rule. D-7 hid for exactly this shape of
// reason (a signal nobody could see), so the "failed" series must exist and read
// 0 from the moment the engine does, not from the first failure.
//
// Called from NewGraphExecutionEngine; Add(0) is idempotent, so a process that
// builds more than one engine publishes the same two series.
func precreateSecretTokenRevocationSeries() {
	secretTokenRevocations.WithLabelValues(revokeOutcomeOK).Add(0)
	secretTokenRevocations.WithLabelValues(revokeOutcomeFailed).Add(0)
}

// recordSecretTokenRevocation increments the revocation counter for one outcome.
func recordSecretTokenRevocation(outcome string) {
	secretTokenRevocations.WithLabelValues(outcome).Inc()
}
