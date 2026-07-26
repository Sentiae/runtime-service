package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// ─────────────────────────────────────────────────────────────────────
// The fleet's durability instruments, and the timer that keeps them true.
//
// ⚠ THE ONE RULE THIS FILE EXISTS TO ENFORCE: a gauge that stopped being
// updated must be visible AS a stopped gauge. A snapshotter or reconciler that
// silently died reports exactly the same numbers as one with nothing to report,
// so every instrument here obeys three constraints:
//
//	1. Prefer an AGE or an ABSOLUTE TIMESTAMP over a bare count. "No snapshot for
//	   nine days" is then a value, not something you can only infer from a series
//	   that stopped moving.
//	2. Never let absence-or-zero read as healthy. A missing fact is published as
//	   MetricUnknown (-1) — impossible for any real age or count — so an alert can
//	   name it explicitly instead of matching it accidentally.
//	3. Emit from a TIMER, not only from the write path. A metric that is updated
//	   only when work succeeds can never report that the work stopped, which is
//	   the whole failure mode.
//
// Instruments are promauto on the DEFAULT registry — the fleet convention:
// otelkit.Init bridges that registry into the OTLP metric pipeline
// (platform-kit/otel, prometheusbridge.NewMetricProducer), and the HTTP surface
// also exposes it at /metrics for hosts that run with telemetry disabled (the
// bare Firecracker fleet host does). Precedent: identity-service
// usecase/plan_metrics.go (periodic gauge cron) and catalog-service
// usecase/metrics.go (usecase_executions_total).
// ─────────────────────────────────────────────────────────────────────

// MetricUnknown is the value every gauge in this file publishes for a fact that
// does not exist: no recovery point has ever been taken, no snapshot has ever
// succeeded, no collection has ever completed.
//
// -1 and not 0, and not "omit the series": an age of 0 means "protected a moment
// ago" (the healthiest possible reading for the least protected possible state),
// and an omitted series means the alert that watches it silently has nothing to
// evaluate. -1 is unreachable for an age (clamped at 0 below) and for a count, so
// `< 0` is an unambiguous "unknown" predicate and `> threshold` can never match it
// by accident.
const MetricUnknown = -1

var (
	// recoveryPointAge is per-resource: how long ago this resource's NEWEST
	// recovery point was taken. MetricUnknown when the resource has none at all —
	// which is the live state that motivated this whole file.
	//
	// Cardinality: one series per LIVE durable claim, and a claim is a customer
	// database (tens, not millions). Reset() on every pass so a decommissioned
	// resource's series disappears instead of freezing at its last age forever.
	recoveryPointAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_point_age_seconds",
		Help: "Age in seconds of a resource's newest recovery point; -1 when the resource has NO recovery point at all.",
	}, []string{"resource_id", "owner_org"})

	// recoveryPointCount is the catalog size per resource. Published alongside the
	// age because 0 is the actionable number and a count cannot be inferred from an
	// age of -1 (a resource could also be unknown for lack of any live claim).
	recoveryPointCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_points",
		Help: "Number of recovery points in a resource's catalog. 0 means the resource is unprotected.",
	}, []string{"resource_id", "owner_org"})

	// recoveryPointOldestAge / recoveryPointNewestAge are the fleet-wide summary
	// over resources that HAVE a recovery point: the worst and best newest-point
	// ages. Both are MetricUnknown when no live resource has any recovery point,
	// because "the oldest recovery point is 0 seconds old" would be a lie in
	// exactly the direction that loses data.
	recoveryPointOldestAge = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_point_oldest_age_seconds",
		Help: "Fleet-wide worst case: the largest newest-recovery-point age across live resources; -1 when no live resource has any recovery point.",
	})
	recoveryPointNewestAge = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_point_newest_age_seconds",
		Help: "Fleet-wide best case: the smallest newest-recovery-point age across live resources; -1 when no live resource has any recovery point.",
	})

	// resourcesUnprotected is the headline number: live claims with zero recovery
	// points. It is a COUNT of the -1 population above, so an alert can fire on
	// `> 0` without having to reason about a sentinel.
	resourcesUnprotected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentiae_fleet_resources_unprotected",
		Help: "Live resource claims with ZERO recovery points — data with no restorable artifact behind it.",
	})
	resourcesLive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentiae_fleet_resources_live",
		Help: "Live (non-tombstoned) resource claims. The denominator of every ratio above.",
	})

	// recoveryPointsByLocation is the fleet's failure-domain census: how many
	// recovery points are PROVEN to exist in two failure domains versus how many are
	// known to exist in one versus how many predate the record (migration 0023).
	//
	// Three separate series and never a ratio: `unknown` is deliberately NOT folded
	// into either side. Folding it into two_domains would invent protection, and
	// folding it into primary_only would invent a definite claim out of a missing
	// fact. All three are published on every pass (including as 0) so an alert on
	// `primary_only > 0` is never evaluating an absent series.
	recoveryPointsByLocation = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_points_by_location",
		Help: "Recovery points by where their blob is known to exist: primary_and_second_domain (two failure domains, verified), primary_only (one — losing the chassis loses it), unknown (predates the record; NOT to be counted as protected).",
	}, []string{"locations"})

	// recoveryPointOldestSingleDomainAge is the number to alert on: how long the
	// LEAST protected recovery point has been sitting in one failure domain.
	//
	// An AGE, not a count, for this file's rule 1 — "the oldest copy nobody has
	// mirrored is nine days old" is then a value rather than something inferred from
	// a count that stopped moving. It spans primary_only AND unknown, because the
	// question is "not PROVABLY in two domains" and 0019/0022's doctrine is that
	// unknown reads as the weakest class.
	//
	// MetricUnknown when no such recovery point exists — which is the healthy state,
	// and must not be reported as an age of 0 (that is the reading a brand-new
	// single-domain copy produces).
	recoveryPointOldestSingleDomainAge = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_recovery_point_oldest_single_domain_age_seconds",
		Help: "Age in seconds of the OLDEST recovery point not provably in two failure domains (primary_only or unknown); -1 when every recovery point is verified in two domains, or when there are none.",
	})

	// snapshotFailures surfaces migration 0018's consecutive_snapshot_failures.
	// A count, not a flag: a blip and a week-long protection outage must not look
	// alike.
	snapshotFailures = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_resource_snapshot_failures",
		Help: "Snapshot attempts that have failed IN A ROW for a resource since the last one that captured a recovery point.",
	}, []string{"resource_id", "owner_org"})

	// snapshotLastSuccessAge is the other half of that pair, and the one an alert
	// should key on: the age of last_snapshot_success_at. MetricUnknown when the
	// resource has never recorded a successful snapshot.
	snapshotLastSuccessAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_resource_snapshot_last_success_age_seconds",
		Help: "Age in seconds of a resource's last SUCCESSFUL snapshot; -1 when no snapshot has ever succeeded for it.",
	}, []string{"resource_id", "owner_org"})

	// netLeasesHeld is the durable microVM addressing plane's occupancy per host.
	netLeasesHeld = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_net_leases_held",
		Help: "microVM addressing leases currently held on a host (rows in fleet_net_leases).",
	}, []string{"host_id"})

	// netPlaneReconciled is the fail-closed signal for this host's addressing
	// plane: 1 the plane was proven and boots are served, 0 it was not and boots
	// are refused, MetricUnknown this instance has no addressing plane at all
	// (non-firecracker executor).
	//
	// It is written at boot AND re-written by every boot's precondition check
	// (NetPlaneGuardedImageBooter), deliberately: the refusal is not a
	// process-lifetime fact — a host self-heals once the cause is gone — and a gauge
	// that only carried the boot-time verdict would keep alarming after the plane
	// recovered, which is exactly the latch the guard exists to remove.
	netPlaneReconciled = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_net_plane_reconciled",
		Help: "1 = the microVM addressing plane was proven on the last check, 0 = it was not and boots are refused, -1 = this instance has no addressing plane.",
	})

	// netLeaseAcquires counts allocation attempts by outcome. Write-path driven on
	// purpose — it answers "are boots being refused", which no periodic read of the
	// lease table can show.
	netLeaseAcquires = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sentiae_fleet_net_lease_acquire_total",
		Help: "microVM addressing lease acquisitions by outcome: allocated, adopted, conflict_retry, refused.",
	}, []string{"outcome"})

	// hostsLive / hostsAttested are the pair that makes a phantom failure domain
	// visible: two live hosts one of which never attested a parseable
	// failure_domain is NOT two failure domains, and HA placement refuses it.
	hostsLive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentiae_fleet_hosts_live",
		Help: "Fleet hosts with status active.",
	})
	hostsAttested = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentiae_fleet_hosts_attested",
		Help: "Live fleet hosts whose failure_domain PARSES (an unattested host is not a failure domain, so it cannot back an HA promise).",
	})

	// ledgerDivergences exposes the report-only ledger↔reality audit's findings.
	// There are live instances of these right now, so a zero here after a pass is
	// itself information.
	ledgerDivergences = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentiae_fleet_ledger_divergences",
		Help: "Ledger↔reality divergences found by the last completed audit pass, by kind (row_without_file, file_without_row, recovery_point_without_object).",
	}, []string{"kind"})

	// ledgerUndetermined is deliberately NOT a divergence kind: an entry the pass
	// could not decide is not a finding of health, and folding it into the
	// divergence vec would let a DB outage read as clean.
	ledgerUndetermined = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentiae_fleet_ledger_undetermined",
		Help: "Entries the last ledger audit pass could NOT decide. Not divergences, and not evidence of health.",
	})

	// ledgerLastSuccess is how a stopped audit becomes visible: the pass runs every
	// 6h, so a timestamp that stops advancing is the alert. Absolute unix seconds,
	// not an age — an age gauge that stops being written looks constant, whereas an
	// unmoving timestamp is visibly old to whoever evaluates now() - value.
	ledgerLastSuccess = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_ledger_reconcile_last_success_timestamp_seconds",
		Help: "Unix time of the last ledger audit pass that COMPLETED; -1 when none has completed in this process.",
	})

	// durabilityCollectionErrors + durabilityLastSuccess are the collector's own
	// self-report. Without them a fail-soft collector is indistinguishable from a
	// healthy one: it holds the previous values (correctly — a DB blip must not zero
	// a durability gauge), and holding stale values is exactly what a dead
	// collector also does.
	durabilityCollectionErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sentiae_fleet_durability_collection_errors_total",
		Help: "Durability metric collection passes that failed. Gauges keep their previous values on failure, so this counter and the timestamp below are the ONLY evidence they are stale.",
	})
	durabilityLastSuccess = unknownUntilCollected(prometheus.GaugeOpts{
		Name: "sentiae_fleet_durability_collection_last_success_timestamp_seconds",
		Help: "Unix time of the last FULLY successful durability collection pass; -1 when none has completed in this process.",
	})

	// usecaseExecutions is §22's mandatory per-use-case counter. Same name and
	// label set as catalog-service and identity-service so one fleet-wide dashboard
	// query covers every service.
	usecaseExecutions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "usecase_executions_total",
		Help: "Total use case executions, labeled by use case name and outcome.",
	}, []string{"name", "outcome"})
)

// Use case execution outcome labels (catalog-service's set, matched verbatim).
const (
	outcomeOK      = "ok"
	outcomeInvalid = "invalid"
	outcomeError   = "error"
)

// Net-lease acquire outcome labels.
const (
	leaseOutcomeAllocated     = "allocated"
	leaseOutcomeAdopted       = "adopted"
	leaseOutcomeConflictRetry = "conflict_retry"
	leaseOutcomeRefused       = "refused"
)

// unknownUntilCollected registers a gauge whose value STARTS at MetricUnknown
// instead of Prometheus's implicit 0. A process that has not collected yet — or
// whose very first collection failed — must not publish a page of zeros that read
// as a healthy fleet, and 0 is a meaningful value for every gauge built this way.
func unknownUntilCollected(opts prometheus.GaugeOpts) prometheus.Gauge {
	g := promauto.NewGauge(opts)
	g.Set(MetricUnknown)
	return g
}

// recordExecution increments the §22 use case counter.
func recordExecution(name, outcome string) {
	usecaseExecutions.WithLabelValues(name, outcome).Inc()
}

// outcomeFor maps an error to the §22 outcome label. An invalid-input error keeps
// the `invalid` label so a caller bug is not counted as a platform fault.
func outcomeFor(err error) string {
	switch {
	case err == nil:
		return outcomeOK
	case errors.Is(err, domain.ErrResourceOwnerOrgRequired),
		errors.Is(err, domain.ErrResourceClaimKeyRequired),
		errors.Is(err, domain.ErrResourceClassUnsupported),
		errors.Is(err, domain.ErrResourceTierUnsupported),
		errors.Is(err, domain.ErrResourceNotFound),
		errors.Is(err, domain.ErrRecoveryPointNotFound):
		return outcomeInvalid
	default:
		return outcomeError
	}
}

// recordLeaseAcquire increments the addressing-lease acquisition counter.
func recordLeaseAcquire(outcome string) {
	netLeaseAcquires.WithLabelValues(outcome).Inc()
}

// PublishNetPlaneReconciled records whether the microVM addressing plane
// reconciled at boot. Called once from the boot wiring (there is no later event
// that changes it): a plane that failed to reconcile refuses every boot for the
// life of the process.
func PublishNetPlaneReconciled(ok bool) {
	if ok {
		netPlaneReconciled.Set(1)
		return
	}
	netPlaneReconciled.Set(0)
}

// PublishNetPlaneNotApplicable records that this instance has no addressing plane
// to reconcile (it is not a Firecracker fleet host). Distinguished from a failed
// reconcile because 0 means "customer boots are being refused here", which would
// be a false alarm on an instance that never boots microVMs.
func PublishNetPlaneNotApplicable() { netPlaneReconciled.Set(MetricUnknown) }

// PublishLedgerReport publishes one COMPLETED ledger audit pass. Only call it for
// a pass that finished: a pass that errored proved nothing, and republishing zeros
// for it would turn an unreadable oracle into a clean bill of health.
func PublishLedgerReport(rep LedgerDivergenceReport, at time.Time) {
	ledgerDivergences.WithLabelValues("row_without_file").Set(float64(rep.RowsWithoutFile))
	ledgerDivergences.WithLabelValues("file_without_row").Set(float64(rep.FilesWithoutRow))
	ledgerDivergences.WithLabelValues("recovery_point_without_object").Set(float64(rep.RecoveryPointsWithoutObject))
	ledgerUndetermined.Set(float64(rep.Undetermined))
	ledgerLastSuccess.Set(float64(at.Unix()))
}

// ─────────────────────────────────────────────────────────────────────
// The collector
// ─────────────────────────────────────────────────────────────────────

// DurabilityCollectEvery is the collection interval.
//
// 60s, matched to the OTLP periodic reader's export interval: a longer interval
// would export a value that is already stale on arrival, and a shorter one would
// re-export the same number several times per push for facts that move on the
// order of minutes to days. The per-pass cost is three indexed queries plus one
// lease count per host, so the interval is chosen for freshness alignment rather
// than for load.
const DurabilityCollectEvery = time.Minute

// DurabilityResourceReader is the read-only slice of the resource ledger the
// collector needs. Narrow by construction: a metric pass must not be able to
// write to the control plane even by mistake.
type DurabilityResourceReader interface {
	ListResourceDurability(ctx context.Context) ([]repository.ResourceDurability, error)
	// ListRecoveryPointLocations is the failure-domain census (migration 0023).
	ListRecoveryPointLocations(ctx context.Context) ([]repository.RecoveryPointLocationFacts, error)
}

// DurabilityHostReader lists the host registry.
type DurabilityHostReader interface {
	List(ctx context.Context) ([]domain.Host, error)
}

// DurabilityLeaseReader counts a host's addressing leases.
type DurabilityLeaseReader interface {
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.NetLease, error)
}

// FleetDurabilityCollector publishes the fleet's durability gauges on a timer.
//
// ⚠ FAIL SOFT, LOUDLY. A collection error leaves every gauge at its previous
// value (zeroing "recovery point age" because the database blinked would invent a
// data-loss alert, and zeroing "unprotected resources" would invent safety) — but
// it increments durabilityCollectionErrors and withholds durabilityLastSuccess,
// so a persistently failing collector is visible as a timestamp that stopped
// advancing rather than as a wall of confident stale numbers.
type FleetDurabilityCollector struct {
	resources DurabilityResourceReader
	hosts     DurabilityHostReader
	leases    DurabilityLeaseReader
	// now is injected so the age computation is testable without sleeping (§30.6).
	now func() time.Time

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewFleetDurabilityCollector constructs the collector. Any reader may be nil:
// the corresponding section is then skipped and reported as skipped, which leaves
// its gauges at MetricUnknown rather than at a fabricated zero.
func NewFleetDurabilityCollector(
	resources DurabilityResourceReader,
	hosts DurabilityHostReader,
	leases DurabilityLeaseReader,
) *FleetDurabilityCollector {
	return &FleetDurabilityCollector{
		resources: resources,
		hosts:     hosts,
		leases:    leases,
		now:       func() time.Time { return time.Now().UTC() },
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start runs the collection loop. The first pass fires immediately so the gauges
// are true within a second of boot instead of after a full interval.
func (c *FleetDurabilityCollector) Start(ctx context.Context) {
	go c.run(ctx)
}

// Stop signals the loop to exit and waits for it (shutdown group, §21).
func (c *FleetDurabilityCollector) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
}

func (c *FleetDurabilityCollector) run(ctx context.Context) {
	defer close(c.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("fleet durability metric collector panicked", "panic", r)
		}
	}()
	c.collectOnce(ctx)
	t := time.NewTicker(DurabilityCollectEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-t.C:
			c.collectOnce(ctx)
		}
	}
}

// collectOnce runs one pass, logging (never propagating) a failure.
func (c *FleetDurabilityCollector) collectOnce(ctx context.Context) {
	if err := c.Collect(ctx); err != nil {
		logger.FromContext(ctx).Error("fleet durability metric collection failed; gauges keep their PREVIOUS values and are now stale",
			"err", err)
	}
}

// Collect runs one collection pass. Sections are independent: a failure in one
// leaves that section's gauges untouched and still lets the others publish, but
// the pass as a whole is a failure — it increments the error counter and does not
// advance the last-success timestamp.
func (c *FleetDurabilityCollector) Collect(ctx context.Context) error {
	var errs []error
	if err := c.collectResources(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.collectRecoveryPointLocations(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.collectHosts(ctx); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		durabilityCollectionErrors.Inc()
		return errors.Join(errs...)
	}
	durabilityLastSuccess.Set(float64(c.now().Unix()))
	return nil
}

func (c *FleetDurabilityCollector) collectResources(ctx context.Context) error {
	if c.resources == nil {
		return errors.New("durability collect: no resource ledger wired")
	}
	facts, err := c.resources.ListResourceDurability(ctx)
	if err != nil {
		return fmt.Errorf("list resource durability: %w", err)
	}
	publishResourceDurability(ComputeResourceDurability(facts, c.now()))
	return nil
}

// collectRecoveryPointLocations publishes the failure-domain census. It is its own
// section so a failure of this query leaves the census gauges at their previous
// values (or at MetricUnknown before the first pass) instead of publishing a fleet
// with zero single-domain copies, which is the flattering lie.
func (c *FleetDurabilityCollector) collectRecoveryPointLocations(ctx context.Context) error {
	if c.resources == nil {
		return errors.New("durability collect: no resource ledger wired")
	}
	facts, err := c.resources.ListRecoveryPointLocations(ctx)
	if err != nil {
		return fmt.Errorf("list recovery point locations: %w", err)
	}
	publishRecoveryPointLocations(ComputeRecoveryPointLocations(facts, c.now()))
	return nil
}

func (c *FleetDurabilityCollector) collectHosts(ctx context.Context) error {
	if c.hosts == nil {
		return errors.New("durability collect: no host registry wired")
	}
	hosts, err := c.hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("list fleet hosts: %w", err)
	}

	live, attested := 0, 0
	for i := range hosts {
		h := &hosts[i]
		if h.Status != domain.HostStatusActive {
			continue
		}
		live++
		// HasAttestedFailureDomain is the ONLY arbiter (it parses): it refuses the
		// `unattested` backfill value and every malformed one, so a host counted here
		// has actually stated what it shares a fate with.
		if h.HasAttestedFailureDomain() {
			attested++
		}
	}
	hostsLive.Set(float64(live))
	hostsAttested.Set(float64(attested))

	if c.leases == nil {
		return errors.New("durability collect: no lease store wired")
	}
	// Reset so a removed host stops reporting the occupancy it had when it left.
	netLeasesHeld.Reset()
	var leaseErrs []error
	for i := range hosts {
		leases, lerr := c.leases.ListByHost(ctx, hosts[i].ID)
		if lerr != nil {
			// Per-host: one unreadable host must not withhold every other host's
			// occupancy. The series for this host is simply absent this pass, and the
			// pass is reported as failed.
			leaseErrs = append(leaseErrs, fmt.Errorf("list leases of host %s: %w", hosts[i].ID, lerr))
			continue
		}
		netLeasesHeld.WithLabelValues(hosts[i].ID.String()).Set(float64(len(leases)))
	}
	return errors.Join(leaseErrs...)
}

// ResourceDurabilityGauges is one resource's computed metric values.
type ResourceDurabilityGauges struct {
	ResourceID string
	OwnerOrg   string
	// RecoveryPointAgeSeconds is MetricUnknown when the resource has no recovery
	// point.
	RecoveryPointAgeSeconds float64
	RecoveryPointCount      float64
	SnapshotFailures        float64
	// SnapshotLastSuccessAgeSeconds is MetricUnknown when no snapshot has ever
	// succeeded for the resource.
	SnapshotLastSuccessAgeSeconds float64
}

// ResourceDurabilitySnapshot is one pass's computed values for the whole fleet.
type ResourceDurabilitySnapshot struct {
	Resources []ResourceDurabilityGauges
	Live      int
	// Unprotected counts live resources with ZERO recovery points.
	Unprotected int
	// OldestAgeSeconds / NewestAgeSeconds summarize only the resources that HAVE a
	// recovery point, and are MetricUnknown when none does.
	OldestAgeSeconds float64
	NewestAgeSeconds float64
}

// ComputeResourceDurability turns the ledger projection into gauge values. Pure
// (no clock, no I/O) so the encoding decisions above are directly testable.
func ComputeResourceDurability(facts []repository.ResourceDurability, now time.Time) ResourceDurabilitySnapshot {
	out := ResourceDurabilitySnapshot{
		Resources:        make([]ResourceDurabilityGauges, 0, len(facts)),
		Live:             len(facts),
		OldestAgeSeconds: MetricUnknown,
		NewestAgeSeconds: MetricUnknown,
	}
	for i := range facts {
		f := &facts[i]
		g := ResourceDurabilityGauges{
			ResourceID:                    f.ResourceID.String(),
			OwnerOrg:                      f.OwnerOrg.String(),
			RecoveryPointCount:            float64(f.RecoveryPointCount),
			SnapshotFailures:              float64(f.ConsecutiveSnapshotFailures),
			RecoveryPointAgeSeconds:       ageSeconds(f.LatestRecoveryPointAt, now),
			SnapshotLastSuccessAgeSeconds: ageSeconds(f.LastSnapshotSuccessAt, now),
		}
		// The catalog count is the authority on "unprotected", not the timestamp: a
		// row with a NULL created_at would otherwise be counted as protection.
		if f.RecoveryPointCount == 0 || f.LatestRecoveryPointAt == nil {
			out.Unprotected++
			// A resource with a counted recovery point but no usable timestamp is not
			// protected in any way we can prove, so it reports unknown rather than an
			// age derived from a missing fact.
			g.RecoveryPointAgeSeconds = MetricUnknown
		} else {
			if out.OldestAgeSeconds == MetricUnknown || g.RecoveryPointAgeSeconds > out.OldestAgeSeconds {
				out.OldestAgeSeconds = g.RecoveryPointAgeSeconds
			}
			if out.NewestAgeSeconds == MetricUnknown || g.RecoveryPointAgeSeconds < out.NewestAgeSeconds {
				out.NewestAgeSeconds = g.RecoveryPointAgeSeconds
			}
		}
		out.Resources = append(out.Resources, g)
	}
	return out
}

// RecoveryPointLocationSnapshot is one pass's failure-domain census.
type RecoveryPointLocationSnapshot struct {
	// CountByLocation carries EVERY known class, including the ones the query
	// returned no rows for (as 0). A class published as absent would leave its alert
	// with nothing to evaluate.
	CountByLocation map[string]float64
	// OldestSingleDomainAgeSeconds is the age of the oldest recovery point NOT
	// provably in two domains (primary_only ∪ unknown), MetricUnknown when there is
	// none.
	OldestSingleDomainAgeSeconds float64
}

// ComputeRecoveryPointLocations turns the catalog census into gauge values. Pure
// (no clock, no I/O) so the encoding decisions are directly testable.
//
// An unrecognized class from the database is counted under its own label rather
// than dropped or folded into a known one: it can only arrive from a writer this
// build does not know about, and silently attributing it to `primary_and_second_domain`
// would be the fail-open. It is treated as NOT-two-domains for the age gauge, for
// the same reason `unknown` is.
func ComputeRecoveryPointLocations(facts []repository.RecoveryPointLocationFacts, now time.Time) RecoveryPointLocationSnapshot {
	out := RecoveryPointLocationSnapshot{
		CountByLocation: map[string]float64{
			string(domain.RecoveryPointLocationsSecondDomain): 0,
			string(domain.RecoveryPointLocationsPrimaryOnly):  0,
			string(domain.RecoveryPointLocationsUnknown):      0,
		},
		OldestSingleDomainAgeSeconds: MetricUnknown,
	}
	for i := range facts {
		f := &facts[i]
		out.CountByLocation[f.Locations] += float64(f.Count)
		if domain.RecoveryPointLocations(f.Locations).InTwoFailureDomains() {
			continue
		}
		// A class with a count but no usable oldest timestamp contributes to the count
		// and not to the age: an age derived from a missing fact would be a fabricated
		// number in the direction that reads as healthy.
		age := ageSeconds(f.OldestCreatedAt, now)
		if age == MetricUnknown {
			continue
		}
		if out.OldestSingleDomainAgeSeconds == MetricUnknown || age > out.OldestSingleDomainAgeSeconds {
			out.OldestSingleDomainAgeSeconds = age
		}
	}
	return out
}

// publishRecoveryPointLocations writes a computed census to the gauges. Reset first
// so a class that no longer has any members stops reporting the count it had — but
// the three known classes are always re-published (as 0 when empty) by
// ComputeRecoveryPointLocations, so Reset never leaves an alert without a series.
func publishRecoveryPointLocations(s RecoveryPointLocationSnapshot) {
	recoveryPointsByLocation.Reset()
	for class, count := range s.CountByLocation {
		recoveryPointsByLocation.WithLabelValues(class).Set(count)
	}
	recoveryPointOldestSingleDomainAge.Set(s.OldestSingleDomainAgeSeconds)
}

// ageSeconds is the age of a timestamp, MetricUnknown when there is none.
//
// Clamped at 0 for a future timestamp: an unclamped negative age could land on
// -1 under clock skew and be read as "no recovery point exists", which is the one
// confusion this encoding must never allow.
func ageSeconds(at *time.Time, now time.Time) float64 {
	if at == nil || at.IsZero() {
		return MetricUnknown
	}
	age := now.Sub(*at).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

// publishResourceDurability writes a computed snapshot to the gauges.
//
// The per-resource vecs are Reset first so a decommissioned resource's series
// disappears rather than freezing at its final age — a frozen series would keep
// reporting a protection state for data that no longer exists.
func publishResourceDurability(s ResourceDurabilitySnapshot) {
	recoveryPointAge.Reset()
	recoveryPointCount.Reset()
	snapshotFailures.Reset()
	snapshotLastSuccessAge.Reset()
	for _, g := range s.Resources {
		recoveryPointAge.WithLabelValues(g.ResourceID, g.OwnerOrg).Set(g.RecoveryPointAgeSeconds)
		recoveryPointCount.WithLabelValues(g.ResourceID, g.OwnerOrg).Set(g.RecoveryPointCount)
		snapshotFailures.WithLabelValues(g.ResourceID, g.OwnerOrg).Set(g.SnapshotFailures)
		snapshotLastSuccessAge.WithLabelValues(g.ResourceID, g.OwnerOrg).Set(g.SnapshotLastSuccessAgeSeconds)
	}
	resourcesLive.Set(float64(s.Live))
	resourcesUnprotected.Set(float64(s.Unprotected))
	recoveryPointOldestAge.Set(s.OldestAgeSeconds)
	recoveryPointNewestAge.Set(s.NewestAgeSeconds)
}
