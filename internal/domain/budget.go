package domain

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

const (
	// List costs are accumulated as integer nano-dollars. All current Anthropic
	// token rates and geographic multipliers are exact in this unit.
	NanoUSDPerUSD  int64 = 1_000_000_000
	NanoUSDPerCent int64 = NanoUSDPerUSD / 100

	webSearchRequestNanoUSD int64 = 10_000_000 // $10 / 1,000 requests.
	runtimeNanoUSDPerHour   int64 = 80_000_000 // $0.08 / active hour.
)

// SessionBudget is the Session-wide public-list-cost ceiling. MonetaryAmount
// uses minor currency units, so MaxListCostCents is the exact parsed `amount`.
type SessionBudget struct {
	MaxListCostCents int64
}

func (b SessionBudget) JSON() map[string]any {
	return map[string]any{
		"type": "limit",
		"max_list_cost": map[string]any{
			"amount":   fmt.Sprintf("%d", b.MaxListCostCents),
			"currency": "USD",
		},
	}
}

// SessionBudgetUpdate carries the update body's budget tri-state. A nil
// *SessionBudgetUpdate means omitted; a non-nil update with Budget nil means
// explicitly remove the existing ceiling.
type SessionBudgetUpdate struct {
	Budget *SessionBudget
}

// MonetaryAmountJSON rounds an exact internal list cost to the nearest whole
// cent, matching Mango's public projection.
func MonetaryAmountJSON(nanoUSD int64) map[string]any {
	if nanoUSD < 0 {
		nanoUSD = 0
	}
	cents := nanoUSD / NanoUSDPerCent
	if nanoUSD%NanoUSDPerCent >= NanoUSDPerCent/2 {
		cents++
	}
	return map[string]any{"amount": fmt.Sprintf("%d", cents), "currency": "USD"}
}

func (s Session) BudgetJSON() any {
	if s.Budget == nil {
		return nil
	}
	return s.Budget.JSON()
}

// UsageJSON is the public cumulative Session usage projection. Runtime cost is
// included only at Session scope; Thread usage uses its model/tool cost alone.
func (s Session) UsageJSON(now time.Time) map[string]any {
	activeSeconds, _ := s.ObservableStats(now)
	listCost := any(nil)
	if s.ListCostKnown {
		listCost = MonetaryAmountJSON(s.ObservableListCostNanoUSD(now))
	}
	return map[string]any{
		"active_seconds": activeSeconds,
		"cache_creation": map[string]any{
			"ephemeral_1h_input_tokens": s.Usage.CacheCreation.Ephemeral1hInputTokens,
			"ephemeral_5m_input_tokens": s.Usage.CacheCreation.Ephemeral5mInputTokens,
		},
		"cache_read_input_tokens": s.Usage.CacheReadInputTokens,
		"input_tokens":            s.Usage.InputTokens,
		"list_cost":               listCost,
		"output_tokens":           s.Usage.OutputTokens,
		"server_tool_use": map[string]any{
			"web_fetch_requests":  s.Usage.ServerToolUse.WebFetchRequests,
			"web_search_requests": s.Usage.ServerToolUse.WebSearchRequests,
		},
	}
}

func (s Session) UsageEventPayload(now time.Time) map[string]any {
	return map[string]any{"usage": s.UsageJSON(now), "budget": s.BudgetJSON()}
}

func (s Session) BudgetReached(now time.Time) bool {
	if s.Budget == nil {
		return false
	}
	if !s.ListCostKnown {
		return true
	}
	return s.ObservableListCostNanoUSD(now) >= s.Budget.MaxListCostCents*NanoUSDPerCent
}

// RuntimeListCostNanoUSD prices de-duplicated Session active time. Session
// timestamps have microsecond precision, so rounding once to nano-dollars is
// more precise than the public whole-cent projection by several orders of
// magnitude.
func RuntimeListCostNanoUSD(activeSeconds float64) int64 {
	if activeSeconds <= 0 {
		return 0
	}
	return int64(math.Round(activeSeconds * float64(runtimeNanoUSDPerHour) / float64(time.Hour/time.Second)))
}

type modelListPrice struct {
	inputPerToken     int64
	outputPerToken    int64
	usRegionSurcharge bool
	fastEligible      bool
}

var datedModelSuffix = regexp.MustCompile(`^(.*?)(?:-[0-9]{8})?$`)

// ModelUsageListCostNanoUSD applies Anthropic's public Messages list rates to
// provider-reported usage. Unknown/router-defined model names are rejected for
// budgeted Sessions rather than assigned a guessed fallback price.
func ModelUsageListCostNanoUSD(
	model Model,
	usage TokenUsage,
) (int64, error) {
	return ModelUsageListCostNanoUSDAt(model, usage, time.Now())
}

// ModelUsageListCostNanoUSDAt accepts the request timestamp so future catalog
// changes can preserve the price in effect when the provider request completed.
func ModelUsageListCostNanoUSDAt(
	model Model,
	usage TokenUsage,
	_ time.Time,
) (int64, error) {
	canonicalID := canonicalAnthropicModelID(model.ID)
	price, ok := anthropicModelListPrice(canonicalID)
	if !ok {
		return 0, Validation("model has no known Anthropic public list price: " + model.ID)
	}
	inputRate := price.inputPerToken
	outputRate := price.outputPerToken
	if usage.Speed == "fast" {
		if !price.fastEligible {
			return 0, Validation("model has no known Anthropic fast-mode list price: " + model.ID)
		}
		// Fast mode for currently supported Opus models is $10/$50 per MTok.
		inputRate, outputRate = 10_000, 50_000
	}
	regionNumerator, regionDenominator := int64(1), int64(1)
	if strings.EqualFold(usage.ProviderRegion, "us") && price.usRegionSurcharge {
		regionNumerator, regionDenominator = 11, 10
	}
	scaled := func(tokens, rate, numerator, denominator int64) int64 {
		return tokens * rate * numerator / denominator
	}
	cost := scaled(usage.InputTokens, inputRate, regionNumerator, regionDenominator)
	cost += scaled(usage.OutputTokens, outputRate, regionNumerator, regionDenominator)
	cost += scaled(usage.CacheCreation.Ephemeral5mInputTokens, inputRate*5, regionNumerator, regionDenominator*4)
	cost += scaled(usage.CacheCreation.Ephemeral1hInputTokens, inputRate*2, regionNumerator, regionDenominator)
	cost += scaled(usage.CacheReadInputTokens, inputRate, regionNumerator, regionDenominator*10)
	cost += usage.ServerToolUse.WebSearchRequests * webSearchRequestNanoUSD
	return cost, nil
}

// ModelResponseListCostNanoUSDAt applies response-level billing rules that
// cannot be inferred from token counters alone. Fable refusals are successful
// provider responses and remain observable usage, but Anthropic does not bill
// the refused request.
func ModelResponseListCostNanoUSDAt(
	model Model,
	usage TokenUsage,
	stopReason string,
	at time.Time,
) (int64, error) {
	if canonicalAnthropicModelID(model.ID) == "claude-fable-5" && stopReason == "refusal" {
		return 0, nil
	}
	return ModelUsageListCostNanoUSDAt(model, usage, at)
}

func HasAnthropicPublicListPrice(model Model) bool {
	_, ok := anthropicModelListPrice(model.ID)
	return ok
}

func anthropicModelListPrice(id string) (modelListPrice, bool) {
	id = canonicalAnthropicModelID(id)
	switch id {
	case "claude-fable-5", "claude-mythos-5":
		return modelListPrice{inputPerToken: 10_000, outputPerToken: 50_000, usRegionSurcharge: true}, true
	case "claude-opus-5":
		return modelListPrice{inputPerToken: 5_000, outputPerToken: 25_000, usRegionSurcharge: true, fastEligible: true}, true
	case "claude-sonnet-5":
		return modelListPrice{inputPerToken: 2_000, outputPerToken: 10_000, usRegionSurcharge: true}, true
	case "claude-opus-4-8":
		return modelListPrice{inputPerToken: 5_000, outputPerToken: 25_000, usRegionSurcharge: true, fastEligible: true}, true
	case "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5":
		return modelListPrice{inputPerToken: 5_000, outputPerToken: 25_000, usRegionSurcharge: id == "claude-opus-4-7" || id == "claude-opus-4-6"}, true
	case "claude-opus-4-1", "claude-opus-4":
		return modelListPrice{inputPerToken: 15_000, outputPerToken: 75_000}, true
	case "claude-sonnet-4-6":
		return modelListPrice{inputPerToken: 3_000, outputPerToken: 15_000, usRegionSurcharge: true}, true
	case "claude-sonnet-4-5", "claude-sonnet-4":
		return modelListPrice{inputPerToken: 3_000, outputPerToken: 15_000}, true
	case "claude-haiku-4-5":
		return modelListPrice{inputPerToken: 1_000, outputPerToken: 5_000}, true
	case "claude-3-5-haiku":
		return modelListPrice{inputPerToken: 800, outputPerToken: 4_000}, true
	default:
		return modelListPrice{}, false
	}
}

func canonicalAnthropicModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	match := datedModelSuffix.FindStringSubmatch(id)
	if len(match) == 2 {
		id = match[1]
	}
	return id
}
