package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	MaxSessionSkills     = 500
	MaxSessionSkillBytes = 500 << 20
)

// SkillExpandedBudgetBytes validates the exact sandbox footprint used for
// Session admission against the per-bundle limit.
func SkillExpandedBudgetBytes(size int64) (int64, bool) {
	if size < 0 || size >= MaxSkillUploadBytes {
		return 0, false
	}
	return size, true
}

// SkillReferenceResolver resolves request-time aliases to immutable custom
// Skill Versions. The Agent and Session services share this boundary so every
// persisted public snapshot contains concrete Version identifiers.
type SkillReferenceResolver interface {
	ResolveSkillReferences(
		context.Context,
		[]domain.SkillReference,
	) ([]domain.SkillReference, error)
}

// ResolveAgentSkillReferences validates Mango's current Skill union and
// delegates custom Version lookup when the effective list is non-empty.
func ResolveAgentSkillReferences(
	ctx context.Context,
	resolver SkillReferenceResolver,
	references []domain.SkillReference,
) ([]domain.SkillReference, error) {
	if err := validateSkillReferenceInputs(references); err != nil {
		return nil, err
	}
	if len(references) == 0 {
		return references, nil
	}
	for index, reference := range references {
		if reference.Type == "anthropic" {
			return nil, domain.Unsupported(fmt.Sprintf(
				"skills[%d]: Anthropic-managed Skills are not supported", index,
			))
		}
	}
	if resolver == nil {
		return nil, domain.Unsupported(
			"custom Skills are unavailable for the configured deployment",
		)
	}
	resolved, err := resolver.ResolveSkillReferences(ctx, references)
	if err != nil {
		return nil, err
	}
	if len(resolved) != len(references) {
		return nil, errors.New("skill resolver returned an incomplete result")
	}
	for index, reference := range resolved {
		if reference.Type != "custom" || reference.SkillID == "" ||
			reference.Version == "" || reference.Version == "latest" {
			return nil, fmt.Errorf("skill resolver returned an invalid result at index %d", index)
		}
	}
	return resolved, nil
}

func validateSkillReferenceInputs(references []domain.SkillReference) error {
	if len(references) > MaxSessionSkills {
		return domain.Validation("skills must contain at most 500 entries")
	}
	for index, reference := range references {
		switch reference.Type {
		case "custom", "anthropic":
		default:
			return domain.Validation(fmt.Sprintf(
				"skills[%d].type must be custom or anthropic", index,
			))
		}
		if reference.SkillID == "" || strings.TrimSpace(reference.SkillID) != reference.SkillID {
			return domain.Validation(fmt.Sprintf("skills[%d].skill_id is required", index))
		}
		if strings.TrimSpace(reference.Version) != reference.Version {
			return domain.Validation(fmt.Sprintf("skills[%d].version is invalid", index))
		}
	}
	return nil
}
