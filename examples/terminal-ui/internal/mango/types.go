package mango

import (
	"context"
	"time"
)

type Session struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ArchivedAt    *time.Time `json:"archived_at"`
	EnvironmentID string     `json:"environment_id"`
	Agent         Agent      `json:"agent"`
	Stats         Stats      `json:"stats"`
	Usage         Usage      `json:"usage"`
}

type Agent struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	System      string           `json:"system"`
	Version     int              `json:"version"`
	ArchivedAt  *time.Time       `json:"archived_at"`
	Tools       []map[string]any `json:"tools"`
	MCPServers  []map[string]any `json:"mcp_servers"`
	Multiagent  *Multiagent      `json:"multiagent"`
	Model       struct {
		ID string `json:"id"`
	} `json:"model"`
}

type AgentReference struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int    `json:"version"`
	// Name is populated when the reference appears inside a resolved Session
	// multi-agent roster; the leaner Agent-resource reference form omits it.
	Name string `json:"name"`
}

type Multiagent struct {
	Type   string           `json:"type"`
	Agents []AgentReference `json:"agents"`
}

type MultiagentInput struct {
	Type   string
	Agents []string
}

type Environment struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ArchivedAt  *time.Time `json:"archived_at"`
	Config      struct {
		Type string `json:"type"`
	} `json:"config"`
}

type CreateAgentInput struct {
	Name       string
	Model      string
	System     string
	Tools      []map[string]any
	MCPServers []map[string]any
	Multiagent *MultiagentInput
}

type CreateEnvironmentInput struct {
	Name        string
	Description string
}

type CreateSessionInput struct {
	AgentID       string
	EnvironmentID string
	Title         string
	InitialPrompt string
}

type Stats struct {
	ActiveSeconds   float64 `json:"active_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	StartupSeconds  float64 `json:"startup_seconds"`
}

type Usage struct {
	InputTokens          int64   `json:"input_tokens"`
	OutputTokens         int64   `json:"output_tokens"`
	CacheReadInputTokens int64   `json:"cache_read_input_tokens"`
	ActiveSeconds        float64 `json:"active_seconds"`
}

type Thread struct {
	ID             string  `json:"id"`
	SessionID      string  `json:"session_id"`
	ParentThreadID *string `json:"parent_thread_id"`
	Status         string  `json:"status"`
	Agent          Agent   `json:"agent"`
	Stats          Stats   `json:"stats"`
	Usage          Usage   `json:"usage"`
}

func (t Thread) Primary() bool { return t.ParentThreadID == nil }

type Event map[string]any

func (e Event) ID() string   { value, _ := e["id"].(string); return value }
func (e Event) Type() string { value, _ := e["type"].(string); return value }

type StreamUpdate struct {
	ThreadID string
	Frame    Event
	Err      error
}

type Attachment struct {
	Session Session
	Threads []Thread
	Events  map[string][]Event
	Updates <-chan StreamUpdate
	Cancel  context.CancelFunc
}

type Summary struct {
	Session Session
	Threads []Thread
}

type ActionKind string

const (
	ActionConfirmation ActionKind = "tool_confirmation"
	ActionCustomResult ActionKind = "custom_tool_result"
	ActionToolResult   ActionKind = "tool_result"
)

type Action struct {
	ID       string
	ThreadID string
	Kind     ActionKind
	Name     string
	Input    string
}

type ActionResponse struct {
	Result      string
	DenyMessage string
	Content     string
	IsError     bool
}

type Backend interface {
	ListSessions(context.Context) ([]Session, error)
	ListAgents(context.Context) ([]Agent, error)
	ListEnvironments(context.Context) ([]Environment, error)
	CreateAgent(context.Context, CreateAgentInput) (Agent, error)
	CreateEnvironment(context.Context, CreateEnvironmentInput) (Environment, error)
	CreateSession(context.Context, CreateSessionInput) (Session, error)
	UpdateSessionTitle(context.Context, string, string) (Session, error)
	ArchiveSession(context.Context, string) (Session, error)
	DeleteSession(context.Context, string) error
	Attach(context.Context, string) (Attachment, error)
	Refresh(context.Context, string) (Summary, error)
	SendMessage(context.Context, string, string) ([]Event, error)
	Interrupt(context.Context, string, string) ([]Event, error)
	ResolveAction(context.Context, string, Action, ActionResponse) ([]Event, error)
}
