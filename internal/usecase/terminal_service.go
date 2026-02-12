package usecase

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// TerminalUseCase defines the interface for terminal session management
type TerminalUseCase interface {
	// CreateTerminalSession creates a new interactive terminal session backed by a VM
	CreateTerminalSession(ctx context.Context, input CreateTerminalSessionInput) (*domain.TerminalSession, error)

	// GetTerminalSession returns a terminal session by ID
	GetTerminalSession(ctx context.Context, id uuid.UUID) (*domain.TerminalSession, error)

	// CloseTerminalSession closes a terminal session and releases the VM
	CloseTerminalSession(ctx context.Context, id uuid.UUID) error

	// ListUserSessions returns active terminal sessions for a user
	ListUserSessions(ctx context.Context, userID uuid.UUID) ([]domain.TerminalSession, error)
}

// CreateTerminalSessionInput represents the input for creating a terminal session
type CreateTerminalSessionInput struct {
	UserID   uuid.UUID       `json:"userId"`
	Language domain.Language `json:"language"`
	RepoID   *uuid.UUID      `json:"repoId,omitempty"`
}

type terminalService struct {
	sessionRepo repository.TerminalSessionRepository
	vmUC        VMUseCase
	vmProvider  VMProvider
}

// NewTerminalService creates a new terminal session service
func NewTerminalService(
	sessionRepo repository.TerminalSessionRepository,
	vmUC VMUseCase,
	vmProvider VMProvider,
) TerminalUseCase {
	return &terminalService{
		sessionRepo: sessionRepo,
		vmUC:        vmUC,
		vmProvider:  vmProvider,
	}
}

func (s *terminalService) CreateTerminalSession(ctx context.Context, input CreateTerminalSessionInput) (*domain.TerminalSession, error) {
	if !input.Language.IsValid() {
		return nil, domain.ErrInvalidLanguage
	}

	session := &domain.TerminalSession{
		ID:        uuid.New(),
		UserID:    input.UserID,
		Language:  input.Language,
		RepoID:    input.RepoID,
		Status:    domain.TerminalSessionStatusCreating,
		CreatedAt: time.Now().UTC(),
	}

	if err := session.Validate(); err != nil {
		return nil, err
	}

	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create terminal session: %w", err)
	}

	// Boot a VM for this terminal session
	vm, err := s.vmUC.CreateVM(ctx, input.Language, 1, 256)
	if err != nil {
		session.MarkError()
		_ = s.sessionRepo.Update(ctx, session)
		return nil, fmt.Errorf("failed to boot VM for terminal: %w", err)
	}

	session.MarkActive(vm.ID, vm.IPAddress)
	if err := s.sessionRepo.Update(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to update terminal session: %w", err)
	}

	log.Printf("Terminal session created: %s (user=%s, lang=%s, vm=%s, ip=%s)",
		session.ID, session.UserID, session.Language, vm.ID, vm.IPAddress)

	return session, nil
}

func (s *terminalService) GetTerminalSession(ctx context.Context, id uuid.UUID) (*domain.TerminalSession, error) {
	return s.sessionRepo.FindByID(ctx, id)
}

func (s *terminalService) CloseTerminalSession(ctx context.Context, id uuid.UUID) error {
	session, err := s.sessionRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}

	if session.Status == domain.TerminalSessionStatusClosed {
		return domain.ErrTerminalSessionClosed
	}

	// Terminate the backing VM
	if session.VMID != uuid.Nil {
		if err := s.vmUC.TerminateVM(ctx, session.VMID); err != nil {
			log.Printf("Warning: failed to terminate VM %s for terminal session %s: %v",
				session.VMID, session.ID, err)
		}
	}

	session.Close()
	if err := s.sessionRepo.Update(ctx, session); err != nil {
		return fmt.Errorf("failed to close terminal session: %w", err)
	}

	log.Printf("Terminal session closed: %s", session.ID)
	return nil
}

func (s *terminalService) ListUserSessions(ctx context.Context, userID uuid.UUID) ([]domain.TerminalSession, error) {
	return s.sessionRepo.FindByUser(ctx, userID)
}
