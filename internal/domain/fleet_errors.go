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
	// ErrReplicaNotFound is returned when no replica matches an id.
	ErrReplicaNotFound = errors.New("fleet replica not found")
	// ErrPlacementNotFound is returned when no placement matches a replica.
	ErrPlacementNotFound = errors.New("fleet placement not found")
	// ErrRouteNotFound is returned when no route matches an id.
	ErrRouteNotFound = errors.New("fleet route not found")
	// ErrVolumeNotFound is returned when no volume matches an id.
	ErrVolumeNotFound = errors.New("fleet volume not found")
	// ErrInvalidHostHealth is returned when a heartbeat reports a health value
	// the fleet does not recognize (see HostHealth.IsValid).
	ErrInvalidHostHealth = errors.New("invalid fleet host health")
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
)
