package domain

import "errors"

// Image-boot workload errors (runtime-fleet CP3).
var (
	// ErrWorkloadNotFound is returned when no image workload matches a handle.
	ErrWorkloadNotFound = errors.New("image workload not found")
	// ErrUnsupportedClass is returned for a workload_class outside {test, resident}.
	ErrUnsupportedClass = errors.New("unsupported workload class")
	// ErrSecretsNotSupported is returned when secret_refs are set — the secrets
	// injection contract (P14) is still open, so CP3 rejects any request that
	// asks for them rather than silently dropping them.
	ErrSecretsNotSupported = errors.New("secret refs not supported yet")
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
