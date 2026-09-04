package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

type sessionSkillBudgetReader struct {
	versions map[string]domain.SkillVersion
}

func (r sessionSkillBudgetReader) ResolveSkillReferences(
	_ context.Context,
	references []domain.SkillReference,
) ([]domain.SkillReference, error) {
	return references, nil
}

func (r sessionSkillBudgetReader) GetVersion(
	_ context.Context,
	skillID, version string,
) (domain.SkillVersion, error) {
	item, ok := r.versions[skillID+"@"+version]
	if !ok {
		return domain.SkillVersion{}, domain.NotFound("Skill Version not found")
	}
	return item, nil
}

func TestValidateSessionSkillBudgetCountsPrimaryAndRosterTogether(t *testing.T) {
	t.Parallel()
	const expanded = int64(29_000_000)
	reader := sessionSkillBudgetReader{versions: make(map[string]domain.SkillVersion)}
	service := &SessionService{skillRef: reader}
	primary := domain.Agent{ID: "agent_primary", Version: 1}
	child := domain.Agent{ID: "agent_child", Version: 1}
	for index := 0; index < 19; index++ {
		skillID := fmt.Sprintf("skill_%02d", index)
		reference := domain.SkillReference{Type: "custom", SkillID: skillID, Version: "1"}
		reader.versions[skillID+"@1"] = domain.SkillVersion{
			SkillID: skillID, Version: "1", UncompressedSizeBytes: expanded,
		}
		if index == 0 {
			primary.Skills = append(primary.Skills, reference)
		} else {
			child.Skills = append(child.Skills, reference)
		}
	}
	roster := []domain.Agent{child}
	err := service.validateSessionSkillBudget(context.Background(), primary, roster)
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindTooLarge {
		t.Fatalf("aggregate budget error = %v", err)
	}
}

func TestValidateSessionSkillBudgetDeduplicatesPrimarySelfRosterScope(t *testing.T) {
	t.Parallel()
	reader := sessionSkillBudgetReader{versions: map[string]domain.SkillVersion{
		"skill_primary@1": {
			SkillID: "skill_primary", Version: "1", UncompressedSizeBytes: 29_000_000,
		},
	}}
	service := &SessionService{skillRef: reader}
	primary := domain.Agent{ID: "agent_primary", Version: 1, Skills: []domain.SkillReference{{
		Type: "custom", SkillID: "skill_primary", Version: "1",
	}}}
	if err := service.validateSessionSkillBudget(
		context.Background(), primary, []domain.Agent{primary},
	); err != nil {
		t.Fatalf("self roster duplicate counted twice: %v", err)
	}
}
