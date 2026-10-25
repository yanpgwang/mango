package mango

import (
	"encoding/json"
	"testing"
)

func TestAutomaticPermissionEvaluationDecodesTypedOutcomes(t *testing.T) {
	for _, fixture := range []struct{ body, decision, reason string }{
		{`{"type":"auto","evaluated_permission":{"type":"allow"}}`, "allow", ""},
		{`{"type":"auto","evaluated_permission":{"type":"ask","reason_code":"indeterminate"}}`, "ask", "indeterminate"},
		{`{"type":"auto","evaluated_permission":{"type":"deny","reason_code":"high_risk"}}`, "deny", "high_risk"},
	} {
		var evaluation ToolPermissionEvaluation
		if err := json.Unmarshal([]byte(fixture.body), &evaluation); err != nil {
			t.Fatal(err)
		}
		if evaluation.AutoEvaluation == nil {
			t.Fatalf("auto discriminator lost: %s", fixture.body)
		}
		decision := evaluation.AutoEvaluation.EvaluatedPermission
		switch fixture.decision {
		case "allow":
			if decision.AutomaticPermissionAllow == nil {
				t.Fatal("allow was not typed")
			}
		case "ask":
			if decision.AutomaticPermissionAsk == nil || decision.AutomaticPermissionAsk.ReasonCode != fixture.reason {
				t.Fatal("ask reason was not typed")
			}
		case "deny":
			if decision.AutomaticPermissionDeny == nil || decision.AutomaticPermissionDeny.ReasonCode != fixture.reason {
				t.Fatal("deny reason was not typed")
			}
		}
	}
}
