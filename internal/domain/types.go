package domain

import (
	"database/sql/driver"
	"encoding/json"
)

// JSONMap is a map[string]interface{} for JSONB storage
type JSONMap map[string]interface{}

// Value implements driver.Valuer for database storage
func (j JSONMap) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
}

// Scan implements sql.Scanner for database retrieval
func (j *JSONMap) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return ErrInvalidData
	}
	return json.Unmarshal(bytes, j)
}

// ExecutionStatus represents the status of a code execution
type ExecutionStatus string

const (
	ExecutionStatusPending   ExecutionStatus = "pending"
	ExecutionStatusQueued    ExecutionStatus = "queued"
	ExecutionStatusRunning   ExecutionStatus = "running"
	ExecutionStatusCompleted ExecutionStatus = "completed"
	ExecutionStatusFailed    ExecutionStatus = "failed"
	ExecutionStatusTimeout   ExecutionStatus = "timeout"
	ExecutionStatusCancelled ExecutionStatus = "cancelled"
)

// IsValid checks if the execution status is valid
func (s ExecutionStatus) IsValid() bool {
	switch s {
	case ExecutionStatusPending, ExecutionStatusQueued, ExecutionStatusRunning,
		ExecutionStatusCompleted, ExecutionStatusFailed, ExecutionStatusTimeout,
		ExecutionStatusCancelled:
		return true
	}
	return false
}

// IsTerminal returns true if the status is a terminal state
func (s ExecutionStatus) IsTerminal() bool {
	switch s {
	case ExecutionStatusCompleted, ExecutionStatusFailed,
		ExecutionStatusTimeout, ExecutionStatusCancelled:
		return true
	}
	return false
}

// VMStatus represents the status of a Firecracker microVM
type VMStatus string

const (
	VMStatusCreating    VMStatus = "creating"
	VMStatusReady       VMStatus = "ready"
	VMStatusRunning     VMStatus = "running"
	VMStatusPaused      VMStatus = "paused"
	VMStatusTerminating VMStatus = "terminating"
	VMStatusTerminated  VMStatus = "terminated"
	VMStatusError       VMStatus = "error"
)

// IsValid checks if the VM status is valid
func (s VMStatus) IsValid() bool {
	switch s {
	case VMStatusCreating, VMStatusReady, VMStatusRunning, VMStatusPaused,
		VMStatusTerminating, VMStatusTerminated, VMStatusError:
		return true
	}
	return false
}

// Language represents a supported programming language
type Language string

const (
	LanguageGo         Language = "go"
	LanguagePython     Language = "python"
	LanguageJavaScript Language = "javascript"
	LanguageTypeScript Language = "typescript"
	LanguageRust       Language = "rust"
	LanguageC          Language = "c"
	LanguageCPP        Language = "cpp"
	LanguageBash       Language = "bash"
)

// IsValid checks if the language is supported
func (l Language) IsValid() bool {
	switch l {
	case LanguageGo, LanguagePython, LanguageJavaScript, LanguageTypeScript,
		LanguageRust, LanguageC, LanguageCPP, LanguageBash:
		return true
	}
	return false
}

// IsCompiled returns true if the language requires compilation
func (l Language) IsCompiled() bool {
	switch l {
	case LanguageGo, LanguageRust, LanguageC, LanguageCPP:
		return true
	}
	return false
}

// NetworkMode represents the network isolation mode for a microVM
type NetworkMode string

const (
	NetworkModeIsolated NetworkMode = "isolated"
	NetworkModeBridged  NetworkMode = "bridged"
	NetworkModeHost     NetworkMode = "host"
)

// IsValid checks if the network mode is valid
func (n NetworkMode) IsValid() bool {
	switch n {
	case NetworkModeIsolated, NetworkModeBridged, NetworkModeHost:
		return true
	}
	return false
}
