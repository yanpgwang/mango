package app

import (
	"context"
	"errors"

	"github.com/yanpgwang/mango/internal/domain"
)

type AgentService struct {
	repo     AgentRepository
	ids      domain.IDGenerator
	clock    domain.Clock
	skillRef SkillReferenceResolver
}

func NewAgentService(
	repo AgentRepository,
	ids domain.IDGenerator,
	clock domain.Clock,
	skillResolvers ...SkillReferenceResolver,
) *AgentService {
	service := &AgentService{repo: repo, ids: ids, clock: clock}
	if len(skillResolvers) > 0 {
		service.skillRef = skillResolvers[0]
	}
	return service
}

func (s *AgentService) Create(ctx context.Context, a domain.Agent) (domain.Agent, error) {
	if err := validateAgent(a); err != nil {
		return domain.Agent{}, err
	}
	resolved, err := ResolveAgentSkillReferences(ctx, s.skillRef, a.Skills)
	if err != nil {
		return domain.Agent{}, err
	}
	a.Skills = resolved
	a.Model = domain.NormalizeModel(a.Model)
	now := s.clock.Now().UTC()
	a.ID = s.ids.NewID(domain.PrefixAgent)
	a.Version = 1
	a.CreatedAt = now
	a.UpdatedAt = now
	resolvedMultiagent, err := s.resolveMultiagent(
		ctx,
		a.ID,
		a.Version,
		a.Name,
		a.Model,
		a.Multiagent,
	)
	if err != nil {
		return domain.Agent{}, err
	}
	a.Multiagent = resolvedMultiagent
	return a, s.repo.PutVersion(ctx, a)
}

func (s *AgentService) Get(ctx context.Context, id string) (domain.Agent, error) {
	return s.repo.Latest(ctx, id)
}

func (s *AgentService) List(ctx context.Context, query AgentListQuery) (AgentListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultAgentListLimit
	}
	return s.repo.ListLatest(ctx, query)
}

func (s *AgentService) Versions(
	ctx context.Context,
	id string,
	query AgentVersionListQuery,
) (AgentVersionListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultAgentListLimit
	}
	return s.repo.Versions(ctx, id, query)
}

func (s *AgentService) Update(ctx context.Context, id string, patch domain.AgentPatch) (domain.Agent, error) {
	effectivePatch := patch
	var current *domain.Agent
	if (patch.Multiagent != nil && patch.Multiagent.Value != nil) || patch.Skills != nil {
		loaded, err := s.repo.Latest(ctx, id)
		if err != nil {
			return domain.Agent{}, err
		}
		if loaded.ArchivedAt != nil {
			return domain.Agent{}, domain.Validation("archived agent is read-only")
		}
		if patch.ExpectedVersion != nil && *patch.ExpectedVersion != loaded.Version {
			return domain.Agent{}, domain.Conflict("agent version mismatch")
		}
		current = &loaded
	}
	if patch.Multiagent != nil && patch.Multiagent.Value != nil {
		// External references can be resolved before the repository opens its
		// serialization transaction. A self reference is temporarily bound to
		// version 1 and rebound to the locked coordinator version below. Pin the
		// owner version implicitly when the client omitted one: roster admission
		// depends on the coordinator model and must not race a concurrent update.
		ownerModel := current.Model
		ownerName := current.Name
		if patch.Model != nil {
			ownerModel = domain.NormalizeModel(*patch.Model)
		}
		if patch.Name != nil {
			ownerName = *patch.Name
		}
		resolved, err := s.resolveMultiagent(ctx, id, 1, ownerName, ownerModel, patch.Multiagent.Value)
		if err != nil {
			return domain.Agent{}, err
		}
		effectivePatch.Multiagent = &domain.NullableMultiagent{Value: resolved}
		if effectivePatch.ExpectedVersion == nil {
			version := current.Version
			effectivePatch.ExpectedVersion = &version
		}
	}
	if patch.Skills != nil {
		// Resolve before UpdateVersion opens its serialization transaction. A
		// production resolver may use the same connection pool as the Agent
		// repository; calling it from the mutation callback can exhaust that pool
		// while every transaction is holding an Agent row lock.
		resolved, err := ResolveAgentSkillReferences(ctx, s.skillRef, *patch.Skills)
		if err != nil {
			return domain.Agent{}, err
		}
		effectivePatch.Skills = &resolved
	}
	return s.repo.UpdateVersion(ctx, id, func(cur domain.Agent) (domain.Agent, bool, error) {
		if cur.ArchivedAt != nil {
			return domain.Agent{}, false, domain.Validation("archived agent is read-only")
		}
		transactionPatch := effectivePatch
		if effectivePatch.Multiagent != nil && effectivePatch.Multiagent.Value != nil {
			transactionPatch.Multiagent = &domain.NullableMultiagent{
				Value: effectivePatch.Multiagent.Value.RebindAgentVersion(cur.ID, cur.Version),
			}
		}
		next, changed, err := cur.Apply(transactionPatch)
		if err != nil {
			return domain.Agent{}, false, err
		}
		if changed && next.Multiagent != nil {
			// A self entry names the Agent Version being created, not the previous
			// version used to perform semantic no-op detection.
			next.Multiagent = next.Multiagent.RebindAgentVersion(cur.ID, cur.Version+1)
		}
		if changed && next.Multiagent != nil && !next.Multiagent.IsResolved() {
			return domain.Agent{}, false, domain.Validation(
				"legacy multiagent configuration must be replaced before updating the Agent",
			)
		}
		if err := validateAgent(next); err != nil {
			return domain.Agent{}, false, err
		}
		if changed {
			next.UpdatedAt = s.clock.Now().UTC()
		}
		return next, changed, nil
	})
}

func (s *AgentService) Archive(ctx context.Context, id string) (domain.Agent, error) {
	return s.repo.Archive(ctx, id, s.clock.Now().UTC())
}

func validateAgent(a domain.Agent) error {
	if a.Name == "" {
		return domain.Validation("name is required")
	}
	if err := domain.ValidateModel(a.Model); err != nil {
		return err
	}
	if err := domain.ValidateToolConfiguration(a.Tools, a.MCPServers); err != nil {
		return domain.Validation("invalid tool configuration: " + err.Error())
	}
	if err := domain.ValidateSkillToolConfiguration(a.Tools, hasRuntimeSkills(a.Skills)); err != nil {
		return domain.Validation("invalid Skill tool configuration: " + err.Error())
	}
	if err := validateMultiagentShape(a.Multiagent); err != nil {
		return err
	}
	if advisor := a.Multiagent.Advisor(); advisor != nil {
		if a.Name == domain.AdvisorAgentName {
			return domain.Validation(
				"multiagent advisor conflicts with the reserved agent name anthropic.advisor",
			)
		}
	}
	return validateMetadata(a.Metadata)
}

func validateMultiagentShape(topology *domain.Multiagent) error {
	if topology == nil {
		return nil
	}
	if topology.IsLegacy() {
		return nil
	}
	if topology.Type != "coordinator" {
		return domain.Validation("multiagent.type must be coordinator")
	}
	if len(topology.Agents) < 1 || len(topology.Agents) > 20 {
		return domain.Validation("multiagent.agents must contain between 1 and 20 entries")
	}
	for _, reference := range topology.Agents {
		switch reference.Type {
		case "self":
			if reference.ID != "" || reference.Version != 0 || reference.Model != "" {
				return domain.Validation("multiagent self entry only accepts type")
			}
		case "agent":
			if reference.ID == "" || reference.Model != "" {
				return domain.Validation("multiagent agent entry requires a non-empty id")
			}
			if reference.Version < 0 {
				return domain.Validation("multiagent agent version must be at least 1")
			}
		case "advisor":
			if reference.ID != "" || reference.Version != 0 || reference.Model == "" {
				return domain.Validation("multiagent advisor entry requires a non-empty model")
			}
		default:
			return domain.Validation("multiagent roster entry type must be agent, self, or advisor")
		}
	}
	return nil
}

// resolveMultiagent turns every documented roster input form into an immutable
// concrete Agent Version reference. This is deliberately an application-layer
// operation: the HTTP layer does not own resource lookup, and the runtime must
// never resolve "latest" again after the coordinator version has been stored.
func (s *AgentService) resolveMultiagent(
	ctx context.Context,
	ownerID string,
	ownerVersion int,
	ownerName string,
	ownerModel domain.Model,
	topology *domain.Multiagent,
) (*domain.Multiagent, error) {
	if topology == nil {
		return nil, nil
	}
	if topology.IsLegacy() {
		return nil, domain.Validation("legacy multiagent configuration must be replaced before use")
	}
	if err := validateMultiagentShape(topology); err != nil {
		return nil, err
	}
	resolved := &domain.Multiagent{Type: "coordinator", Agents: make([]domain.AgentReference, 0, len(topology.Agents))}
	seen := make(map[string]struct{}, len(topology.Agents))
	seenSelf := false
	var advisor *domain.AgentReference
	reservedNameFound := false
	for _, reference := range topology.Agents {
		if reference.Type == "advisor" {
			if advisor != nil {
				return nil, domain.Validation("multiagent.agents may contain at most one advisor entry")
			}
			entry := domain.AgentReference{Type: "advisor", Model: reference.Model}
			advisor = &entry
			continue
		}
		var target domain.Agent
		if reference.Type == "self" {
			if seenSelf {
				return nil, domain.Validation("multiagent.agents may contain at most one self entry")
			}
			seenSelf = true
			target = domain.Agent{
				ID: ownerID, Version: ownerVersion, Name: ownerName, Model: ownerModel,
			}
		} else {
			if reference.ID == ownerID {
				return nil, domain.Validation("multiagent must use a self entry to reference its coordinator")
			}
			var err error
			if reference.Version == 0 {
				target, err = s.repo.Latest(ctx, reference.ID)
			} else {
				target, err = s.repo.GetVersion(ctx, reference.ID, reference.Version)
			}
			if err != nil {
				var domainErr *domain.DomainError
				if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
					return nil, domain.Validation("multiagent references an agent that does not exist")
				}
				return nil, err
			}
			if target.ArchivedAt != nil {
				return nil, domain.Validation("multiagent references an archived agent")
			}
			if target.Multiagent != nil {
				return nil, domain.Validation("multiagent references are limited to one coordinator level")
			}
		}
		if _, duplicate := seen[target.ID]; duplicate {
			return nil, domain.Validation("multiagent.agents must reference distinct agents")
		}
		reservedNameFound = reservedNameFound || target.Name == domain.AdvisorAgentName
		seen[target.ID] = struct{}{}
		resolved.Agents = append(resolved.Agents, domain.AgentReference{
			Type: "agent", ID: target.ID, Version: target.Version,
		})
	}
	if advisor != nil {
		if reservedNameFound {
			return nil, domain.Validation(
				"multiagent advisor conflicts with the reserved agent name anthropic.advisor",
			)
		}
		resolved.Agents = append(resolved.Agents, *advisor)
	}
	return resolved, nil
}

func hasRuntimeSkills(skills []domain.SkillReference) bool {
	for _, skill := range skills {
		if !skill.IsLegacy() {
			return true
		}
	}
	return false
}
