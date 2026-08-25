package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestDeploymentGitRepositoryResourceHTTPContract(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	service := &sdkDeploymentService{item: domain.Deployment{
		ID: "depl_repository", AgentID: "agent_repository", AgentVersion: 1,
		EnvironmentID: "env_repository", Name: "Repository audit",
		Metadata: map[string]string{}, Status: domain.DeploymentStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}}
	handler := NewServer(Deps{Deployments: service}, Config{}).Handler()
	response := do(handler, http.MethodPost, "/v1/deployments", `{
		"agent":"agent_repository",
		"environment_id":"env_repository",
		"name":"Repository audit",
		"initial_events":[{"type":"user.message","content":[{"type":"text","text":"audit"}]}],
		"resources":[{"type":"git_repository","url":"https://github.com/acme/widgets.git",
			"checkout":{"type":"commit","sha":"0123456789ABCDEF0123456789ABCDEF01234567"},
			"mount_path":"/workspace/audit"}]
	}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create Deployment -> %d: %s", response.Code, response.Body.String())
	}
	if len(service.item.Resources) != 1 {
		t.Fatalf("stored resources = %+v", service.item.Resources)
	}
	stored := service.item.Resources[0]
	if stored.Type != domain.SessionResourceTypeGitRepository ||
		stored.RepositoryURL != "https://github.com/acme/widgets.git" ||
		stored.RepositoryCheckoutType != domain.GitRepositoryCheckoutCommit ||
		stored.RepositoryCheckoutValue != "0123456789abcdef0123456789abcdef01234567" ||
		stored.MountPath == nil || *stored.MountPath != "/workspace/audit" {
		t.Fatalf("stored Git repository template = %+v", stored)
	}
	var body struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantResource := map[string]any{
		"type": "git_repository", "url": "https://github.com/acme/widgets.git",
		"checkout": map[string]any{
			"type": "commit", "sha": "0123456789abcdef0123456789abcdef01234567",
		},
		"mount_path": "/workspace/audit",
	}
	if len(body.Resources) != 1 || !reflect.DeepEqual(body.Resources[0], wantResource) {
		t.Fatalf("Deployment repository response = %#v", body.Resources)
	}
}

func TestDeploymentToJSONRedactsInternalInitialEventFields(t *testing.T) {
	item := domain.Deployment{InitialEvents: []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "public"}},
			"id":      "forged_event_id", "type": "forged.type",
			"processed_at":                      "forged timestamp",
			domain.InternalOutcomeRubricContent: "private rubric",
			domain.InternalFileMessageContents: map[string]any{
				"0": map[string]any{"content": "private File content"},
			},
		},
	}}}

	wire := deploymentToJSON(item)
	events, ok := wire["initial_events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("initial_events = %#v", wire["initial_events"])
	}
	event, ok := events[0].(map[string]any)
	if !ok || event["type"] != domain.EvUserMessage || event["content"] == nil {
		t.Fatalf("public initial event = %#v", events[0])
	}
	for _, key := range []string{
		"id", "processed_at", domain.InternalOutcomeRubricContent,
		domain.InternalFileMessageContents,
	} {
		if _, present := event[key]; present {
			t.Fatalf("private or forged field %q leaked: %#v", key, event)
		}
	}
}
