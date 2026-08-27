package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/yanpgwang/mango/internal/domain"
)

// This file holds the transport DTO <-> internal domain mappings. Internal
// fields (sequence numbers, run/lease bookkeeping) must never cross into a
// response, and the public wire shapes here are the single source used by
// send, list, and stream alike.

func parseModel(raw any) (domain.Model, error) {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return domain.Model{}, domain.Validation("model id is required")
		}
		return domain.Model{ID: v}, nil
	case map[string]any:
		m := domain.Model{}
		for key := range v {
			switch key {
			case "id", "effort", "speed":
			default:
				return domain.Model{}, domain.Validation(fmt.Sprintf("unknown model field %q", key))
			}
		}
		if id, ok := v["id"].(string); ok {
			m.ID = id
		}
		if rawEffort, present := v["effort"]; present {
			m.EffortExplicit = true
			switch effort := rawEffort.(type) {
			case string:
				m.Effort = effort
			case map[string]any:
				if len(effort) != 1 {
					return domain.Model{}, domain.Validation("model effort object must contain only type")
				}
				if t, ok := effort["type"].(string); ok {
					m.Effort = t
				}
			default:
				return domain.Model{}, domain.Validation("model effort must be a string or object")
			}
			if m.Effort == "" {
				return domain.Model{}, domain.Validation("model effort level is required")
			}
		}
		if rawSpeed, present := v["speed"]; present {
			sp, ok := rawSpeed.(string)
			if !ok || sp == "" {
				return domain.Model{}, domain.Validation("model speed must be a string")
			}
			m.Speed = sp
			m.SpeedExplicit = true
		}
		if m.ID == "" {
			return domain.Model{}, domain.Validation("model id is required")
		}
		if err := domain.ValidateModel(m); err != nil {
			return domain.Model{}, err
		}
		return m, nil
	}
	return domain.Model{}, domain.Validation("model must be a string or object")
}

func parseNullableStrict(raw json.RawMessage, field string) (*domain.NullableString, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return &domain.NullableString{Set: true}, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, domain.Validation(field + " must be a string or null")
	}
	return &domain.NullableString{Set: true, Value: &value}, nil
}

// parseOptionalNonNullJSON preserves the distinction between an omitted field
// and an explicit JSON null for optional, non-nullable request properties.
// encoding/json otherwise decodes null into the zero value of strings, slices,
// maps, and pointers, which can silently turn an invalid request into a default
// or no-op update.
func parseOptionalNonNullJSON[T any](
	raw json.RawMessage,
	field string,
) (*T, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, domain.Validation(field + " cannot be null")
	}
	var value T
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, domain.Validation(field + " has an invalid value")
	}
	return &value, nil
}

func agentToJSON(a domain.Agent) map[string]any {
	model := map[string]any{"id": a.Model.ID}
	if a.Model.Effort != "" {
		model["effort"] = map[string]any{"type": a.Model.Effort}
	}
	if a.Model.Speed != "" {
		model["speed"] = a.Model.Speed
	}
	out := map[string]any{
		"id": a.ID, "type": "agent", "version": a.Version, "name": a.Name,
		"model": model, "metadata": orEmptyMap(a.Metadata), "multiagent": a.Multiagent,
		"tools": orEmpty(a.Tools), "mcp_servers": orEmpty(a.MCPServers), "skills": orEmpty(a.Skills),
		"created_at": a.CreatedAt.Format(timeFmt), "updated_at": a.UpdatedAt.Format(timeFmt),
	}
	out["system"] = a.System
	out["description"] = a.Description
	if a.ArchivedAt != nil {
		out["archived_at"] = a.ArchivedAt.Format(timeFmt)
	} else {
		out["archived_at"] = nil
	}
	return out
}

// agentSnapshotJSON builds the resolved public agent snapshot embedded in a
// session's `agent` field. It omits agent-resource-level bookkeeping
// (created_at/updated_at/archived_at) that the session agent object does not
// carry, matching BetaManagedAgentsSessionAgent. The projection itself lives in
// the domain because `session.updated` events must publish the same shape from
// the durable ledger.
func agentSnapshotJSON(a domain.Agent) map[string]any {
	return a.SessionSnapshotJSON()
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
