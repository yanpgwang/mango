package mango

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOptionalThreeStatesAndFalse(t *testing.T) {
	type payload struct {
		Absent Optional[string] `json:"absent,omitzero"`
		Null   Optional[string] `json:"null,omitzero"`
		False  Optional[bool]   `json:"false,omitzero"`
		Empty  Optional[string] `json:"empty,omitzero"`
		Zero   Optional[int64]  `json:"zero,omitzero"`
	}
	input := payload{Null: Null[string](), False: Some(false), Empty: Some(""), Zero: Some(int64(0))}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"null":null,"false":false,"empty":"","zero":0}` {
		t.Fatalf("states lost: %s", data)
	}
	var output payload
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	if output.Absent.IsSet() || !output.Null.IsNull() || !output.False.IsSet() {
		t.Fatalf("states lost: %#v", output)
	}
	if _, ok := output.Null.Get(); ok {
		t.Error("null reported as value")
	}
}

func TestTypedUnionRoundTripAndUnknownVariants(t *testing.T) {
	for _, raw := range []string{`"model-test"`, `{"id":"model-test","effort":"high","speed":"fast"}`} {
		var model ModelInput
		if err := json.Unmarshal([]byte(raw), &model); err != nil {
			t.Fatal(err)
		}
		if model.String == nil && model.Object == nil {
			t.Errorf("no typed variant for %s", raw)
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, []byte(raw), encoded)
	}
	var multiagent Multiagent
	if err := json.Unmarshal([]byte("null"), &multiagent); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(multiagent)
	if err != nil || string(data) != "null" {
		t.Fatalf("union null %s %v", data, err)
	}
	var event SessionEvent
	raw := []byte(`{"type":"future.event","new_field":[1,2]}`)
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, raw, data)
	if _, err := json.Marshal(ModelInput{String: Ptr("x"), Object: &ModelInputObject{ID: "y"}}); err == nil {
		t.Fatal("ambiguous union accepted")
	}
}

func TestTypedEventUnionDecodesConcreteVariant(t *testing.T) {
	raw := []byte(`{"id":"event_1","type":"agent.\u006dessage","processed_at":null,"content":[{"type":"text","text":"hello"}]}`)
	var event SessionEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event.AgentMessageEvent == nil || event.AgentMessageEvent.Content[0].Text != "hello" {
		t.Fatalf("event %#v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, raw, encoded)
}

func TestInvalidKnownVariantDoesNotLeavePartialDecode(t *testing.T) {
	raw := []byte(`{"id":"event_1","type":"agent.message","processed_at":null,"content":[{"type":"text","text":42}]}`)
	var event SessionEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event.AgentMessageEvent != nil || len(event.Raw) == 0 {
		t.Fatalf("partial variant retained: %#v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, raw, encoded)
}

func TestNestedEventStreamUnionKeepsPreviewVariantsTyped(t *testing.T) {
	for _, raw := range []string{
		`{"type":"event_start","event":{"id":"e1","type":"agent.message"}}`,
		`{"type":"event_delta","event_id":"e1","delta":{"type":"content_delta","index":0,"content":{"type":"text","text":"hello"}}}`,
		`{"type":"agent.message","id":"e1","processed_at":null,"content":[{"type":"text","text":"hello"}]}`,
	} {
		var frame EventStreamFrame
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.EventStart == nil && frame.EventDelta == nil && (frame.SessionEvent == nil || frame.SessionEvent.AgentMessageEvent == nil) {
			t.Fatalf("frame lost its concrete variant: %#v", frame)
		}
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, []byte(raw), encoded)
	}
}

func TestOpenJSONSchemaKeepsAdditionalFields(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)
	var schema CustomToolInputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || len(schema.AdditionalProperties) != 2 {
		t.Fatalf("schema %#v", schema)
	}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, raw, data)
	schema.AdditionalProperties["type"] = json.RawMessage(`"array"`)
	if _, err := json.Marshal(schema); err == nil {
		t.Fatal("allowed additional properties to overwrite typed field")
	}
}

func assertJSONEqual(t *testing.T, left, right []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON differs\n%s\n%s", left, right)
	}
}
