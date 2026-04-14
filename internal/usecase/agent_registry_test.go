package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

type memRepo struct {
	agents   map[uuid.UUID]*domain.RuntimeAgent
	policies map[uuid.UUID]*domain.AgentRoutingPolicy
}

func newMemRepo() *memRepo {
	return &memRepo{
		agents:   map[uuid.UUID]*domain.RuntimeAgent{},
		policies: map[uuid.UUID]*domain.AgentRoutingPolicy{},
	}
}

func (r *memRepo) Create(_ context.Context, a *domain.RuntimeAgent) error {
	r.agents[a.ID] = a
	return nil
}
func (r *memRepo) Update(_ context.Context, a *domain.RuntimeAgent) error {
	r.agents[a.ID] = a
	return nil
}
func (r *memRepo) Get(_ context.Context, id uuid.UUID) (*domain.RuntimeAgent, error) {
	return r.agents[id], nil
}
func (r *memRepo) GetPolicy(_ context.Context, orgID uuid.UUID) (*domain.AgentRoutingPolicy, error) {
	return r.policies[orgID], nil
}

type fakeRemote struct {
	called bool
}

func (f *fakeRemote) Dispatch(context.Context, *domain.RuntimeAgent, ExecutionPayload) (ExecutionResult, error) {
	f.called = true
	return ExecutionResult{ExitCode: 0, Stdout: "remote"}, nil
}

type fakeLocal struct{ called bool }

func (f *fakeLocal) Execute(context.Context, ExecutionPayload) (ExecutionResult, error) {
	f.called = true
	return ExecutionResult{ExitCode: 0, Stdout: "local"}, nil
}

func TestRegisterAndHeartbeat(t *testing.T) {
	repo := newMemRepo()
	reg := NewAgentRegistry(repo)

	out, err := reg.Register(context.Background(), RegisterInput{
		OrganizationID: uuid.New(),
		Name:           "vpc-1",
		Endpoint:       "grpcs://agent.example:8443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("expected plaintext token on registration output")
	}
	if out.Agent.Status != domain.AgentStatusPending {
		t.Fatalf("status=%s, want pending", out.Agent.Status)
	}

	// Wrong token must fail and NOT flip status to online.
	if err := reg.Heartbeat(context.Background(), out.Agent.ID, "wrong", "fp-1"); err == nil {
		t.Fatal("expected auth failure for wrong token")
	}
	if repo.agents[out.Agent.ID].Status == domain.AgentStatusOnline {
		t.Fatal("bad token must not mark online")
	}

	// Right token flips to online and pins fingerprint.
	if err := reg.Heartbeat(context.Background(), out.Agent.ID, out.Token, "fp-1"); err != nil {
		t.Fatal(err)
	}
	a := repo.agents[out.Agent.ID]
	if a.Status != domain.AgentStatusOnline {
		t.Fatalf("status=%s, want online", a.Status)
	}
	if a.Fingerprint != "fp-1" {
		t.Fatalf("fingerprint=%q, want fp-1", a.Fingerprint)
	}

	// Fingerprint mismatch after pinning must be refused.
	if err := reg.Heartbeat(context.Background(), out.Agent.ID, out.Token, "fp-2"); err == nil {
		t.Fatal("expected fingerprint mismatch to fail")
	}
}

func TestDispatcher_RoutesToRemoteWhenPolicySet(t *testing.T) {
	repo := newMemRepo()
	reg := NewAgentRegistry(repo)
	orgID := uuid.New()

	out, _ := reg.Register(context.Background(), RegisterInput{OrganizationID: orgID, Endpoint: "x"})
	_ = reg.Heartbeat(context.Background(), out.Agent.ID, out.Token, "fp")

	repo.policies[orgID] = &domain.AgentRoutingPolicy{
		OrganizationID:   orgID,
		PreferredAgentID: &out.Agent.ID,
		FallbackToLocal:  true,
	}

	remote := &fakeRemote{}
	local := &fakeLocal{}
	disp := NewDispatcher(reg, remote, local)

	r, err := disp.Execute(context.Background(), orgID, ExecutionPayload{Language: "go", Code: "..."})
	if err != nil {
		t.Fatal(err)
	}
	if !remote.called || local.called {
		t.Fatalf("expected remote dispatch, got remote=%v local=%v", remote.called, local.called)
	}
	if r.Stdout != "remote" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestDispatcher_FallsBackToLocalWhenAgentStale(t *testing.T) {
	repo := newMemRepo()
	reg := NewAgentRegistry(repo)
	orgID := uuid.New()

	out, _ := reg.Register(context.Background(), RegisterInput{OrganizationID: orgID, Endpoint: "x"})
	_ = reg.Heartbeat(context.Background(), out.Agent.ID, out.Token, "fp")

	// Age the last_seen so the agent looks stale.
	oldSeen := time.Now().Add(-10 * time.Minute)
	repo.agents[out.Agent.ID].LastSeenAt = &oldSeen

	repo.policies[orgID] = &domain.AgentRoutingPolicy{
		OrganizationID:   orgID,
		PreferredAgentID: &out.Agent.ID,
		FallbackToLocal:  true,
	}

	remote := &fakeRemote{}
	local := &fakeLocal{}
	disp := NewDispatcher(reg, remote, local)

	if _, err := disp.Execute(context.Background(), orgID, ExecutionPayload{}); err != nil {
		t.Fatal(err)
	}
	if remote.called || !local.called {
		t.Fatalf("expected local fallback, got remote=%v local=%v", remote.called, local.called)
	}
}

func TestDispatcher_StrictModeErrorsWhenAgentOffline(t *testing.T) {
	repo := newMemRepo()
	reg := NewAgentRegistry(repo)
	orgID := uuid.New()

	out, _ := reg.Register(context.Background(), RegisterInput{OrganizationID: orgID, Endpoint: "x"})
	// No heartbeat — status stays pending.

	repo.policies[orgID] = &domain.AgentRoutingPolicy{
		OrganizationID:   orgID,
		PreferredAgentID: &out.Agent.ID,
		FallbackToLocal:  false,
	}

	disp := NewDispatcher(reg, &fakeRemote{}, &fakeLocal{})
	_, err := disp.Execute(context.Background(), orgID, ExecutionPayload{})
	if !errors.Is(err, ErrNoPreferredAgent) {
		t.Fatalf("expected ErrNoPreferredAgent, got %v", err)
	}
}
