package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"golang.org/x/crypto/ssh"
)

// TerminalHandler handles terminal-related HTTP requests
type TerminalHandler struct {
	terminalUC usecase.TerminalUseCase
}

// NewTerminalHandler creates a new terminal handler
func NewTerminalHandler(uc usecase.TerminalUseCase) *TerminalHandler {
	return &TerminalHandler{terminalUC: uc}
}

// RegisterRoutes registers terminal routes on the router
func (h *TerminalHandler) RegisterRoutes(r chi.Router) {
	r.Route("/terminal-sessions", func(r chi.Router) {
		r.Post("/", h.CreateTerminalSession)
		r.Get("/", h.ListTerminalSessions)
		r.Get("/{id}", h.GetTerminalSession)
		r.Delete("/{id}", h.DeleteTerminalSession)
		r.Get("/{id}/ws", h.HandleTerminalWebSocket)
	})
}

// CreateTerminalSessionRequest represents the request body for creating a terminal session
type CreateTerminalSessionRequest struct {
	Language string  `json:"language"`
	RepoID   *string `json:"repoId,omitempty"`
}

// TerminalSessionResponse is the API response for a terminal session
type TerminalSessionResponse struct {
	ID           string  `json:"id"`
	UserID       string  `json:"userId"`
	VMID         string  `json:"vmId"`
	Language     string  `json:"language"`
	RepoID       *string `json:"repoId,omitempty"`
	Status       string  `json:"status"`
	WebsocketURL string  `json:"websocketUrl"`
	CreatedAt    string  `json:"createdAt"`
}

// CreateTerminalSession handles POST /api/v1/terminal-sessions
func (h *TerminalHandler) CreateTerminalSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := GetUserIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "User not authenticated")
		return
	}

	var req CreateTerminalSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	lang := domain.Language(req.Language)
	if req.Language == "" {
		lang = domain.LanguageBash
	}
	if !lang.IsValid() {
		RespondBadRequest(w, "Unsupported language", map[string]any{
			"language": req.Language,
		})
		return
	}

	input := usecase.CreateTerminalSessionInput{
		UserID:   userID,
		Language: lang,
	}

	if req.RepoID != nil {
		repoID, err := uuid.Parse(*req.RepoID)
		if err != nil {
			RespondBadRequest(w, "Invalid repoId format", nil)
			return
		}
		input.RepoID = &repoID
	}

	session, err := h.terminalUC.CreateTerminalSession(r.Context(), input)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidLanguage) {
			RespondBadRequest(w, err.Error(), nil)
			return
		}
		RespondInternalError(w, "Failed to create terminal session")
		return
	}

	RespondCreated(w, terminalSessionToResponse(session, r))
}

// GetTerminalSession handles GET /api/v1/terminal-sessions/{id}
func (h *TerminalHandler) GetTerminalSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	session, err := h.terminalUC.GetTerminalSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrTerminalSessionNotFound) {
			RespondNotFound(w, "Terminal session not found")
			return
		}
		RespondInternalError(w, "Failed to get terminal session")
		return
	}

	RespondSuccess(w, terminalSessionToResponse(session, r))
}

// ListTerminalSessions handles GET /api/v1/terminal-sessions
func (h *TerminalHandler) ListTerminalSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := GetUserIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "User not authenticated")
		return
	}

	sessions, err := h.terminalUC.ListUserSessions(r.Context(), userID)
	if err != nil {
		RespondInternalError(w, "Failed to list terminal sessions")
		return
	}

	items := make([]TerminalSessionResponse, len(sessions))
	for i, s := range sessions {
		items[i] = terminalSessionToResponse(&s, r)
	}

	RespondSuccess(w, map[string]any{
		"items": items,
	})
}

// DeleteTerminalSession handles DELETE /api/v1/terminal-sessions/{id}
func (h *TerminalHandler) DeleteTerminalSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.terminalUC.CloseTerminalSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrTerminalSessionNotFound) {
			RespondNotFound(w, "Terminal session not found")
			return
		}
		if errors.Is(err, domain.ErrTerminalSessionClosed) {
			RespondBadRequest(w, "Terminal session already closed", nil)
			return
		}
		RespondInternalError(w, "Failed to close terminal session")
		return
	}

	RespondNoContent(w)
}

// WebSocket message protocol
type terminalMessage struct {
	Type string `json:"type"` // stdin, stdout, stderr, resize
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development; restrict in production
	},
}

// HandleTerminalWebSocket handles GET /api/v1/terminal-sessions/{id}/ws
func (h *TerminalHandler) HandleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	session, err := h.terminalUC.GetTerminalSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrTerminalSessionNotFound) {
			RespondNotFound(w, "Terminal session not found")
			return
		}
		RespondInternalError(w, "Failed to get terminal session")
		return
	}

	if session.Status != domain.TerminalSessionStatusActive {
		RespondBadRequest(w, "Terminal session is not active", nil)
		return
	}

	if session.IPAddress == "" {
		RespondBadRequest(w, "Terminal VM has no IP address", nil)
		return
	}

	// Upgrade to WebSocket
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed for session %s: %v", sessionID, err)
		return
	}
	defer conn.Close()

	// Establish SSH connection to the VM
	sshClient, err := dialSSH(session.IPAddress)
	if err != nil {
		log.Printf("SSH connection failed for session %s (ip=%s): %v", sessionID, session.IPAddress, err)
		writeWSError(conn, fmt.Sprintf("Failed to connect to VM: %v", err))
		return
	}
	defer sshClient.Close()

	// Open SSH session with PTY
	sshSession, err := sshClient.NewSession()
	if err != nil {
		log.Printf("SSH session creation failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to create SSH session")
		return
	}
	defer sshSession.Close()

	// Request PTY
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sshSession.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		log.Printf("PTY request failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to allocate terminal")
		return
	}

	// Get stdin/stdout pipes
	stdinPipe, err := sshSession.StdinPipe()
	if err != nil {
		log.Printf("Stdin pipe failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to set up terminal input")
		return
	}

	stdoutPipe, err := sshSession.StdoutPipe()
	if err != nil {
		log.Printf("Stdout pipe failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to set up terminal output")
		return
	}

	stderrPipe, err := sshSession.StderrPipe()
	if err != nil {
		log.Printf("Stderr pipe failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to set up terminal error output")
		return
	}

	// Start shell
	if err := sshSession.Shell(); err != nil {
		log.Printf("Shell start failed for session %s: %v", sessionID, err)
		writeWSError(conn, "Failed to start shell")
		return
	}

	log.Printf("Terminal WebSocket connected: session=%s, ip=%s", sessionID, session.IPAddress)

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Goroutine: SSH stdout → WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				msg := terminalMessage{Type: "stdout", Data: string(buf[:n])}
				if writeErr := writeWSJSON(conn, msg); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("SSH stdout read error for session %s: %v", sessionID, err)
				}
				return
			}
		}
	}()

	// Goroutine: SSH stderr → WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				msg := terminalMessage{Type: "stderr", Data: string(buf[:n])}
				if writeErr := writeWSJSON(conn, msg); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("SSH stderr read error for session %s: %v", sessionID, err)
				}
				return
			}
		}
	}()

	// Goroutine: WebSocket → SSH stdin + handle resize
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for {
			_, rawMsg, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("WebSocket read error for session %s: %v", sessionID, err)
				}
				return
			}

			var msg terminalMessage
			if err := json.Unmarshal(rawMsg, &msg); err != nil {
				log.Printf("Invalid terminal message for session %s: %v", sessionID, err)
				continue
			}

			switch msg.Type {
			case "stdin":
				if _, err := stdinPipe.Write([]byte(msg.Data)); err != nil {
					log.Printf("SSH stdin write error for session %s: %v", sessionID, err)
					return
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					if err := sshSession.WindowChange(msg.Rows, msg.Cols); err != nil {
						log.Printf("Window resize failed for session %s: %v", sessionID, err)
					}
				}
			}
		}
	}()

	// Goroutine: Ping/pong keepalive
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	// Wait for SSH session to finish
	_ = sshSession.Wait()

	// Close WebSocket gracefully
	conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
		time.Now().Add(5*time.Second),
	)

	wg.Wait()
	log.Printf("Terminal WebSocket disconnected: session=%s", sessionID)
}

// dialSSH establishes an SSH connection to a VM
func dialSSH(ipAddress string) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(""), // Firecracker VMs typically use passwordless root
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	client, err := ssh.Dial("tcp", ipAddress+":22", config)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", ipAddress, err)
	}
	return client, nil
}

// writeWSJSON writes a JSON message to a WebSocket connection
func writeWSJSON(conn *websocket.Conn, msg terminalMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// writeWSError sends an error message over WebSocket
func writeWSError(conn *websocket.Conn, errMsg string) {
	msg := terminalMessage{Type: "stderr", Data: "Error: " + errMsg + "\r\n"}
	_ = writeWSJSON(conn, msg)
}

func terminalSessionToResponse(s *domain.TerminalSession, r *http.Request) TerminalSessionResponse {
	resp := TerminalSessionResponse{
		ID:        s.ID.String(),
		UserID:    s.UserID.String(),
		VMID:      s.VMID.String(),
		Language:  string(s.Language),
		Status:    string(s.Status),
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if s.RepoID != nil {
		repoIDStr := s.RepoID.String()
		resp.RepoID = &repoIDStr
	}

	// Build WebSocket URL from request host
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	resp.WebsocketURL = fmt.Sprintf("%s://%s/api/v1/terminal-sessions/%s/ws", scheme, r.Host, s.ID.String())

	return resp
}
