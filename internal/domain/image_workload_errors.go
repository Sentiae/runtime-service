package domain

import "errors"

// Image-boot workload errors (runtime-fleet CP3).
var (
	// ErrWorkloadNotFound is returned when no image workload matches a handle.
	ErrWorkloadNotFound = errors.New("image workload not found")
	// ErrUnsupportedClass is returned for a workload_class outside {test, resident}.
	ErrUnsupportedClass = errors.New("unsupported workload class")
	// ErrSecretsNotSupported is returned when a TEST-class workload sets
	// secret_refs. The resident class resolves + delivers them (P14/I32); the
	// test class has no resolver wired, so it rejects them rather than silently
	// booting a workload without the secrets it declared.
	ErrSecretsNotSupported = errors.New("secret refs are only supported for resident workloads")
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
)
