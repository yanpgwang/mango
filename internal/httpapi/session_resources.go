package httpapi

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func sessionResourceToJSON(resource domain.SessionResource) map[string]any {
	var instructions any
	if resource.MemoryInstructions != "" {
		instructions = resource.MemoryInstructions
	}
	return map[string]any{
		"memory_store_id": resource.MemoryStoreID,
		"type":            domain.SessionResourceTypeMemoryStore,
		"access":          resource.MemoryAccess,
		"description":     resource.MemoryStoreDescription,
		"instructions":    instructions,
		"mount_path":      resource.MountPath,
		"name":            resource.MemoryStoreName,
	}
}

func parseSessionResourceInputs(
	raw *[]json.RawMessage,
) ([]app.MemorySessionResourceInput, error) {
	if raw == nil || len(*raw) == 0 {
		return nil, nil
	}
	if len(*raw) > domain.MaxSessionMemoryStores {
		return nil, domain.Validation("resources may contain at most 8 Memory Stores")
	}
	memories := make([]app.MemorySessionResourceInput, 0, len(*raw))
	for _, item := range *raw {
		resourceType, err := parseSessionResourceType(item)
		if err != nil {
			return nil, err
		}
		if resourceType != domain.SessionResourceTypeMemoryStore {
			return nil, domain.Unsupported(
				"self-hosted Session resources support Memory Stores only; stage files and repositories in the worker workspace",
			)
		}
		input, err := parseSessionMemoryResourceInput(item)
		if err != nil {
			return nil, err
		}
		memories = append(memories, input)
	}
	return memories, nil
}

func parseSessionResourceType(raw json.RawMessage) (string, error) {
	var discriminator struct {
		Type string `json:"type"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&discriminator); err != nil {
		return "", domain.Validation("session resource must be a valid resource object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", domain.Validation("session resource must contain exactly one JSON object")
	}
	if discriminator.Type == "" {
		return "", domain.Validation("session resource type is required")
	}
	return discriminator.Type, nil
}

func parseSessionMemoryResourceInput(raw json.RawMessage) (app.MemorySessionResourceInput, error) {
	var input struct {
		Type          string                    `json:"type"`
		MemoryStoreID string                    `json:"memory_store_id"`
		Access        optionalJSONField[string] `json:"access"`
		Instructions  optionalJSONField[string] `json:"instructions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return app.MemorySessionResourceInput{}, domain.Validation(
			"session resource must be a valid Memory Store resource object",
		)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return app.MemorySessionResourceInput{}, domain.Validation(
			"session resource must contain exactly one JSON object",
		)
	}
	if input.Type != domain.SessionResourceTypeMemoryStore {
		return app.MemorySessionResourceInput{}, domain.Validation(
			"session resource type must be memory_store",
		)
	}
	if input.MemoryStoreID == "" {
		return app.MemorySessionResourceInput{}, domain.Validation("memory_store_id is required")
	}
	if input.Access.Null || input.Instructions.Null {
		return app.MemorySessionResourceInput{}, domain.Validation(
			"access and instructions cannot be null",
		)
	}
	access := domain.MemoryAccessReadWrite
	if input.Access.Present {
		access = input.Access.Value
	}
	if access != domain.MemoryAccessReadWrite && access != domain.MemoryAccessReadOnly {
		return app.MemorySessionResourceInput{}, domain.Validation(
			"access must be read_write or read_only",
		)
	}
	instructions := ""
	if input.Instructions.Present {
		instructions = input.Instructions.Value
	}
	return app.MemorySessionResourceInput{
		MemoryStoreID: input.MemoryStoreID, Access: access, Instructions: instructions,
	}, nil
}
