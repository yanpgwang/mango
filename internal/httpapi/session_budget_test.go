package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestParseSessionBudget(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		amount int64
		valid  bool
	}{
		{name: "omitted", raw: "", valid: true},
		{name: "null", raw: "null", valid: true},
		{name: "zero", raw: `{"type":"limit","max_list_cost":{"amount":"0","currency":"USD"}}`, valid: true},
		{name: "cents", raw: `{"type":"limit","max_list_cost":{"amount":"2500","currency":"USD"}}`, amount: 2500, valid: true},
		{name: "number", raw: `{"type":"limit","max_list_cost":{"amount":25,"currency":"USD"}}`},
		{name: "decimal", raw: `{"type":"limit","max_list_cost":{"amount":"25.0","currency":"USD"}}`},
		{name: "leading zero", raw: `{"type":"limit","max_list_cost":{"amount":"025","currency":"USD"}}`},
		{name: "negative", raw: `{"type":"limit","max_list_cost":{"amount":"-1","currency":"USD"}}`},
		{name: "currency", raw: `{"type":"limit","max_list_cost":{"amount":"25","currency":"EUR"}}`},
		{name: "unknown", raw: `{"type":"limit","max_list_cost":{"amount":"25","currency":"USD"},"extra":true}`},
		{name: "overflow", raw: `{"type":"limit","max_list_cost":{"amount":"922337203686","currency":"USD"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget, err := parseSessionBudget(json.RawMessage(test.raw))
			if !test.valid {
				if err == nil {
					t.Fatalf("parse accepted budget %+v", budget)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.raw == "" || test.raw == "null" {
				if budget != nil {
					t.Fatalf("empty budget = %+v", budget)
				}
				return
			}
			if budget == nil || budget.MaxListCostCents != test.amount {
				t.Fatalf("budget = %+v, want %d cents", budget, test.amount)
			}
		})
	}
}

func TestParseSessionBudgetUpdateDistinguishesOmittedAndNull(t *testing.T) {
	omitted, err := parseSessionBudgetUpdate(nil)
	if err != nil || omitted != nil {
		t.Fatalf("omitted update = %+v, err=%v", omitted, err)
	}
	cleared, err := parseSessionBudgetUpdate(json.RawMessage("null"))
	if err != nil || cleared == nil || cleared.Budget != nil {
		t.Fatalf("null update = %+v, err=%v", cleared, err)
	}
}

func TestBudgetedSessionRejectsUnknownModelWithoutHidingZeroCost(t *testing.T) {
	handler := NewTestHandler(t)
	agentID := createID(t, handler, "POST", "/v1/agents",
		`{"name":"router alias","model":"router/claude"}`)
	environmentID := createID(t, handler, "POST", "/v1/environments",
		`{"name":"cloud","config":{"type":"self_hosted"}}`)

	withoutBudget := do(handler, "POST", "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)
	if withoutBudget.Code != http.StatusOK {
		t.Fatalf("unbudgeted unknown model -> %d: %s", withoutBudget.Code, withoutBudget.Body)
	}
	usage, _ := decodeBody(t, withoutBudget.Body.Bytes())["usage"].(map[string]any)
	listCost, _ := usage["list_cost"].(map[string]any)
	if listCost["amount"] != "0" {
		t.Fatalf("zero-use list cost = %#v", usage["list_cost"])
	}

	withBudget := do(handler, "POST", "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"budget":{"type":"limit","max_list_cost":{"amount":"100","currency":"USD"}}}`)
	if withBudget.Code != http.StatusBadRequest {
		t.Fatalf("budgeted unknown model -> %d, want 400: %s", withBudget.Code, withBudget.Body)
	}
}
