package domain

import (
	"testing"
	"time"
)

func TestModelUsageListCostNanoUSD(t *testing.T) {
	usage := TokenUsage{
		InputTokens: 1_000, OutputTokens: 100,
		CacheCreation: CacheCreationUsage{
			Ephemeral5mInputTokens: 200,
			Ephemeral1hInputTokens: 300,
		},
		CacheReadInputTokens: 400,
		ServerToolUse:        ServerToolUsage{WebSearchRequests: 2, WebFetchRequests: 7},
		Speed:                "standard",
		ProviderRegion:       "us",
	}
	cost, err := ModelUsageListCostNanoUSD(
		Model{ID: "claude-opus-4-8-20260801"},
		usage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 33_145_000 {
		t.Fatalf("list cost = %d nanoUSD, want 33145000", cost)
	}
	if amount := MonetaryAmountJSON(cost)["amount"]; amount != "3" {
		t.Fatalf("rounded monetary amount = %v, want 3 cents", amount)
	}
	usage.ProviderRegion = ""
	baseCost, err := ModelUsageListCostNanoUSD(
		Model{ID: "claude-opus-4-8-20260801"}, usage,
	)
	if err != nil || baseCost != 31_950_000 {
		t.Fatalf("base list cost = %d nanoUSD, err=%v", baseCost, err)
	}
}

func TestModelUsageListCostRejectsUnknownAndUnsupportedReportedFastMode(t *testing.T) {
	if _, err := ModelUsageListCostNanoUSD(
		Model{ID: "router/claude"}, TokenUsage{InputTokens: 1},
	); err == nil {
		t.Fatal("unknown router alias received a guessed list price")
	}
	if _, err := ModelUsageListCostNanoUSD(
		Model{ID: "claude-sonnet-5"}, TokenUsage{InputTokens: 1, Speed: "fast"},
	); err == nil {
		t.Fatal("unsupported provider-reported fast mode received a guessed price")
	}
}

func TestFableRefusalKeepsUsageButHasNoListCost(t *testing.T) {
	cost, err := ModelResponseListCostNanoUSDAt(
		Model{ID: "claude-fable-5"},
		TokenUsage{InputTokens: 1_000},
		"refusal",
		time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
	)
	if err != nil || cost != 0 {
		t.Fatalf("refused Fable response cost = %d, err=%v", cost, err)
	}
}

func TestRuntimeListCostAndCentRounding(t *testing.T) {
	if got := RuntimeListCostNanoUSD(3600); got != 80_000_000 {
		t.Fatalf("one runtime hour = %d nanoUSD", got)
	}
	if got := MonetaryAmountJSON(5_000_000)["amount"]; got != "1" {
		t.Fatalf("half-cent rounding = %v, want 1", got)
	}
}

func TestSonnet5LaunchPriceRemainsStandard(t *testing.T) {
	model := Model{ID: "claude-sonnet-5"}
	usage := TokenUsage{InputTokens: 1_000, OutputTokens: 1_000}
	beforeOriginalDeadline, err := ModelUsageListCostNanoUSDAt(
		model, usage, time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC),
	)
	if err != nil || beforeOriginalDeadline != 12_000_000 {
		t.Fatalf("price before original deadline = %d, err=%v", beforeOriginalDeadline, err)
	}
	afterOriginalDeadline, err := ModelUsageListCostNanoUSDAt(
		model, usage, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	)
	if err != nil || afterOriginalDeadline != 12_000_000 {
		t.Fatalf("price after original deadline = %d, err=%v", afterOriginalDeadline, err)
	}
}

func TestSessionBudgetLifecycleCannotAddOrReAdd(t *testing.T) {
	without := updatableSession()
	limit := &SessionBudget{MaxListCostCents: 100}
	if _, _, err := without.ApplyUpdate(SessionUpdate{
		Budget: &SessionBudgetUpdate{Budget: limit},
	}); err == nil {
		t.Fatal("Session created without budget accepted one later")
	}

	with := updatableSession()
	with.Budget = &SessionBudget{MaxListCostCents: 50}
	changed, change, err := with.ApplyUpdate(SessionUpdate{
		Budget: &SessionBudgetUpdate{Budget: limit},
	})
	if err != nil || !change.Budget || changed.Budget.MaxListCostCents != 100 {
		t.Fatalf("change budget = %+v, change=%+v, err=%v", changed, change, err)
	}
	removed, change, err := changed.ApplyUpdate(SessionUpdate{
		Budget: &SessionBudgetUpdate{},
	})
	if err != nil || !change.Budget || removed.Budget != nil {
		t.Fatalf("remove budget = %+v, change=%+v, err=%v", removed, change, err)
	}
	if _, _, err := removed.ApplyUpdate(SessionUpdate{
		Budget: &SessionBudgetUpdate{Budget: limit},
	}); err == nil {
		t.Fatal("removed budget was re-added")
	}
}
