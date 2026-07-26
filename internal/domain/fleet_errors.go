package domain

import "errors"

// Durable fleet control-plane errors (runtime-fleet CP4).
var (
	// ErrFleetHostNotFound is returned when no fleet host matches an id.
	// (domain.ErrHostNotFound already names the scheduler-stub host aggregate.)
	ErrFleetHostNotFound = errors.New("fleet host not found")
	// ErrFleetAppNotFound is returned when no fleet app matches an id or component+env.
	ErrFleetAppNotFound = errors.New("fleet app not found")
	// ErrFleetAppOwnerOrgRequired is returned when an app provision carries no
	// owner org. The app row IS the tenancy boundary for fleet_apps — there is no
	// RLS on this table (migrations/0012_create_fleet_resources.up.sql: "owner_org
	// is a column, not a policy") — and the row also carries the secret refs the
	// replica runtime resolves under whatever org the row happens to hold. An
	// org-less row is therefore not a benign default, it is an unscoped row.
	ErrFleetAppOwnerOrgRequired = errors.New("fleet app requires an owner org")
	// ErrAppBacksDurableResource is returned when an app-level decommission is
	// asked to tear down the backing app of a LIVE durable resource claim
	// (fleet_resources). The app seam knows nothing about resources: it drains the
	// replicas, DELETES the ext4 backing files (DeleteAppVolumes) and drops the app
	// row, so a customer's dedicated database would be destroyed — with no
	// snapshot — through a verb that never consulted the claim. The
	// snapshot-first guarantee lives on the RESOURCE seam
	// (DecommissionDedicated), so the resource is the only legitimate way in; this
	// sentinel sends the caller there. Only a claim that is still live blocks:
	// a resource already tombstoned (decommissioned_at stamped) is mid- or
	// post-teardown and its own path is what is calling through.
	ErrAppBacksDurableResource = errors.New("fleet app backs a durable resource")
	// ErrReplicaNotFound is returned when no replica matches an id.
	ErrReplicaNotFound = errors.New("fleet replica not found")
	// ErrPlacementNotFound is returned when no placement matches a replica.
	ErrPlacementNotFound = errors.New("fleet placement not found")
	// ErrRouteNotFound is returned when no route matches an id.
	ErrRouteNotFound = errors.New("fleet route not found")
	// ErrIngressHostTaken is returned when an app's DERIVED ingress host is already
	// routed to a different app (the unique index on fleet_routes.host_pattern,
	// migrations/0006). The host is a pure function of (component_id, env), so a
	// component id must be globally unique across the fleet — two apps cannot share
	// one. Untranslated this was a bare Internal AND a permanent wedge: the
	// fleet_apps row is committed before the route insert, so every retry re-found
	// the app and re-failed on the same insert with no diagnosable cause.
	ErrIngressHostTaken = errors.New("ingress host already routed to another fleet app")
	// ErrVolumeNotFound is returned when no volume matches an id.
	ErrVolumeNotFound = errors.New("fleet volume not found")
	// ErrInvalidHostHealth is returned when a heartbeat reports a health value
	// the fleet does not recognize (see HostHealth.IsValid).
	ErrInvalidHostHealth = errors.New("invalid fleet host health")
	// ErrHostCapacityUnmeasured is returned when a host cannot read its own
	// physical capacity (cpu/memory/disk). Registration is refused rather than
	// falling back to a code default: an unmeasured host that advertises a
	// plausible-looking number is how the fleet came to believe a 40GB machine had
	// 50GB, and the scheduler places customer databases on that belief.
	ErrHostCapacityUnmeasured = errors.New("fleet host capacity could not be measured")
	// ErrHostCapacityOverAdvertised is returned when a CONFIGURED capacity exceeds
	// the measured one. Under-advertising is a legitimate reservation;
	// over-advertising is a claim on resources the machine does not have, and for
	// disk it is the claim that makes a host accept a volume it cannot materialize.
	ErrHostCapacityOverAdvertised = errors.New("configured fleet host capacity exceeds measured capacity")
	// ErrHostDiskReserveInvalid is returned when the disk headroom reserve leaves
	// no advertisable disk at all, or is negative (which would ADD capacity).
	ErrHostDiskReserveInvalid = errors.New("invalid fleet host disk reserve")

	// ── Host placement facts (SentiaeDB standard-ha slice 0, D-196) ──────────
	// Both are REFUSALS at registration, not defaults. A default here would be a
	// fail-open on the two facts the HA placement invariant is made of, and a host
	// that registered without them cannot be corrected retroactively: the values
	// describe a building and a network, and only a human knows them.

	// ErrHostFailureDomainRequired is returned when a host registers without a
	// failure domain (see FailureDomain). "Different host" on one chassis is not a
	// different failure domain, and only a human-supplied fact can say which it is.
	ErrHostFailureDomainRequired = errors.New("fleet host failure domain required")
	// ErrHostFailureDomainInvalid is returned when a supplied failure domain is not
	// the frozen site/power/network encoding. Refused rather than stored as-is: a
	// bare label would compare unequal to every other label and thereby satisfy the
	// anti-affinity invariant vacuously — two machines on one breaker would read as
	// two power domains.
	ErrHostFailureDomainInvalid = errors.New("fleet host failure domain invalid")
	// ErrHostRegionRequired is returned when a host registers with no region. The
	// HA invariant is different-domain AND SAME-REGION (D-196 amendment 2), and two
	// empty regions compare EQUAL — so an unlabelled host would satisfy the
	// same-region half vacuously while D-190 has already baked a region into the
	// customer's permanent hostname.
	ErrHostRegionRequired = errors.New("fleet host region required")

	// ── The standard-ha placement invariant (design §5.1, slice 1) ───────────
	// Four sentinels rather than one because the operator action differs
	// completely, and "HA unavailable" would send someone shopping for hardware
	// they may already own.

	// ErrHAHostsInsufficient — fewer than two hosts that could hold a member exist
	// at all. This is the true state of the fleet today: one machine is one failure
	// domain, so standard-ha is unprovisionable and is REFUSED rather than
	// simulated (D-196: "until then standard-ha is refused, never simulated").
	ErrHAHostsInsufficient = errors.New("standard-ha requires two live hosts in different failure domains")
	// ErrHAFailureDomainUnattested — enough live hosts exist, but fewer than two of
	// them have stated a failure domain (or a region), so the fleet cannot prove
	// they would not die together. Config, not hardware.
	ErrHAFailureDomainUnattested = errors.New("standard-ha requires two live hosts with attested failure domains")
	// ErrHAFailureDomainShared — every candidate host is in the SAME failure
	// domain. A second host is not a second domain: this is the negative control
	// the whole tier rests on, because a pair inside one chassis would look `ha`,
	// `ready`, two members and two hosts, while surviving nothing.
	ErrHAFailureDomainShared = errors.New("standard-ha requires two live hosts in different failure domains (all candidates share one)")
	// ErrHARegionSplit — different failure domains exist, but no two of them are in
	// the same region. The standby must be same-region BY CONSTRUCTION: D-190 puts
	// the region inside the permanent hostname a customer has already pasted into
	// an application, so a cross-region standby would serve a name that names the
	// wrong place, permanently.
	ErrHARegionSplit = errors.New("standard-ha requires the two failure domains to be in the same region")
	// ErrHAAvailabilityClassInvalid — a claim named an availability class the fleet
	// does not recognize. Refused rather than coerced to 'single': silently
	// downgrading a claim would hand back a resource weaker than the one asked for,
	// with nothing anywhere saying so.
	ErrHAAvailabilityClassInvalid = errors.New("fleet resource availability class invalid")
	// ErrHAPlacementUnknowable — the provisioner has no way to read the live host
	// inventory, so it cannot prove the invariant. Fail closed: an unprovable
	// invariant is an unmet one.
	ErrHAPlacementUnknowable = errors.New("standard-ha placement cannot be evaluated on this host")
	// ErrNoSchedulableHost is returned when the scheduler finds no live host
	// that satisfies a placement request's resource + constraint filters.
	ErrNoSchedulableHost = errors.New("no schedulable host")
	// ErrVolumeBackendUnavailable is returned when the volume backing-file backend
	// is not available (non-firecracker host) so a volume is never silently faked.
	ErrVolumeBackendUnavailable = errors.New("volume backend unavailable")
	// ErrVolumeAppNotScalable is returned when a volume-bearing app is asked to run
	// more than one replica — a persistent volume is single-writer this cycle.
	ErrVolumeAppNotScalable = errors.New("volume-bearing app cannot scale beyond one replica")
	// ErrVolumeRestoreInProgress is returned when a boot is refused because the
	// app's data volume is being restored in place (D-184). The stand-off is the
	// point: a VM booted here would hold an fd to the OLD inode while the restore
	// renames a new backing file onto the path — silent wrong state.
	ErrVolumeRestoreInProgress = errors.New("volume restore in progress")
	// ErrVolumesNotSupported is returned when a workload class that has no volume
	// path (the test class) is provisioned with volumes.
	ErrVolumesNotSupported = errors.New("volumes are only supported for resident workloads")
	// ErrStatefulHostUnavailable is a non-fatal signal that a stateful app's
	// affinity host is dead/stale: the app is degraded rather than moved off its
	// data (no cross-host restore this cycle).
	ErrStatefulHostUnavailable = errors.New("stateful app affinity host unavailable")
	// ErrActivationTimeout is returned when the activator (scale-to-zero wake path,
	// rt#11) does not observe a healthy resident replica within its budget. It maps
	// to a retryable 503 so the caller retries rather than the request being dropped.
	ErrActivationTimeout = errors.New("fleet activation timed out")
	// ErrAnonymousWakeRefused is returned when the scale-to-zero activator (rt#11)
	// is asked to wake an app it cannot PROVE is a plain scale-to-zero HTTP
	// workload. The wake path is reached without authentication — its only caller
	// is the co-located gateway, and the app is selected by a caller-supplied
	// hostname — so it may only ever boot the ONE class of workload for which
	// "a request arrived" is the whole authority needed. Everything else
	// (a data-engine app above all: booting one is a durability-relevant
	// transition over customer data) belongs behind an authenticated seam. The
	// refusal is PERMANENT, not retryable: the caller must be told no, not told
	// to try again.
	ErrAnonymousWakeRefused = errors.New("anonymous wake refused: app is not a plain scale-to-zero HTTP workload")
	// ErrPauseUnsafeForResidentVM is returned when a resident (data-bearing) VM is
	// handed to a component that can PAUSE it. Firecracker v1.16.0's vsock does not
	// survive Pause/Resume (#fc-vsock-dies-on-pause-resume, proven live): after one
	// pause the guest control channel is dead for the VM's whole lifetime, which
	// takes quiesced snapshots, clean shutdown and park with it — i.e. every
	// durability guarantee a database VM exists to provide.
	ErrPauseUnsafeForResidentVM = errors.New("resident VM may not be registered with a component that pauses it")
	// ErrVMClassUndeclared is returned when a VM is handed to a pausing component
	// without declaring its class. The declaration is MANDATORY on purpose: an
	// undeclared VM is refused, so a future caller cannot wire a resident VM into a
	// pause path by simply not setting a flag.
	ErrVMClassUndeclared = errors.New("VM class must be declared before registering with a component that pauses it")

	// ── microVM addressing plane (fleet_net_leases, see fleet_net_lease.go) ──
	// Every one of these REFUSES a boot. That direction is the whole design: an
	// address/uid/chroot handed out twice is cross-tenant access to customer data,
	// so an allocation the plane cannot prove is unique is not made at all.

	// ErrNetCoordinateOutOfRange is returned when a host ordinal, local slot or
	// derived net index falls outside the plane's fences. It is a refusal rather
	// than a clamp: a clamped coordinate lands on a /30 another live VM holds.
	ErrNetCoordinateOutOfRange = errors.New("microVM net coordinate out of range")
	// ErrNetLeaseExhausted is returned when this host has no free local slot. The
	// allocator refuses the boot rather than wrapping around, because wrapping
	// means handing a running VM's uid and chroot to a second tenant.
	ErrNetLeaseExhausted = errors.New("microVM net lease slots exhausted on this host")
	// ErrNetLeaseConflict is returned when a lease INSERT lost a race on one of
	// the unique fences and the allocator exhausted its retries. The conflict is
	// the fence working — the loser must not boot.
	ErrNetLeaseConflict = errors.New("microVM net lease conflict")
	// ErrNetLeaseNotFound is returned when no lease matches an owner.
	ErrNetLeaseNotFound = errors.New("microVM net lease not found")
	// ErrHostNetOrdinalUnset is returned when this host has no assigned
	// net_ordinal. Without it there is no /30 block to allocate from, and
	// defaulting to 0 would collide with whichever host legitimately owns it.
	ErrHostNetOrdinalUnset = errors.New("fleet host has no assigned net ordinal")
	// ErrNetOrdinalExhausted is returned when every host ordinal is taken. The
	// fleet cannot admit a further host at this stride without a re-split.
	ErrNetOrdinalExhausted = errors.New("fleet host net ordinals exhausted")
	// ErrNetPlaneUnreconciled is returned by every boot on a host whose
	// addressing plane could not be reconciled at startup. A host that cannot
	// prove which addresses are held must not hand any out.
	ErrNetPlaneUnreconciled = errors.New("microVM net addressing plane is unreconciled on this host")
)
