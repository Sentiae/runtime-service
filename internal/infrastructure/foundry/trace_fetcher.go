package foundry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// OpsTraceFetcher pulls a captured trace from ops-service. It implements
// usecase.TraceFetcher without importing the usecase package (to avoid
// an import cycle); the usecase code is structurally typed against the
// TraceFetcher interface and ProductionTrace value.
type OpsTraceFetcher struct {
	baseURL string
	client  *http.Client
}

// NewOpsTraceFetcher builds the fetcher. baseURL is the ops-service
// origin (e.g. http://ops-service:8080). If empty, the resulting fetcher
// is returned anyway and will fail at fetch time so the misconfiguration
// is loud.
func NewOpsTraceFetcher(baseURL string, timeout time.Duration) *OpsTraceFetcher {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &OpsTraceFetcher{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
	}
}

// FetchedTrace mirrors the structure that usecase.ProductionTrace
// expects. The fields and JSON tags match exactly.
type FetchedTrace struct {
	TraceID        string            `json:"trace_id"`
	ServiceID      string            `json:"service_id"`
	OrganizationID uuid.UUID         `json:"organization_id"`
	HTTPMethod     string            `json:"http_method,omitempty"`
	HTTPPath       string            `json:"http_path,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`
	ResponseStatus int               `json:"response_status,omitempty"`
	ResponseBody   string            `json:"response_body,omitempty"`
	DBQueries      []string          `json:"db_queries,omitempty"`
	SideEffects    []string          `json:"side_effects,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Language       string            `json:"language,omitempty"`
	Framework      string            `json:"framework,omitempty"`
}

// Fetch returns the trace as JSON-decoded into FetchedTrace. The usecase
// layer maps it into its own ProductionTrace.
func (f *OpsTraceFetcher) Fetch(ctx context.Context, traceID, serviceID string) (*FetchedTrace, error) {
	if f.baseURL == "" {
		return nil, fmt.Errorf("ops trace fetcher: base URL not configured")
	}
	if traceID == "" {
		return nil, fmt.Errorf("ops trace fetcher: trace_id required")
	}
	endpoint := fmt.Sprintf("%s/api/v1/ops/traces/%s", f.baseURL, url.PathEscape(traceID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if serviceID != "" {
		req.Header.Set("X-Service-ID", serviceID)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch trace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ops returned %s: %s", resp.Status, string(body))
	}
	var envelope struct {
		Data FetchedTrace `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode trace: %w", err)
	}
	return &envelope.Data, nil
}
