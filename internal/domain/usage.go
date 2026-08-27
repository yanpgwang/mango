package domain

// CacheCreationUsage is the prompt-cache creation breakdown exposed by the
// Mango Session usage object.
type CacheCreationUsage struct {
	Ephemeral1hInputTokens int64
	Ephemeral5mInputTokens int64
}

// ServerToolUsage is the provider-reported count of server-executed tools.
// Web search has a per-request public list price; web fetch is currently
// reported for observability but has no separate request fee.
type ServerToolUsage struct {
	WebFetchRequests  int64
	WebSearchRequests int64
}

// TokenUsage is cumulative model usage. Individual model responses carry a
// value with the same shape; Workflow code sums every provider round in a
// public turn and PostgreSQL applies the turn total exactly once.
type TokenUsage struct {
	CacheCreation        CacheCreationUsage
	CacheReadInputTokens int64
	InputTokens          int64
	OutputTokens         int64
	ServerToolUse        ServerToolUsage
	// Speed is the provider-reported inference mode for one model request. It is
	// intentionally not accumulated into Session usage; span events use it to
	// report the actual mode (which may differ from a requested fast fallback).
	Speed string
	// ProviderRegion is the provider-reported region for one model request. It is
	// retained only for internal list-cost accounting and is not accumulated or
	// exposed through Mango's public Agent, Session, or usage resources.
	ProviderRegion string
}

// ContextTokens returns the provider-measured context immediately after one
// response. Anthropic reports uncached input, cache creation, and cache reads
// as disjoint input buckets; all occupy the request context. Output is included
// because the assistant response becomes input to the next request.
func (u TokenUsage) ContextTokens() int64 {
	return u.InputTokens +
		u.CacheCreation.Ephemeral1hInputTokens +
		u.CacheCreation.Ephemeral5mInputTokens +
		u.CacheReadInputTokens +
		u.OutputTokens
}

func (u *TokenUsage) Add(other TokenUsage) {
	u.CacheCreation.Ephemeral1hInputTokens += other.CacheCreation.Ephemeral1hInputTokens
	u.CacheCreation.Ephemeral5mInputTokens += other.CacheCreation.Ephemeral5mInputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.ServerToolUse.WebFetchRequests += other.ServerToolUse.WebFetchRequests
	u.ServerToolUse.WebSearchRequests += other.ServerToolUse.WebSearchRequests
}
