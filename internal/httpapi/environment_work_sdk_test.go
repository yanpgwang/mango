package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestAnthropicSDKResearch_EnvironmentWorkSurface(t *testing.T) {
	t.Parallel()
	service := newSDKEnvironmentWorkService()
	server := httptest.NewServer(NewServer(Deps{EnvironmentWork: service}, Config{
		RequireAuth: true,
	}).Handler())
	t.Cleanup(server.Close)
	client := anthropic.NewClient(
		option.WithBaseURL(server.URL+"/"), option.WithAuthToken("test-key"),
	)
	ctx := context.Background()

	got, err := client.Beta.Environments.Work.Get(ctx, service.work.ID, anthropic.BetaEnvironmentWorkGetParams{
		EnvironmentID: service.work.EnvironmentID,
	})
	if err != nil || got.ID != service.work.ID || got.Data.ID != service.work.SessionID ||
		got.Type != "work" || got.Secret != "" {
		t.Fatalf("Get Work = %+v, err=%v", got, err)
	}
	assertRawObjectHasFields(t, got.RawJSON(),
		"id", "acknowledged_at", "created_at", "data", "environment_id",
		"latest_heartbeat_at", "metadata", "secret", "started_at", "state",
		"stop_requested_at", "stopped_at", "type",
	)

	updated, err := client.Beta.Environments.Work.Update(ctx, service.work.ID, anthropic.BetaEnvironmentWorkUpdateParams{
		EnvironmentID: service.work.EnvironmentID,
		BetaSelfHostedWorkUpdateRequest: anthropic.BetaSelfHostedWorkUpdateRequestParam{
			Metadata: map[string]string{"worker_pool": "gpu"},
		},
	})
	if err != nil || updated.Metadata["worker_pool"] != "gpu" {
		t.Fatalf("Update Work = %+v, err=%v", updated, err)
	}
	nullPatch, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		server.URL+"/v1/environments/"+service.work.EnvironmentID+"/work/"+service.work.ID,
		bytes.NewBufferString(`{"metadata":{"worker_pool":null}}`))
	nullPatch.Header.Set("content-type", "application/json")
	nullPatch.Header.Set("authorization", "Bearer test-key")
	nullResponse, err := http.DefaultClient.Do(nullPatch)
	if err != nil {
		t.Fatalf("raw null metadata patch: %v", err)
	}
	_ = nullResponse.Body.Close()
	service.mu.Lock()
	_, metadataStillPresent := service.work.Metadata["worker_pool"]
	service.mu.Unlock()
	if nullResponse.StatusCode != http.StatusOK || metadataStillPresent {
		t.Fatalf("raw null metadata patch status=%d still_present=%t",
			nullResponse.StatusCode, metadataStillPresent)
	}

	page, err := client.Beta.Environments.Work.List(ctx, service.work.EnvironmentID, anthropic.BetaEnvironmentWorkListParams{
		Limit: param.NewOpt(int64(20)),
	})
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != service.work.ID {
		t.Fatalf("List Work = %+v, err=%v", page, err)
	}

	polled, err := client.Beta.Environments.Work.Poll(ctx, service.work.EnvironmentID, anthropic.BetaEnvironmentWorkPollParams{
		BlockMs: param.NewOpt(int64(1)),
	})
	if err != nil || polled.ID != service.work.ID || polled.Secret != service.secret {
		t.Fatalf("Poll Work = %+v, err=%v", polled, err)
	}
	acked, err := client.Beta.Environments.Work.Ack(ctx, service.work.ID, anthropic.BetaEnvironmentWorkAckParams{
		EnvironmentID: service.work.EnvironmentID,
	})
	if err != nil || acked.State != anthropic.BetaSelfHostedWorkStateStarting || acked.Secret != "" {
		t.Fatalf("Ack Work = %+v, err=%v", acked, err)
	}
	heartbeat, err := client.Beta.Environments.Work.Heartbeat(ctx, service.work.ID, anthropic.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID: service.work.EnvironmentID, DesiredTTLSeconds: param.NewOpt(int64(30)),
		ExpectedLastHeartbeat: param.NewOpt("NO_HEARTBEAT"),
	})
	if err != nil || !heartbeat.LeaseExtended || heartbeat.TTLSeconds != 30 || heartbeat.Type != "work_heartbeat" {
		t.Fatalf("Heartbeat Work = %+v, err=%v", heartbeat, err)
	}
	stats, err := client.Beta.Environments.Work.Stats(ctx, service.work.EnvironmentID, anthropic.BetaEnvironmentWorkStatsParams{})
	if err != nil || stats.Type != "work_queue_stats" || stats.WorkersPolling != 1 {
		t.Fatalf("Stats Work = %+v, err=%v", stats, err)
	}

	// Anthropic's server returns 204 for Stop even though the generated method
	// currently declares a Work JSON response. The official WorkPoller uses the
	// same response-body bypass until that SDK spec discrepancy is corrected.
	var raw *http.Response
	_, err = client.Beta.Environments.Work.Stop(ctx, service.work.ID, anthropic.BetaEnvironmentWorkStopParams{
		EnvironmentID: service.work.EnvironmentID,
		BetaSelfHostedWorkStopRequest: anthropic.BetaSelfHostedWorkStopRequestParam{
			Force: param.NewOpt(true),
		},
	}, option.WithResponseBodyInto(&raw))
	if err != nil || raw == nil || raw.StatusCode != http.StatusNoContent {
		t.Fatalf("Stop Work status=%v err=%v", raw, err)
	}
}

func TestEnvironmentWorkPollUsesWorkerIDQuery(t *testing.T) {
	service := newSDKEnvironmentWorkService()
	server := httptest.NewServer(NewServer(Deps{EnvironmentWork: service}, Config{
		RequireAuth: true,
	}).Handler())
	t.Cleanup(server.Close)

	target := server.URL + "/v1/environments/" + service.work.EnvironmentID +
		"/work/poll?worker_id=worker-local"
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("authorization", "Bearer test-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d, want 200", response.StatusCode)
	}
	service.mu.Lock()
	workerID := service.workerID
	service.mu.Unlock()
	if workerID != "worker-local" {
		t.Fatalf("worker ID = %q, want worker-local", workerID)
	}

	invalid, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		server.URL+"/v1/environments/"+service.work.EnvironmentID+"/work/poll?worker_id=", nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Header.Set("authorization", "Bearer test-key")
	invalidResponse, err := server.Client().Do(invalid)
	if err != nil {
		t.Fatal(err)
	}
	_ = invalidResponse.Body.Close()
	if invalidResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty worker_id status = %d, want 400", invalidResponse.StatusCode)
	}
}

func TestEnvironmentWorkHeartbeatRejectsOversizedTTL(t *testing.T) {
	service := newSDKEnvironmentWorkService()
	server := httptest.NewServer(NewServer(Deps{EnvironmentWork: service}, Config{
		RequireAuth: true,
	}).Handler())
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		server.URL+"/v1/environments/"+service.work.EnvironmentID+"/work/"+
			service.work.ID+"/heartbeat?desired_ttl_seconds=301", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("authorization", "Bearer test-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized TTL status = %d, want 400", response.StatusCode)
	}
}

type sdkEnvironmentWorkService struct {
	mu        sync.Mutex
	work      domain.EnvironmentWork
	polls     int
	workerID  string
	acks      int
	stops     int
	heartbeat time.Time
	secret    string
}

func newSDKEnvironmentWorkService() *sdkEnvironmentWorkService {
	now := time.Date(2026, 8, 9, 12, 0, 0, 123000000, time.UTC)
	return &sdkEnvironmentWorkService{secret: "worksec_sdk", work: domain.EnvironmentWork{
		ID: "work_sdk", EnvironmentID: "env_self_hosted", SessionID: "sesn_sdk",
		State: domain.EnvironmentWorkQueued, Metadata: map[string]string{},
		CreatedAt: now, TTLSeconds: 30,
	}}
}

func (s *sdkEnvironmentWorkService) Get(
	context.Context, string, string,
) (domain.EnvironmentWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.work, nil
}

func (s *sdkEnvironmentWorkService) Update(
	_ context.Context, _, _ string, metadata map[string]*string,
) (domain.EnvironmentWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range metadata {
		if value == nil {
			delete(s.work.Metadata, key)
		} else {
			s.work.Metadata[key] = *value
		}
	}
	return s.work, nil
}

func (s *sdkEnvironmentWorkService) List(
	context.Context, string, app.EnvironmentWorkListQuery,
) (app.EnvironmentWorkListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return app.EnvironmentWorkListPage{Work: []domain.EnvironmentWork{s.work}}, nil
}

func (s *sdkEnvironmentWorkService) Ack(
	context.Context, string, string,
) (domain.EnvironmentWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks++
	now := s.work.CreatedAt.Add(time.Second)
	s.work.State = domain.EnvironmentWorkStarting
	s.work.AcknowledgedAt = &now
	return s.work, nil
}

func (s *sdkEnvironmentWorkService) Heartbeat(
	context.Context, string, string, *string, *int64,
) (domain.EnvironmentWorkHeartbeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeat = s.work.CreatedAt.Add(2 * time.Second)
	s.work.State = domain.EnvironmentWorkActive
	s.work.LatestHeartbeatAt = &s.heartbeat
	return domain.EnvironmentWorkHeartbeat{
		LastHeartbeat: s.heartbeat, LeaseExtended: true,
		State: domain.EnvironmentWorkActive, TTLSeconds: 30,
	}, nil
}

func (s *sdkEnvironmentWorkService) Poll(
	_ context.Context, _ string, workerID string, _ time.Duration, _ *time.Duration,
) (*domain.EnvironmentWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerID = workerID
	s.polls++
	if s.polls > 1 {
		return nil, nil
	}
	work := s.work
	work.Secret = s.secret
	return &work, nil
}

func (s *sdkEnvironmentWorkService) Stop(context.Context, string, string, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	s.work.State = domain.EnvironmentWorkStopped
	return nil
}

func (s *sdkEnvironmentWorkService) Stats(
	context.Context, string,
) (domain.EnvironmentWorkQueueStats, error) {
	return domain.EnvironmentWorkQueueStats{WorkersPolling: 1}, nil
}
