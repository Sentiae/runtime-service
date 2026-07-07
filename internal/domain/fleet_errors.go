package domain

import "errors"

// Durable fleet control-plane errors (runtime-fleet CP4).
var (
	// ErrFleetHostNotFound is returned when no fleet host matches an id.
	// (domain.ErrHostNotFound already names the scheduler-stub host aggregate.)
	ErrFleetHostNotFound = errors.New("fleet host not found")
	// ErrFleetAppNotFound is returned when no fleet app matches an id or component+env.
	ErrFleetAppNotFound = errors.New("fleet app not found")
	// ErrReplicaNotFound is returned when no replica matches an id.
	ErrReplicaNotFound = errors.New("fleet replica not found")
	// ErrPlacementNotFound is returned when no placement matches a replica.
	ErrPlacementNotFound = errors.New("fleet placement not found")
	// ErrRouteNotFound is returned when no route matches an id.
	ErrRouteNotFound = errors.New("fleet route not found")
	// ErrVolumeNotFound is returned when no volume matches an id.
	ErrVolumeNotFound = errors.New("fleet volume not found")
	// ErrSecretBindingNotFound is returned when no secret binding matches an id.
	ErrSecretBindingNotFound = errors.New("fleet secret binding not found")
)
