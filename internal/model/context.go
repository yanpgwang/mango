package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	defaultContextWindowTokens = 200_000
	defaultContextBufferTokens = 13_000
	mediumContextBufferTokens  = 30_000
	largeContextBufferTokens   = 50_000
	toolResultGrowthTokens     = 15_000
)

// ContextProfile is the small portion of Catwalk model metadata needed for
// request admission. Found is false for custom Claude-compatible model ids,
// which use the conservative fallback window.
type ContextProfile struct {
	ContextWindow int
	Found         bool
}

// ContextLimits are the model-specific thresholds for one request. InputLimit
// reserves the actual request max output. CompactThreshold leaves additional
// headroom for one unusually large model/tool round.
type ContextLimits struct {
	ContextWindow    int
	MaxOutputTokens  int
	InputLimit       int
	CompactThreshold int
	GrowthReserve    int
}

type ContextMeasurement struct {
	Tokens int
	Exact  bool
}

var anthropicContextModels = loadAnthropicContextModels()

func loadAnthropicContextModels() []catwalk.Model {
	for _, provider := range embedded.GetAll() {
		if provider.ID == catwalk.InferenceProviderAnthropic {
			return append([]catwalk.Model(nil), provider.Models...)
		}
	}
	return nil
}

// ResolveContextProfile uses Catwalk's embedded Anthropic catalog. A dated
// provider id may extend a known catalog id, but a shorter ambiguous id must
// never inherit a larger model's window. Arbitrary router and test ids retain
// the conservative Claude-compatible fallback.
func ResolveContextProfile(modelID string) ContextProfile {
	modelID = strings.TrimSpace(modelID)
	for _, candidate := range anthropicContextModels {
		if candidate.ID == modelID {
			return contextProfile(candidate)
		}
	}
	for _, candidate := range anthropicContextModels {
		if strings.HasPrefix(modelID, candidate.ID+"-") {
			return contextProfile(candidate)
		}
	}
	// Catwalk sometimes lists only a dated provider id while Anthropic also
	// accepts its undated alias (for example claude-sonnet-4-5). Resolve that
	// shorter alias only when every matching catalog entry agrees on the window.
	// A broad id such as claude-opus-4 spans both 200k and 1m models and must
	// therefore keep the conservative fallback.
	var aliasProfile ContextProfile
	for _, candidate := range anthropicContextModels {
		if !strings.HasPrefix(candidate.ID, modelID+"-") {
			continue
		}
		profile := contextProfile(candidate)
		if !aliasProfile.Found {
			aliasProfile = profile
			continue
		}
		if aliasProfile.ContextWindow != profile.ContextWindow {
			return ContextProfile{ContextWindow: defaultContextWindowTokens}
		}
	}
	if aliasProfile.Found {
		return aliasProfile
	}
	return ContextProfile{ContextWindow: defaultContextWindowTokens}
}

func contextProfile(candidate catwalk.Model) ContextProfile {
	window := int(candidate.ContextWindow)
	if window <= 0 {
		window = defaultContextWindowTokens
	}
	return ContextProfile{ContextWindow: window, Found: true}
}

func RequestContextLimits(request Request) ContextLimits {
	window := ResolveContextProfile(request.Model).ContextWindow
	maxOutput := request.MaxTokens
	if maxOutput <= 0 {
		maxOutput = defaultMaxTokens
	}
	if maxOutput >= window {
		maxOutput = window / 4
	}
	buffer := defaultContextBufferTokens
	if window >= 800_000 {
		buffer = largeContextBufferTokens
	} else if window >= 400_000 {
		buffer = mediumContextBufferTokens
	}
	inputLimit := window - maxOutput
	growthReserve := maxOutput + toolResultGrowthTokens
	// The compaction target must satisfy both the model-tier safety buffer and
	// the predicted next-round output/tool growth. Otherwise a 200k model would
	// compact to inputLimit-13k while immediately reserving roughly 19k, forcing
	// another avoidable compaction on the next continuation.
	reserve := buffer
	if growthReserve > reserve {
		reserve = growthReserve
	}
	threshold := inputLimit - reserve
	if threshold < 1 {
		threshold = 1
	}
	return ContextLimits{
		ContextWindow: window, MaxOutputTokens: maxOutput,
		InputLimit: inputLimit, CompactThreshold: threshold,
		GrowthReserve: growthReserve,
	}
}

// MeasureRequestContext follows CCB's canonical rule: use the most recent
// provider-reported usage whose request and measured message prefix still
// match, then estimate only content appended after it. If no anchor is safe,
// estimate the complete request including system and tools.
func MeasureRequestContext(request Request) ContextMeasurement {
	requestFingerprint := RequestContextFingerprint(request)
	for index := len(request.Messages) - 1; index >= 0; index-- {
		anchor := request.Messages[index].ContextUsage
		if anchor == nil || anchor.RequestFingerprint != requestFingerprint ||
			anchor.ContentBlocks < 0 ||
			anchor.ContentBlocks > len(request.Messages[index].Content) {
			continue
		}
		if messagePrefixFingerprint(request.Messages, index, anchor.ContentBlocks) !=
			anchor.PrefixFingerprint {
			continue
		}
		delta := messagesAfterAnchor(request.Messages, index, anchor.ContentBlocks)
		return ContextMeasurement{
			Tokens: int(anchor.Usage.ContextTokens()) + domain.EstimateMessagesTokens(delta),
			Exact:  true,
		}
	}
	return ContextMeasurement{Tokens: estimateFullRequestTokens(request)}
}

func estimateFullRequestTokens(request Request) int {
	tokens := domain.EstimateTextTokens(request.System) +
		domain.EstimateMessagesTokens(request.Messages)
	if encoded, err := json.Marshal(request.Tools); err == nil {
		tokens += domain.EstimateTextTokens(string(encoded))
	}
	return tokens
}

// RequestContextFingerprint intentionally excludes Messages. Message-prefix
// integrity is checked independently so messages added after an exact anchor
// can be estimated as a delta.
func RequestContextFingerprint(request Request) string {
	value := struct {
		Model     string
		Effort    string
		Speed     string
		System    string
		MaxTokens int
		Tools     []ToolSchema
	}{
		Model: request.Model, Effort: request.Effort, Speed: request.Speed,
		System:    request.System,
		MaxTokens: RequestContextLimits(request).MaxOutputTokens,
		Tools:     request.Tools,
	}
	encoded, _ := json.Marshal(value)
	return hashContextBytes(encoded)
}

// AnchoredAssistantMessage attaches exact usage to the assistant response at
// the precise merged-message boundary that the next provider request sees.
func AnchoredAssistantMessage(request Request, response Response) domain.Message {
	message := domain.Message{Role: domain.RoleAssistant, Content: response.Content}
	if response.Usage.ContextTokens() <= 0 {
		return message
	}
	anchored := appendAssistantForContext(request.Messages, message)
	last := len(anchored) - 1
	message.ContextUsage = &domain.ContextUsageAnchor{
		Usage:              response.Usage,
		RequestFingerprint: RequestContextFingerprint(request),
		PrefixFingerprint: messagePrefixFingerprint(
			anchored, last, len(anchored[last].Content),
		),
		ContentBlocks: len(anchored[last].Content),
	}
	return message
}

func appendAssistantForContext(
	messages []domain.Message,
	assistant domain.Message,
) []domain.Message {
	out := append([]domain.Message(nil), messages...)
	if len(out) > 0 && out[len(out)-1].Role == domain.RoleAssistant {
		last := out[len(out)-1]
		last.Content = append(append([]domain.ContentBlock(nil), last.Content...), assistant.Content...)
		last.ContextUsage = assistant.ContextUsage
		out[len(out)-1] = last
		return out
	}
	return append(out, assistant)
}

func messagesAfterAnchor(
	messages []domain.Message,
	index int,
	contentBlocks int,
) []domain.Message {
	var delta []domain.Message
	if contentBlocks < len(messages[index].Content) {
		delta = append(delta, domain.Message{
			Role:    messages[index].Role,
			Content: messages[index].Content[contentBlocks:],
		})
	}
	delta = append(delta, messages[index+1:]...)
	return delta
}

func messagePrefixFingerprint(
	messages []domain.Message,
	lastIndex int,
	lastContentBlocks int,
) string {
	if lastIndex < 0 || lastIndex >= len(messages) {
		return ""
	}
	type fingerprintMessage struct {
		Role    domain.Role
		Content []domain.ContentBlock
	}
	prefix := make([]fingerprintMessage, 0, lastIndex+1)
	for index := 0; index <= lastIndex; index++ {
		content := messages[index].Content
		if index == lastIndex {
			if lastContentBlocks < 0 || lastContentBlocks > len(content) {
				return ""
			}
			content = content[:lastContentBlocks]
		}
		prefix = append(prefix, fingerprintMessage{
			Role: messages[index].Role, Content: content,
		})
	}
	encoded, _ := json.Marshal(prefix)
	return hashContextBytes(encoded)
}

func hashContextBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
