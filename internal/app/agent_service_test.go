package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func newAgentService(t *testing.T) *AgentService {
	t.Helper()
	return NewAgentService(newMemoryAgentRepository(), domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()})
}

func TestAgentService_CreateThenVersionedUpdate(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	a, err := s.Create(ctx, domain.Agent{Name: "a", Model: domain.Model{ID: "claude-opus-4-8"}})
	if err != nil || a.Version != 1 {
		t.Fatalf("create: %+v err=%v", a, err)
	}
	name := "b"
	up, err := s.Update(ctx, a.ID, domain.AgentPatch{Name: &name})
	if err != nil || up.Version != 2 || up.Name != "b" {
		t.Fatalf("update: %+v err=%v", up, err)
	}
	// no-op update returns same version
	noop, _ := s.Update(ctx, a.ID, domain.AgentPatch{})
	if noop.Version != 2 {
		t.Fatalf("no-op should stay v2, got %d", noop.Version)
	}
	// stale expected version -> conflict
	bad := 1
	if _, err := s.Update(ctx, a.ID, domain.AgentPatch{Name: &name, ExpectedVersion: &bad}); err == nil {
		t.Fatal("expected conflict on stale version")
	}
}

func TestAgentService_CreateValidation(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()

	// empty name
	_, err := s.Create(ctx, domain.Agent{})
	if err == nil {
		t.Fatal("expected validation error for empty name")
	}
	de, ok := err.(*domain.DomainError)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("expected DomainError KindValidation, got %v", err)
	}

	// name set but no model ID
	_, err = s.Create(ctx, domain.Agent{Name: "ok-name"})
	if err == nil {
		t.Fatal("expected validation error for empty model ID")
	}
	de, ok = err.(*domain.DomainError)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("expected DomainError KindValidation for missing model, got %v", err)
	}
}

func TestAgentService_ArchiveIdempotent(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()

	a, err := s.Create(ctx, domain.Agent{Name: "x", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Version != 1 {
		t.Fatalf("expected v1 after create, got %d", a.Version)
	}
	name := "x v2"
	current, err := s.Update(ctx, a.ID, domain.AgentPatch{Name: &name})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Archiving is lifecycle state, not a configuration change: it must keep
	// the current version and must not append to version history.
	ar1, err := s.Archive(ctx, a.ID)
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if ar1.ArchivedAt == nil {
		t.Fatal("ArchivedAt should be set after first archive")
	}
	if ar1.Version != current.Version {
		t.Fatalf("archive changed version: got %d want %d", ar1.Version, current.Version)
	}
	versionPage, err := s.Versions(ctx, a.ID, AgentVersionListQuery{})
	if err != nil {
		t.Fatalf("versions after archive: %v", err)
	}
	if len(versionPage.Versions) != 2 {
		t.Fatalf("archive appended configuration history: got %d versions, want 2", len(versionPage.Versions))
	}
	for _, version := range versionPage.Versions {
		if version.ArchivedAt == nil {
			t.Fatalf("version %d did not reflect resource archival", version.Version)
		}
	}

	// A second archive is idempotent.
	ar2, err := s.Archive(ctx, a.ID)
	if err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if ar2.ArchivedAt == nil {
		t.Fatal("ArchivedAt should still be set on second archive")
	}
	if ar2.Version != ar1.Version {
		t.Fatalf("version should not bump on idempotent archive: got %d want %d", ar2.Version, ar1.Version)
	}

	// Archived agents are read-only, including otherwise-no-op updates.
	if _, err := s.Update(ctx, a.ID, domain.AgentPatch{}); err == nil {
		t.Fatal("expected update of archived agent to fail")
	} else if de, ok := err.(*domain.DomainError); !ok || de.Kind != domain.KindValidation {
		t.Fatalf("expected validation error for archived update, got %v", err)
	}
}

func TestAgentService_MultiagentReplacementPersists(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	first, err := s.Create(ctx, domain.Agent{Name: "first", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, domain.Agent{Name: "second", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Create(ctx, domain.Agent{
		Name:  "coordinator",
		Model: domain.Model{ID: "m"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{
			{Type: "agent", ID: first.ID},
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Multiagent == nil || a.Multiagent.Type != "coordinator" ||
		a.Multiagent.Agents[0].Version != first.Version {
		t.Fatalf("create lost multiagent: %#v", a.Multiagent)
	}

	replacement := &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{
		{Type: "agent", ID: second.ID},
	}}
	updated, err := s.Update(ctx, a.ID, domain.AgentPatch{Multiagent: &domain.NullableMultiagent{Value: replacement}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(updated.Multiagent.Agents) != 1 || updated.Multiagent.Agents[0].ID != second.ID ||
		updated.Multiagent.Agents[0].Version != second.Version {
		t.Fatalf("replacement was not persisted: %#v", updated.Multiagent)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Multiagent == nil || got.Multiagent.Type != "coordinator" {
		t.Fatalf("stored multiagent missing: %#v", got.Multiagent)
	}

	updated, err = s.Update(ctx, a.ID, domain.AgentPatch{Multiagent: &domain.NullableMultiagent{}})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if updated.Multiagent != nil {
		t.Fatalf("clear retained multiagent: %#v", updated.Multiagent)
	}
	got, err = s.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got.Multiagent != nil {
		t.Fatalf("stored multiagent was not cleared: %#v", got.Multiagent)
	}
}

func TestAgentService_MultiagentPinsLatestAndRebindsSelf(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	peer, err := s.Create(ctx, domain.Agent{Name: "peer", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	peerName := "peer v2"
	peer, err = s.Update(ctx, peer.ID, domain.AgentPatch{Name: &peerName})
	if err != nil {
		t.Fatal(err)
	}

	coordinator, err := s.Create(ctx, domain.Agent{
		Name: "coordinator", Model: domain.Model{ID: "m"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{
			{Type: "agent", ID: peer.ID},
			{Type: "self"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := coordinator.Multiagent.Agents[0]; got.ID != peer.ID || got.Version != 2 {
		t.Fatalf("latest reference = %#v, want peer v2", got)
	}
	if got := coordinator.Multiagent.Agents[1]; got.ID != coordinator.ID || got.Version != 1 || got.Type != "agent" {
		t.Fatalf("self reference = %#v, want concrete coordinator v1", got)
	}

	peerName = "peer v3"
	if _, err := s.Update(ctx, peer.ID, domain.AgentPatch{Name: &peerName}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(ctx, coordinator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Multiagent.Agents[0].Version != 2 {
		t.Fatalf("coordinator roster drifted to peer v%d", stored.Multiagent.Agents[0].Version)
	}

	coordinatorName := "coordinator v2"
	updated, err := s.Update(ctx, coordinator.ID, domain.AgentPatch{Name: &coordinatorName})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.Multiagent.Agents[1].Version != 2 {
		t.Fatalf("updated coordinator/self versions = %d/%d, want 2/2", updated.Version, updated.Multiagent.Agents[1].Version)
	}

	selfOnly := &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "self"}}}
	selfCoordinator, err := s.Create(ctx, domain.Agent{
		Name: "recursive", Model: domain.Model{ID: "m"}, Multiagent: selfOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := s.Update(ctx, selfCoordinator.ID, domain.AgentPatch{
		Multiagent: &domain.NullableMultiagent{Value: selfOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	if noOp.Version != 1 || noOp.Multiagent.Agents[0].Version != 1 {
		t.Fatalf("semantic self no-op created version: %#v", noOp)
	}
}

func TestAgentService_MultiagentRejectsInvalidReferences(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	ordinary, err := s.Create(ctx, domain.Agent{Name: "ordinary", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := s.Create(ctx, domain.Agent{
		Name: "coordinator", Model: domain.Model{ID: "m"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "self"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.Create(ctx, domain.Agent{Name: "archived", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Archive(ctx, archived.ID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		entries []domain.AgentReference
	}{
		{name: "missing", entries: []domain.AgentReference{{Type: "agent", ID: "agent_missing"}}},
		{name: "archived", entries: []domain.AgentReference{{Type: "agent", ID: archived.ID}}},
		{name: "nested", entries: []domain.AgentReference{{Type: "agent", ID: coordinator.ID}}},
		{name: "duplicate", entries: []domain.AgentReference{
			{Type: "agent", ID: ordinary.ID}, {Type: "agent", ID: ordinary.ID, Version: 1},
		}},
		{name: "duplicate-self", entries: []domain.AgentReference{{Type: "self"}, {Type: "self"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Create(ctx, domain.Agent{
				Name: tc.name, Model: domain.Model{ID: "m"},
				Multiagent: &domain.Multiagent{Type: "coordinator", Agents: tc.entries},
			})
			if err == nil {
				t.Fatal("expected validation error")
			}
			var domainErr *domain.DomainError
			if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestAgentService_AdvisorResolvesLastAndValidatesRoster(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	peer, err := s.Create(ctx, domain.Agent{
		Name: "peer", Model: domain.Model{ID: "claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := s.Create(ctx, domain.Agent{
		Name: "coordinator", Model: domain.Model{ID: "claude-sonnet-5"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{
			{Type: "advisor", Model: "claude-opus-5"},
			{Type: "agent", ID: peer.ID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinator.Multiagent.Agents) != 2 ||
		coordinator.Multiagent.Agents[0].ID != peer.ID ||
		coordinator.Multiagent.Agents[1].Type != "advisor" ||
		coordinator.Multiagent.Agents[1].Model != "claude-opus-5" {
		t.Fatalf("resolved advisor roster = %#v", coordinator.Multiagent)
	}
	portable, err := s.Create(ctx, domain.Agent{
		Name: "portable-advisor", Model: domain.Model{ID: "router-executor"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "advisor", Model: "router-reviewer",
		}}},
	})
	if err != nil || portable.Multiagent.Advisor() == nil ||
		portable.Multiagent.Advisor().Model != "router-reviewer" {
		t.Fatalf("provider-neutral advisor = %#v, err=%v", portable.Multiagent, err)
	}

	cases := []domain.Agent{
		{
			Name: "duplicate", Model: domain.Model{ID: "claude-sonnet-5"},
			Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{
				{Type: "advisor", Model: "claude-opus-5"},
				{Type: "advisor", Model: "claude-opus-5"},
			}},
		},
		{
			Name: domain.AdvisorAgentName, Model: domain.Model{ID: "claude-sonnet-5"},
			Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
				Type: "advisor", Model: "claude-opus-5",
			}}},
		},
	}
	for _, candidate := range cases {
		if _, err := s.Create(ctx, candidate); err == nil {
			t.Fatalf("accepted invalid advisor Agent %q", candidate.Name)
		}
	}
}

func TestAgentService_LegacyMultiagentMustBeReplacedBeforeUpdate(t *testing.T) {
	repo := newMemoryAgentRepository()
	s := NewAgentService(repo, domain.NewSeqIDGen(), domain.FixedClock{T: time.Unix(1, 0).UTC()})
	var legacy domain.Multiagent
	if err := json.Unmarshal([]byte(`{"extension":true,"agents":[1]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	agent := domain.Agent{
		ID: "agent_legacy", Version: 1, Name: "legacy", Model: domain.NormalizeModel(domain.Model{ID: "m"}),
		Multiagent: &legacy, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	if err := repo.PutVersion(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	name := "changed"
	if _, err := s.Update(context.Background(), agent.ID, domain.AgentPatch{Name: &name}); err == nil {
		t.Fatal("legacy roster was carried into a new Agent Version")
	}
	cleared, err := s.Update(context.Background(), agent.ID, domain.AgentPatch{
		Multiagent: &domain.NullableMultiagent{},
	})
	if err != nil {
		t.Fatalf("clear legacy roster: %v", err)
	}
	if cleared.Version != 2 || cleared.Multiagent != nil {
		t.Fatalf("cleared legacy roster = %#v", cleared)
	}
}

func TestAgentService_MetadataConstraintsAndPostPatchValidation(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{name: "non-string", metadata: map[string]any{"key": 1}},
		{name: "long-key", metadata: map[string]any{strings.Repeat("k", 65): "value"}},
		{name: "long-value", metadata: map[string]any{"key": strings.Repeat("v", 513)}},
	}
	tooMany := make(map[string]any, 17)
	for i := 0; i < 17; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	cases = append(cases, struct {
		name     string
		metadata map[string]any
	}{name: "too-many", metadata: tooMany})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Create(ctx, domain.Agent{
				Name: "agent", Model: domain.Model{ID: "m"}, Metadata: tc.metadata,
			}); err == nil {
				t.Fatal("expected invalid metadata to fail")
			} else if de, ok := err.(*domain.DomainError); !ok || de.Kind != domain.KindValidation {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}

	full := make(map[string]any, 16)
	for i := 0; i < 16; i++ {
		full[fmt.Sprintf("k%d", i)] = "v"
	}
	a, err := s.Create(ctx, domain.Agent{
		Name: "agent", Model: domain.Model{ID: "m"}, Metadata: full,
	})
	if err != nil {
		t.Fatalf("create at metadata limit: %v", err)
	}
	if _, err := s.Update(ctx, a.ID, domain.AgentPatch{
		Metadata: map[string]any{"overflow": "v"},
	}); err == nil {
		t.Fatal("expected resulting 17-key metadata bag to fail")
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after rejected update: %v", err)
	}
	if got.Version != 1 || len(got.Metadata) != 16 {
		t.Fatalf("rejected metadata patch mutated state: v%d %#v", got.Version, got.Metadata)
	}

	updated, err := s.Update(ctx, a.ID, domain.AgentPatch{
		Metadata: map[string]any{"k0": nil, "replacement": "ok"},
	})
	if err != nil {
		t.Fatalf("delete-and-add patch at limit: %v", err)
	}
	if len(updated.Metadata) != 16 || updated.Metadata["replacement"] != "ok" {
		t.Fatalf("post-patch metadata validation/merge wrong: %#v", updated.Metadata)
	}
}

func TestAgentService_ConcurrentExpectedVersionOnlyOneCommits(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()
	agent, err := s.Create(ctx, domain.Agent{
		Name: "v1", Model: domain.Model{ID: "m"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	expected := 1
	start := make(chan struct{})
	type updateResult struct {
		agent domain.Agent
		err   error
	}
	results := make(chan updateResult, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			updated, err := s.Update(ctx, agent.ID, domain.AgentPatch{
				Name: &name, ExpectedVersion: &expected,
			})
			results <- updateResult{agent: updated, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for result := range results {
		if result.err == nil {
			successes++
			if result.agent.Version != 2 {
				t.Errorf("successful update version = %d, want 2", result.agent.Version)
			}
			continue
		}
		de, ok := result.err.(*domain.DomainError)
		if !ok || de.Kind != domain.KindConflict {
			t.Errorf("losing update error = %v, want conflict/HTTP 409", result.err)
			continue
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	versionPage, err := s.Versions(ctx, agent.ID, AgentVersionListQuery{})
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versionPage.Versions) != 2 || versionPage.Versions[1].Version != 2 {
		t.Fatalf("version history after concurrent update = %#v", versionPage.Versions)
	}
}

func TestAgentService_UpdateArchiveRaceCannotResurrectAgent(t *testing.T) {
	s := newAgentService(t)
	ctx := context.Background()

	for i := 0; i < 40; i++ {
		agent, err := s.Create(ctx, domain.Agent{
			Name: fmt.Sprintf("agent-%d", i), Model: domain.Model{ID: "m"},
		})
		if err != nil {
			t.Fatalf("iteration %d create: %v", i, err)
		}

		expected := 1
		updatedName := fmt.Sprintf("updated-%d", i)
		start := make(chan struct{})
		var updateErr, archiveErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, updateErr = s.Update(ctx, agent.ID, domain.AgentPatch{
				Name: &updatedName, ExpectedVersion: &expected,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, archiveErr = s.Archive(ctx, agent.ID)
		}()
		close(start)
		wg.Wait()

		if archiveErr != nil {
			t.Fatalf("iteration %d archive: %v", i, archiveErr)
		}
		if updateErr != nil {
			de, ok := updateErr.(*domain.DomainError)
			if !ok || (de.Kind != domain.KindValidation && de.Kind != domain.KindConflict) {
				t.Fatalf("iteration %d update error = %v", i, updateErr)
			}
		}

		latest, err := s.Get(ctx, agent.ID)
		if err != nil {
			t.Fatalf("iteration %d get: %v", i, err)
		}
		if latest.ArchivedAt == nil {
			t.Fatalf("iteration %d race left latest v%d unarchived", i, latest.Version)
		}
		versionPage, err := s.Versions(ctx, agent.ID, AgentVersionListQuery{})
		if err != nil {
			t.Fatalf("iteration %d versions: %v", i, err)
		}
		for _, version := range versionPage.Versions {
			if version.ArchivedAt == nil {
				t.Fatalf("iteration %d race left v%d unarchived: %#v", i, version.Version, versionPage.Versions)
			}
		}
	}
}
