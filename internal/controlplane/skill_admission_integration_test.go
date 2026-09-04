package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

type controlplaneSkillInstructionLoader struct{}

func (controlplaneSkillInstructionLoader) LoadSkillInstructions(
	context.Context,
	domain.SkillVersion,
) ([]byte, error) {
	return []byte("---\nname: admission\ndescription: Admission Skill\n---\nUse it.\n"), nil
}

func TestPostgresHTTPSkillAdmissionUsesEffectiveAgentConfiguration(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	repo := pg.NewSkillRepository(fixture.store)
	now := fixture.clock.Now()
	skill := domain.Skill{
		ID: "skill_admission", DisplayTitle: "Admission", Source: "custom",
		CreatedAt: now, UpdatedAt: now,
	}
	version := controlplaneSkillVersion(skill.ID, "1", now, true)
	if err := repo.BeginSkill(ctx, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompleteVersion(ctx, skill.ID, version.Version, app.BlobInfo{}); err != nil {
		t.Fatal(err)
	}
	skills := app.NewSkillService(repo, nil, fixture.ids, fixture.clock)
	sessions := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil), fixture.ids, fixture.clock, skills,
	)
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(fixture.store), Agents: fixture.agentRepo,
		Environments: fixture.environmentRepo, Sessions: sessions,
		IDGenerator: fixture.ids, Clock: fixture.clock,
	})
	handler := httpapi.NewServer(httpapi.Deps{
		Agents:   app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock, skills),
		Envs:     app.NewEnvironmentService(fixture.environmentRepo, fixture.ids, fixture.clock),
		Sessions: sessions, Deployments: deployments,
	}, httpapi.Config{}).Handler()
	selfHostedID := createResource(t, handler, "/v1/environments",
		`{"name":"external","config":{"type":"self_hosted"}}`)
	cloudID := createResource(t, handler, "/v1/environments",
		`{"name":"managed","config":{"type":"cloud"}}`)
	const skillJSON = `[{"type":"custom","skill_id":"skill_admission","version":"1"}]`
	const toolJSON = `[{"type":"agent_toolset_20260401"}]`
	plainID := createResource(t, handler, "/v1/agents",
		`{"name":"plain","model":"claude-test","tools":`+toolJSON+`}`)
	skilledID := createResource(t, handler, "/v1/agents",
		`{"name":"skilled","model":"claude-test","tools":`+toolJSON+`,"skills":`+skillJSON+`}`)
	selfID := createResource(t, handler, "/v1/agents",
		`{"name":"self coordinator","model":"claude-test","tools":`+toolJSON+`,"skills":`+skillJSON+
			`,"multiagent":{"type":"coordinator","agents":[{"type":"self"}]}}`)
	peerID := createResource(t, handler, "/v1/agents",
		`{"name":"peer coordinator","model":"claude-test","tools":`+toolJSON+
			`,"multiagent":{"type":"coordinator","agents":[{"type":"agent","id":"`+skilledID+`","version":1}]}}`)
	quote := func(id string) string { return `"` + id + `"` }
	const initial = `[{"type":"user.message","content":[{"type":"text","text":"start"}]}]`
	for _, tc := range []struct {
		name         string
		agent        string
		cloud        bool
		cloudBundles bool
		omitInitial  bool
		wantError    string
	}{
		{name: "primary", agent: quote(skilledID)},
		{name: "idle Session accepts Skills", agent: quote(skilledID), omitInitial: true},
		{name: "pinned primary", agent: `{"type":"agent","id":"` + skilledID + `","version":1}`},
		{name: "override adds Skills", agent: `{"type":"agent_with_overrides","id":"` + plainID + `","skills":` + skillJSON + `}`},
		{name: "external roster", agent: quote(peerID)},
		{name: "clearing primary does not clear peer", agent: `{"type":"agent_with_overrides","id":"` + peerID + `","skills":[]}`},
		{name: "self roster", agent: quote(selfID)},
		{name: "override clears primary and self", agent: `{"type":"agent_with_overrides","id":"` + selfID + `","skills":[]}`},
		{name: "plain external", agent: quote(plainID)},
		{name: "external ignores cloud capability flag", agent: quote(skilledID), cloudBundles: true},
		{name: "cloud rejects without capability", agent: quote(skilledID), cloud: true, wantError: "custom Skills are unavailable for the configured cloud sandbox provider"},
		{name: "cloud accepts with capability", agent: quote(peerID), cloud: true, cloudBundles: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions.ConfigureCloudSkillBundles(tc.cloudBundles)
			environmentID := selfHostedID
			if tc.cloud {
				environmentID = cloudID
			}
			before := skillAdmissionCounts(t, fixture, selfHostedID)
			payload := `{"agent":` + tc.agent + `,"environment_id":"` + environmentID + `"`
			if !tc.omitInitial {
				payload += `,"initial_events":` + initial
			}
			response := request(t, handler, http.MethodPost, "/v1/sessions", payload+`}`)
			if tc.wantError != "" {
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("create -> %d: %s", response.Code, response.Body.String())
				}
				var body struct {
					Error struct{ Type, Message string }
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Type != "invalid_request_error" || body.Error.Message != tc.wantError {
					t.Fatalf("capability error = %+v", body.Error)
				}
				if after := skillAdmissionCounts(t, fixture, selfHostedID); after != before {
					t.Fatalf("rejected input left durable work: before=%v after=%v", before, after)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("create -> %d: %s", response.Code, response.Body.String())
			}
			var created struct{ ID string }
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if tc.cloud {
				return // Bundle execution is covered by the Docker Skill service test.
			}
			after := skillAdmissionCounts(t, fixture, selfHostedID)
			expected := [3]int{before[0] + 1, before[1] + 1, before[2] + 1}
			if tc.omitInitial {
				expected = [3]int{before[0] + 1, before[1], before[2]}
			}
			if after != expected {
				t.Fatalf("accepted input durable counts: before=%v after=%v want=%v", before, after, expected)
			}
			if tc.omitInitial {
				return
			}
			events, err := fixture.store.QueryEvents(ctx, created.ID, app.EventQuery{
				Limit: 10, Types: []string{domain.EvUserMessage},
			})
			if err != nil || len(events) != 1 {
				t.Fatalf("initial message = %+v, %v", events, err)
			}
			prepared, err := temporalpkg.NewActivities(
				nil, temporalpkg.NewStoreSource(fixture.store), nil, nil, fixture.ids,
			).WithSkillInstructionLoader(controlplaneSkillInstructionLoader{}).
				PrepareTurn(ctx, temporalpkg.PrepareTurnInput{SessionID: created.ID, TriggerEventID: events[0].ID})
			if err != nil || prepared.FatalError != "" {
				t.Fatalf("accepted external configuration failed preparation: %+v, %v", prepared, err)
			}
		})
	}

	t.Run("deployment Runs preserve self-hosted Skill pins", func(t *testing.T) {
		deploymentID := createResource(t, handler, "/v1/deployments",
			`{"name":"self-hosted Skills","agent":"`+skilledID+`","environment_id":"`+selfHostedID+
				`","initial_events":`+initial+`,"schedule":{"type":"cron","expression":"0 * * * *","timezone":"UTC"}}`)
		before := skillAdmissionCounts(t, fixture, selfHostedID)
		manual := request(t, handler, http.MethodPost, "/v1/deployments/"+deploymentID+"/run", "")
		if manual.Code != http.StatusOK {
			t.Fatalf("manual Run -> %d: %s", manual.Code, manual.Body.String())
		}
		var manualRun struct {
			SessionID *string `json:"session_id"`
			ErrorType string  `json:"error_type"`
		}
		if err := json.Unmarshal(manual.Body.Bytes(), &manualRun); err != nil ||
			manualRun.SessionID == nil || manualRun.ErrorType != "" {
			t.Fatalf("manual Run = %+v, %v", manualRun, err)
		}
		item, err := deployments.Get(ctx, deploymentID)
		if err != nil || item.Status != domain.DeploymentStatusActive || item.Schedule == nil {
			t.Fatalf("deployment after manual Run = %+v, %v", item, err)
		}
		scheduled, err := deployments.RunScheduled(
			ctx, deploymentID, item.Schedule.UpcomingRunsAt[0],
		)
		if err != nil || scheduled.SessionID == nil || scheduled.ErrorType != "" {
			t.Fatalf("scheduled Run = %+v, %v", scheduled, err)
		}
		item, err = deployments.Get(ctx, deploymentID)
		if err != nil || item.Status != domain.DeploymentStatusActive {
			t.Fatalf("deployment after scheduled Run = %+v, %v", item, err)
		}
		after := skillAdmissionCounts(t, fixture, selfHostedID)
		want := [3]int{before[0] + 2, before[1] + 2, before[2] + 2}
		if after != want {
			t.Fatalf("deployment Run durable counts: before=%v after=%v want=%v", before, after, want)
		}
	})
}

func skillAdmissionCounts(t *testing.T, fixture postgresFixture, environmentID string) [3]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := fixture.store.ListSessions(ctx, app.ListPage{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	work, err := pg.NewEnvironmentWorkRepository(fixture.store).ListWork(
		ctx, environmentID, app.EnvironmentWorkListQuery{Limit: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	wakeups, err := fixture.store.ListWakeupsForDelivery(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	return [3]int{len(sessions.Sessions), len(work.Work), len(wakeups)}
}
