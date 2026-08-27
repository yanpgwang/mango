package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestResolveContextProfileUsesEmbeddedCatwalkAndFallback(t *testing.T) {
	known := ResolveContextProfile("claude-opus-4-8")
	require.True(t, known.Found)
	require.Equal(t, 1_000_000, known.ContextWindow)

	dated := ResolveContextProfile("claude-opus-4-8-20260801")
	require.Equal(t, known, dated)

	shortAlias := ResolveContextProfile("claude-sonnet-4-5")
	require.True(t, shortAlias.Found)
	require.Equal(t, 200_000, shortAlias.ContextWindow)

	ambiguousShortID := ResolveContextProfile("claude-opus-4")
	require.False(t, ambiguousShortID.Found)
	require.Equal(t, 200_000, ambiguousShortID.ContextWindow,
		"a short id must not inherit claude-opus-4-8's 1m window")

	fallback := ResolveContextProfile("router/private-claude")
	require.False(t, fallback.Found)
	require.Equal(t, 200_000, fallback.ContextWindow)
}

func TestMeasureRequestContextUsesLatestExactUsagePlusDelta(t *testing.T) {
	initial := Request{
		Model: "claude-sonnet-4-6", System: "system",
		Messages: []domain.Message{{
			Role:    domain.RoleUser,
			Content: []domain.ContentBlock{{Type: "text", Text: "run the tool"}},
		}},
	}
	response := Response{
		Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: "tool_1", ToolName: "read",
		}},
		Usage: domain.TokenUsage{
			InputTokens: 10, CacheReadInputTokens: 2, OutputTokens: 5,
			CacheCreation: domain.CacheCreationUsage{
				Ephemeral1hInputTokens: 3, Ephemeral5mInputTokens: 4,
			},
		},
	}
	assistant := AnchoredAssistantMessage(initial, response)
	toolResult := domain.Message{
		Role: domain.RoleUser,
		Content: []domain.ContentBlock{{
			Type: "tool_result", ToolResultFor: "tool_1", Text: "result",
		}},
	}
	next := initial
	next.Messages = append(append([]domain.Message(nil), initial.Messages...), assistant, toolResult)

	measurement := MeasureRequestContext(next)
	require.True(t, measurement.Exact)
	require.Equal(t,
		24+domain.EstimateMessagesTokens([]domain.Message{toolResult}),
		measurement.Tokens,
	)
}

func TestAnchoredAssistantMessageExcludesPricingAndRoutingUsage(t *testing.T) {
	anchored := AnchoredAssistantMessage(Request{Model: "claude-sonnet-5"}, Response{
		Content: []domain.ContentBlock{{Type: "text", Text: "answer"}},
		Usage: domain.TokenUsage{
			InputTokens: 10, OutputTokens: 2, Speed: "fast", ProviderRegion: "us",
			ServerToolUse: domain.ServerToolUsage{WebSearchRequests: 1},
		},
	})
	require.NotNil(t, anchored.ContextUsage)
	require.Equal(t, int64(12), anchored.ContextUsage.Usage.ContextTokens())

	encoded, err := json.Marshal(anchored)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "ProviderRegion")
	require.NotContains(t, string(encoded), "ServerToolUse")
	require.NotContains(t, string(encoded), `"Speed"`)
	require.NotContains(t, string(encoded), `"us"`)
}

func TestMeasureRequestContextRejectsStaleRequestAndPrefixAnchors(t *testing.T) {
	initial := Request{
		Model: "claude-sonnet-4-6", System: "before",
		Messages: []domain.Message{{
			Role:    domain.RoleUser,
			Content: []domain.ContentBlock{{Type: "text", Text: "original"}},
		}},
	}
	assistant := AnchoredAssistantMessage(initial, Response{
		Content: []domain.ContentBlock{{Type: "text", Text: "answer"}},
		Usage:   domain.TokenUsage{InputTokens: 80_000, OutputTokens: 4_000},
	})

	changedSystem := initial
	changedSystem.System = "after"
	changedSystem.Messages = append(changedSystem.Messages, assistant)
	require.False(t, MeasureRequestContext(changedSystem).Exact)

	changedPrefix := initial
	changedPrefix.Messages = []domain.Message{
		{
			Role:    domain.RoleUser,
			Content: []domain.ContentBlock{{Type: "text", Text: "compacted replacement"}},
		},
		assistant,
	}
	measurement := MeasureRequestContext(changedPrefix)
	require.False(t, measurement.Exact)
	require.Less(t, measurement.Tokens, 1_000)
}

func TestAnchoredAssistantSupportsMergedContinuation(t *testing.T) {
	request := Request{
		Model: "claude-sonnet-4-6",
		Messages: []domain.Message{
			{
				Role:    domain.RoleUser,
				Content: []domain.ContentBlock{{Type: "text", Text: "start"}},
			},
			{
				Role:    domain.RoleAssistant,
				Content: []domain.ContentBlock{{Type: "text", Text: "paused"}},
			},
		},
	}
	anchored := AnchoredAssistantMessage(request, Response{
		Content: []domain.ContentBlock{{Type: "text", Text: "continued"}},
		Usage:   domain.TokenUsage{InputTokens: 100, OutputTokens: 10},
	})
	merged := append([]domain.Message(nil), request.Messages...)
	merged[len(merged)-1].Content = append(merged[len(merged)-1].Content, anchored.Content...)
	merged[len(merged)-1].ContextUsage = anchored.ContextUsage

	measurement := MeasureRequestContext(Request{
		Model: request.Model, Messages: merged,
	})
	require.True(t, measurement.Exact)
	require.Equal(t, 110, measurement.Tokens)
}

func TestRequestContextLimitsReserveActualOutputAndTieredBuffer(t *testing.T) {
	large := RequestContextLimits(Request{Model: "claude-opus-4-8", MaxTokens: 8_000})
	require.Equal(t, 1_000_000, large.ContextWindow)
	require.Equal(t, 8_000, large.MaxOutputTokens)
	require.Equal(t, 942_000, large.CompactThreshold)
	require.Equal(t, 23_000, large.GrowthReserve)

	ordinary := RequestContextLimits(Request{Model: "unknown"})
	require.Equal(t, 200_000, ordinary.ContextWindow)
	require.Equal(t, defaultMaxTokens, ordinary.MaxOutputTokens)
	require.Equal(t, 19_096, ordinary.GrowthReserve)
	require.Equal(t, 176_808, ordinary.CompactThreshold)
	require.LessOrEqual(t,
		ordinary.CompactThreshold+ordinary.GrowthReserve,
		ordinary.InputLimit,
	)
}
