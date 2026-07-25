// Package app holds runtime-service's process-level wiring that is neither a
// handler nor a use case. Today that is exactly one thing: the platform error
// registry binding (CLAUDE.md §16.3).
//
// This service predates the constitution and keeps its lifecycle in
// cmd/server/main.go rather than an app.Server (see the service CLAUDE.md), so
// RegisterErrors is EXPORTED and called from the bootstrap — where the five
// sibling services that already have an internal/app (delivery, node,
// knowledge, composition, codegen) call an unexported registerErrors() from
// their app/server.go instead. Same file, same purpose, one caller difference
// forced by the missing server.go.
package app

import (
	"net/http"

	pkerrors "github.com/sentiae/platform-kit/errors"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"

	"google.golang.org/grpc/codes"
)

// RegisterErrors maps the SecretResolver (P14) sentinels to HTTP/gRPC codes for
// boundary translation (CLAUDE.md §16). Called once at bootstrap, before the
// gRPC server serves.
//
// Why platform-kit sentinels and not this service's own domain errors: the
// secret seam's failures arrive from platform-kit/secret, cross runtime's
// usecase layer under %w, and surface through fleetError. Until this
// registration existed, NONE of them were mapped, so every one collapsed to
// codes.Internal + "internal server error" — a genuine CROSS-TENANT DENIAL was
// indistinguishable on the wire from a nil-pointer panic
// (#secret-resolve-no-sentinel, D-162b). Registering a library's sentinels at a
// consuming service's boundary is the established house pattern:
// delivery-service does exactly this for platform-kit/tenantdb's RLS sentinels.
//
// The runtime-local fleet domain errors deliberately stay in fleetError's
// hand-map: they predate this registry and their curated, non-leaky messages
// are load-bearing. fleetError consults this registry only for what it does not
// hand-map, so these mappings have ONE source of truth.
func RegisterErrors() {
	// I28 tenancy denial. A caller asking for another org's secret is refused
	// structurally, before any Vault call. It is an AUTHORIZATION failure and
	// MUST NOT be Internal — an operator reading a 403/PermissionDenied in the
	// evidence trail is seeing the tenancy boundary hold, whereas Internal is
	// indistinguishable from a crash. Mirrors delivery-service's mapping of
	// tenantdb.ErrNoActiveOrg / ErrOrgNotAuthorized (the identical semantic).
	pkerrors.Register(secret.ErrCrossTenantSecret, http.StatusForbidden, codes.PermissionDenied)

	// Caller input faults. ErrUnscopedSecretRef covers BOTH a structurally
	// malformed ref (no "#<field>") and a well-formed but non-tenant-namespaced
	// one: authorizeRef returns it for either, since a ref it cannot attribute to
	// an org is refused the same way regardless of why.
	pkerrors.Register(secret.ErrUnscopedSecretRef, http.StatusBadRequest, codes.InvalidArgument)

	// A miss is only ever reported to the ref's OWNING tenant: authorizeRef
	// denies a foreign caller (PermissionDenied, above) before any Vault call,
	// so NotFound cannot be used as a cross-tenant existence oracle.
	pkerrors.Register(secret.ErrSecretNotFound, http.StatusNotFound, codes.NotFound)

	// Resolver-cannot-operate faults (no Vault client; no per-deployment token
	// handed in by the control plane, D-125). These are host/wiring faults, not
	// caller-input faults, and they fail closed — the VM never boots. Mapped to
	// the same 503/FailedPrecondition the nearest existing sentinels already
	// use: runtime's own domain.ErrSecretResolverUnavailable (FailedPrecondition
	// in fleetError) and delivery's ErrApprovalGateUnavailable /
	// ErrSecurityGateUnavailable (503 + FailedPrecondition).
	pkerrors.Register(secret.ErrVaultUnavailable, http.StatusServiceUnavailable, codes.FailedPrecondition)
	pkerrors.Register(secret.ErrNoHandedToken, http.StatusServiceUnavailable, codes.FailedPrecondition)

	// The fleet-app tenancy guard (#two-orgs-same-claim-key-share-one-database). A
	// provision without an owner org is a CALLER-INPUT fault, not a host fault: the
	// app row is the tenancy boundary for fleet_apps (no RLS there) and an org-less
	// row is unscoped, so it is refused before anything is written. fleetError
	// hand-maps it to the same codes.InvalidArgument with a caller-facing message;
	// this registration covers the paths that translate through the registry.
	pkerrors.Register(domain.ErrFleetAppOwnerOrgRequired, http.StatusBadRequest, codes.InvalidArgument)
}
