package domain

import (
	"reflect"
	"time"
)

type Environment struct {
	ID          string
	Name        string
	Description string
	Metadata    map[string]any
	Scope       string
	ConfigType  string // "self_hosted"
	Config      map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  *time.Time
}

// SessionConfig returns an isolated JSON-shaped copy of the worker
// configuration captured for a Session.
func (e Environment) SessionConfig() map[string]any {
	return cloneEnvironmentObject(e.Config)
}

func cloneEnvironmentObject(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneEnvironmentValue(value)
	}
	return cloned
}

func cloneEnvironmentValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneEnvironmentObject(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneEnvironmentValue(nested)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// EnvironmentPatch is the domain form of POST /v1/environments/{id}. Nil
// pointers preserve scalar fields. Metadata is a per-key patch: null and empty
// string delete a key, matching the public Environment update contract.
type EnvironmentPatch struct {
	Name        *string
	Description *string
	Metadata    map[string]any
	Scope       *string
	Config      *map[string]any
}

func (e Environment) Apply(patch EnvironmentPatch) (Environment, bool) {
	next := e
	if e.Metadata != nil {
		next.Metadata = make(map[string]any, len(e.Metadata))
		for key, value := range e.Metadata {
			next.Metadata[key] = value
		}
	}
	if patch.Name != nil {
		next.Name = *patch.Name
	}
	if patch.Description != nil {
		next.Description = *patch.Description
	}
	for key, value := range patch.Metadata {
		text, isString := value.(string)
		if value == nil || (isString && text == "") {
			delete(next.Metadata, key)
			continue
		}
		if next.Metadata == nil {
			next.Metadata = map[string]any{}
		}
		next.Metadata[key] = value
	}
	if patch.Scope != nil {
		next.Scope = *patch.Scope
	}
	if patch.Config != nil {
		next.Config = cloneEnvironmentObject(*patch.Config)
	}
	return next, !environmentFieldsEqual(e, next)
}

func environmentFieldsEqual(left, right Environment) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}
