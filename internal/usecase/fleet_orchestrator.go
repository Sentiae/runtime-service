package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetOrchestrator is the runtime-fleet CP4 §9#7 reconciler: it drives a
// FleetApp's actual replica set toward its desired replica count by placing
// (scheduler §9#5) + booting (replica runtime §9#6) shortfall replicas,
// replacing dead ones (restart_policy), and draining surplus. ProvisionApp /
// HealthApp / ScaleApp / DecommissionApp are the app-model P7 surface the
// resident workload class binds to.
type FleetOrchestrator struct {
	apps      repository.FleetAppRepository
	replicas  repository.ReplicaRepository
	scheduler *FleetScheduler
	runtime   *FleetReplicaRuntime

	// resources is the P19 claim ledger, consulted for ONE question:
	// does a live durable resource back this app? DecommissionApp refuses when
	// one does. Constructor-injected rather than a Set* option because a nil
	// ledger cannot mean "guard off" — see refuseIfResourceBacked.
	resources repository.FleetResourceRepository

	// volumes drives the persistent-volume lifecycle (rt#9). Nil on a build with
	// no volume support wired — the orchestrator then behaves statelessly.
	volumes *FleetVolumeManager

	// routes + ingressDomain + ingress drive the fleet-owned ingress (rt#8,
	// D-079). Nil routes/ingress leaves the app URL on the resident endpoint and
	// pushes nothing to a gateway (non-firecracker / unwired builds, tests).
	routes        repository.RouteRepository
	ingressDomain string
	ingress       IngressSyncer

	// activityFeed reports per-host request activity observed at the ingress
	// gateway (Caddy access log). SweepIdle consults it so an app served directly
	// through Caddy — bypassing the activator that stamps LastActiveAt — is not
	// wrongly scaled to zero (#fleet-scale-to-zero-activity-feed, D-122). Nil on
	// builds with no feed wired → SweepIdle keeps its pre-D-122 behavior.
	activityFeed ActivityFeed

	// tokenStore holds each app's handed per-deployment Vault token in memory
	// (D-125): ProvisionApp stashes it, the replica runtime reads it at boot, and
	// DecommissionApp revokes it. Nil where no handed-token path is wired.
	tokenStore *FleetSecretTokenStore

	// netFabric realizes P21 fleet network policy (CP4.5 §9 #5). Nil on builds with
	// no fabric wired (non-firecracker, tests) — ProvisionApp then REFUSES any
	// descriptor carrying a system_id rather than booting it unenforced, and an app
	// with no system_id is unaffected (it reaches no peer either way).
	netFabric *FleetNetworkFabric

	// registryTokenStore holds each app's handed per-deployment registry PULL token
	// in memory (D-124): ProvisionApp stashes it, the replica runtime reads it at
	// materialize (the image pull), and DecommissionApp drops it. Nil leaves the
	// materialize path on the shared registry service key (back-compat).
	registryTokenStore *FleetRegistryTokenStore

	// condLogLast throttles the placement-blocked condition LINES (never the
	// checks themselves — see placeableOnBackingFile). Keyed
	// "<app_id>|<condition>" → the time that line was last emitted. A sync.Map
	// because reconcile ticks run concurrently across apps.
	condLogLast sync.Map

	// clock is the time seam (§30.6). Defaults to time.Now; tests drive the
	// throttle window through it rather than sleeping.
	clock func() time.Time
}

// ActivityFeed reports per-host request activity observed at the ingress gateway
// (the Caddy access log). SweepIdle uses it to avoid scaling a directly-served
// app to zero (#fleet-scale-to-zero-activity-feed, D-122).
type ActivityFeed interface {
	// LastActivity returns the last time host was seen in the access log and
	// whether it has been observed at all.
	LastActivity(host string) (time.Time, bool)
	// Warm reports whether the feed has ingested the access log and its answers
	// can be trusted. A cold or errored feed makes SweepIdle fail safe (treat
	// every app as active).
	Warm() bool
}

// NewFleetOrchestrator constructs the reconciler.
func NewFleetOrchestrator(
	apps repository.FleetAppRepository,
	replicas repository.ReplicaRepository,
	scheduler *FleetScheduler,
	runtime *FleetReplicaRuntime,
	resources repository.FleetResourceRepository,
) *FleetOrchestrator {
	return &FleetOrchestrator{
		apps:      apps,
		replicas:  replicas,
		scheduler: scheduler,
		runtime:   runtime,
		resources: resources,
		clock:     time.Now,
	}
}

// SetVolumeManager wires the persistent-volume manager (rt#9). Optional: a nil
// manager leaves every app stateless.
func (uc *FleetOrchestrator) SetVolumeManager(vm *FleetVolumeManager) { uc.volumes = vm }

// SetNetworkFabric wires the P21 fleet network fabric (CP4.5 §9 #5). Leaving it
// nil does NOT disable enforcement — it makes every system-scoped provision fail
// closed (there is no "skip policies, boot anyway" branch).
func (uc *FleetOrchestrator) SetNetworkFabric(f *FleetNetworkFabric) { uc.netFabric = f }

// SetIngress wires the fleet-owned ingress (rt#8, D-079): the route repository
// (durable host→app records), the base domain for platform-issued hostnames,
// and the syncer that pushes the desired route set to the gateway. All optional
// and nil-safe — a nil routes repo leaves the app URL on the resident endpoint
// and SyncIngress a no-op.
func (uc *FleetOrchestrator) SetIngress(routes repository.RouteRepository, ingressDomain string, ingress IngressSyncer) {
	uc.routes = routes
	uc.ingressDomain = ingressDomain
	uc.ingress = ingress
}

// SetActivityFeed wires the ingress access-log activity feed consulted by
// SweepIdle (#fleet-scale-to-zero-activity-feed, D-122). Optional and nil-safe:
// a nil feed leaves SweepIdle's pre-D-122 behavior intact.
func (uc *FleetOrchestrator) SetActivityFeed(feed ActivityFeed) { uc.activityFeed = feed }

// SetTokenStore wires the in-memory handed-token store (D-125). Optional and
// nil-safe: a nil store keeps the pre-D-125 mint-on-host behavior.
func (uc *FleetOrchestrator) SetTokenStore(ts *FleetSecretTokenStore) { uc.tokenStore = ts }

// SetRegistryTokenStore wires the in-memory handed registry-pull-token store
// (D-124). Optional and nil-safe: a nil store leaves the materialize path on the
// shared registry service key (back-compat).
func (uc *FleetOrchestrator) SetRegistryTokenStore(ts *FleetRegistryTokenStore) {
	uc.registryTokenStore = ts
}

// dnsLabelMaxLen is the maximum length of a single DNS label in octets
// (RFC 1035 §2.3.4: "labels 63 octets or less"). A longer label is not merely
// ugly — it is INVALID: resolvers reject it and ACME/CertMagic refuses to issue a
// certificate for it, so the app would be unreachable over TLS.
const dnsLabelMaxLen = 63

// hostLabelHashLen is how many hex chars of the full label's sha256 are appended
// when a label has to be truncated. 8 hex chars = 32 bits of discrimination, so
// two DIFFERENT over-long labels sharing a truncated prefix still produce
// different hosts.
const hostLabelHashLen = 8

// hostForApp derives the platform-issued hostname for an app:
// <sanitize(componentID)>-<sanitize(env)>.<ingressDomain>, with the first label
// held inside the DNS limit.
//
// It MUST be a pure function of the app row: ensureRoute re-derives the host when
// an app has no route, and migration 0015 deliberately DELETES resource routes so
// they are recreated from the current component id. Anything time- or
// random-dependent here would produce a second, different host on every call.
//
// ⚠ sanitizeSlug is lossy — "a/b", "a-b", "a_b" and "a.b" all collapse to "a-b" —
// so two distinct component ids can still derive one host. That is pre-existing
// and, since dc08424 put the owning org inside a resource's component id, it is
// confined to a SINGLE org. It also now fails closed legibly rather than as a bare
// 500: the unique index on fleet_routes.host_pattern (migration 0006) surfaces as
// domain.ErrIngressHostTaken. Not fixed here.
func (uc *FleetOrchestrator) hostForApp(app *domain.FleetApp) string {
	return dnsLabel(sanitizeSlug(app.ComponentID)+"-"+sanitizeSlug(app.Env)) + "." + uc.ingressDomain
}

// dnsLabel returns label unchanged when it fits in a DNS label, and otherwise a
// deterministic ≤63-octet stand-in: a truncated prefix plus "-" plus the first
// hostLabelHashLen hex chars of sha256(label) — the hash taken over the FULL
// pre-truncation label, so ids that differ only past the cut still differ here.
//
// Why any of this is needed: the first label is
// sanitizeSlug(component_id)-sanitizeSlug(env), and component_id is unvalidated
// free text. A dedicated resource's id is "resource/<org-uuid>/<claim_key>", whose
// slug is 8+36+1+len(claim) octets before the env is even appended — so a plain
// claim key like "postgres-main" with env "prod" overflows 63 and yields a host no
// resolver accepts and no CA will certify. Truncating keeps such a claim WORKING;
// refusing the provision would turn a legitimate claim key into a hard failure.
//
// The arithmetic, explicitly: keep = 63 − 1 ('-' separator) − 8 (hash) = 54 octets
// of prefix, so the result is at most 54 + 1 + 8 = 63. Trailing '-' is trimmed off
// the prefix before joining because a label may not begin or end with '-' and the
// cut can land mid-separator (that only shortens the result — still ≤63). len() is
// octets, which equals characters here: sanitizeSlug emits only [a-z0-9-] ASCII.
func dnsLabel(label string) string {
	if len(label) <= dnsLabelMaxLen {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	suffix := hex.EncodeToString(sum[:])[:hostLabelHashLen]
	prefix := strings.TrimRight(label[:dnsLabelMaxLen-1-hostLabelHashLen], "-")
	return prefix + "-" + suffix
}

// sanitizeSlug lowercases and reduces a label to DNS-safe [a-z0-9-], collapsing
// runs of other characters to a single '-' and trimming leading/trailing '-'.
func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// stripScheme reduces an "http://ip:port" endpoint to the "ip:port" dial target
// the ingress gateway proxies to.
func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return endpoint
}

// httpServedWorkload reports whether this descriptor describes a workload the
// fleet's HTTP edge can actually serve — the only kind that may be given an
// ingress route.
//
// A dedicated data engine is not one. Giving it a route published a
// platform-issued hostname for a customer's database, made the internal CA issue a
// certificate for it, and handed the gateway a reverse-proxy upstream of
// <guest-ip>:5432 — where Postgres rejects the HTTP bytes, so nothing ever worked
// through it. It was pure attack surface, and that same hostname was the key the
// unauthenticated wake path looked apps up by. Postgres is reached at L4 (db-gate),
// never through the HTTP edge.
//
// Two independent signals, because either alone could be forgotten by a future
// caller: the descriptor's declared resource class, and the reserved in-guest
// data-engine port. Both fail toward "no route", which is the safe direction — an
// app without a route still runs and is still reachable on its private endpoint,
// while an unwanted route is a published hostname nobody asked for.
func httpServedWorkload(in FleetProvisionInput) bool {
	return in.ResourceClass == "" && in.Port != residentPGPort
}

// ensureRoute makes sure the app has exactly one ingress route (idempotent): if
// none exists it creates the platform-host route at path "/". No-op when the
// route repo is not wired.
func (uc *FleetOrchestrator) ensureRoute(ctx context.Context, app *domain.FleetApp) error {
	if uc.routes == nil {
		return nil
	}
	existing, err := uc.routes.ListByApp(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}
	now := time.Now().UTC()
	route := &domain.Route{
		ID:          uuid.New(),
		AppID:       app.ID,
		HostPattern: uc.hostForApp(app),
		PathPrefix:  "/",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := uc.routes.Create(ctx, route); err != nil {
		// The derived host IS the diagnosis, and the caller only gets a code plus a
		// fixed message (fleetError never echoes error text), so it goes to the
		// operator log here. Load-bearing for the host-taken case: without it a
		// conflict is a permanently retrying provision with nothing to look at.
		logger.FromContext(ctx).Error("fleet ingress route create failed",
			"app_id", app.ID, "host", route.HostPattern, "err", err)
		return fmt.Errorf("create route: %w", err)
	}
	return nil
}

// SyncIngress builds the current desired route set (every app's routes mapped to
// its resident replica endpoints) and pushes it to the gateway. No-op when the
// ingress or route repo is not wired. Called each reconcile tick.
func (uc *FleetOrchestrator) SyncIngress(ctx context.Context) error {
	if uc.ingress == nil || uc.routes == nil {
		return nil
	}
	apps, err := uc.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	desired := make([]IngressRoute, 0, len(apps))
	for i := range apps {
		routes, err := uc.routes.ListByApp(ctx, apps[i].ID)
		if err != nil {
			return fmt.Errorf("list routes: %w", err)
		}
		if len(routes) == 0 {
			continue
		}
		replicas, err := uc.replicas.ListByApp(ctx, apps[i].ID)
		if err != nil {
			return fmt.Errorf("list replicas: %w", err)
		}
		upstreams := make([]string, 0, len(replicas))
		for j := range replicas {
			if replicas[j].State == domain.ReplicaStateResident && replicas[j].Endpoint != "" {
				upstreams = append(upstreams, stripScheme(replicas[j].Endpoint))
			}
		}
		for j := range routes {
			desired = append(desired, IngressRoute{
				Host:         routes[j].HostPattern,
				CustomDomain: routes[j].CustomDomain,
				Upstreams:    upstreams,
			})
		}
	}
	return uc.ingress.Sync(ctx, desired)
}

// ProvisionApp upserts the FleetApp for (ComponentID, Env) from the descriptor,
// reconciles it once synchronously, and returns the app handle plus a resident
// replica's endpoint (empty when none is resident yet — delivery polls Health).
func (uc *FleetOrchestrator) ProvisionApp(ctx context.Context, in FleetProvisionInput) (string, string, error) {
	// The owning org is REQUIRED, checked before any row is written and before any
	// placement happens. The app row is the tenancy boundary for fleet_apps —
	// there is no RLS on this table (see
	// migrations/0012_create_fleet_resources.up.sql: "owner_org is a column, not a
	// policy") — and an org-less row also carries the secret refs that
	// fleet_replica_runtime.go resolves under whatever org the row happens to
	// hold. An empty org is therefore not a benign default, it is an UNSCOPED row:
	// the next provision with an empty org would match it on
	// (component_id, env, '') and inherit it. Live data holds zero such rows in
	// either control-plane DB, so this fails closed on a case that does not
	// currently occur.
	if in.OwnerOrg == "" {
		return "", "", domain.ErrFleetAppOwnerOrgRequired
	}

	vcpu := in.VCPU
	if vcpu < 1 {
		vcpu = 1
	}
	memMB := int64(in.MemoryMB)
	if memMB < 512 {
		memMB = 512
	}
	// secret_refs is JSONB NOT NULL DEFAULT '[]' — GORM's serializer:json writes a
	// nil slice as SQL NULL (the DB default never applies on an explicit write), so
	// normalize before any create/update (mirrors the nil-labels guard in
	// RegisterHost). Every secret-less provision (resident/volume) passes nil here.
	refs := in.SecretRefs
	if refs == nil {
		refs = []string{}
	}

	// rt#11 — scale-to-zero bounds from the descriptor. A 0 max_replicas defaults
	// to 1 (today's behavior); min_replicas defaults to 0. idle_ttl/scale_to_zero
	// carry through verbatim (0/false disables idle scale-down).
	maxReplicas := in.MaxReplicas
	if maxReplicas < 1 {
		maxReplicas = 1
	}
	minReplicas := in.MinReplicas
	if minReplicas < 0 {
		minReplicas = 0
	}

	// CP4.5 §9 #5 — P21 network membership gate, BEFORE any row is written or any
	// replica placed. A descriptor claiming a system_id must have a host that can
	// prove its enforcement posture AND an ACTIVE network for (system_id, env);
	// otherwise nothing boots. An empty system_id claims no membership and is
	// unaffected — it reaches no fleet peer either way (the pre-#5 behavior).
	if in.SystemID != "" {
		if uc.netFabric == nil {
			return "", "", domain.ErrNetworkEnforcerUnavailable
		}
		if err := uc.netFabric.RequireNetwork(ctx, in.SystemID, in.Env); err != nil {
			return "", "", err
		}
	}

	app, err := uc.apps.FindByComponentEnv(ctx, in.ComponentID, in.Env, in.OwnerOrg)
	switch {
	case err == nil:
		app.SystemID = in.SystemID
		app.ImageRepository = in.Repository
		app.ImageDigest = in.Digest
		app.Port = in.Port
		app.ResourcesVCPU = vcpu
		app.ResourcesMemMB = memMB
		app.OwnerOrg = in.OwnerOrg
		app.SecretRefs = refs
		app.MinReplicas = minReplicas
		app.MaxReplicas = maxReplicas
		app.ScaleToZero = in.ScaleToZero
		app.IdleTTLSeconds = in.IdleTTLSeconds
		if app.DesiredReplicas < 1 {
			app.DesiredReplicas = 1
		}
		now := time.Now().UTC()
		// A re-provision is activity — refresh the idle clock so a freshly
		// redeployed app is not immediately swept to zero.
		app.LastActiveAt = now
		app.UpdatedAt = now
		if err := uc.apps.Update(ctx, app); err != nil {
			return "", "", fmt.Errorf("update fleet app: %w", err)
		}
	case errors.Is(err, domain.ErrFleetAppNotFound):
		now := time.Now().UTC()
		app = &domain.FleetApp{
			ID:              uuid.New(),
			ComponentID:     in.ComponentID,
			Env:             in.Env,
			OwnerOrg:        in.OwnerOrg,
			SystemID:        in.SystemID,
			ImageRepository: in.Repository,
			ImageDigest:     in.Digest,
			SecretRefs:      refs,
			DesiredReplicas: 1,
			MinReplicas:     minReplicas,
			MaxReplicas:     maxReplicas,
			ScaleToZero:     in.ScaleToZero,
			IdleTTLSeconds:  in.IdleTTLSeconds,
			LastActiveAt:    now,
			Port:            in.Port,
			ResourcesVCPU:   vcpu,
			ResourcesMemMB:  memMB,
			RestartPolicy:   domain.RestartPolicyAlways,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := uc.apps.Create(ctx, app); err != nil {
			return "", "", fmt.Errorf("create fleet app: %w", err)
		}
	default:
		return "", "", fmt.Errorf("lookup fleet app: %w", err)
	}

	// D-125 — stash the handed per-deployment Vault token in memory (never on the
	// app row) keyed by app id, BEFORE the first reconcile boots a replica, so the
	// boot's HandedTokenEnvelopeResolver has it. Empty (secret-less deploy / Vault
	// unset) is a no-op. Renewed for the lifetime; revoked on DecommissionApp.
	//
	// Memory-only is deliberate, which makes THIS the only place a token can come
	// back after a runtime-service restart: a durable resource's re-provision is
	// routed here precisely so the store is repopulated before the reconcile below
	// tries to boot (#p19-handed-token-not-rehandable). Put is idempotent for the
	// same value, so a re-provision of a healthy app costs nothing here.
	if uc.tokenStore != nil {
		uc.tokenStore.Put(app.ID, in.VaultToken)
	}

	// D-124 — stash the handed per-deployment registry pull token in memory (never
	// on the app row) keyed by app id, BEFORE the first reconcile pulls the image,
	// so the materialize presents it as the registry Basic password. Empty
	// (pre-cutover / non-fleet) is a no-op → the shared service key is used. Dropped
	// on DecommissionApp.
	if uc.registryTokenStore != nil {
		uc.registryTokenStore.Put(app.ID, in.RegistryPullToken)
	}

	// rt#9 — materialize the app's persistent volumes before the first placement
	// so a replica can attach the data disk at boot. A volume-bearing app is
	// single-writer: it must not run more than one replica.
	if uc.volumes != nil {
		vols, verr := uc.volumes.EnsureAppVolumes(ctx, app.ID, in.Volumes)
		if verr != nil {
			return "", "", fmt.Errorf("ensure volumes: %w", verr)
		}
		if len(vols) > 0 && app.DesiredReplicas > 1 {
			return "", "", domain.ErrVolumeAppNotScalable
		}
	}

	// rt#8 — ensure the app's ingress route exists before the first placement so
	// the next reconcile tick's SyncIngress publishes it. Idempotent. A data-resource
	// app gets NO route (httpServedWorkload): the HTTP edge cannot serve it.
	httpServed := httpServedWorkload(in)
	if httpServed {
		if err := uc.ensureRoute(ctx, app); err != nil {
			return "", "", err
		}
	} else if uc.routes != nil {
		logger.FromContext(ctx).Info("fleet ingress: no HTTP route for a data-resource app (reached at L4, never through the HTTP edge)",
			"app_id", app.ID, "component_id", app.ComponentID, "env", app.Env,
			"resource_class", in.ResourceClass, "port", in.Port)
	}

	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return "", "", err
	}

	// rt#8 — when ingress is wired the public URL is the stable Caddy-served host
	// (https), independent of which replica is resident. Non-ingress builds — and an
	// app that has no ingress route because the HTTP edge cannot serve it — fall
	// back to a resident replica's private endpoint (preserves the pre-rt#8 path).
	if uc.routes != nil && httpServed {
		return app.ID.String(), "https://" + uc.hostForApp(app), nil
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return "", "", fmt.Errorf("list replicas: %w", err)
	}
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateResident {
			return app.ID.String(), replicas[i].Endpoint, nil
		}
	}
	return app.ID.String(), "", nil
}

// ReconcileApp drives one app toward its desired replica count. Per-replica
// scheduling/boot failures are logged and retried next tick — only a hard repo
// error aborts. Idempotent.
func (uc *FleetOrchestrator) ReconcileApp(ctx context.Context, appID uuid.UUID) error {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return nil
		}
		return fmt.Errorf("load app: %w", err)
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	// rt#9 stateful safety — a volume-bearing app is pinned to the host that holds
	// its data. If that host is DEAD/stale we must NOT reschedule the replica
	// elsewhere (that would run it off empty disk): degrade the app and stop. The
	// affinity host being LIVE means the normal dead→shortfall path below reboots
	// the replica on the SAME host (the scheduler honors AffinityHostID), which
	// re-attaches the same backing file. Stateless apps are unaffected.
	if uc.volumes != nil {
		affHost, pinned, aerr := uc.volumes.AffinityHost(ctx, app.ID)
		if aerr != nil {
			logger.FromContext(ctx).Warn("fleet reconcile: affinity host lookup", "app_id", app.ID, "err", aerr)
		} else if pinned && affHost != nil {
			live, lerr := uc.scheduler.IsHostLive(ctx, *affHost)
			if lerr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: affinity host liveness", "app_id", app.ID, "err", lerr)
			} else if !live {
				if derr := uc.volumes.MarkDegraded(ctx, app.ID); derr != nil {
					logger.FromContext(ctx).Warn("fleet reconcile: mark degraded", "app_id", app.ID, "err", derr)
				}
				logger.FromContext(ctx).Warn("fleet reconcile: stateful affinity host unavailable, degrading",
					"app_id", app.ID, "host_id", *affHost, "err", domain.ErrStatefulHostUnavailable)
				return nil
			}
		}
	}

	// Dead replicas → decommission (frees the handle + deletes the row). With
	// restart_policy=always the shortfall step below re-creates them.
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateDead {
			if derr := uc.runtime.DecommissionReplica(ctx, replicas[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission dead replica", "replica_id", replicas[i].ID, "err", derr)
			}
		}
	}

	// Refresh resident health; a now-dead resident is decommissioned so it is
	// replaced by the shortfall step.
	for i := range replicas {
		if replicas[i].State != domain.ReplicaStateResident {
			continue
		}
		healthy, herr := uc.runtime.RefreshHealth(ctx, &replicas[i])
		if herr != nil {
			logger.FromContext(ctx).Warn("fleet reconcile: refresh health", "replica_id", replicas[i].ID, "err", herr)
			continue
		}
		if !healthy {
			if derr := uc.runtime.DecommissionReplica(ctx, replicas[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission dead resident", "replica_id", replicas[i].ID, "err", derr)
			}
		}
	}

	// Recount occupying replicas after the dead cleanup.
	occupying := make([]domain.Replica, 0, len(replicas))
	for i := range replicas {
		switch replicas[i].State {
		case domain.ReplicaStateScheduled, domain.ReplicaStateBooting, domain.ReplicaStateResident:
			occupying = append(occupying, replicas[i])
		}
	}

	switch {
	case len(occupying) < app.DesiredReplicas:
		// A volume-bearing app whose backing FILE is gone cannot be booted by any
		// number of attempts. Without this gate every tick minted a fresh replica
		// row, materialized the image and resolved the boot secrets, watched the VM
		// die on the missing disk, and did it again ~10s later — forever. Bounded
		// (it replaces rather than accumulates) but permanent noise, and it buried
		// the actual cause of a live incident.
		//
		// ⚠ THIS IS A PER-TICK PRECONDITION, NOT A LATCH — do not "improve" it into
		// one. Recording it as VolumeStatusDegraded is the obvious move and it is
		// wrong: NOTHING in this service ever clears that status, so it would trade
		// a churning resource for a permanently stuck one, which is strictly worse.
		// Evaluated fresh on every tick, the resource is one returned file away from
		// self-healing: the moment the backing file is back the next tick places
		// normally, with no operator verb and no state to unwind. Same shape as the
		// ErrNoSchedulableHost defer below.
		if !uc.placeableOnBackingFile(ctx, app.ID) {
			return nil
		}
		shortfall := app.DesiredReplicas - len(occupying)
		// rt#9 — a volume-bearing app is pinned to the host that holds its data:
		// every replica placement targets the affinity host so the same backing
		// file is re-attached. Resolved once per reconcile tick.
		var affHostID *uuid.UUID
		pinned := false
		if uc.volumes != nil {
			h, p, aerr := uc.volumes.AffinityHost(ctx, app.ID)
			if aerr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: affinity host lookup", "app_id", app.ID, "err", aerr)
			} else {
				affHostID, pinned = h, p
			}
		}
		for n := 0; n < shortfall; n++ {
			req := PlacementRequest{
				AppID:      app.ID,
				NeedVCPU:   app.ResourcesVCPU,
				NeedMemMB:  app.ResourcesMemMB,
				NeedDiskMB: 0,
				Constraint: domain.PlacementConstraintBinPack,
			}
			if pinned && affHostID != nil {
				req.AffinityHostID = affHostID
			}
			host, serr := uc.scheduler.SelectHost(ctx, req)
			if serr != nil {
				if errors.Is(serr, domain.ErrNoSchedulableHost) {
					logger.FromContext(ctx).Warn("fleet reconcile: no schedulable host, deferring", "app_id", app.ID)
					return nil
				}
				return fmt.Errorf("select host: %w", serr)
			}
			// First placement of a volume-bearing app: pin its data to this host
			// (write-once). Subsequent placements this tick target the same host.
			if uc.volumes != nil && !pinned {
				hasVol, herr := uc.volumes.HasVolumes(ctx, app.ID)
				if herr != nil {
					logger.FromContext(ctx).Warn("fleet reconcile: has-volumes lookup", "app_id", app.ID, "err", herr)
				} else if hasVol {
					if berr := uc.volumes.BindToHost(ctx, app.ID, host); berr != nil {
						logger.FromContext(ctx).Warn("fleet reconcile: bind volume host", "app_id", app.ID, "err", berr)
					} else {
						bound := host
						affHostID, pinned = &bound, true
					}
				}
			}
			hostID := host
			now := time.Now().UTC()
			replica := &domain.Replica{
				ID:              uuid.New(),
				AppID:           app.ID,
				HostID:          &hostID,
				ImageRepository: app.ImageRepository,
				ImageDigest:     app.ImageDigest,
				State:           domain.ReplicaStateScheduled,
				RestartPolicy:   app.RestartPolicy,
				Port:            app.Port,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if cerr := uc.replicas.Create(ctx, replica); cerr != nil {
				return fmt.Errorf("create replica: %w", cerr)
			}
			if berr := uc.runtime.BootReplica(ctx, replica.ID); berr != nil {
				// BootReplica marks the replica dead on failure; next tick retries.
				logger.FromContext(ctx).Warn("fleet reconcile: boot replica", "replica_id", replica.ID, "err", berr)
			}
		}
	case len(occupying) > app.DesiredReplicas:
		surplus := surplusOrder(occupying)
		drain := len(occupying) - app.DesiredReplicas
		for i := 0; i < drain && i < len(surplus); i++ {
			if derr := uc.runtime.DecommissionReplica(ctx, surplus[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission surplus replica", "replica_id", surplus[i].ID, "err", derr)
			}
		}
	}

	// CP4.5 §9 #5 — re-resolve this app's network against the LIVE replica set.
	// Guest IPs are allocated per BOOT, so the chain must be recompiled whenever
	// the set changes; driving that from the tick is what makes per-boot IPs safe
	// and lets a reboot self-heal with no delivery call. Log-and-continue: a sync
	// error must never stop reconcile, and it fails closed anyway — the enforcer
	// leaves the system chain flushed rather than carrying stale addresses.
	if uc.netFabric != nil && app.SystemID != "" {
		if nerr := uc.netFabric.SyncForApp(ctx, app.SystemID, app.Env); nerr != nil {
			logger.FromContext(ctx).Error("fleet reconcile: sync network policy",
				"app_id", app.ID, "system_id", app.SystemID, "err", nerr)
		}
	}

	return nil
}

// Condition tokens recorded when a volume-bearing app cannot be placed on this
// tick. They are logged, never latched — see placeableOnBackingFile.
const (
	// conditionBackingFileMissing — the volume's backing file is absent while its
	// DIRECTORY is present. The volume store is mounted and the file is not in
	// it: the data-loss shape, and the one an operator must be told about.
	conditionBackingFileMissing = "backing-file-missing"
	// conditionVolumeStoreUnavailable — the backing file's own directory is
	// absent. An unmounted volume store looks exactly like this and it comes back
	// on its own, so this is a DEFER, never a data-loss claim.
	conditionVolumeStoreUnavailable = "volume-store-unavailable"
)

// conditionLogInterval is how often ONE (app, condition) placement-blocked line
// may be emitted. The reconciler re-evaluates the condition every tick (~10s)
// and an unconvergeable resource never stops failing it, so logging every
// evaluation buries every other line in the journal. It throttles the LINE only:
// the check still runs every tick and still answers the same thing.
const conditionLogInterval = 10 * time.Minute

// shouldLogCondition reports whether this (app, condition) line may be emitted
// now, recording the emission when it may. The first occurrence always logs;
// afterwards at most one line per conditionLogInterval.
func (uc *FleetOrchestrator) shouldLogCondition(appID uuid.UUID, condition string) bool {
	key := appID.String() + "|" + condition
	now := uc.now()
	prev, loaded := uc.condLogLast.LoadOrStore(key, now)
	if !loaded {
		return true // first occurrence: log immediately
	}
	last, ok := prev.(time.Time)
	if ok && now.Sub(last) < conditionLogInterval {
		return false
	}
	// CompareAndSwap so a concurrent tick that already claimed this window does
	// not produce a second line.
	return uc.condLogLast.CompareAndSwap(key, prev, now)
}

// clearConditionLogs drops this app's throttle entries. Called whenever the app
// is placeable again: without it a stale timestamp would silently swallow the
// first line of a RECURRENCE, which is exactly the line an operator needs.
func (uc *FleetOrchestrator) clearConditionLogs(appID uuid.UUID) {
	uc.condLogLast.Delete(appID.String() + "|" + conditionBackingFileMissing)
	uc.condLogLast.Delete(appID.String() + "|" + conditionVolumeStoreUnavailable)
}

// now reads the clock seam, tolerating an orchestrator built without one.
func (uc *FleetOrchestrator) now() time.Time {
	if uc.clock != nil {
		return uc.clock()
	}
	return time.Now()
}

// placeableOnBackingFile reports whether a shortfall replica may be placed for
// this app on THIS tick, by checking that the data it would attach still exists.
//
// It answers true for everything it cannot disprove: a stateless app, a volume
// with no materialized backing path yet, a repository lookup that failed, a stat
// that failed for any reason other than absence. Blocking a boot on an
// inconclusive answer would be the far more damaging mistake, so only a file
// PROVEN absent stops a placement — and even then only for this tick.
//
// The check is against the local filesystem, which is the same assumption every
// other volume path in this service already makes (Ensure materializes, boot
// attaches and the snapshotter copies the backing file locally on the host that
// holds the app's affinity).
func (uc *FleetOrchestrator) placeableOnBackingFile(ctx context.Context, appID uuid.UUID) bool {
	placeable := uc.evalPlaceableOnBackingFile(ctx, appID)
	if placeable {
		// The condition (if any) has cleared — forget the throttle so a
		// recurrence logs on its first tick.
		uc.clearConditionLogs(appID)
	}
	return placeable
}

// evalPlaceableOnBackingFile is the check itself. It runs on every tick and its
// answer is the sole input to placement; only the LOGGING it does is throttled.
func (uc *FleetOrchestrator) evalPlaceableOnBackingFile(ctx context.Context, appID uuid.UUID) bool {
	if uc.volumes == nil {
		return true // no volume support wired: every app is stateless here
	}
	// The volume-bearing apps this guards are single-volume by construction (a
	// dedicated resource's descriptor requests exactly one, and in-place restore
	// refuses anything else), so the primary volume IS the app's data.
	vol, has, err := uc.volumes.PrimaryVolume(ctx, appID)
	if err != nil {
		logger.FromContext(ctx).Warn("fleet reconcile: primary volume lookup", "app_id", appID, "err", err)
		return true
	}
	if !has || vol.BackingPath == "" {
		return true
	}
	if _, serr := os.Stat(vol.BackingPath); serr == nil {
		return true
	} else if !os.IsNotExist(serr) {
		logger.FromContext(ctx).Warn("fleet reconcile: stat backing file",
			"app_id", appID, "volume_id", vol.ID, "backing_path", vol.BackingPath, "err", serr)
		return true // an unreadable stat is not evidence of absence
	}

	// The file is absent. WHY it is absent decides what this means, and getting it
	// wrong in the loud direction is its own damage: an unmounted volume store
	// makes every file under it vanish at once, and reporting that as customer
	// data loss sends an operator hunting a restore for data that is intact.
	if _, derr := os.Stat(filepath.Dir(vol.BackingPath)); derr != nil {
		if uc.shouldLogCondition(appID, conditionVolumeStoreUnavailable) {
			logger.FromContext(ctx).Warn("fleet reconcile: volume store unavailable, deferring placement (this is NOT data loss — the store is not mounted)",
				"app_id", appID, "volume_id", vol.ID, "backing_path", vol.BackingPath,
				"condition", conditionVolumeStoreUnavailable, "err", derr)
		}
		return false
	}
	if uc.shouldLogCondition(appID, conditionBackingFileMissing) {
		logger.FromContext(ctx).Error("fleet reconcile: backing file is missing from a mounted volume store — skipping placement (no replica, no image materialize, no secret resolve) until the file returns or the resource is restored from a recovery point",
			"app_id", appID, "volume_id", vol.ID, "backing_path", vol.BackingPath,
			"condition", conditionBackingFileMissing)
	}
	return false
}

// ReconcileAll reconciles every known app. A per-app error is logged, never
// aborting the loop.
func (uc *FleetOrchestrator) ReconcileAll(ctx context.Context) error {
	apps, err := uc.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	for i := range apps {
		if rerr := uc.ReconcileApp(ctx, apps[i].ID); rerr != nil {
			logger.FromContext(ctx).Error("fleet reconcile: app failed", "app_id", apps[i].ID, "err", rerr)
		}
	}
	return nil
}

// SweepIdle scales every eligible scale-to-zero app down to zero replicas
// (rt#11, D-082). An app is eligible when it opts into scale-to-zero, has a
// positive idle_ttl, has been inactive longer than that ttl (now-LastActiveAt),
// AND still has a resident replica to drain. LastActiveAt is stamped at provision
// and refreshed by the activator on each wake — nothing else refreshes it this
// increment, so a busy app served directly through the gateway is at worst
// scaled down and cold-woken on its next request (never a dropped request). A
// per-app error is logged, never aborting the sweep. Called each reconcile tick.
func (uc *FleetOrchestrator) SweepIdle(ctx context.Context) error {
	apps, err := uc.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	now := time.Now().UTC()
	for i := range apps {
		app := &apps[i]
		if !app.ScaleToZero || app.IdleTTLSeconds <= 0 {
			continue
		}
		if now.Sub(app.LastActiveAt) <= time.Duration(app.IdleTTLSeconds)*time.Second {
			continue
		}
		replicas, rerr := uc.replicas.ListByApp(ctx, app.ID)
		if rerr != nil {
			logger.FromContext(ctx).Warn("fleet sweep: list replicas", "app_id", app.ID, "err", rerr)
			continue
		}
		hasResident := false
		for j := range replicas {
			if replicas[j].State == domain.ReplicaStateResident {
				hasResident = true
				break
			}
		}
		if !hasResident {
			continue
		}
		// #fleet-scale-to-zero-activity-feed (D-122): an app served directly through
		// Caddy (bypassing the activator) shows fresh access-log activity even though
		// nothing re-stamped LastActiveAt. Consult the feed before draining; a
		// cold/errored feed fails safe (skip the sweep, keep the app running).
		if uc.sweepBlockedByActivity(ctx, app) {
			continue
		}
		if _, serr := uc.ScaleApp(ctx, app.ID, 0); serr != nil {
			logger.FromContext(ctx).Warn("fleet sweep: scale to zero", "app_id", app.ID, "err", serr)
		}
	}
	return nil
}

// sweepBlockedByActivity reports whether SweepIdle must SKIP scaling app to zero.
// It returns true (skip) when a feed is wired AND either: the feed is cold /
// errored / the app's host cannot be resolved (fail-safe: never scale a
// possibly-busy app to zero, D-122), OR the feed observed a request AFTER
// app.LastActiveAt (the app is served directly through Caddy → re-stamp
// LastActiveAt and keep it running). It returns false (allow the sweep) when no
// feed is wired, or the feed is warm and saw no traffic since the last stamp.
func (uc *FleetOrchestrator) sweepBlockedByActivity(ctx context.Context, app *domain.FleetApp) bool {
	if uc.activityFeed == nil {
		return false
	}
	if !uc.activityFeed.Warm() {
		logger.FromContext(ctx).Warn("fleet sweep: activity feed cold, treating app as active", "app_id", app.ID)
		return true
	}
	hosts, err := uc.appHosts(ctx, app)
	if err != nil {
		logger.FromContext(ctx).Warn("fleet sweep: resolve app hosts, treating app as active", "app_id", app.ID, "err", err)
		return true
	}
	var latest time.Time
	seen := false
	for _, h := range hosts {
		if t, ok := uc.activityFeed.LastActivity(h); ok {
			seen = true
			if t.After(latest) {
				latest = t
			}
		}
	}
	if seen && latest.After(app.LastActiveAt) {
		uc.stampActive(ctx, app)
		return true
	}
	return false
}

// appHosts returns the ingress host(s) an app is served under (host_pattern +
// custom_domain per route). Falls back to the platform-issued host when the
// route repo is not wired.
func (uc *FleetOrchestrator) appHosts(ctx context.Context, app *domain.FleetApp) ([]string, error) {
	if uc.routes == nil {
		return []string{uc.hostForApp(app)}, nil
	}
	routes, err := uc.routes.ListByApp(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	hosts := make([]string, 0, len(routes)*2)
	for i := range routes {
		if routes[i].HostPattern != "" {
			hosts = append(hosts, routes[i].HostPattern)
		}
		if routes[i].CustomDomain != "" {
			hosts = append(hosts, routes[i].CustomDomain)
		}
	}
	return hosts, nil
}

// stampActive refreshes app.LastActiveAt (and persists it) so a directly-served
// app the feed just saw is not re-swept next tick. Mirrors the activator's stamp
// on the wake path. Best-effort — a persist failure is logged, never aborting
// the sweep.
func (uc *FleetOrchestrator) stampActive(ctx context.Context, app *domain.FleetApp) {
	now := time.Now().UTC()
	app.LastActiveAt = now
	app.UpdatedAt = now
	if err := uc.apps.Update(ctx, app); err != nil {
		logger.FromContext(ctx).Warn("fleet sweep: stamp last_active_at", "app_id", app.ID, "err", err)
	}
}

// HealthApp reports the aggregate health of an app's replica set. The bool
// reports whether appID is a known app (false → caller falls back to the
// workload path).
func (uc *FleetOrchestrator) HealthApp(ctx context.Context, appID uuid.UUID) (FleetHealthOutput, bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return FleetHealthOutput{}, false, nil
		}
		return FleetHealthOutput{}, false, fmt.Errorf("load app: %w", err)
	}

	// rt#9 — a degraded stateful app (its affinity host is gone) is terminal this
	// cycle: surface it directly rather than reporting the dead replica set.
	if uc.volumes != nil {
		degraded, derr := uc.volumes.IsDegraded(ctx, app.ID)
		if derr != nil {
			logger.FromContext(ctx).Warn("fleet health: degraded lookup", "app_id", app.ID, "err", derr)
		} else if degraded {
			return FleetHealthOutput{
				State:   "degraded",
				Healthy: false,
				Message: "stateful app affinity host unavailable",
			}, true, nil
		}
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return FleetHealthOutput{}, true, fmt.Errorf("list replicas: %w", err)
	}

	healthy := 0
	pending := 0
	url := ""
	for i := range replicas {
		switch replicas[i].State {
		case domain.ReplicaStateResident:
			ok, herr := uc.runtime.RefreshHealth(ctx, &replicas[i])
			if herr != nil {
				logger.FromContext(ctx).Warn("fleet health: refresh", "replica_id", replicas[i].ID, "err", herr)
			}
			if ok {
				healthy++
				if url == "" {
					url = replicas[i].Endpoint
				}
			}
		case domain.ReplicaStateScheduled, domain.ReplicaStateBooting:
			pending++
		}
	}

	out := FleetHealthOutput{URL: url}
	switch {
	case healthy > 0:
		out.State = "running"
		out.Healthy = true
	case pending > 0:
		out.State = "booting"
	default:
		out.State = "failed"
	}
	// rt#8 — the public URL is the stable Caddy-served host when ingress is wired
	// (matches ProvisionApp), not the private resident endpoint.
	if uc.routes != nil {
		out.URL = "https://" + uc.hostForApp(app)
	}
	out.Message = fmt.Sprintf("healthy=%d pending=%d desired=%d replicas=%d", healthy, pending, app.DesiredReplicas, len(replicas))
	return out, true, nil
}

// refuseIfResourceBacked refuses an app-level teardown of an app that a LIVE
// durable resource claim still backs (domain.ErrAppBacksDurableResource).
//
// This closes a data-loss path, not a bookkeeping one: DecommissionApp deletes
// the ext4 BACKING FILES (DeleteAppVolumes) and then the app row, while
// fleet_resources.app_id carries no FK and nothing looked backwards — so a
// dedicated Postgres could be destroyed, with no recovery point, through a verb
// that never knew a resource owned it. The snapshot-first guarantee lives on the
// resource seam (DecommissionDedicated), which is therefore the only way in.
//
// It fails CLOSED on every uncertainty — an unwired ledger and an unreadable one
// both refuse — because "I could not check" must never mean "no claim exists".
// The legitimate resource teardown passes because DecommissionDedicated stamps
// decommissioned_at BEFORE calling down, which takes the claim out of "live";
// there is deliberately no caller-supplied bypass.
func (uc *FleetOrchestrator) refuseIfResourceBacked(ctx context.Context, appID uuid.UUID) error {
	if uc.resources == nil {
		return fmt.Errorf("%w: no resource claim ledger is wired, so app %s cannot be shown to be free of one", domain.ErrAppBacksDurableResource, appID)
	}
	res, err := uc.resources.FindLiveResourceByApp(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrResourceNotFound) {
			return nil // the ordinary case: an app nothing claims
		}
		return fmt.Errorf("look up durable resource claim of app %s: %w", appID, err)
	}
	return fmt.Errorf("%w: app %s backs %s/%s resource %s (phase %s) — decommission the RESOURCE (DecommissionResource), which snapshots first",
		domain.ErrAppBacksDurableResource, appID, res.Class, res.Tier, res.ID, res.Phase)
}

// DecommissionApp scales the app to zero (draining all replicas) and deletes
// the app row. The bool reports whether appID is a known app.
func (uc *FleetOrchestrator) DecommissionApp(ctx context.Context, appID uuid.UUID) (bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load app: %w", err)
	}

	// BEFORE anything destructive — before the drain, before the backing files,
	// before the row. A guard that refuses after the file is unlinked is worthless.
	if err := uc.refuseIfResourceBacked(ctx, app.ID); err != nil {
		return true, err
	}

	app.DesiredReplicas = 0
	app.UpdatedAt = time.Now().UTC()
	if err := uc.apps.Update(ctx, app); err != nil {
		return true, fmt.Errorf("update fleet app: %w", err)
	}
	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return true, err
	}
	// rt#9 — reclaim the on-host ext4 backing files before the app row is deleted
	// (the fleet_apps cascade drops only the fleet_volumes rows, never the backing
	// files → they leak permanently otherwise). Must run while ListByApp still
	// returns the volumes. Only the app-level decommission reclaims; a replica
	// restart (DecommissionReplica) leaves the backing file so data survives.
	if uc.volumes != nil {
		if err := uc.volumes.DeleteAppVolumes(ctx, app.ID); err != nil {
			return true, fmt.Errorf("delete app volumes: %w", err)
		}
	}
	// rt#8 — drop the app's ingress routes so the next SyncIngress stops serving
	// its host (before the app row goes, mirroring the volume reclaim above).
	if uc.routes != nil {
		if err := uc.routes.DeleteByApp(ctx, app.ID); err != nil {
			return true, fmt.Errorf("delete app routes: %w", err)
		}
	}
	if err := uc.apps.Delete(ctx, app.ID); err != nil {
		return true, fmt.Errorf("delete fleet app: %w", err)
	}
	// D-125 — the deployment is gone: revoke the handed Vault token (revoke-self)
	// and stop its renewer. Best-effort (a Vault error is logged inside Revoke; the
	// host still stops bearing the token).
	if uc.tokenStore != nil {
		uc.tokenStore.Revoke(ctx, app.ID)
	}
	// D-124 — the deployment is gone: drop the handed registry pull token so the
	// host stops bearing it. Nil-safe / idempotent.
	if uc.registryTokenStore != nil {
		uc.registryTokenStore.Delete(app.ID)
	}
	return true, nil
}

// OwnerOrgForApp returns the owner org of the app identified by appID. The bool
// reports whether appID is a known app (false → caller falls back to the
// workload path). The by-handle caller-org check (#fleet-handle-ops-org-check,
// D-083) uses this to authorize Health/Decommission/Scale on a leaked handle.
func (uc *FleetOrchestrator) OwnerOrgForApp(ctx context.Context, appID uuid.UUID) (string, bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load app: %w", err)
	}
	return app.OwnerOrg, true, nil
}

// ScaleApp sets the app's desired replica count and reconciles. The bool
// reports whether appID is a known app.
func (uc *FleetOrchestrator) ScaleApp(ctx context.Context, appID uuid.UUID, replicas int) (bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load app: %w", err)
	}

	if replicas < 0 {
		replicas = 0
	}
	// rt#9 — a volume-bearing app is single-writer: reject a scale beyond one.
	if replicas > 1 && uc.volumes != nil {
		hasVol, herr := uc.volumes.HasVolumes(ctx, app.ID)
		if herr != nil {
			return true, fmt.Errorf("has-volumes lookup: %w", herr)
		}
		if hasVol {
			return true, domain.ErrVolumeAppNotScalable
		}
	}
	app.DesiredReplicas = replicas
	app.UpdatedAt = time.Now().UTC()
	if err := uc.apps.Update(ctx, app); err != nil {
		return true, fmt.Errorf("update fleet app: %w", err)
	}
	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return true, err
	}
	return true, nil
}

// surplusOrder ranks occupying replicas for draining: prefer scheduled/booting
// over resident, and within a tier the newest first.
func surplusOrder(occupying []domain.Replica) []domain.Replica {
	ordered := make([]domain.Replica, len(occupying))
	copy(ordered, occupying)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := drainRank(ordered[i].State), drainRank(ordered[j].State)
		if pi != pj {
			return pi < pj
		}
		return ordered[i].CreatedAt.After(ordered[j].CreatedAt)
	})
	return ordered
}

// drainRank orders replica states for draining: pending states drain before
// resident ones.
func drainRank(s domain.ReplicaState) int {
	switch s {
	case domain.ReplicaStateScheduled, domain.ReplicaStateBooting:
		return 0
	default:
		return 1
	}
}
