package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// RuntimeAgentHandler exposes:
//
//	POST /agents                 — register a new agent, returns plaintext token
//	POST /agents/{id}/heartbeat  — auth via bearer token, marks agent online
//	GET  /agents                 — list org's agents
//	GET  /agents/{id}            — fetch details
//	PUT  /agents/policy          — upsert org routing policy
//	GET  /agents/policy          — read org policy
//
// Token authentication for heartbeat is handled here (rather than via
// standard JWT middleware) because the agent-to-runtime flow doesn't
// carry a user identity — only the agent secret.
type RuntimeAgentHandler struct {
	registry *usecase.AgentRegistry
	repo     *postgres.RuntimeAgentRepository
}

func NewRuntimeAgentHandler(registry *usecase.AgentRegistry, repo *postgres.RuntimeAgentRepository) *RuntimeAgentHandler {
	return &RuntimeAgentHandler{registry: registry, repo: repo}
}

func (h *RuntimeAgentHandler) RegisterRoutes(r chi.Router) {
	r.Route("/agents", func(r chi.Router) {
		r.Get("/", h.List)
		r.Post("/", h.Register)
		r.Get("/policy", h.GetPolicy)
		r.Put("/policy", h.UpsertPolicy)
		r.Get("/{id}", h.Get)
		r.Post("/{id}/heartbeat", h.Heartbeat)
	})
}

type registerAgentRequest struct {
	Name         string            `json:"name"`
	Endpoint     string            `json:"endpoint"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func (h *RuntimeAgentHandler) Register(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}
	var req registerAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}
	out, err := h.registry.Register(r.Context(), usecase.RegisterInput{
		OrganizationID: orgID,
		Name:           req.Name,
		Endpoint:       req.Endpoint,
		Capabilities:   req.Capabilities,
		Labels:         req.Labels,
	})
	if err != nil {
		RespondBadRequest(w, err.Error(), nil)
		return
	}
	RespondCreated(w, map[string]any{
		"agent": out.Agent,
		"token": out.Token, // plaintext — shown once
	})
}

type heartbeatRequest struct {
	Fingerprint string `json:"fingerprint,omitempty"`
}

func (h *RuntimeAgentHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid agent id", nil)
		return
	}
	token := extractBearerToken(r)
	if token == "" {
		RespondUnauthorized(w, "Missing bearer token")
		return
	}
	var req heartbeatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.registry.Heartbeat(r.Context(), id, token, req.Fingerprint); err != nil {
		RespondUnauthorized(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RuntimeAgentHandler) List(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}
	rows, err := h.repo.ListByOrg(r.Context(), orgID)
	if err != nil {
		RespondInternalError(w, "Failed to list agents")
		return
	}
	RespondSuccess(w, rows)
}

func (h *RuntimeAgentHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid agent id", nil)
		return
	}
	a, err := h.repo.Get(r.Context(), id)
	if err != nil {
		RespondInternalError(w, "Failed to load agent")
		return
	}
	if a == nil {
		RespondNotFound(w, "Agent not found")
		return
	}
	RespondSuccess(w, a)
}

type policyRequest struct {
	PreferredAgentID *uuid.UUID        `json:"preferred_agent_id,omitempty"`
	FallbackToLocal  bool              `json:"fallback_to_local"`
	LabelSelectors   map[string]string `json:"label_selectors,omitempty"`
}

func (h *RuntimeAgentHandler) UpsertPolicy(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}
	var req policyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}
	p := &domain.AgentRoutingPolicy{
		OrganizationID:   orgID,
		PreferredAgentID: req.PreferredAgentID,
		FallbackToLocal:  req.FallbackToLocal,
		LabelSelectors:   req.LabelSelectors,
	}
	if err := h.repo.UpsertPolicy(r.Context(), p); err != nil {
		RespondInternalError(w, "Failed to save policy")
		return
	}
	RespondSuccess(w, p)
}

func (h *RuntimeAgentHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}
	p, err := h.repo.GetPolicy(r.Context(), orgID)
	if err != nil {
		RespondInternalError(w, "Failed to load policy")
		return
	}
	if p == nil {
		RespondSuccess(w, map[string]any{"policy": nil})
		return
	}
	RespondSuccess(w, p)
}

// extractBearerToken pulls the token from an Authorization header. We
// don't use the shared AuthMiddleware here because agents authenticate
// with their own registered secret, not a user JWT.
func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
