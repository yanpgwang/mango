package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/yanpgwang/mango/internal/domain"
)

type EnvironmentService struct {
	env   EnvironmentRepository
	ids   domain.IDGenerator
	clock domain.Clock
}

func NewEnvironmentService(
	env EnvironmentRepository,
	ids domain.IDGenerator,
	clock domain.Clock,
) *EnvironmentService {
	return &EnvironmentService{env: env, ids: ids, clock: clock}
}

func (s *EnvironmentService) Create(ctx context.Context, e domain.Environment) (domain.Environment, error) {
	if e.ConfigType == "" {
		if configType, ok := e.Config["type"].(string); ok {
			e.ConfigType = configType
		} else if e.Config == nil {
			e.ConfigType = "self_hosted"
		}
	}
	if err := validateEnvironment(e); err != nil {
		return domain.Environment{}, err
	}
	e.Config = map[string]any{"type": "self_hosted"}
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	now := s.clock.Now().UTC()
	e.ID = s.ids.NewID(domain.PrefixEnv)
	e.CreatedAt = now
	e.UpdatedAt = now
	return e, s.env.Put(ctx, e)
}

func (s *EnvironmentService) Update(
	ctx context.Context,
	id string,
	patch domain.EnvironmentPatch,
) (domain.Environment, error) {
	current, err := s.env.Get(ctx, id)
	if err != nil {
		return domain.Environment{}, err
	}
	if current.ArchivedAt != nil {
		return domain.Environment{}, domain.Validation("archived environment is read-only")
	}
	if patch.Scope != nil && *patch.Scope == "" {
		return domain.Environment{}, domain.Validation("scope must be organization or account")
	}

	if patch.Config != nil {
		rawConfig := *patch.Config
		configType, ok := rawConfig["type"].(string)
		if !ok {
			return domain.Environment{}, domain.Validation("config type must be self_hosted")
		}
		if err := validateEnvironmentConfig(rawConfig, configType); err != nil {
			return domain.Environment{}, err
		}
		normalized := map[string]any{"type": "self_hosted"}
		patch.Config = &normalized
	}

	next, changed := current.Apply(patch)
	if patch.Config != nil {
		next.ConfigType = (*patch.Config)["type"].(string)
	}
	if err := validateEnvironment(next); err != nil {
		return domain.Environment{}, err
	}
	if !changed {
		return current, nil
	}
	next.UpdatedAt = s.clock.Now().UTC()
	return s.env.Update(ctx, next)
}

func validateEnvironment(environment domain.Environment) error {
	if environment.Name == "" {
		return domain.Validation("name is required")
	}
	if environment.ConfigType != "self_hosted" {
		return domain.Validation("config type must be self_hosted")
	}
	if environment.Scope != "" && environment.Scope != "organization" && environment.Scope != "account" {
		return domain.Validation("scope must be organization or account")
	}
	if err := validateMetadata(environment.Metadata); err != nil {
		return err
	}
	return validateEnvironmentConfig(environment.Config, environment.ConfigType)
}

func validateEnvironmentConfig(config map[string]any, configType string) error {
	if configType != "self_hosted" {
		return domain.Validation("config type must be self_hosted")
	}
	if value, present := config["type"]; present {
		valueType, ok := value.(string)
		if !ok || valueType != configType {
			return domain.Validation("config type must be self_hosted")
		}
	}
	allowed := map[string]struct{}{"type": {}}
	return rejectUnknownEnvironmentFields(config, allowed, "config")
}

func rejectUnknownEnvironmentFields(
	values map[string]any,
	allowed map[string]struct{},
	object string,
) error {
	unknown := make([]string, 0)
	for key := range values {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return domain.Validation(fmt.Sprintf("unknown environment %s field %q", object, unknown[0]))
}

func (s *EnvironmentService) Get(ctx context.Context, id string) (domain.Environment, error) {
	return s.env.Get(ctx, id)
}

func (s *EnvironmentService) List(
	ctx context.Context,
	query EnvironmentListQuery,
) (EnvironmentListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultEnvironmentListLimit
	}
	return s.env.List(ctx, query)
}

func (s *EnvironmentService) Archive(ctx context.Context, id string) (domain.Environment, error) {
	return s.env.Archive(ctx, id, s.clock.Now().UTC())
}

func (s *EnvironmentService) Delete(ctx context.Context, id string) error {
	return s.env.DeleteIfUnreferenced(ctx, id)
}
