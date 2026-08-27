// Package model defines the inference client the agent core calls. The
// interface is deliberately small and vendor-neutral so the transport (real
// Messages API, offline fake, or another provider) is swappable without
// touching the agent core or the domain.
package model

import (
	"context"

	"github.com/yanpgwang/mango/internal/domain"
)

type ToolSchema struct {
	// Type is set for provider-native server tools. Ordinary client tools leave
	// it empty and provide Description/InputSchema.
	Type        string
	Name        string
	Description string
	InputSchema map[string]any
}

type Request struct {
	Model     string
	Effort    string
	Speed     string
	System    string
	Messages  []domain.Message
	MaxTokens int
	Tools     []ToolSchema
}

type Response struct {
	Content    []domain.ContentBlock
	StopReason string
	Usage      domain.TokenUsage
}

// StreamCallbacks exposes only public, privacy-safe progress signals. Thinking
// content is intentionally absent: callers may announce that thinking began,
// but must never receive provider reasoning through this channel.
type StreamCallbacks struct {
	OnTextDelta     func(index int, text string)
	OnThinkingStart func()
}

// RichStreamingClient is an optional extension implemented by transports that
// can distinguish privacy-safe stream lifecycle signals. Client remains the
// compatibility floor for simple providers and existing test doubles.
type RichStreamingClient interface {
	CreateMessageStreamWithCallbacks(
		ctx context.Context,
		req Request,
		callbacks StreamCallbacks,
	) (Response, error)
}

type Client interface {
	CreateMessage(ctx context.Context, req Request) (Response, error)

	// CreateMessageStream is the streaming variant of CreateMessage. It invokes
	// onDelta once per text delta (index is the content-block index; currently
	// always 0), in order, as the reply is produced, then returns the same
	// Response CreateMessage would return for the same request. Callers that do
	// not care about incremental deltas can call CreateMessage instead.
	//
	// When the response is not streamable text (e.g. a tool_use turn), onDelta
	// is not called and the full Response is returned directly.
	CreateMessageStream(ctx context.Context, req Request, onDelta func(index int, text string)) (Response, error)
}
