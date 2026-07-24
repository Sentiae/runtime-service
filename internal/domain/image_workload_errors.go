package domain

import "errors"

// Image-boot workload errors (runtime-fleet CP3).
var (
	// ErrWorkloadNotFound is returned when no image workload matches a handle.
	ErrWorkloadNotFound = errors.New("image workload not found")
	// ErrUnsupportedClass is returned for a workload_class outside {test, resident, job}.
	ErrUnsupportedClass = errors.New("unsupported workload class")
	// ErrSecretsNotSupported is returned when a TEST-class workload sets
	// secret_refs. The resident + job classes resolve + deliver them (P14/I32);
	// the test class has no resolver wired, so it rejects them rather than
	// silently booting a workload without the secrets it declared.
	ErrSecretsNotSupported = errors.New("secret refs are only supported for resident and job workloads")
	// ErrSecretResolverUnavailable is returned when a resident app declares
	// secret_refs but no secret resolver is wired on this host (Vault
	// unreachable at boot). The boot fails closed — a workload never runs
	// without the secrets it declared (I32).
	ErrSecretResolverUnavailable = errors.New("secret resolver unavailable")
	// ErrSecretOwnerOrgMissing is returned when a resident app declares
	// secret_refs but carries no owner org to scope resolution to (D-069/I28).
	// The boot fails closed rather than resolve against an unattested tenant.
	ErrSecretOwnerOrgMissing = errors.New("secret refs require an owner org")
	// ErrImageRefIncomplete is returned when the OCI image reference is missing
	// a registry, repository, or digest.
	ErrImageRefIncomplete = errors.New("image reference incomplete")
	// ErrResidentPortRequired is returned when a resident workload omits the
	// guest port it listens on.
	ErrResidentPortRequired = errors.New("resident workload requires a guest port")
	// ErrImageBootUnavailable is returned when image boot is requested on a host
	// without the firecracker executor (no KVM). The fail-loud booter returns it
	// so an unbootable request never silently succeeds.
	ErrImageBootUnavailable = errors.New("image boot requires the firecracker host")
	// ErrGuestControlUnavailable is returned when a post-boot guest control op
	// (SYNCFS/FREEZE/THAW/SHUTDOWN, D-185a) is requested for a VM that has no
	// control channel: a host without the firecracker executor, or a workload
	// booted without one (every non-resident class, and any resident boot that
	// carried no sealed secret push to deliver the control token in). Failing
	// loud is the point — a silently skipped quiesce reports a consistent
	// snapshot of a filesystem that was never flushed.
	ErrGuestControlUnavailable = errors.New("guest control channel unavailable")
	// ErrScaleNotSupported is returned when Scale is called on a one-shot job
	// handle. A job runs to completion exactly once — there is no replica count
	// to set, so the call is a caller error, not a silent no-op.
	ErrScaleNotSupported = errors.New("scale is not supported for job workloads")
	// ErrTestCommandNotSupported is returned when a JOB-class workload sets
	// test_command. test_command is shell-interpolated (/bin/sh -c); a job's
	// entrypoint override is job_command, which is argv-exact and never passed
	// through a shell. Rejected rather than silently ignored (mirrors the test
	// class's secret_refs/volumes rejections).
	ErrTestCommandNotSupported = errors.New("test_command is not supported for job workloads (use job_command)")
	// ErrJobCommandNotSupported is returned when a non-job workload sets
	// job_command. The argv-exact override is a job-class concept; a test-class
	// caller means test_command.
	ErrJobCommandNotSupported = errors.New("job_command is only supported for job workloads")
	// ErrIdempotencyKeyNotSupported is returned when a non-job workload sets an
	// idempotency_key. At-most-once is a job-class guarantee; accepting the key
	// elsewhere would imply a deduplication that does not happen.
	ErrIdempotencyKeyNotSupported = errors.New("idempotency_key is only supported for job workloads")
	// ErrIdempotencyOwnerOrgMissing is returned when a job supplies an
	// idempotency_key but no owner org. Uniqueness is scoped to (owner_org,
	// idempotency_key) so a key can never resolve across tenants (I28); without
	// an attested org there is no scope to enforce, so it fails closed.
	ErrIdempotencyOwnerOrgMissing = errors.New("idempotency_key requires an owner org")
)
