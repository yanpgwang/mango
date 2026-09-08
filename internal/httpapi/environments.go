package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func envToJSON(e domain.Environment) map[string]any {
	out := map[string]any{
		"id": e.ID, "type": "environment", "name": e.Name,
		"description": e.Description, "metadata": orEmptyMap(e.Metadata),
		"config":     environmentConfigToJSON(e),
		"created_at": e.CreatedAt.Format(timeFmt), "updated_at": e.UpdatedAt.Format(timeFmt),
	}
	if e.Scope != "" {
		out["scope"] = e.Scope
	}
	if e.ArchivedAt != nil {
		out["archived_at"] = e.ArchivedAt.Format(timeFmt)
	} else {
		out["archived_at"] = nil
	}
	return out
}

func environmentConfigToJSON(e domain.Environment) map[string]any {
	return map[string]any{"type": "self_hosted"}
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string          `json:"name"`
		Description json.RawMessage `json:"description"`
		Metadata    json.RawMessage `json:"metadata"`
		Scope       json.RawMessage `json:"scope"`
		Config      json.RawMessage `json:"config"`
	}
	if err := decodeJSONBody(r, &in); err != nil {
		writeError(w, err)
		return
	}
	description, err := parseEnvironmentOptionalString(in.Description, "description")
	if err != nil {
		writeError(w, err)
		return
	}
	scope, err := parseEnvironmentOptionalString(in.Scope, "scope")
	if err != nil {
		writeError(w, err)
		return
	}
	metadata, err := parseEnvironmentOptionalObject(in.Metadata, "metadata")
	if err != nil {
		writeError(w, err)
		return
	}
	config, err := parseEnvironmentOptionalObject(in.Config, "config")
	if err != nil {
		writeError(w, err)
		return
	}
	var ct string
	if config != nil {
		if t, ok := config["type"].(string); ok {
			ct = t
		}
	}
	e, err := s.deps.Envs.Create(r.Context(), domain.Environment{
		Name: in.Name, Description: description, Metadata: metadata, Scope: scope,
		ConfigType: ct, Config: config,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, envToJSON(e))
}

func parseEnvironmentOptionalString(raw json.RawMessage, field string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return "", domain.Validation(field + " cannot be null")
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", domain.Validation(field + " must be a string")
	}
	return value, nil
}

func parseEnvironmentOptionalObject(raw json.RawMessage, field string) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, domain.Validation(field + " cannot be null")
	}
	var value map[string]any
	if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
		return nil, domain.Validation(field + " must be an object")
	}
	return value, nil
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		Metadata    json.RawMessage `json:"metadata"`
		Scope       json.RawMessage `json:"scope"`
		Config      json.RawMessage `json:"config"`
	}
	if err := decodeJSONBody(r, &in); err != nil {
		writeError(w, err)
		return
	}
	name, err := parseEnvironmentUpdateString(in.Name, "name")
	if err != nil {
		writeError(w, err)
		return
	}
	description, err := parseEnvironmentUpdateString(in.Description, "description")
	if err != nil {
		writeError(w, err)
		return
	}
	scope, err := parseEnvironmentUpdateString(in.Scope, "scope")
	if err != nil {
		writeError(w, err)
		return
	}
	metadata, err := parseEnvironmentMetadataPatch(in.Metadata)
	if err != nil {
		writeError(w, err)
		return
	}
	config, err := parseEnvironmentOptionalObject(in.Config, "config")
	if err != nil {
		writeError(w, err)
		return
	}
	patch := domain.EnvironmentPatch{
		Name: name, Description: description, Metadata: metadata, Scope: scope,
	}
	if len(bytes.TrimSpace(in.Config)) > 0 {
		patch.Config = &config
	}
	updated, err := s.deps.Envs.Update(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envToJSON(updated))
}

func parseEnvironmentUpdateString(raw json.RawMessage, field string) (*string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	value, err := parseEnvironmentOptionalString(raw, field)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func parseEnvironmentMetadataPatch(raw json.RawMessage) (map[string]any, error) {
	metadata, err := parseEnvironmentOptionalObject(raw, "metadata")
	if err != nil || metadata == nil {
		return metadata, err
	}
	for _, value := range metadata {
		if value == nil {
			continue
		}
		if _, ok := value.(string); !ok {
			return nil, domain.Validation("metadata values must be strings or null")
		}
	}
	return metadata, nil
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	e, err := s.deps.Envs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, envToJSON(e))
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	query, filter, err := parseEnvironmentListParams(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Envs.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]any, 0, len(page.Environments))
	for _, e := range page.Environments {
		out = append(out, envToJSON(e))
	}
	var nextPage any
	if page.HasNext && len(page.Environments) > 0 {
		last := page.Environments[len(page.Environments)-1]
		nextPage = encodeResourceCursor(resourceCursor{
			Kind:      environmentListCursorKind,
			CreatedAt: last.CreatedAt.UTC().Format(timeFmt),
			ID:        last.ID,
			Filter:    filter.fingerprint(),
		})
	}
	writeJSON(w, 200, map[string]any{"data": out, "next_page": nextPage})
}

func parseEnvironmentListParams(
	r *http.Request,
) (app.EnvironmentListQuery, environmentCursorFilter, error) {
	values := r.URL.Query()
	query := app.EnvironmentListQuery{Limit: app.DefaultEnvironmentListLimit}
	filter := environmentCursorFilter{}

	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil {
			return app.EnvironmentListQuery{}, environmentCursorFilter{}, err
		}
		if limit > app.MaxEnvironmentListLimit {
			return app.EnvironmentListQuery{}, environmentCursorFilter{},
				domain.Validation("limit must not exceed 1000")
		}
		query.Limit = limit
	}
	if values.Has("include_archived") {
		include, err := parseResourceListBool(values.Get("include_archived"), "include_archived")
		if err != nil {
			return app.EnvironmentListQuery{}, environmentCursorFilter{}, err
		}
		query.IncludeArchived = include
	}
	filter.IncludeArchived = query.IncludeArchived

	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), environmentListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return app.EnvironmentListQuery{}, environmentCursorFilter{},
				domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return app.EnvironmentListQuery{}, environmentCursorFilter{},
				domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, filter, nil
}

func (s *Server) archiveEnvironment(w http.ResponseWriter, r *http.Request) {
	e, err := s.deps.Envs.Archive(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, envToJSON(e))
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Envs.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": r.PathValue("id"), "type": "environment_deleted",
	})
}
