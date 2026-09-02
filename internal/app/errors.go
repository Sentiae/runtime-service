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
// The division of labour, and it is deliberate (§32 deviation, recorded in the
// service CLAUDE.md): the REGISTRY owns errors whose meaning is defined
// ELSEWHERE — platform-kit library sentinels that would otherwise collapse to
// Internal — while fleetError's HAND-MAP owns the errors this service defines,
// whose curated non-leaky messages are load-bearing (pkerrors.ToGRPC's default
// echoes raw err.Error(), which would put DB and Vault text on the wire).
// fleetError consults this registry only in its default branch, i.e. only for what
// it does not hand-map, so every mapping has ONE source of truth — and registering
// a sentinel that fleetError already hand-maps would be provably DEAD code (the
// switch case always matches first) while looking like an enforced control. Do not
// add the fleet domain sentinels here.
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

	// Phase 4 node-runner sentinels. These ARE runtime's own domain errors, so
	// they look like the exception the note above forbids — they are not.
	// fleetError hand-maps only what the FLEET RPCs return; the graph RPCs go
	// through pkerrors.ToGRPC, which reads this registry and nothing else, so
	// without these registrations every Phase 4 refusal would reach the caller
	// as Internal and be indistinguishable from a crash. Each mapping states
	// whose fault the refusal is:
	//
	//   FailedPrecondition — the graph or the host is not in a state where the
	//   call can proceed (a legacy row, a missing secret, an unconfigured
	//   runner). Retrying the same call unchanged cannot help.
	pkerrors.Register(domain.ErrLegacyGraph, http.StatusPreconditionFailed, codes.FailedPrecondition)
	pkerrors.Register(domain.ErrSecretTokenRequired, http.StatusPreconditionFailed, codes.FailedPrecondition)
	pkerrors.Register(domain.ErrRequiredSecretAbsent, http.StatusPreconditionFailed, codes.FailedPrecondition)
	pkerrors.Register(domain.ErrGraphDebugRetired, http.StatusPreconditionFailed, codes.FailedPrecondition)
	pkerrors.Register(domain.ErrNodeRunnerNotReady, http.StatusPreconditionFailed, codes.FailedPrecondition)

	//   InvalidArgument — the request itself is wrong: a retired field, a
	//   malformed ref, a plan that does not reconstitute. The caller must
	//   change what it sends.
	pkerrors.Register(domain.ErrNodeRefRequired, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrLegacyNodeInput, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrInvalidNodeRef, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrInvalidPortSpec, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrInvalidSecretSpec, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrInvalidRole, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrInvalidEgressPattern, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrPlanInvalid, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrSeededOutputsRetired, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrSecretTokenUnexpected, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrTriggerInputInvalid, http.StatusBadRequest, codes.InvalidArgument)
	pkerrors.Register(domain.ErrNodeConfigInvalid, http.StatusBadRequest, codes.InvalidArgument)

	//   Unavailable — the registry did not answer. Retrying may succeed.
	pkerrors.Register(domain.ErrBundlePullFailed, http.StatusServiceUnavailable, codes.Unavailable)

	//   ResourceExhausted — every invocation subnet is in use. Retry later.
	pkerrors.Register(domain.ErrNodeRunnerBusy, http.StatusTooManyRequests, codes.ResourceExhausted)
}
