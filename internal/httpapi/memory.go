package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

type optionalJSONField[T any] struct {
	Value   T
	Present bool
	Null    bool
}

func (f *optionalJSONField[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		f.Null = true
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&f.Value)
}

type memoryStoreCreateRequest struct {
	Name        string                               `json:"name"`
	Description optionalJSONField[string]            `json:"description"`
	Metadata    optionalJSONField[map[string]string] `json:"metadata"`
}

type memoryStoreUpdateRequest struct {
	Name        optionalJSONField[string]             `json:"name"`
	Description optionalJSONField[string]             `json:"description"`
	Metadata    optionalJSONField[map[string]*string] `json:"metadata"`
}

type memoryCreateRequest struct {
	Path    string                    `json:"path"`
	Content optionalJSONField[string] `json:"content"`
}

type memoryUpdateRequest struct {
	Path         optionalJSONField[string]                    `json:"path"`
	Content      optionalJSONField[string]                    `json:"content"`
	Precondition optionalJSONField[memoryPreconditionRequest] `json:"precondition"`
}

type memoryPreconditionRequest struct {
	Type          string  `json:"type"`
	ContentSHA256 *string `json:"content_sha256"`
}

func (s *Server) createMemoryStore(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	var body memoryStoreCreateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Description.Null || body.Metadata.Null {
		writeError(w, domain.Validation("description and metadata cannot be null"))
		return
	}
	description := ""
	if body.Description.Present {
		description = body.Description.Value
	}
	var metadata map[string]string
	if body.Metadata.Present {
		metadata = body.Metadata.Value
	}
	created, err := s.deps.Memory.CreateStore(r.Context(), app.MemoryStoreCreateInput{
		Name: body.Name, Description: description, Metadata: metadata,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryStoreToJSON(created))
}

func (s *Server) getMemoryStore(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	item, err := s.deps.Memory.GetStore(r.Context(), r.PathValue("store_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryStoreToJSON(item))
}

func (s *Server) updateMemoryStore(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	var body memoryStoreUpdateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Name.Null || body.Description.Null || body.Metadata.Null {
		writeError(w, domain.Validation("name, description, and metadata cannot be null"))
		return
	}
	var name, description *string
	if body.Name.Present {
		name = &body.Name.Value
	}
	if body.Description.Present {
		description = &body.Description.Value
	}
	var metadata map[string]*string
	if body.Metadata.Present {
		metadata = body.Metadata.Value
	}
	item, err := s.deps.Memory.UpdateStore(r.Context(), r.PathValue("store_id"), app.MemoryStoreUpdateInput{
		Name: name, Description: description, Metadata: metadata,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryStoreToJSON(item))
}

func (s *Server) listMemoryStores(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	query, filter, err := parseMemoryStoreListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Memory.ListStores(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Stores))
	for _, item := range page.Stores {
		data = append(data, memoryStoreToJSON(item))
	}
	var next any
	if page.HasNext && len(page.Stores) > 0 {
		last := page.Stores[len(page.Stores)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: memoryStoreListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasNext, "next_page": next,
	})
}

func (s *Server) archiveMemoryStore(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	item, err := s.deps.Memory.ArchiveStore(r.Context(), r.PathValue("store_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryStoreToJSON(item))
}

func (s *Server) deleteMemoryStore(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	id := r.PathValue("store_id")
	if err := s.deps.Memory.DeleteStore(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "type": "memory_store_deleted"})
}

func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	full, err := parseMemoryView(r, false)
	if err != nil {
		writeError(w, err)
		return
	}
	var body memoryCreateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if !body.Content.Present || body.Content.Null {
		writeError(w, domain.Validation("content is required"))
		return
	}
	item, err := s.deps.Memory.CreateMemory(r.Context(), r.PathValue("store_id"), app.MemoryCreateInput{
		Path: body.Path, Content: body.Content.Value, Actor: requestMemoryActor(r),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryToJSON(item, full))
}

func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	full, err := parseMemoryView(r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	item, err := s.deps.Memory.GetMemory(
		r.Context(), r.PathValue("store_id"), r.PathValue("memory_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryToJSON(item, full))
}

func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	full, err := parseMemoryView(r, false)
	if err != nil {
		writeError(w, err)
		return
	}
	var body memoryUpdateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Path.Null || body.Content.Null || body.Precondition.Null {
		writeError(w, domain.Validation("path, content, and precondition cannot be null"))
		return
	}
	var path, content *string
	if body.Path.Present {
		path = &body.Path.Value
	}
	if body.Content.Present {
		content = &body.Content.Value
	}
	var expected *string
	if body.Precondition.Present {
		if body.Precondition.Value.Type != "content_sha256" || body.Precondition.Value.ContentSHA256 == nil {
			writeError(w, domain.Validation("precondition must contain type content_sha256 and content_sha256"))
			return
		}
		expected = body.Precondition.Value.ContentSHA256
	}
	item, err := s.deps.Memory.UpdateMemory(
		r.Context(), r.PathValue("store_id"), r.PathValue("memory_id"),
		app.MemoryUpdateInput{
			Path: path, Content: content, ExpectedContentSHA: expected,
			Actor: requestMemoryActor(r),
		},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryToJSON(item, full))
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	query, filter, err := parseMemoryListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Memory.ListMemories(r.Context(), r.PathValue("store_id"), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Memory != nil {
			data = append(data, memoryToJSON(*item.Memory, query.Full))
		} else {
			data = append(data, map[string]any{"path": item.Prefix, "type": "memory_prefix"})
		}
	}
	var next any
	if page.HasNext && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		path := last.Prefix
		if last.Memory != nil {
			path = last.Memory.Path
		}
		next = encodeMemoryListCursor(memoryListCursor{
			Kind: "memory_list", StoreID: r.PathValue("store_id"), Path: path,
			Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasNext, "next_page": next,
	})
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	var expected *string
	if value := r.URL.Query().Get("expected_content_sha256"); value != "" || r.URL.Query().Has("expected_content_sha256") {
		expected = &value
	}
	item, err := s.deps.Memory.DeleteMemory(
		r.Context(), r.PathValue("store_id"), r.PathValue("memory_id"), expected,
		requestMemoryActor(r),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": item.ID, "type": "memory_deleted"})
}

func (s *Server) getMemoryVersion(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	full, err := parseMemoryView(r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	item, err := s.deps.Memory.GetMemoryVersion(
		r.Context(), r.PathValue("store_id"), r.PathValue("version_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryVersionToJSON(item, full))
}

func (s *Server) listMemoryVersions(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	query, full, filter, err := parseMemoryVersionListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Memory.ListMemoryVersions(r.Context(), r.PathValue("store_id"), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Versions))
	for _, item := range page.Versions {
		data = append(data, memoryVersionToJSON(item, full))
	}
	var next any
	if page.HasNext && len(page.Versions) > 0 {
		last := page.Versions[len(page.Versions)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: memoryVersionListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasNext, "next_page": next,
	})
}

func (s *Server) redactMemoryVersion(w http.ResponseWriter, r *http.Request) {
	if !s.memoryConfigured(w) {
		return
	}
	item, err := s.deps.Memory.RedactMemoryVersion(
		r.Context(), r.PathValue("store_id"), r.PathValue("version_id"),
		requestMemoryActor(r),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryVersionToJSON(item, true))
}

func (s *Server) memoryConfigured(w http.ResponseWriter) bool {
	if s.deps.Memory != nil {
		return true
	}
	writeError(w, domain.Unsupported("Memory API is not configured"))
	return false
}

type memoryStoreCursorFilter struct {
	CreatedAtGte    string `json:"created_at_gte,omitempty"`
	CreatedAtLte    string `json:"created_at_lte,omitempty"`
	IncludeArchived bool   `json:"include_archived"`
}

func (f memoryStoreCursorFilter) fingerprint() string { return resourceFilterFingerprint(f) }

func parseMemoryStoreListQuery(r *http.Request) (app.MemoryStoreListQuery, memoryStoreCursorFilter, error) {
	values := r.URL.Query()
	query := app.MemoryStoreListQuery{Limit: app.DefaultMemoryStoreListLimit}
	filter := memoryStoreCursorFilter{}
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxMemoryStoreListLimit {
			return query, filter, domain.Validation("limit must be between 1 and 100")
		}
		query.Limit = limit
	}
	if values.Has("include_archived") {
		value, err := parseResourceListBool(values.Get("include_archived"), "include_archived")
		if err != nil {
			return query, filter, err
		}
		query.IncludeArchived, filter.IncludeArchived = value, value
	}
	if raw := values.Get("created_at[gte]"); raw != "" || values.Has("created_at[gte]") {
		parsed, ok := parseTimeParam(raw)
		if !ok {
			return query, filter, domain.Validation("created_at[gte] must be an RFC 3339 timestamp")
		}
		value := parsed.UTC()
		query.CreatedAtGte = &value
		filter.CreatedAtGte = value.Format(timeFmt)
	}
	if raw := values.Get("created_at[lte]"); raw != "" || values.Has("created_at[lte]") {
		parsed, ok := parseTimeParam(raw)
		if !ok {
			return query, filter, domain.Validation("created_at[lte] must be an RFC 3339 timestamp")
		}
		value := parsed.UTC()
		query.CreatedAtLte = &value
		filter.CreatedAtLte = value.Format(timeFmt)
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), memoryStoreListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return query, filter, domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return query, filter, domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, filter, nil
}

type memoryListCursorFilter struct {
	PathPrefix string `json:"path_prefix,omitempty"`
	Depth      int    `json:"depth"`
	Full       bool   `json:"full"`
}

func (f memoryListCursorFilter) fingerprint() string { return resourceFilterFingerprint(f) }

type memoryListCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	StoreID string `json:"store_id"`
	Path    string `json:"path"`
	Filter  string `json:"filter"`
}

func encodeMemoryListCursor(cursor memoryListCursor) string {
	cursor.Version = 1
	body, _ := json.Marshal(cursor)
	return resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(body)
}

func decodeMemoryListCursor(token, storeID string) (memoryListCursor, bool) {
	raw, ok := strings.CutPrefix(token, resourceCursorPrefix)
	if !ok {
		return memoryListCursor{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return memoryListCursor{}, false
	}
	var cursor memoryListCursor
	if json.Unmarshal(body, &cursor) != nil || cursor.Version != 1 ||
		cursor.Kind != "memory_list" || cursor.StoreID != storeID ||
		cursor.Path == "" || cursor.Filter == "" {
		return memoryListCursor{}, false
	}
	return cursor, true
}

func parseMemoryListQuery(r *http.Request) (app.MemoryListQuery, memoryListCursorFilter, error) {
	values := r.URL.Query()
	full, err := parseMemoryView(r, false)
	if err != nil {
		return app.MemoryListQuery{}, memoryListCursorFilter{}, err
	}
	query := app.MemoryListQuery{
		PathPrefix: values.Get("path_prefix"), Limit: app.DefaultMemoryListLimit, Full: full,
	}
	if values.Has("depth") {
		depth, parseErr := strconv.Atoi(values.Get("depth"))
		if parseErr != nil {
			return query, memoryListCursorFilter{}, domain.Validation("depth must be 0 or 1")
		}
		query.Depth = depth
	}
	maxLimit := app.MaxMemoryListLimit
	if full {
		maxLimit = app.MaxFullMemoryListLimit
	}
	if values.Has("limit") {
		limit, parseErr := parseResourceListLimit(values.Get("limit"))
		if parseErr != nil || limit > maxLimit {
			return query, memoryListCursorFilter{}, domain.Validation("limit must be between 1 and " + strconv.Itoa(maxLimit))
		}
		query.Limit = limit
	}
	filter := memoryListCursorFilter{PathPrefix: query.PathPrefix, Depth: query.Depth, Full: full}
	if values.Has("page") {
		cursor, ok := decodeMemoryListCursor(values.Get("page"), r.PathValue("store_id"))
		if !ok || cursor.Filter != filter.fingerprint() {
			return query, filter, domain.Validation("invalid page cursor")
		}
		query.AfterPath = cursor.Path
	}
	return query, filter, nil
}

type memoryVersionCursorFilter struct {
	StoreID    string `json:"store_id"`
	APIKeyID   string `json:"api_key_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	MemoryID   string `json:"memory_id,omitempty"`
	Operation  string `json:"operation,omitempty"`
	CreatedGte string `json:"created_at_gte,omitempty"`
	CreatedLte string `json:"created_at_lte,omitempty"`
	Full       bool   `json:"full"`
}

func (f memoryVersionCursorFilter) fingerprint() string { return resourceFilterFingerprint(f) }

func parseMemoryVersionListQuery(r *http.Request) (app.MemoryVersionListQuery, bool, memoryVersionCursorFilter, error) {
	values := r.URL.Query()
	full, err := parseMemoryView(r, false)
	if err != nil {
		return app.MemoryVersionListQuery{}, false, memoryVersionCursorFilter{}, err
	}
	query := app.MemoryVersionListQuery{
		APIKeyID: values.Get("api_key_id"), SessionID: values.Get("session_id"),
		MemoryID: values.Get("memory_id"), Operation: values.Get("operation"),
		Limit: app.DefaultMemoryVersionListLimit,
	}
	filter := memoryVersionCursorFilter{
		StoreID: r.PathValue("store_id"), APIKeyID: query.APIKeyID,
		SessionID: query.SessionID, MemoryID: query.MemoryID,
		Operation: query.Operation, Full: full,
	}
	if values.Has("limit") {
		limit, parseErr := parseResourceListLimit(values.Get("limit"))
		if parseErr != nil || limit > app.MaxMemoryVersionListLimit {
			return query, full, filter, domain.Validation("limit must be between 1 and 100")
		}
		query.Limit = limit
	}
	if raw := values.Get("created_at[gte]"); raw != "" || values.Has("created_at[gte]") {
		parsed, ok := parseTimeParam(raw)
		if !ok {
			return query, full, filter, domain.Validation("created_at[gte] must be an RFC 3339 timestamp")
		}
		value := parsed.UTC()
		query.CreatedAtGte = &value
		filter.CreatedGte = value.Format(timeFmt)
	}
	if raw := values.Get("created_at[lte]"); raw != "" || values.Has("created_at[lte]") {
		parsed, ok := parseTimeParam(raw)
		if !ok {
			return query, full, filter, domain.Validation("created_at[lte] must be an RFC 3339 timestamp")
		}
		value := parsed.UTC()
		query.CreatedAtLte = &value
		filter.CreatedLte = value.Format(timeFmt)
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), memoryVersionListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return query, full, filter, domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return query, full, filter, domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, full, filter, nil
}

func parseMemoryView(r *http.Request, defaultFull bool) (bool, error) {
	view := r.URL.Query().Get("view")
	switch view {
	case "":
		return defaultFull, nil
	case "basic":
		return false, nil
	case "full":
		return true, nil
	default:
		return false, domain.Validation("view must be basic or full")
	}
}

func memoryStoreToJSON(item domain.MemoryStore) map[string]any {
	var archived any
	if item.ArchivedAt != nil {
		archived = item.ArchivedAt.Format(timeFmt)
	}
	metadata := item.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	return map[string]any{
		"id": item.ID, "type": "memory_store", "name": item.Name,
		"description": item.Description, "metadata": metadata,
		"created_at": item.CreatedAt.Format(timeFmt),
		"updated_at": item.UpdatedAt.Format(timeFmt), "archived_at": archived,
	}
}

func memoryToJSON(item domain.Memory, full bool) map[string]any {
	var content any
	if full {
		content = item.Content
	}
	return map[string]any{
		"id": item.ID, "type": "memory", "memory_store_id": item.MemoryStoreID,
		"memory_version_id": item.MemoryVersionID, "path": item.Path,
		"content": content, "content_size_bytes": item.ContentSize,
		"content_sha256": item.ContentSHA256,
		"created_at":     item.CreatedAt.Format(timeFmt), "updated_at": item.UpdatedAt.Format(timeFmt),
	}
}

func memoryVersionToJSON(item domain.MemoryVersion, full bool) map[string]any {
	var content any
	if full && item.Content != nil && item.RedactedAt == nil {
		content = *item.Content
	}
	var path, size, sha, redactedAt, redactedBy any
	if item.Path != nil {
		path = *item.Path
	}
	if item.ContentSize != nil {
		size = *item.ContentSize
	}
	if item.ContentSHA256 != nil {
		sha = *item.ContentSHA256
	}
	if item.RedactedAt != nil {
		redactedAt = item.RedactedAt.Format(timeFmt)
	}
	if item.RedactedBy != nil {
		redactedBy = memoryActorToJSON(*item.RedactedBy)
	}
	return map[string]any{
		"id": item.ID, "type": "memory_version",
		"memory_store_id": item.MemoryStoreID, "memory_id": item.MemoryID,
		"operation": item.Operation, "path": path, "content": content,
		"content_size_bytes": size, "content_sha256": sha,
		"created_at":  item.CreatedAt.Format(timeFmt),
		"created_by":  memoryActorToJSON(item.CreatedBy),
		"redacted_at": redactedAt, "redacted_by": redactedBy,
	}
}

func memoryActorToJSON(actor domain.MemoryActor) map[string]any {
	result := map[string]any{"type": actor.Type}
	switch actor.Type {
	case "session_actor":
		result["session_id"] = actor.ID
	case "user_actor":
		result["user_id"] = actor.ID
	default:
		result["api_key_id"] = actor.ID
	}
	return result
}

func requestMemoryActor(r *http.Request) domain.MemoryActor {
	if scope, ok := workspace.FromContext(r.Context()); ok && scope.Session != nil {
		return domain.MemoryActor{Type: "session_actor", ID: scope.Session.SessionID}
	}
	credential, present, valid := requestBearerToken(r)
	if !present || !valid {
		return domain.MemoryActor{Type: "api_actor", ID: "api_key_local"}
	}
	sum := sha256.Sum256([]byte(credential))
	return domain.MemoryActor{
		Type: "api_actor", ID: "api_key_" + hex.EncodeToString(sum[:12]),
	}
}
