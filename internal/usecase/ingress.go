package usecase

import "context"

// IngressRoute is one desired host→upstreams mapping the ingress gateway must
// serve. Host is the platform-issued hostname (<slug>-<env>.<ingress-domain>);
// CustomDomain, when set, is an additional owner-supplied host for the same app.
// Upstreams are "ip:port" dial targets (the resident replicas). A route with no
// upstreams is a placement that has no live replica yet.
type IngressRoute struct {
	Host         string
	CustomDomain string
	Upstreams    []string
}

// IngressSyncer applies the full desired ingress route set to the ingress
// gateway. Implementations replace the entire config atomically (idempotent,
// self-healing) so the reconciler can push the current desired state each tick.
type IngressSyncer interface {
	Sync(ctx context.Context, routes []IngressRoute) error
}
