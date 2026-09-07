package mango

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestTeamInputsEncodePublicContract(t *testing.T) {
	request := AgentCreateRequest{
		Name: "lead", Model: ModelID("executor"), System: SomePtr("Delegate reviews."),
		Multiagent: Some(Coordinator(RosterAgent("agent_latest"),
			RosterAgentVersion("agent_reviewer", 3), Self(), Advisor("review-model"))),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"name":"lead","model":"executor","system":"Delegate reviews.","multiagent":{"type":"coordinator","agents":["agent_latest",{"type":"agent","id":"agent_reviewer","version":3},{"type":"self"},{"type":"advisor","model":"review-model"}]}}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encoded team: %s", encoded)
	}
	for _, test := range []struct {
		input any
		want  string
	}{
		{AgentID("agent_lead"), `"agent_lead"`},
		{AgentVersion("agent_lead", 2), `{"id":"agent_lead","type":"agent","version":2}`},
		{ModelSettings(ModelInputObject{ID: "model", Speed: Some("standard")}), `{"id":"model","speed":"standard"}`},
		{UserMessage("Review this"), `{"content":[{"text":"Review this","type":"text"}],"type":"user.message"}`},
	} {
		encoded, err := json.Marshal(test.input)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != test.want {
			t.Errorf("got %s, want %s", encoded, test.want)
		}
	}
}

func TestScopedClientRebindsNestedResourceCredentials(t *testing.T) {
	var headers []string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/v1/sessions/sesn_1/threads/sthr_1/events" {
			t.Errorf("path: %s", r.URL.Path)
		}
		headers = append(headers, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"next_page":null}`))
	})
	scoped := client.withAPIKey("session-token")
	for _, c := range []*Client{scoped, client} {
		if _, err := c.Sessions.Threads.Events.List(context.Background(), "sesn_1", "sthr_1", ListSessionThreadEventsParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(headers, []string{"Bearer session-token", "Bearer test-secret"}) {
		t.Fatalf("credential isolation: %v", headers)
	}
}
