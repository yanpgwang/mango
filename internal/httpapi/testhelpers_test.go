package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

// The HTTP suite uses test-only fakes to exercise wire behavior. Durable
// behavior belongs to the PostgreSQL/Temporal integration suite; keeping these
// fakes in _test.go prevents them from becoming a second runtime backend.
func NewTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandler(t, Config{}, false)
}

func NewTestHandlerWithPreviews(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandler(t, Config{}, true)
}

func newTestHandler(t *testing.T, cfg Config, previews bool) http.Handler {
	t.Helper()
	handler, _ := newTestHandlerWithSessions(t, cfg, previews)
	return handler
}

// newTestHandlerWithSessions also exposes the session fake so a test can force
// a status the fake runtime never produces on its own, such as a session that
// is mid-turn.
func newTestHandlerWithSessions(
	t *testing.T,
	cfg Config,
	previews bool,
) (http.Handler, *testSessionService) {
	t.Helper()
	ids := domain.NewSeqIDGen()
	clock := domain.FixedClock{T: time.Unix(1000, 0).UTC()}
	agentsRepo := newTestAgentRepository()
	environmentsRepo := newTestEnvironmentRepository()
	skillResolver := newTestSkillResolver()
	agents := app.NewAgentService(agentsRepo, ids, clock, skillResolver)
	environments := app.NewEnvironmentService(
		environmentsRepo, ids, clock,
		app.EnvironmentCapabilities{PackageSetup: true, LimitedNetwork: true},
	)
	hub := app.NewHub(256)
	sessions := newTestSessionService(
		agentsRepo,
		environmentsRepo,
		ids,
		clock,
		hub,
		previews,
		skillResolver,
	)
	resources := &testSessionResourceService{sessions: sessions, ids: ids, clock: clock}
	return NewServer(Deps{
		Agents: agents, Envs: environments, Sessions: sessions,
		Events: sessions, Stream: hub, SessionResources: resources,
	}, cfg).Handler(), sessions
}

type testAgentRepository struct {
	mu       sync.Mutex
	versions map[string][]domain.Agent
}

func newTestAgentRepository() *testAgentRepository {
	return &testAgentRepository{versions: make(map[string][]domain.Agent)}
}

func (r *testAgentRepository) PutVersion(_ context.Context, agent domain.Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.versions[agent.ID] = append(r.versions[agent.ID], agent)
	return nil
}

func (r *testAgentRepository) UpdateVersion(
	_ context.Context,
	id string,
	update func(domain.Agent) (domain.Agent, bool, error),
) (domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.versions[id]
	if len(versions) == 0 {
		return domain.Agent{}, domain.NotFound("agent not found")
	}
	current := versions[len(versions)-1]
	next, changed, err := update(current)
	if err != nil {
		return domain.Agent{}, err
	}
	if !changed {
		return current, nil
	}
	next.Version = current.Version + 1
	r.versions[id] = append(versions, next)
	return next, nil
}

func (r *testAgentRepository) Archive(
	_ context.Context,
	id string,
	at time.Time,
) (domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.versions[id]
	if len(versions) == 0 {
		return domain.Agent{}, domain.NotFound("agent not found")
	}
	if versions[len(versions)-1].ArchivedAt == nil {
		for index := range versions {
			versions[index].ArchivedAt = &at
			versions[index].UpdatedAt = at
		}
		r.versions[id] = versions
	}
	return versions[len(versions)-1], nil
}

func (r *testAgentRepository) Latest(_ context.Context, id string) (domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.versions[id]
	if len(versions) == 0 {
		return domain.Agent{}, domain.NotFound("agent not found")
	}
	return versions[len(versions)-1], nil
}

func (r *testAgentRepository) GetVersion(
	_ context.Context,
	id string,
	version int,
) (domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, agent := range r.versions[id] {
		if agent.Version == version {
			return agent, nil
		}
	}
	return domain.Agent{}, domain.NotFound("agent version not found")
}

func (r *testAgentRepository) Versions(
	_ context.Context,
	id string,
	query app.AgentVersionListQuery,
) (app.AgentVersionListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.versions[id]
	if len(versions) == 0 {
		return app.AgentVersionListPage{}, domain.NotFound("agent not found")
	}
	pageVersions := make([]domain.Agent, 0, query.Limit+1)
	for _, version := range versions {
		if version.Version <= query.AfterVersion {
			continue
		}
		pageVersions = append(pageVersions, version)
		if query.Limit > 0 && len(pageVersions) > query.Limit {
			break
		}
	}
	page := app.AgentVersionListPage{Versions: pageVersions}
	if query.Limit > 0 && len(pageVersions) > query.Limit {
		page.Versions = pageVersions[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *testAgentRepository) ListLatest(
	_ context.Context,
	query app.AgentListQuery,
) (app.AgentListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agents := make([]domain.Agent, 0, len(r.versions))
	for _, versions := range r.versions {
		latest := versions[len(versions)-1]
		if !query.IncludeArchived && latest.ArchivedAt != nil {
			continue
		}
		if query.CreatedAtGte != nil && latest.CreatedAt.Before(*query.CreatedAtGte) {
			continue
		}
		if query.CreatedAtLte != nil && latest.CreatedAt.After(*query.CreatedAtLte) {
			continue
		}
		if query.After != nil &&
			(latest.CreatedAt.After(query.After.CreatedAt) ||
				(latest.CreatedAt.Equal(query.After.CreatedAt) && latest.ID >= query.After.ID)) {
			continue
		}
		agents = append(agents, latest)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].CreatedAt.After(agents[j].CreatedAt) ||
			(agents[i].CreatedAt.Equal(agents[j].CreatedAt) && agents[i].ID > agents[j].ID)
	})
	page := app.AgentListPage{Agents: agents}
	if query.Limit > 0 && len(agents) > query.Limit {
		page.Agents = agents[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

type testEnvironmentRepository struct {
	mu         sync.Mutex
	values     map[string]domain.Environment
	references map[string]int
}

func newTestEnvironmentRepository() *testEnvironmentRepository {
	return &testEnvironmentRepository{
		values: make(map[string]domain.Environment), references: make(map[string]int),
	}
}

func (r *testEnvironmentRepository) Put(
	_ context.Context,
	environment domain.Environment,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[environment.ID] = environment
	return nil
}

func (r *testEnvironmentRepository) Update(
	_ context.Context,
	environment domain.Environment,
) (domain.Environment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.values[environment.ID]
	if !ok {
		return domain.Environment{}, domain.NotFound("environment not found")
	}
	if current.ArchivedAt != nil {
		return domain.Environment{}, domain.Validation("archived environment is read-only")
	}
	environment.ArchivedAt = current.ArchivedAt
	r.values[environment.ID] = environment
	return environment, nil
}

func (r *testEnvironmentRepository) Archive(
	_ context.Context,
	id string,
	archivedAt time.Time,
) (domain.Environment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	environment, ok := r.values[id]
	if !ok {
		return domain.Environment{}, domain.NotFound("environment not found")
	}
	if environment.ArchivedAt == nil {
		environment.ArchivedAt = &archivedAt
		environment.UpdatedAt = archivedAt
		r.values[id] = environment
	}
	return environment, nil
}

func (r *testEnvironmentRepository) Get(
	_ context.Context,
	id string,
) (domain.Environment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	environment, ok := r.values[id]
	if !ok {
		return domain.Environment{}, domain.NotFound("environment not found")
	}
	return environment, nil
}

func (r *testEnvironmentRepository) List(
	_ context.Context,
	query app.EnvironmentListQuery,
) (app.EnvironmentListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	environments := make([]domain.Environment, 0, len(r.values))
	for _, environment := range r.values {
		if !query.IncludeArchived && environment.ArchivedAt != nil {
			continue
		}
		if query.After != nil &&
			(environment.CreatedAt.After(query.After.CreatedAt) ||
				(environment.CreatedAt.Equal(query.After.CreatedAt) &&
					environment.ID >= query.After.ID)) {
			continue
		}
		environments = append(environments, environment)
	}
	sort.Slice(environments, func(i, j int) bool {
		return environments[i].CreatedAt.After(environments[j].CreatedAt) ||
			(environments[i].CreatedAt.Equal(environments[j].CreatedAt) &&
				environments[i].ID > environments[j].ID)
	})
	page := app.EnvironmentListPage{Environments: environments}
	if query.Limit > 0 && len(environments) > query.Limit {
		page.Environments = environments[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *testEnvironmentRepository) DeleteIfUnreferenced(
	_ context.Context,
	id string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.values[id]; !ok {
		return domain.NotFound("environment not found")
	}
	if r.references[id] > 0 {
		return domain.Conflict("environment is referenced by a session")
	}
	delete(r.values, id)
	return nil
}

func (r *testEnvironmentRepository) addReference(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.references[id]++
}

func (r *testEnvironmentRepository) removeReference(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.references[id] > 0 {
		r.references[id]--
	}
}

type testSessionService struct {
	mu           sync.Mutex
	agents       *testAgentRepository
	environments *testEnvironmentRepository
	ids          domain.IDGenerator
	clock        domain.Clock
	hub          *app.Hub
	previews     bool
	skillRef     app.SkillReferenceResolver
	sessions     map[string]domain.Session
	events       map[string][]domain.Event
	sequences    map[string]int64
}

func newTestSessionService(
	agents *testAgentRepository,
	environments *testEnvironmentRepository,
	ids domain.IDGenerator,
	clock domain.Clock,
	hub *app.Hub,
	previews bool,
	skillRef app.SkillReferenceResolver,
) *testSessionService {
	return &testSessionService{
		agents: agents, environments: environments, ids: ids, clock: clock,
		hub: hub, previews: previews, skillRef: skillRef,
		sessions: make(map[string]domain.Session),
		events:   make(map[string][]domain.Event), sequences: make(map[string]int64),
	}
}

func (s *testSessionService) Create(
	ctx context.Context,
	input app.CreateSessionInput,
) (domain.Session, error) {
	if err := app.ValidateMetadata(input.Metadata); err != nil {
		return domain.Session{}, err
	}
	var (
		agent domain.Agent
		err   error
	)
	if input.AgentVersion != nil {
		agent, err = s.agents.GetVersion(ctx, input.AgentID, *input.AgentVersion)
	} else {
		agent, err = s.agents.Latest(ctx, input.AgentID)
	}
	if err != nil {
		return domain.Session{}, domain.Validation("agent not found")
	}
	if agent.ArchivedAt != nil {
		return domain.Session{}, domain.Validation("agent is archived")
	}
	environment, err := s.environments.Get(ctx, input.EnvironmentID)
	if err != nil {
		return domain.Session{}, domain.Validation("environment not found")
	}
	if environment.ArchivedAt != nil {
		return domain.Session{}, domain.Validation("environment is archived")
	}
	snapshot := agent
	if input.Overrides != nil {
		snapshot = snapshot.WithOverrides(*input.Overrides)
	}
	snapshot.Skills, err = app.ResolveAgentSkillReferences(ctx, s.skillRef, snapshot.Skills)
	if err != nil {
		return domain.Session{}, err
	}
	var roster []domain.Agent
	if agent.Multiagent != nil {
		roster = make([]domain.Agent, 0, len(agent.Multiagent.Agents))
		for _, reference := range agent.Multiagent.Agents {
			member := snapshot
			if reference.ID != agent.ID {
				member, err = s.agents.GetVersion(ctx, reference.ID, reference.Version)
				if err != nil || member.ArchivedAt != nil {
					return domain.Session{}, domain.Validation(
						"multiagent references an agent version that is missing or archived",
					)
				}
			}
			member.Multiagent = nil
			roster = append(roster, member)
		}
	}
	modelsPriceable := domain.HasAnthropicPublicListPrice(snapshot.Model)
	for _, member := range roster {
		modelsPriceable = modelsPriceable && domain.HasAnthropicPublicListPrice(member.Model)
	}
	if input.Budget != nil && !modelsPriceable {
		return domain.Session{}, domain.Validation(
			"budgeted sessions require every agent model to have a known Anthropic public list price",
		)
	}
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	now := s.clock.Now().UTC()
	session := domain.Session{
		ID: s.ids.NewID(domain.PrefixSession), AgentID: agent.ID,
		AgentVersion: agent.Version, AgentSnapshot: snapshot, MultiagentRoster: roster,
		EnvironmentID: environment.ID, EnvironmentType: environment.ConfigType,
		EnvironmentConfig: environment.SessionConfig(),
		Status:            domain.StatusIdle,
		Title:             input.Title, Metadata: metadata, CreatedAt: now, UpdatedAt: now,
		VaultIDs: append([]string(nil), input.VaultIDs...), ListCostKnown: true,
		Budget: input.Budget,
	}
	for _, inputResource := range input.Resources {
		mountPath, err := domain.NormalizeSessionFileMountPath(
			inputResource.FileID, inputResource.MountPath,
		)
		if err != nil {
			return domain.Session{}, err
		}
		session.Resources = append(session.Resources, domain.SessionResource{
			ID: s.ids.NewID(domain.PrefixSessionResource), SessionID: session.ID,
			SourceFileID: inputResource.FileID, FileID: s.ids.NewID(domain.PrefixFile),
			MountPath: mountPath, CreatedAt: now, UpdatedAt: now,
			State: domain.SessionResourceActive,
		})
	}
	for _, inputResource := range input.MemoryResources {
		name := "Project Memory"
		mountPath, err := domain.NormalizeSessionMemoryStoreMountPath(name)
		if err != nil {
			return domain.Session{}, err
		}
		session.Resources = append(session.Resources, domain.SessionResource{
			ID: s.ids.NewID(domain.PrefixSessionResource), SessionID: session.ID,
			ResourceType:           domain.SessionResourceTypeMemoryStore,
			MemoryStoreID:          inputResource.MemoryStoreID,
			MemoryAccess:           inputResource.Access,
			MemoryInstructions:     inputResource.Instructions,
			MemoryStoreName:        name,
			MemoryStoreDescription: "Shared project conventions.",
			MountPath:              mountPath, CreatedAt: now, UpdatedAt: now,
			State: domain.SessionResourceActive,
		})
	}
	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
	s.environments.addReference(environment.ID)
	if len(input.InitialEvents) > 0 {
		if _, err := s.SendEvent(ctx, session.ID, input.InitialEvents); err != nil {
			return domain.Session{}, err
		}
	}
	return session, nil
}

type testSkillResolver struct {
	mu      sync.Mutex
	latest  map[string]string
	missing map[string]bool
}

func newTestSkillResolver() *testSkillResolver {
	return &testSkillResolver{
		latest: map[string]string{}, missing: map[string]bool{"skill_missing": true},
	}
}

func (r *testSkillResolver) ResolveSkillReferences(
	_ context.Context,
	references []domain.SkillReference,
) ([]domain.SkillReference, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved := make([]domain.SkillReference, len(references))
	for index, reference := range references {
		if reference.Type != "custom" {
			return nil, domain.Unsupported("Anthropic-managed Skills are not supported")
		}
		if r.missing[reference.SkillID] {
			return nil, domain.Validation("custom Skill Version not found")
		}
		version := reference.Version
		if version == "" || version == "latest" {
			version = r.latest[reference.SkillID]
			if version == "" {
				version = "1759178010641129"
			}
		}
		resolved[index] = domain.SkillReference{
			Type: "custom", SkillID: reference.SkillID, Version: version,
		}
	}
	return resolved, nil
}

func (r *testSkillResolver) setLatest(skillID, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[skillID] = version
}

func (s *testSessionService) setLatestSkillVersion(skillID, version string) {
	if resolver, ok := s.skillRef.(*testSkillResolver); ok {
		resolver.setLatest(skillID, version)
	}
}

type testSessionResourceService struct {
	sessions *testSessionService
	ids      domain.IDGenerator
	clock    domain.Clock
}

func (s *testSessionResourceService) Add(
	_ context.Context,
	sessionID string,
	input app.FileSessionResourceInput,
) (domain.SessionResource, error) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	session, ok := s.sessions.sessions[sessionID]
	if !ok {
		return domain.SessionResource{}, domain.NotFound("session not found")
	}
	mountPath, err := domain.NormalizeSessionFileMountPath(input.FileID, input.MountPath)
	if err != nil {
		return domain.SessionResource{}, err
	}
	for _, existing := range session.Resources {
		if existing.MountPath == mountPath {
			return domain.SessionResource{}, domain.Conflict("mount_path is already in use")
		}
	}
	now := s.clock.Now().UTC()
	resource := domain.SessionResource{
		ID: s.ids.NewID(domain.PrefixSessionResource), SessionID: sessionID,
		SourceFileID: input.FileID, FileID: s.ids.NewID(domain.PrefixFile),
		MountPath: mountPath, CreatedAt: now, UpdatedAt: now,
		State: domain.SessionResourceActive,
	}
	session.Resources = append(session.Resources, resource)
	s.sessions.sessions[sessionID] = session
	return resource, nil
}

func (s *testSessionResourceService) Get(
	_ context.Context,
	sessionID string,
	resourceID string,
) (domain.SessionResource, error) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	session, ok := s.sessions.sessions[sessionID]
	if !ok {
		return domain.SessionResource{}, domain.NotFound("session not found")
	}
	for _, resource := range session.Resources {
		if resource.ID == resourceID {
			return resource, nil
		}
	}
	return domain.SessionResource{}, domain.NotFound("session resource not found")
}

func (s *testSessionResourceService) List(
	_ context.Context,
	sessionID string,
	query app.SessionResourceListQuery,
) (app.SessionResourceListPage, error) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	session, ok := s.sessions.sessions[sessionID]
	if !ok {
		return app.SessionResourceListPage{}, domain.NotFound("session not found")
	}
	start := 0
	if query.Boundary != nil {
		for index, resource := range session.Resources {
			if resource.ID == query.Boundary.ID {
				start = index + 1
				break
			}
		}
	}
	limit := query.Limit
	if limit == 0 {
		limit = len(session.Resources)
	}
	end := start + limit
	if end > len(session.Resources) {
		end = len(session.Resources)
	}
	page := app.SessionResourceListPage{
		Resources: append([]domain.SessionResource(nil), session.Resources[start:end]...),
	}
	page.HasMore = end < len(session.Resources)
	return page, nil
}

func (s *testSessionResourceService) Delete(
	_ context.Context,
	sessionID string,
	resourceID string,
) (domain.SessionResource, error) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	session, ok := s.sessions.sessions[sessionID]
	if !ok {
		return domain.SessionResource{}, domain.NotFound("session not found")
	}
	for index, resource := range session.Resources {
		if resource.ID != resourceID {
			continue
		}
		session.Resources = append(session.Resources[:index], session.Resources[index+1:]...)
		s.sessions.sessions[sessionID] = session
		return resource, nil
	}
	return domain.SessionResource{}, domain.NotFound("session resource not found")
}

func (s *testSessionService) Get(
	_ context.Context,
	id string,
) (domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return domain.Session{}, domain.NotFound("session not found")
	}
	return session, nil
}

func (s *testSessionService) List(
	_ context.Context,
	query app.ListPage,
) (app.SessionListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]domain.Session, 0, len(s.sessions))
	statuses := make(map[domain.Status]bool, len(query.Statuses))
	for _, status := range query.Statuses {
		statuses[status] = true
	}
	for _, session := range s.sessions {
		if !query.IncludeArchived && session.ArchivedAt != nil {
			continue
		}
		if query.AgentID != "" && session.AgentID != query.AgentID {
			continue
		}
		if query.AgentVersion != nil && session.AgentVersion != *query.AgentVersion {
			continue
		}
		if len(statuses) > 0 && !statuses[session.Status] {
			continue
		}
		if query.DeploymentID != nil || query.MemoryStoreID != nil {
			continue
		}
		if query.CreatedAtGt != nil && !session.CreatedAt.After(*query.CreatedAtGt) {
			continue
		}
		if query.CreatedAtGte != nil && session.CreatedAt.Before(*query.CreatedAtGte) {
			continue
		}
		if query.CreatedAtLt != nil && !session.CreatedAt.Before(*query.CreatedAtLt) {
			continue
		}
		if query.CreatedAtLte != nil && session.CreatedAt.After(*query.CreatedAtLte) {
			continue
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		less := sessions[i].CreatedAt.Before(sessions[j].CreatedAt) ||
			(sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) &&
				sessions[i].ID < sessions[j].ID)
		if query.Desc {
			return !less
		}
		return less
	})

	start, end := 0, len(sessions)
	if query.Boundary != nil {
		index := len(sessions)
		for candidate := range sessions {
			if sessions[candidate].ID == query.Boundary.ID &&
				sessions[candidate].CreatedAt.Equal(query.Boundary.CreatedAt) {
				index = candidate
				break
			}
		}
		if index < len(sessions) {
			if query.Boundary.Backward {
				end = index
				start = max(0, end-query.Limit)
			} else {
				start = index + 1
			}
		}
	}
	if end > start+query.Limit {
		end = start + query.Limit
	}
	page := append([]domain.Session(nil), sessions[start:end]...)
	return app.SessionListPage{
		Sessions: page,
		HasPrev:  start > 0,
		HasNext:  end < len(sessions),
	}, nil
}

func (s *testSessionService) SendEvent(
	_ context.Context,
	sessionID string,
	drafts []domain.EventDraft,
) ([]domain.Event, error) {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return nil, domain.NotFound("session not found")
	}
	if session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
		s.mu.Unlock()
		return nil, domain.Conflict("session does not accept events")
	}
	committed := make([]domain.Event, 0, len(drafts))
	var generated []domain.EventDraft
	for _, draft := range drafts {
		event := s.appendEventLocked(sessionID, draft)
		committed = append(committed, event)
		switch draft.Type {
		case domain.EvUserMessage:
			text := eventText(draft.Payload)
			if strings.Contains(text, "tool:") {
				generated = append(generated, domain.EventDraft{
					Type: domain.EvAgentCustomToolUse,
					Payload: map[string]any{
						"name": "get_metrics", "input": map[string]any{},
					},
				})
			} else {
				generated = append(generated,
					domain.EventDraft{
						Type: domain.EvAgentMessage,
						Payload: map[string]any{
							"content": []any{
								map[string]any{"type": "text", "text": "echo: " + text},
							},
						},
					},
					idleDraft(),
				)
			}
		case domain.EvUserCustomToolResult:
			generated = append(generated,
				domain.EventDraft{
					Type:    domain.EvAgentMessage,
					Payload: map[string]any{"content": draft.Payload["content"]},
				},
				idleDraft(),
			)
		}
	}
	generatedEvents := make([]domain.Event, 0, len(generated))
	for _, draft := range generated {
		if s.previews && draft.Type == domain.EvAgentMessage {
			eventID := s.ids.NewID(domain.PrefixEvent)
			draft.ID = eventID
			s.hub.PublishPreview(sessionID, domain.PreviewFrame{
				Kind: domain.PreviewEventStart, EventID: eventID,
				EventType: domain.EvAgentMessage,
			})
			s.hub.PublishPreview(sessionID, domain.PreviewFrame{
				Kind: domain.PreviewEventDelta, EventID: eventID,
				EventType: domain.EvAgentMessage, Text: eventText(draft.Payload),
			})
		}
		generatedEvents = append(generatedEvents, s.appendEventLocked(sessionID, draft))
	}
	s.mu.Unlock()

	for _, event := range append(append([]domain.Event(nil), committed...), generatedEvents...) {
		s.hub.Publish(sessionID, event)
	}
	return committed, nil
}

// forceStatus drives the fake into a status its scripted runtime never reaches
// on its own, so preconditions such as "the session must be idle" are testable
// at the wire boundary.
func (s *testSessionService) forceStatus(id string, status domain.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[id]
	session.Status = status
	s.sessions[id] = session
}

// Update mirrors the durable backend: the idle precondition, the domain apply,
// and the session.updated event all happen while the fake holds its session
// lock.
func (s *testSessionService) Update(
	_ context.Context,
	id string,
	update domain.SessionUpdate,
) (domain.Session, error) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return domain.Session{}, domain.NotFound("session not found")
	}
	if update.TouchesAgent() && session.Status != domain.StatusIdle {
		s.mu.Unlock()
		return domain.Session{}, domain.Conflict(
			"session must be idle to update agent configuration; interrupt it first",
		)
	}
	next, change, err := session.ApplyUpdate(update)
	if err != nil {
		s.mu.Unlock()
		return domain.Session{}, err
	}
	if !change.Any() {
		s.mu.Unlock()
		return session, nil
	}
	next.UpdatedAt = s.clock.Now().UTC()
	s.sessions[id] = next
	event := s.appendEventLocked(id, domain.EventDraft{
		Type:    domain.EvSessionUpdated,
		Payload: domain.SessionUpdatedPayload(next, change),
	})
	s.mu.Unlock()
	s.hub.Publish(id, event)
	return next, nil
}

func (s *testSessionService) Archive(
	_ context.Context,
	id string,
) (domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return domain.Session{}, domain.NotFound("session not found")
	}
	if session.ArchivedAt == nil {
		now := s.clock.Now().UTC()
		session.ArchivedAt = &now
		session.UpdatedAt = now
		s.sessions[id] = session
	}
	return session, nil
}

func (s *testSessionService) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return domain.NotFound("session not found")
	}
	terminal := s.appendEventLocked(id, domain.EventDraft{Type: domain.EvSessionDeleted})
	delete(s.sessions, id)
	delete(s.events, id)
	delete(s.sequences, id)
	s.mu.Unlock()
	s.environments.removeReference(session.EnvironmentID)
	s.hub.CloseSession(id, terminal)
	return nil
}

func (s *testSessionService) Query(
	_ context.Context,
	sessionID string,
	query app.EventQuery,
) ([]domain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, domain.NotFound("session not found")
	}
	types := make(map[string]bool, len(query.Types))
	for _, eventType := range query.Types {
		types[eventType] = true
	}
	events := make([]domain.Event, 0, len(s.events[sessionID]))
	for _, event := range s.events[sessionID] {
		if len(types) > 0 && !types[event.Type] {
			continue
		}
		if query.ProcessedAtGt != nil &&
			(event.ProcessedAt == nil || !event.ProcessedAt.After(*query.ProcessedAtGt)) {
			continue
		}
		if query.ProcessedAtGte != nil &&
			(event.ProcessedAt == nil || event.ProcessedAt.Before(*query.ProcessedAtGte)) {
			continue
		}
		if query.ProcessedAtLt != nil &&
			(event.ProcessedAt == nil || !event.ProcessedAt.Before(*query.ProcessedAtLt)) {
			continue
		}
		if query.ProcessedAtLte != nil &&
			(event.ProcessedAt == nil || event.ProcessedAt.After(*query.ProcessedAtLte)) {
			continue
		}
		if query.Boundary != nil {
			comparison := compareEventPageKey(
				event.ProcessedAt,
				event.Sequence,
				query.Boundary.ProcessedAt,
				query.Boundary.Sequence,
			)
			if (!query.Desc && comparison <= 0) || (query.Desc && comparison >= 0) {
				continue
			}
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		comparison := compareEventPageKey(
			events[i].ProcessedAt,
			events[i].Sequence,
			events[j].ProcessedAt,
			events[j].Sequence,
		)
		if query.Desc {
			return comparison > 0
		}
		return comparison < 0
	})
	if query.Limit > 0 && len(events) > query.Limit {
		events = events[:query.Limit]
	}
	return events, nil
}

// compareEventPageKey compares the public List Events ordering key in
// ascending order. Unprocessed events sort after processed events; receipt
// sequence makes equal and nil timestamps deterministic.
func compareEventPageKey(leftAt *time.Time, leftSequence int64, rightAt *time.Time, rightSequence int64) int {
	switch {
	case leftAt == nil && rightAt != nil:
		return 1
	case leftAt != nil && rightAt == nil:
		return -1
	case leftAt != nil && rightAt != nil:
		if leftAt.Before(*rightAt) {
			return -1
		}
		if leftAt.After(*rightAt) {
			return 1
		}
	}
	if leftSequence < rightSequence {
		return -1
	}
	if leftSequence > rightSequence {
		return 1
	}
	return 0
}

func (s *testSessionService) appendEventLocked(
	sessionID string,
	draft domain.EventDraft,
) domain.Event {
	s.sequences[sessionID]++
	now := s.clock.Now().UTC()
	eventID := draft.ID
	if eventID == "" {
		eventID = s.ids.NewID(domain.PrefixEvent)
	}
	event := domain.Event{
		ID: eventID, SessionID: sessionID, Sequence: s.sequences[sessionID],
		Type: draft.Type, Payload: draft.Payload, CreatedAt: now,
	}
	if domain.ProcessedOnReceipt(draft.Type) {
		event.ProcessedAt = &now
	}
	s.events[sessionID] = append(s.events[sessionID], event)
	return event
}

func eventText(payload map[string]any) string {
	content, _ := payload["content"].([]any)
	var text strings.Builder
	for _, item := range content {
		block, _ := item.(map[string]any)
		value, _ := block["text"].(string)
		text.WriteString(value)
	}
	return text.String()
}

func idleDraft() domain.EventDraft {
	return domain.EventDraft{
		Type: domain.EvSessionStatusIdle,
		Payload: map[string]any{
			"stop_reason": map[string]any{"type": "end_turn"},
		},
	}
}
