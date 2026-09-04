// Package controlplane wires the public HTTP resource semantics to the
// PostgreSQL ledger and Temporal admission boundary.
package controlplane

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
)

type SessionOrchestrator interface {
	CreateAPISession(
		context.Context,
		domain.Session,
		[]domain.EventDraft,
		...[]app.PreparedSessionResource,
	) (domain.Session, []domain.Event, error)
	Admit(context.Context, string, []domain.EventDraft) ([]domain.Event, error)
	TerminateSession(context.Context, string) error
}

type deploymentSessionOrchestrator interface {
	CreateDeploymentSession(
		context.Context,
		domain.Session,
		[]domain.EventDraft,
		domain.DeploymentRun,
		...[]app.PreparedSessionResource,
	) (domain.Session, []domain.Event, error)
}

// OutcomeRubricReader resolves and validates one reusable Files API rubric.
type OutcomeRubricReader interface {
	ReadOutcomeRubric(context.Context, string) (string, error)
}

// MessageFileReader resolves one top-level File into a bounded UTF-8 snapshot
// before event admission.
type MessageFileReader interface {
	ReadMessageFile(context.Context, string) (domain.FileMessageContent, error)
}

// SessionService owns public Session validation and delegates the atomic
// session/event admission boundary to PostgreSQL plus the Temporal outbox.
type SessionService struct {
	store        *pg.Store
	agents       app.AgentRepository
	environments app.EnvironmentRepository
	orchestrator SessionOrchestrator
	ids          domain.IDGenerator
	clock        domain.Clock
	resources    *SessionResourceService
	outcomeFiles OutcomeRubricReader
	messageFiles MessageFileReader
	memoryStores interface {
		GetStore(context.Context, string) (domain.MemoryStore, error)
	}
	skillRef      app.SkillReferenceResolver
	vaultsEnabled bool
	// cloudSkillBundles gates server-managed sandbox materialization. External
	// worker Skill activation is unsupported independently of this capability.
	cloudSkillBundles bool
}

// EnableFileOutcomeRubrics installs the internal Files reader used to resolve
// and snapshot reusable rubric text before event admission. This capability is
// independent of sandbox File mounts.
func (s *SessionService) EnableFileOutcomeRubrics(reader OutcomeRubricReader) {
	s.outcomeFiles = reader
}

// EnableFileMessageContent installs the internal Files reader used to resolve
// and snapshot text-only document sources before event admission.
func (s *SessionService) EnableFileMessageContent(reader MessageFileReader) {
	s.messageFiles = reader
}

// EnableVaults permits Session Vault attachments for deployments whose API and
// worker share a configured credential keyring.
func (s *SessionService) EnableVaults() {
	s.vaultsEnabled = true
}

func NewSessionService(
	store *pg.Store,
	agents app.AgentRepository,
	environments app.EnvironmentRepository,
	orchestrator SessionOrchestrator,
	ids domain.IDGenerator,
	clock domain.Clock,
	skillRef app.SkillReferenceResolver,
	resourceServices ...*SessionResourceService,
) *SessionService {
	service := &SessionService{
		store: store, agents: agents, environments: environments,
		orchestrator: orchestrator, ids: ids, clock: clock, skillRef: skillRef,
		cloudSkillBundles: true,
	}
	if len(resourceServices) > 0 {
		service.resources = resourceServices[0]
	}
	return service
}

// ConfigureCloudSkillBundles declares whether the configured cloud sandbox
// adapter can materialize custom Skill bundles. Self-hosted Sessions reject
// custom Skills regardless of the configured cloud adapter.
func (s *SessionService) ConfigureCloudSkillBundles(enabled bool) {
	s.cloudSkillBundles = enabled
}

// EnableMemoryStoreResources installs the deployment's Memory Store reader.
// Composition calls this only when the configured sandbox adapter can expose
// durable /mnt/memory mounts; API admission otherwise fails explicitly.
func (s *SessionService) EnableMemoryStoreResources(reader interface {
	GetStore(context.Context, string) (domain.MemoryStore, error)
}) {
	s.memoryStores = reader
}

func (s *SessionService) Create(
	ctx context.Context,
	input app.CreateSessionInput,
) (domain.Session, error) {
	if len(input.Resources)+len(input.MemoryResources)+len(input.RepositoryResources) > app.MaxSessionResources {
		return domain.Session{}, domain.Validation(
			"resources must contain at most 500 entries",
		)
	}
	if len(input.VaultIDs) > 0 && !s.vaultsEnabled {
		return domain.Session{}, domain.Unsupported(
			"Session Vaults are unavailable for the configured deployment",
		)
	}
	seenVaults := make(map[string]struct{}, len(input.VaultIDs))
	for _, vaultID := range input.VaultIDs {
		if vaultID == "" {
			return domain.Session{}, domain.Validation("vault_ids must not contain empty IDs")
		}
		if _, duplicate := seenVaults[vaultID]; duplicate {
			return domain.Session{}, domain.Validation("vault_ids must not contain duplicates")
		}
		seenVaults[vaultID] = struct{}{}
	}
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
	if agent.Multiagent != nil && !agent.Multiagent.IsResolved() {
		return domain.Session{}, domain.Validation(
			"legacy multiagent configuration must be replaced before creating a Session",
		)
	}
	environment, err := s.environments.Get(ctx, input.EnvironmentID)
	if err != nil {
		return domain.Session{}, domain.Validation("environment not found")
	}
	if environment.ArchivedAt != nil {
		return domain.Session{}, domain.Validation("environment is archived")
	}
	if len(input.InitialEvents) > 50 {
		return domain.Session{}, domain.Validation("initial_events exceeds 50")
	}
	for _, event := range input.InitialEvents {
		allowed := domain.IsInitialEventType(event.Type) ||
			(input.DeploymentID != nil && event.Type == domain.EvSystemMessage)
		if !allowed {
			return domain.Session{}, domain.Validation(
				"initial_events contains an unsupported event type",
			)
		}
	}
	input.InitialEvents, err = s.resolveOutcomeRubrics(ctx, input.InitialEvents)
	if err != nil {
		return domain.Session{}, err
	}
	input.InitialEvents, err = s.resolveMessageFiles(ctx, input.InitialEvents)
	if err != nil {
		return domain.Session{}, err
	}

	snapshot := agent
	if input.Overrides != nil {
		snapshot = agent.WithOverrides(*input.Overrides)
	}
	snapshot.Skills, err = app.ResolveAgentSkillReferences(
		ctx,
		s.skillRef,
		snapshot.Skills,
	)
	if err != nil {
		return domain.Session{}, err
	}
	roster, err := s.resolveSessionMultiagentRoster(ctx, agent, snapshot)
	if err != nil {
		return domain.Session{}, err
	}
	modelsPriceable := domain.HasAnthropicPublicListPrice(snapshot.Model)
	for _, member := range roster {
		modelsPriceable = modelsPriceable && domain.HasAnthropicPublicListPrice(member.Model)
	}
	if advisor := agent.Multiagent.Advisor(); advisor != nil {
		modelsPriceable = modelsPriceable && domain.HasAnthropicPublicListPrice(
			domain.Model{ID: advisor.Model},
		)
	}
	if input.Budget != nil && !modelsPriceable {
		return domain.Session{}, domain.Validation(
			"budgeted sessions require every agent model to have a known Anthropic public list price",
		)
	}
	hasSkills := len(snapshot.Skills) > 0
	for _, member := range roster {
		hasSkills = hasSkills || len(member.Skills) > 0
	}
	if environment.ConfigType == "cloud" && hasSkills && !s.cloudSkillBundles {
		return domain.Session{}, domain.Unsupported(
			"custom Skills are unavailable for the configured cloud sandbox provider",
		)
	}
	if environment.ConfigType == "self_hosted" && len(input.MemoryResources) > 0 {
		return domain.Session{}, domain.Unsupported(
			"Memory Store resources are unavailable for self-hosted Sessions",
		)
	}
	if environment.ConfigType == "self_hosted" && len(input.RepositoryResources) > 0 {
		return domain.Session{}, domain.Unsupported(
			"Git repository resources are unavailable for self-hosted Sessions",
		)
	}
	if err := domain.ValidateToolConfiguration(
		snapshot.Tools,
		snapshot.MCPServers,
	); err != nil {
		return domain.Session{}, domain.Validation(
			"invalid agent tool configuration: " + err.Error(),
		)
	}
	if err := domain.ValidateSkillToolConfiguration(
		snapshot.Tools,
		len(snapshot.Skills) > 0,
	); err != nil {
		return domain.Session{}, domain.Validation(
			"invalid agent Skill tool configuration: " + err.Error(),
		)
	}
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	now := s.clock.Now().UTC()
	session := domain.Session{
		ID:                s.ids.NewID(domain.PrefixSession),
		AgentID:           agent.ID,
		AgentVersion:      agent.Version,
		EnvironmentID:     environment.ID,
		DeploymentID:      input.DeploymentID,
		EnvironmentType:   environment.ConfigType,
		EnvironmentConfig: environment.SessionConfig(),
		Status:            domain.StatusIdle,
		Title:             input.Title,
		Metadata:          metadata,
		AgentSnapshot:     snapshot,
		MultiagentRoster:  roster,
		ListCostKnown:     true,
		Budget:            input.Budget,
		VaultIDs:          append([]string(nil), input.VaultIDs...),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	prepared, err := s.prepareMemoryStoreResources(ctx, session, input.MemoryResources)
	if err != nil {
		return domain.Session{}, err
	}
	if len(input.Resources) > 0 {
		if s.resources == nil {
			return domain.Session{}, domain.Unsupported(
				"File resources are unavailable for the configured deployment",
			)
		}
		fileResources, prepareErr := s.resources.PrepareForSession(ctx, session, input.Resources)
		err = prepareErr
		if err != nil {
			return domain.Session{}, err
		}
		prepared = append(prepared, fileResources...)
	}
	if len(input.RepositoryResources) > 0 {
		if s.resources == nil {
			return domain.Session{}, domain.Unsupported(
				"Git repository resources are unavailable for the configured deployment",
			)
		}
		var stagedBytes int64
		for _, item := range prepared {
			if item.Blob.SizeBytes > app.MaxSessionResourceBytes-stagedBytes {
				s.resources.DiscardPrepared(ctx, prepared)
				return domain.Session{}, domain.TooLarge(
					"Session Resources exceed the 500 MB aggregate limit",
				)
			}
			stagedBytes += item.Blob.SizeBytes
		}
		repositoryResources, prepareErr := s.resources.PrepareRepositoriesForSession(
			ctx, session, input.RepositoryResources, stagedBytes,
		)
		if prepareErr != nil {
			s.resources.DiscardPrepared(ctx, prepared)
			return domain.Session{}, prepareErr
		}
		prepared = append(prepared, repositoryResources...)
	}
	if len(prepared) > 0 {
		session.Resources = make([]domain.SessionResource, len(prepared))
		for index := range prepared {
			session.Resources[index] = prepared[index].Resource
		}
	}
	var created domain.Session
	if input.DeploymentRun != nil {
		deploymentOrchestrator, ok := s.orchestrator.(deploymentSessionOrchestrator)
		if !ok {
			return domain.Session{}, domain.Unsupported(
				"Deployment Session admission is unavailable for the configured runtime",
			)
		}
		run := *input.DeploymentRun
		run.SessionID = &session.ID
		created, _, err = deploymentOrchestrator.CreateDeploymentSession(
			ctx, session, input.InitialEvents, run, prepared,
		)
	} else {
		created, _, err = s.orchestrator.CreateAPISession(
			ctx, session, input.InitialEvents, prepared,
		)
	}
	if err != nil && s.resources != nil {
		var domainErr *domain.DomainError
		if errors.As(err, &domainErr) {
			s.resources.DiscardPrepared(ctx, prepared)
		}
	}
	return created, err
}

// resolveSessionMultiagentRoster expands the immutable version pins stored on
// an Agent resource into the full Agent definitions required by the Session
// contract. The PostgreSQL admission transaction locks these same versions,
// closing the archival race between this read and Session creation.
func (s *SessionService) resolveSessionMultiagentRoster(
	ctx context.Context,
	coordinator domain.Agent,
	snapshot domain.Agent,
) ([]domain.Agent, error) {
	if coordinator.Multiagent == nil {
		return nil, nil
	}
	roster := make([]domain.Agent, 0, len(coordinator.Multiagent.Agents))
	for _, reference := range coordinator.Multiagent.Agents {
		if reference.Type == "advisor" {
			continue
		}
		var member domain.Agent
		if reference.ID == coordinator.ID {
			member = snapshot
		} else {
			var err error
			member, err = s.agents.GetVersion(ctx, reference.ID, reference.Version)
			if err != nil || member.ArchivedAt != nil {
				return nil, domain.Validation(
					"multiagent references an agent version that is missing or archived",
				)
			}
		}
		// Roster members are one level deep. Keeping the coordinator topology on
		// a self snapshot would make the public Session shape recursive and would
		// incorrectly grant a child the right to spawn another generation.
		member.Multiagent = nil
		roster = append(roster, member)
	}
	return roster, nil
}

func (s *SessionService) prepareMemoryStoreResources(
	ctx context.Context,
	session domain.Session,
	inputs []app.MemorySessionResourceInput,
) ([]app.PreparedSessionResource, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > domain.MaxSessionMemoryStores {
		return nil, domain.Validation("resources may contain at most 8 Memory Stores")
	}
	if s.memoryStores == nil {
		return nil, domain.Unsupported(
			"Memory Store resources are unavailable for the configured deployment",
		)
	}
	seenStores := make(map[string]struct{}, len(inputs))
	seenMounts := make(map[string]struct{}, len(inputs))
	prepared := make([]app.PreparedSessionResource, 0, len(inputs))
	now := session.CreatedAt.UTC()
	for _, input := range inputs {
		if input.MemoryStoreID == "" {
			return nil, domain.Validation("memory_store_id is required")
		}
		if _, duplicate := seenStores[input.MemoryStoreID]; duplicate {
			return nil, domain.Validation("a Memory Store may be attached only once")
		}
		seenStores[input.MemoryStoreID] = struct{}{}
		if input.Access == "" {
			input.Access = domain.MemoryAccessReadWrite
		}
		if input.Access != domain.MemoryAccessReadWrite &&
			input.Access != domain.MemoryAccessReadOnly {
			return nil, domain.Validation("access must be read_write or read_only")
		}
		if !utf8.ValidString(input.Instructions) ||
			utf8.RuneCountInString(input.Instructions) > domain.MaxSessionMemoryInstructionsChars {
			return nil, domain.Validation(
				"instructions must contain at most 4096 valid UTF-8 characters",
			)
		}
		store, err := s.memoryStores.GetStore(ctx, input.MemoryStoreID)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
				return nil, domain.Validation("memory store not found")
			}
			return nil, err
		}
		if store.ArchivedAt != nil {
			return nil, domain.Validation("memory store is archived")
		}
		mountPath, err := domain.NormalizeSessionMemoryStoreMountPath(store.Name)
		if err != nil {
			return nil, err
		}
		if _, collision := seenMounts[mountPath]; collision {
			return nil, domain.Validation(
				"attached Memory Store names must produce distinct mount paths",
			)
		}
		seenMounts[mountPath] = struct{}{}
		prepared = append(prepared, app.PreparedSessionResource{Resource: domain.SessionResource{
			ID:                     s.ids.NewID(domain.PrefixSessionResource),
			SessionID:              session.ID,
			ResourceType:           domain.SessionResourceTypeMemoryStore,
			MemoryStoreID:          store.ID,
			MemoryAccess:           input.Access,
			MemoryInstructions:     input.Instructions,
			MemoryStoreName:        store.Name,
			MemoryStoreDescription: store.Description,
			MountPath:              mountPath,
			CreatedAt:              now,
			UpdatedAt:              now,
			State:                  domain.SessionResourceActive,
		}})
	}
	return prepared, nil
}

func (s *SessionService) Get(ctx context.Context, id string) (domain.Session, error) {
	return s.store.GetSessionForWorkspace(ctx, id)
}

func (s *SessionService) List(
	ctx context.Context,
	query app.ListPage,
) (app.SessionListPage, error) {
	return s.store.ListSessions(ctx, query)
}

func (s *SessionService) SendEvent(
	ctx context.Context,
	id string,
	drafts []domain.EventDraft,
) ([]domain.Event, error) {
	if err := s.store.AssertSessionWorkspace(ctx, id); err != nil {
		return nil, err
	}
	for _, draft := range drafts {
		switch draft.Type {
		case domain.EvUserMessage,
			domain.EvUserDefineOutcome,
			domain.EvUserCustomToolResult,
			domain.EvUserToolResult,
			domain.EvUserToolConfirmation,
			domain.EvSystemMessage:
			// define_outcome is processed on receipt; messages schedule ordinary
			// turns; custom results and confirmations claim the durable
			// pending-action barrier and wake the SessionWorkflow only when the full
			// result set is present.
		case domain.EvUserInterrupt:
			// PostgreSQL resolves the optional Thread target under the Session
			// admission lock. An omitted target fans out to every active Thread.
		default:
			return nil, domain.Unsupported(
				"this client event is not supported on the PostgreSQL backend",
			)
		}
	}
	prepared, err := s.resolveOutcomeRubrics(ctx, drafts)
	if err != nil {
		return nil, err
	}
	prepared, err = s.resolveMessageFiles(ctx, prepared)
	if err != nil {
		return nil, err
	}
	return s.orchestrator.Admit(ctx, id, prepared)
}

func (s *SessionService) resolveMessageFiles(
	ctx context.Context,
	drafts []domain.EventDraft,
) ([]domain.EventDraft, error) {
	prepared := append([]domain.EventDraft(nil), drafts...)
	cache := make(map[string]domain.FileMessageContent)
	totalCharacters := 0
	for draftIndex, draft := range prepared {
		if draft.Type != domain.EvUserMessage {
			continue
		}
		blocks, _ := draft.Payload["content"].([]any)
		var snapshots []domain.FileMessageContent
		for contentIndex, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] != "document" {
				continue
			}
			source, _ := block["source"].(map[string]any)
			if source["type"] != "file" {
				continue
			}
			fileID, _ := source["file_id"].(string)
			if fileID == "" {
				return nil, domain.Validation("document file source requires file_id")
			}
			if s.messageFiles == nil {
				return nil, domain.Unsupported(
					"file message content is unavailable because Files storage is not configured",
				)
			}
			snapshot, ok := cache[fileID]
			if !ok {
				var err error
				snapshot, err = s.messageFiles.ReadMessageFile(ctx, fileID)
				if err != nil {
					return nil, err
				}
				cache[fileID] = snapshot
			}
			snapshot.ContentIndex = contentIndex
			totalCharacters += utf8.RuneCountInString(snapshot.Content)
			if totalCharacters > domain.MaxFileMessageCharacters {
				return nil, domain.Validation(
					"file message content must contain at most 262144 characters per admission",
				)
			}
			snapshots = append(snapshots, snapshot)
		}
		if len(snapshots) > 0 {
			draft.Payload = domain.WithFileMessageContents(draft.Payload, snapshots)
			prepared[draftIndex] = draft
		}
	}
	return prepared, nil
}

func (s *SessionService) resolveOutcomeRubrics(
	ctx context.Context,
	drafts []domain.EventDraft,
) ([]domain.EventDraft, error) {
	prepared := append([]domain.EventDraft(nil), drafts...)
	for index, draft := range prepared {
		if draft.Type != domain.EvUserDefineOutcome {
			continue
		}
		rubric, ok := draft.Payload["rubric"].(map[string]any)
		if !ok || rubric["type"] != "file" {
			continue
		}
		fileID, ok := rubric["file_id"].(string)
		if !ok || fileID == "" {
			return nil, domain.Validation("file rubric requires file_id")
		}
		if s.outcomeFiles == nil {
			return nil, domain.Unsupported(
				"file outcome rubrics are unavailable because Files storage is not configured",
			)
		}
		content, err := s.outcomeFiles.ReadOutcomeRubric(ctx, fileID)
		if err != nil {
			return nil, err
		}
		draft.Payload = domain.WithOutcomeRubricContent(draft.Payload, content)
		prepared[index] = draft
	}
	return prepared, nil
}

// Update applies the documented session update body. Validation of the merged
// result and the idle precondition for mid-session agent changes both run
// inside the store transaction that holds the session's admission lock, so a
// concurrent turn admission cannot slip between the check and the write.
func (s *SessionService) Update(
	ctx context.Context,
	id string,
	update domain.SessionUpdate,
) (domain.Session, error) {
	if err := s.store.AssertSessionWorkspace(ctx, id); err != nil {
		return domain.Session{}, err
	}
	return s.store.UpdateSession(ctx, id, update)
}

func (s *SessionService) Archive(ctx context.Context, id string) (domain.Session, error) {
	if err := s.store.AssertSessionWorkspace(ctx, id); err != nil {
		return domain.Session{}, err
	}
	return s.store.ArchiveSession(ctx, id)
}

func (s *SessionService) Delete(ctx context.Context, id string) error {
	if err := s.store.AssertSessionWorkspace(ctx, id); err != nil {
		return err
	}
	// Fence new admission before stopping orchestration and releasing the
	// provider sandbox. Without this phase, an admission could make the session
	// running in the gap before physical deletion.
	if err := s.store.PrepareSessionDeletion(ctx, id); err != nil {
		return err
	}
	if err := s.orchestrator.TerminateSession(ctx, id); err != nil {
		// Keep the fence on an ambiguous external result. Retrying DELETE safely
		// repeats Workflow termination and idempotent sandbox cleanup.
		return err
	}
	if s.resources != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := s.resources.CleanupSession(cleanupCtx, id)
		cancel()
		if err != nil {
			return err
		}
	}
	memoryCleanupCtx, memoryCleanupCancel := context.WithTimeout(
		context.WithoutCancel(ctx), 5*time.Second,
	)
	err := s.store.FinalizeSessionMemoryResources(memoryCleanupCtx, id)
	memoryCleanupCancel()
	if err != nil {
		return err
	}
	// Once termination succeeds, finish the fenced delete even if the client
	// disconnects. If the database write fails, the marker intentionally remains
	// and a later DELETE can safely retry the termination/finalization sequence.
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.store.FinalizeSessionDeletion(finalizeCtx, id)
}
