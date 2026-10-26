package model

import (
	"encoding/json"
	"time"
)

const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"

	StopFinalAnswer  = "final_answer_returned"
	StopStepLimit    = "step_limit_reached"
	StopAttemptLimit = "attempt_limit_reached"
	StopRetryLimit   = "retry_limit_reached"
	StopModelError   = "model_error"
	StopApproval     = "approval_denied"
	StopPersistence  = "persistence_error"
	CheckpointSchema = "phase1-v1"
	ArtifactSchema   = "pico.go.run.v1"
	SessionSchema    = "pico.go.session.v1"
	EventSchema      = "pico.go.event.v1"
	ResumeNone       = "no-checkpoint"
	ResumeFull       = "full-valid"
	ResumeWorkspace  = "workspace-mismatch"
	ResumeSchema     = "schema-mismatch"
)

type ToolCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type ToolResult struct {
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Output        string         `json:"output,omitempty"`
	Error         string         `json:"error,omitempty"`
	ErrorCode     string         `json:"error_code,omitempty"`
	AffectedPaths []string       `json:"affected_paths,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type Message struct {
	ID            string       `json:"id,omitempty"`
	ToolCalls     []ToolCall   `json:"tool_calls,omitempty"`
	ToolCallID    string       `json:"tool_call_id,omitempty"`
	Role          string       `json:"role"`
	Content       string       `json:"content"`
	CreatedAt     time.Time    `json:"created_at"`
	NativeContent []NativeItem `json:"native_content,omitempty"`
}

// NativeItem preserves opaque reasoning/signature blocks for the same protocol.
// It is provider data, never an executable tool or an instruction to the host.
type NativeItem struct {
	Protocol string          `json:"protocol"`
	Data     json.RawMessage `json:"data"`
}

type TaskState struct {
	SchemaVersion         string    `json:"schema_version"`
	RunID                 string    `json:"run_id"`
	TaskID                string    `json:"task_id"`
	UserRequest           string    `json:"user_request"`
	Status                string    `json:"status"`
	ToolSteps             int       `json:"tool_steps"`
	Attempts              int       `json:"attempts"`
	LastTool              string    `json:"last_tool,omitempty"`
	StopReason            string    `json:"stop_reason,omitempty"`
	FinalAnswer           string    `json:"final_answer,omitempty"`
	CheckpointID          string    `json:"checkpoint_id,omitempty"`
	ResumeStatus          string    `json:"resume_status,omitempty"`
	ChangedPaths          []string  `json:"changed_paths,omitempty"`
	VerificationAttempted bool      `json:"verification_attempted,omitempty"`
	VerificationSucceeded bool      `json:"verification_succeeded,omitempty"`
	ReadinessReminded     bool      `json:"readiness_reminded,omitempty"`
	StartedAt             time.Time `json:"started_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type TraceEvent struct {
	SchemaVersion string         `json:"schema_version"`
	Type          string         `json:"type"`
	RunID         string         `json:"run_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
	Payload       map[string]any `json:"payload,omitempty"`
}

type RunReport struct {
	SchemaVersion string         `json:"schema_version"`
	RunID         string         `json:"run_id"`
	SessionID     string         `json:"session_id"`
	Status        string         `json:"status"`
	StopReason    string         `json:"stop_reason"`
	FinalAnswer   string         `json:"final_answer,omitempty"`
	Attempts      int            `json:"attempts"`
	ToolSteps     int            `json:"tool_steps"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	PromptMeta    map[string]any `json:"prompt_metadata,omitempty"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
}

type Session struct {
	Revision      uint64         `json:"revision,omitempty"`
	Epoch         uint64         `json:"epoch,omitempty"`
	Fence         string         `json:"-"`
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	Workspace     string         `json:"workspace"`
	MemoryUser    string         `json:"memory_user,omitempty"`
	Messages      []Message      `json:"messages"`
	Memory        map[string]any `json:"memory,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type Checkpoint struct {
	SessionID     string            `json:"session_id,omitempty"`
	SchemaVersion string            `json:"schema_version"`
	ID            string            `json:"id"`
	RunID         string            `json:"run_id"`
	Goal          string            `json:"goal"`
	NextStep      string            `json:"next_step,omitempty"`
	KeyFiles      map[string]string `json:"key_files,omitempty"`
	WorkspaceHash string            `json:"workspace_fingerprint"`
	CreatedAt     time.Time         `json:"created_at"`
}
