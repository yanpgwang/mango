package pg

import (
	"context"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestPostgresListAgentsPagesLatestVersionsOnly(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	agents := app.NewAgentService(NewAgentRepository(store), &seqIDGen{}, fixedClock{})

	created := make([]domain.Agent, 0, 5)
	for range 5 {
		agent, err := agents.Create(ctx, domain.Agent{
			Name: "coder", Model: domain.Model{ID: "claude-test"},
		})
		if err != nil {
			t.Fatalf("create agent: %v", err)
		}
		created = append(created, agent)
	}
	name := "coder-v2"
	if _, err := agents.Update(ctx, created[0].ID, domain.AgentPatch{Name: &name}); err != nil {
		t.Fatalf("update agent: %v", err)
	}

	seen := map[string]int{}
	versions := map[string]int{}
	var boundary *app.ResourcePageBoundary
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 5 {
			t.Fatal("pagination did not terminate")
		}
		page, err := store.ListAgents(ctx, app.AgentListQuery{Limit: 2, After: boundary})
		if err != nil {
			t.Fatalf("list agents: %v", err)
		}
		for _, agent := range page.Agents {
			seen[agent.ID]++
			versions[agent.ID] = agent.Version
		}
		if !page.HasNext {
			break
		}
		last := page.Agents[len(page.Agents)-1]
		boundary = &app.ResourcePageBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(seen) != len(created) {
		t.Fatalf("saw %d agents, want %d", len(seen), len(created))
	}
	for _, agent := range created {
		if seen[agent.ID] != 1 {
			t.Errorf("agent %s appeared %d times", agent.ID, seen[agent.ID])
		}
	}
	if versions[created[0].ID] != 2 {
		t.Fatalf("updated agent listed at version %d, want 2", versions[created[0].ID])
	}
}

func TestPostgresListAgentsFiltersAndOrder(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	agents := app.NewAgentService(NewAgentRepository(store), &seqIDGen{}, fixedClock{})

	first, err := agents.Create(ctx, domain.Agent{Name: "a", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := agents.Create(ctx, domain.Agent{Name: "b", Model: domain.Model{ID: "m"}})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	page, err := store.ListAgents(ctx, app.AgentListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := listedAgentIDs(page.Agents); len(got) != 2 || got[0] != second.ID || got[1] != first.ID {
		t.Fatalf("order = %v, want [%s %s]", got, second.ID, first.ID)
	}

	gte := second.CreatedAt
	page, err = store.ListAgents(ctx, app.AgentListQuery{Limit: 10, CreatedAtGte: &gte})
	if err != nil {
		t.Fatalf("created_at[gte]: %v", err)
	}
	if got := listedAgentIDs(page.Agents); len(got) != 1 || got[0] != second.ID {
		t.Fatalf("created_at[gte] = %v, want %s", got, second.ID)
	}
	lte := first.CreatedAt
	page, err = store.ListAgents(ctx, app.AgentListQuery{Limit: 10, CreatedAtLte: &lte})
	if err != nil {
		t.Fatalf("created_at[lte]: %v", err)
	}
	if got := listedAgentIDs(page.Agents); len(got) != 1 || got[0] != first.ID {
		t.Fatalf("created_at[lte] = %v, want %s", got, first.ID)
	}

	if _, err := agents.Archive(ctx, first.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	page, err = store.ListAgents(ctx, app.AgentListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list after archive: %v", err)
	}
	if got := listedAgentIDs(page.Agents); len(got) != 1 || got[0] != second.ID {
		t.Fatalf("default list after archive = %v", got)
	}
	page, err = store.ListAgents(ctx, app.AgentListQuery{Limit: 10, IncludeArchived: true})
	if err != nil {
		t.Fatalf("include archived: %v", err)
	}
	if len(page.Agents) != 2 {
		t.Fatalf("include archived returned %d agents, want 2", len(page.Agents))
	}
}

func TestPostgresListAgentVersionsPaginationAndArchiveProjection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	agents := app.NewAgentService(NewAgentRepository(store), &seqIDGen{}, fixedClock{})

	agent, err := agents.Create(ctx, domain.Agent{
		Name: "coder v1", Model: domain.Model{ID: "claude-test"},
	})
	if err != nil {
		t.Fatalf("create Agent: %v", err)
	}
	for version := 2; version <= 5; version++ {
		name := "coder v" + itoa(int64(version))
		agent, err = agents.Update(ctx, agent.ID, domain.AgentPatch{Name: &name})
		if err != nil {
			t.Fatalf("create version %d: %v", version, err)
		}
	}

	var got []int
	afterVersion := 0
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 5 {
			t.Fatal("version pagination did not terminate")
		}
		page, err := agents.Versions(ctx, agent.ID, app.AgentVersionListQuery{
			AfterVersion: afterVersion, Limit: 2,
		})
		if err != nil {
			t.Fatalf("list versions: %v", err)
		}
		for _, version := range page.Versions {
			got = append(got, version.Version)
		}
		if !page.HasNext {
			break
		}
		afterVersion = page.Versions[len(page.Versions)-1].Version
	}
	if len(got) != 5 {
		t.Fatalf("listed versions = %v, want [1 2 3 4 5]", got)
	}
	for index, version := range got {
		if version != index+1 {
			t.Fatalf("listed versions = %v, want [1 2 3 4 5]", got)
		}
	}

	if _, err := agents.Archive(ctx, agent.ID); err != nil {
		t.Fatalf("archive Agent: %v", err)
	}
	page, err := agents.Versions(ctx, agent.ID, app.AgentVersionListQuery{Limit: 2})
	if err != nil {
		t.Fatalf("list archived versions: %v", err)
	}
	for _, version := range page.Versions {
		if version.ArchivedAt == nil {
			t.Fatalf("archived Agent version %d has no archived_at", version.Version)
		}
	}
}

func TestPostgresListEnvironmentsPaginationAndArchiveFilter(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	environments := app.NewEnvironmentService(
		NewEnvironmentRepository(store), &seqIDGen{}, fixedClock{},
	)

	created := make([]domain.Environment, 0, 5)
	for range 5 {
		environment, err := environments.Create(ctx, domain.Environment{
			Name: "self-hosted", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"},
		})
		if err != nil {
			t.Fatalf("create environment: %v", err)
		}
		created = append(created, environment)
	}

	seen := map[string]int{}
	var boundary *app.ResourcePageBoundary
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 5 {
			t.Fatal("pagination did not terminate")
		}
		page, err := store.ListEnvironments(ctx, app.EnvironmentListQuery{
			Limit: 2, After: boundary,
		})
		if err != nil {
			t.Fatalf("list environments: %v", err)
		}
		for _, environment := range page.Environments {
			seen[environment.ID]++
		}
		if !page.HasNext {
			break
		}
		last := page.Environments[len(page.Environments)-1]
		boundary = &app.ResourcePageBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	for _, environment := range created {
		if seen[environment.ID] != 1 {
			t.Errorf("environment %s appeared %d times", environment.ID, seen[environment.ID])
		}
	}

	if _, err := environments.Archive(ctx, created[0].ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	page, err := store.ListEnvironments(ctx, app.EnvironmentListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list after archive: %v", err)
	}
	if len(page.Environments) != 4 {
		t.Fatalf("default list returned %d environments, want 4", len(page.Environments))
	}
	page, err = store.ListEnvironments(ctx, app.EnvironmentListQuery{
		Limit: 10, IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("include archived: %v", err)
	}
	if len(page.Environments) != 5 {
		t.Fatalf("include archived returned %d environments, want 5", len(page.Environments))
	}
}

func listedAgentIDs(agents []domain.Agent) []string {
	ids := make([]string, len(agents))
	for index, agent := range agents {
		ids[index] = agent.ID
	}
	return ids
}
