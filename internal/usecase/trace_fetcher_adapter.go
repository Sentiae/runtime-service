package usecase

import (
	"context"

	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
)

// opsTraceFetcherAdapter wires foundry.OpsTraceFetcher into the
// TraceFetcher interface owned by the usecase package. Living here
// (rather than in foundry) keeps the foundry package free of usecase
// imports while still letting the DI container inject a real fetcher.
type opsTraceFetcherAdapter struct {
	inner *foundry.OpsTraceFetcher
}

// NewOpsTraceFetcherAdapter wraps the foundry-side fetcher.
func NewOpsTraceFetcherAdapter(inner *foundry.OpsTraceFetcher) TraceFetcher {
	return &opsTraceFetcherAdapter{inner: inner}
}

// FetchTrace satisfies TraceFetcher.
func (a *opsTraceFetcherAdapter) FetchTrace(ctx context.Context, traceID, serviceID string) (*ProductionTrace, error) {
	if a.inner == nil {
		return nil, nil
	}
	t, err := a.inner.Fetch(ctx, traceID, serviceID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, nil
	}
	return &ProductionTrace{
		TraceID:        t.TraceID,
		ServiceID:      t.ServiceID,
		OrganizationID: t.OrganizationID,
		HTTPMethod:     t.HTTPMethod,
		HTTPPath:       t.HTTPPath,
		RequestBody:    t.RequestBody,
		ResponseStatus: t.ResponseStatus,
		ResponseBody:   t.ResponseBody,
		DBQueries:      t.DBQueries,
		SideEffects:    t.SideEffects,
		Headers:        t.Headers,
		Language:       t.Language,
		Framework:      t.Framework,
	}, nil
}
