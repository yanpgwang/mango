package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestOfficialGoSDKDeploymentSurface(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	next := now.Add(time.Hour)
	service := &sdkDeploymentService{item: domain.Deployment{
		ID: "depl_sdk", AgentID: "agent_sdk", AgentVersion: 3,
		EnvironmentID: "env_sdk", Name: "SDK deployment", Description: "scheduled",
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "Run the check",
			}}},
		}},
		Metadata: map[string]string{"team": "runtime"}, Status: domain.DeploymentStatusActive,
		Schedule: &domain.DeploymentSchedule{
			Expression: "0 * * * *", Timezone: "UTC", UpcomingRunsAt: []time.Time{next},
		},
		CreatedAt: now, UpdatedAt: now,
	}}
	service.run = domain.DeploymentRun{
		ID: "drun_sdk", DeploymentID: service.item.ID,
		AgentID: service.item.AgentID, AgentVersion: service.item.AgentVersion,
		SessionID: stringPointer("sesn_sdk"), TriggerType: domain.DeploymentTriggerManual,
		CreatedAt: now,
	}
	server := httptest.NewServer(NewServer(Deps{Deployments: service}, Config{}).Handler())
	t.Cleanup(server.Close)
	client := anthropic.NewClient(
		option.WithBaseURL(server.URL+"/"), option.WithAuthToken("test-key"),
	)

	created, err := client.Beta.Deployments.New(context.Background(), anthropic.BetaDeploymentNewParams{
		Agent:         anthropic.BetaDeploymentNewParamsAgentUnion{OfString: anthropic.String("agent_sdk")},
		EnvironmentID: "env_sdk", Name: "SDK deployment",
		InitialEvents: []anthropic.BetaManagedAgentsDeploymentInitialEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: "Run the check",
					},
				}},
			},
		}},
		Schedule: anthropic.BetaManagedAgentsScheduleParams{
			Type:       anthropic.BetaManagedAgentsScheduleParamsTypeCron,
			Expression: "0 * * * *", Timezone: "UTC",
		},
		Budget: param.NullStruct[anthropic.BetaManagedAgentsBudgetLimitParam](),
	})
	if err != nil {
		t.Fatalf("create Deployment through SDK: %v", err)
	}
	if created.ID != service.item.ID || created.Agent.Version != 3 ||
		created.Schedule.Expression != "0 * * * *" {
		t.Fatalf("created Deployment = %+v", created)
	}
	if created.JSON.Budget.Raw() != "null" {
		t.Fatalf("created Deployment budget = %q, want null", created.JSON.Budget.Raw())
	}
	limit := anthropic.BetaManagedAgentsBudgetLimitParam{
		Type: anthropic.BetaManagedAgentsBudgetLimitTypeLimit,
		MaxListCost: anthropic.BetaMonetaryAmountParam{
			Amount: "2500", Currency: anthropic.BetaCurrencyUsd,
		},
	}
	budgeted, err := client.Beta.Deployments.New(context.Background(), anthropic.BetaDeploymentNewParams{
		Agent:         anthropic.BetaDeploymentNewParamsAgentUnion{OfString: anthropic.String("agent_sdk")},
		EnvironmentID: "env_sdk", Name: "Budgeted deployment", Budget: limit,
		InitialEvents: []anthropic.BetaManagedAgentsDeploymentInitialEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: "Run",
					},
				}},
			},
		}},
	})
	if err != nil || !strings.Contains(budgeted.RawJSON(), `"amount":"2500"`) {
		t.Fatalf("create budgeted Deployment: deployment=%+v err=%v", budgeted, err)
	}

	if _, err := client.Beta.Deployments.Get(
		context.Background(), service.item.ID, anthropic.BetaDeploymentGetParams{},
	); err != nil {
		t.Fatalf("get Deployment through SDK: %v", err)
	}
	if _, err := client.Beta.Deployments.Update(
		context.Background(), service.item.ID, anthropic.BetaDeploymentUpdateParams{
			Name: anthropic.String("Updated SDK deployment"),
		},
	); err != nil {
		t.Fatalf("update Deployment through SDK: %v", err)
	}
	if _, err := client.Beta.Deployments.Update(
		context.Background(), service.item.ID, anthropic.BetaDeploymentUpdateParams{
			Budget: param.NullStruct[anthropic.BetaManagedAgentsBudgetLimitParam](),
		},
	); err != nil {
		t.Fatalf("null Deployment budget update through SDK: %v", err)
	}
	updated, err := client.Beta.Deployments.Update(
		context.Background(), service.item.ID, anthropic.BetaDeploymentUpdateParams{Budget: limit},
	)
	if err != nil || !strings.Contains(updated.RawJSON(), `"amount":"2500"`) {
		t.Fatalf("reset Deployment budget: deployment=%+v err=%v", updated, err)
	}
	listed, err := client.Beta.Deployments.List(
		context.Background(), anthropic.BetaDeploymentListParams{Limit: anthropic.Int(20)},
	)
	if err != nil || len(listed.Data) != 1 {
		t.Fatalf("list Deployments through SDK: page=%+v err=%v", listed, err)
	}
	if _, err := client.Beta.Deployments.Pause(
		context.Background(), service.item.ID, anthropic.BetaDeploymentPauseParams{},
	); err != nil {
		t.Fatalf("pause Deployment through SDK: %v", err)
	}
	if _, err := client.Beta.Deployments.Unpause(
		context.Background(), service.item.ID, anthropic.BetaDeploymentUnpauseParams{},
	); err != nil {
		t.Fatalf("unpause Deployment through SDK: %v", err)
	}
	if _, err := client.Beta.Deployments.Archive(
		context.Background(), service.item.ID, anthropic.BetaDeploymentArchiveParams{},
	); err != nil {
		t.Fatalf("archive Deployment through SDK: %v", err)
	}
	run, err := client.Beta.Deployments.Run(
		context.Background(), service.item.ID, anthropic.BetaDeploymentRunParams{},
	)
	if err != nil || run.ID != service.run.ID || run.SessionID != "sesn_sdk" {
		t.Fatalf("run Deployment through SDK: run=%+v err=%v", run, err)
	}
	if _, err := client.Beta.DeploymentRuns.Get(
		context.Background(), service.run.ID, anthropic.BetaDeploymentRunGetParams{},
	); err != nil {
		t.Fatalf("get Deployment Run through SDK: %v", err)
	}
	runs, err := client.Beta.DeploymentRuns.List(
		context.Background(), anthropic.BetaDeploymentRunListParams{
			DeploymentID: anthropic.String(service.item.ID),
		},
	)
	if err != nil || len(runs.Data) != 1 || runs.Data[0].ID != service.run.ID {
		t.Fatalf("list Deployment Runs through SDK: page=%+v err=%v", runs, err)
	}
}

type sdkDeploymentService struct {
	item domain.Deployment
	run  domain.DeploymentRun
}

func (s *sdkDeploymentService) Create(
	_ context.Context,
	in app.DeploymentCreateInput,
) (domain.Deployment, error) {
	s.item.Budget = in.Budget
	s.item.Resources = in.Resources
	return s.item, nil
}

func (s *sdkDeploymentService) Get(context.Context, string) (domain.Deployment, error) {
	return s.item, nil
}

func (s *sdkDeploymentService) Update(
	_ context.Context,
	_ string,
	patch domain.DeploymentPatch,
) (domain.Deployment, error) {
	if patch.BudgetSet {
		s.item.Budget = patch.Budget
	}
	return s.item, nil
}

func (s *sdkDeploymentService) List(
	context.Context,
	app.DeploymentListQuery,
) (app.DeploymentListPage, error) {
	return app.DeploymentListPage{Deployments: []domain.Deployment{s.item}}, nil
}

func (s *sdkDeploymentService) Archive(context.Context, string) (domain.Deployment, error) {
	return s.item, nil
}

func (s *sdkDeploymentService) Pause(context.Context, string) (domain.Deployment, error) {
	item := s.item
	item.Status = domain.DeploymentStatusPaused
	item.PausedReason = &domain.DeploymentPausedReason{Type: "manual"}
	return item, nil
}

func (s *sdkDeploymentService) Unpause(context.Context, string) (domain.Deployment, error) {
	return s.item, nil
}

func (s *sdkDeploymentService) Run(context.Context, string) (domain.DeploymentRun, error) {
	return s.run, nil
}

func (s *sdkDeploymentService) GetRun(context.Context, string) (domain.DeploymentRun, error) {
	return s.run, nil
}

func (s *sdkDeploymentService) ListRuns(
	context.Context,
	app.DeploymentRunListQuery,
) (app.DeploymentRunListPage, error) {
	return app.DeploymentRunListPage{Runs: []domain.DeploymentRun{s.run}}, nil
}

func stringPointer(value string) *string { return &value }
