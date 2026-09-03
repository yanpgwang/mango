package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const defaultEventLimit = 100

// maxPageLimit is the shared upper bound on the `limit` query parameter for both
// the List Sessions and List Events endpoints. A request above this bound is a
// validation error rather than a silently-clamped value, so a client that asks
// for more than the server will serve learns it explicitly. Existing defaults
// and cursor semantics are unchanged.
const maxPageLimit = 1000

// maxDeltaOptIn bounds the number of event_deltas[] opt-in values a stream
// request may carry.
const maxDeltaOptIn = 100

// deltaOptInTypes is the closed set of event types a client may opt into for
// preview frames. Only agent.message previews are currently emitted, but the
// opt-in contract accepts agent.thinking too.
var deltaOptInTypes = map[string]bool{
	domain.EvAgentMessage:  true,
	domain.EvAgentThinking: true,
}

// toDrafts converts raw event objects (top-level tagged union) into internal
// drafts, flattening every field except "type" into the payload. It does not
// validate; callers validate types separately.
func toDrafts(items []map[string]any) []domain.EventDraft {
	var out []domain.EventDraft
	for _, it := range items {
		t, _ := it["type"].(string)
		if t == "" {
			continue
		}
		payload := map[string]any{}
		for k, v := range it {
			if k != "type" {
				payload[k] = v
			}
		}
		out = append(out, domain.EventDraft{Type: t, Payload: payload})
	}
	return out
}

// eventToJSON renders the public wire shape of a persisted event: a top-level
// tagged union of {id, type, ...type-specific fields, processed_at}. The
// internal sequence number is never emitted.
func eventToJSON(e domain.Event) map[string]any {
	out := map[string]any{"id": e.ID, "type": e.Type}
	for k, v := range e.Payload {
		if k == "id" || k == "type" || k == "processed_at" || strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = v
	}
	if e.ProcessedAt != nil {
		out["processed_at"] = e.ProcessedAt.Format(timeFmt)
	} else {
		out["processed_at"] = nil
	}
	return out
}

func (s *Server) sendEvents(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []map[string]any `json:"events"`
	}
	if err := decodeJSONBody(r, &in); err != nil {
		writeError(w, err)
		return
	}
	if len(in.Events) == 0 {
		writeError(w, domain.Validation("events must contain at least one event"))
		return
	}
	// Validate the tagged-union variant before touching the session.
	for _, it := range in.Events {
		if err := validateClientEvent(it); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := validateClientEventBatch(in.Events); err != nil {
		writeError(w, err)
		return
	}
	if requestScope, _ := workspace.FromContext(r.Context()); requestScope.Session != nil {
		for _, event := range in.Events {
			typeName, _ := event["type"].(string)
			if !domain.IsSessionCredentialEvent(typeName) {
				writeError(w, domain.Permission(
					"session credential may only send tool-result events",
				))
				return
			}
		}
	}
	drafts := toDrafts(in.Events)
	out, err := s.deps.Sessions.SendEvent(r.Context(), r.PathValue("id"), drafts)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]any, 0, len(out))
	for _, e := range out {
		data = append(data, eventToJSON(e))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.deps.Sessions.Get(r.Context(), sessionID); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()

	order := q.Get("order")
	if order != "" && order != "asc" && order != "desc" {
		writeError(w, domain.Validation("order must be asc or desc"))
		return
	}
	desc := order == "desc"
	limit := defaultEventLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, domain.Validation("limit must be a positive integer"))
			return
		}
		if n > maxPageLimit {
			writeError(w, domain.Validation("limit must not exceed 1000"))
			return
		}
		limit = n
	}

	eq := app.EventQuery{Limit: limit, Desc: desc}
	if types, ok := q["types[]"]; ok {
		eq.Types = types
	}
	cursorFilter := eventCursorFilter{Types: eq.Types}
	for _, item := range []struct {
		key        string
		dst        **time.Time
		normalized *string
	}{
		{"created_at[gt]", &eq.ProcessedAtGt, &cursorFilter.CreatedAtGt},
		{"created_at[gte]", &eq.ProcessedAtGte, &cursorFilter.CreatedAtGte},
		{"created_at[lt]", &eq.ProcessedAtLt, &cursorFilter.CreatedAtLt},
		{"created_at[lte]", &eq.ProcessedAtLte, &cursorFilter.CreatedAtLte},
	} {
		if raw := q.Get(item.key); raw != "" {
			t, ok := parseTimeParam(raw)
			if !ok {
				writeError(w, domain.Validation(item.key+" must be an RFC 3339 timestamp"))
				return
			}
			*item.dst = t
			*item.normalized = t.UTC().Format(timeFmt)
		}
	}
	filterFingerprint := cursorFilter.fingerprint()

	// An opaque page cursor supersedes the default bounds. It must have been
	// created with the same order as this request.
	if page := q.Get("page"); page != "" {
		c, ok := decodeEventCursor(page)
		if !ok {
			writeError(w, domain.Validation("invalid page cursor"))
			return
		}
		wantOrder := "asc"
		if desc {
			wantOrder = "desc"
		}
		if c.Order != wantOrder {
			writeError(w, domain.Validation("page cursor order mismatch"))
			return
		}
		if c.SessionID != sessionID || c.ThreadID != "" || c.Filter != filterFingerprint {
			writeError(w, domain.Validation("page cursor scope mismatch"))
			return
		}
		boundary := &app.EventPageBoundary{Sequence: c.Sequence}
		if !c.Unprocessed {
			processedAt, ok := parseTimeParam(c.ProcessedAt)
			if !ok {
				writeError(w, domain.Validation("invalid page cursor"))
				return
			}
			boundary.ProcessedAt = processedAt
		}
		eq.Boundary = boundary
	}

	// Fetch one extra row to decide whether a next page exists.
	eq.Limit = limit + 1
	hist, err := s.deps.Events.Query(r.Context(), sessionID, eq)
	if err != nil {
		writeError(w, err)
		return
	}

	var nextPage any
	if len(hist) > limit {
		hist = hist[:limit]
		if len(hist) > 0 {
			last := hist[len(hist)-1]
			order := "asc"
			if desc {
				order = "desc"
			}
			cursor := eventCursor{
				Order: order, SessionID: sessionID, Filter: filterFingerprint,
				Sequence: last.Sequence,
			}
			if last.ProcessedAt == nil {
				cursor.Unprocessed = true
			} else {
				cursor.ProcessedAt = last.ProcessedAt.UTC().Format(timeFmt)
			}
			nextPage = encodeEventCursor(cursor)
		}
	}

	data := make([]any, 0, len(hist))
	for _, e := range hist {
		data = append(data, eventToJSON(e))
	}
	writeJSON(w, 200, map[string]any{"data": data, "next_page": nextPage})
}

func validateClientEvent(event map[string]any) error {
	t, ok := event["type"].(string)
	if !ok || t == "" || !domain.IsClientSubmittable(t) {
		return domain.Validation("event type not accepted from clients: " + t)
	}
	if _, ok := event["id"]; ok {
		return domain.Validation("client events must not include id")
	}
	if _, ok := event["processed_at"]; ok {
		return domain.Validation("client events must not include processed_at")
	}
	allowedFields := map[string]map[string]bool{
		domain.EvUserMessage:          {"type": true, "content": true},
		domain.EvSystemMessage:        {"type": true, "content": true},
		domain.EvUserInterrupt:        {"type": true, "session_thread_id": true},
		domain.EvUserCustomToolResult: {"type": true, "custom_tool_use_id": true, "content": true, "is_error": true, "session_thread_id": true},
		domain.EvUserToolResult:       {"type": true, "tool_use_id": true, "content": true, "is_error": true, "session_thread_id": true},
		domain.EvUserToolConfirmation: {"type": true, "tool_use_id": true, "result": true, "deny_message": true, "session_thread_id": true},
		domain.EvUserDefineOutcome:    {"type": true, "description": true, "rubric": true, "max_iterations": true},
	}
	for key := range event {
		if !allowedFields[t][key] {
			return domain.Validation(fmt.Sprintf("unknown field %q for %s", key, t))
		}
	}

	requireString := func(key string) error {
		if value, ok := event[key].(string); !ok || value == "" {
			return domain.Validation(fmt.Sprintf("%s is required for %s", key, t))
		}
		return nil
	}
	validateContent := func(
		required bool,
		allowedTypes map[string]bool,
		allowFileDocument bool,
	) error {
		content, ok := event["content"].([]any)
		if !ok {
			if !required {
				if _, present := event["content"]; !present {
					return nil
				}
			}
			return domain.Validation(fmt.Sprintf("content is required for %s", t))
		}
		if required && len(content) == 0 {
			return domain.Validation(fmt.Sprintf("content is required for %s", t))
		}
		if len(content) > 1000 {
			return domain.Validation("content must contain at most 1000 blocks")
		}
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				return domain.Validation("content blocks must be objects")
			}
			blockType, _ := block["type"].(string)
			if blockType == "" {
				return domain.Validation("content block type is required")
			}
			if !allowedTypes[blockType] {
				return domain.Validation(fmt.Sprintf("content block type %q is not allowed for %s", blockType, t))
			}
			if err := validateClientContentBlock(block, allowFileDocument); err != nil {
				return err
			}
		}
		return nil
	}
	messageContent := map[string]bool{"text": true, "image": true, "document": true}
	resultContent := map[string]bool{
		"text": true, "image": true, "document": true, "search_result": true,
	}
	systemContent := map[string]bool{"text": true}

	switch t {
	case domain.EvUserMessage:
		return validateContent(true, messageContent, true)
	case domain.EvSystemMessage:
		return validateContent(true, systemContent, false)
	case domain.EvUserInterrupt:
		if err := optionalString(event, "session_thread_id"); err != nil {
			return err
		}
		if threadID, present := event["session_thread_id"].(string); present && threadID == "" {
			return domain.Validation("session_thread_id must not be empty")
		} else if present && !strings.HasPrefix(threadID, domain.PrefixSessionThread) {
			return domain.Validation("session_thread_id must start with sthr_")
		}
		return nil
	case domain.EvUserCustomToolResult:
		if err := requireString("custom_tool_use_id"); err != nil {
			return err
		}
		if err := validateContent(false, resultContent, false); err != nil {
			return err
		}
		if err := optionalString(event, "session_thread_id"); err != nil {
			return err
		}
	case domain.EvUserToolResult:
		if err := requireString("tool_use_id"); err != nil {
			return err
		}
		if err := validateContent(false, resultContent, false); err != nil {
			return err
		}
		if err := optionalString(event, "session_thread_id"); err != nil {
			return err
		}
	case domain.EvUserToolConfirmation:
		if err := requireString("tool_use_id"); err != nil {
			return err
		}
		result, _ := event["result"].(string)
		if result != "allow" && result != "deny" {
			return domain.Validation("result must be allow or deny for user.tool_confirmation")
		}
		if _, present := event["deny_message"]; present {
			if result != "deny" {
				return domain.Validation("deny_message is only allowed when result is deny")
			}
			if err := optionalString(event, "deny_message"); err != nil {
				return err
			}
		}
		if err := optionalString(event, "session_thread_id"); err != nil {
			return err
		}
	case domain.EvUserDefineOutcome:
		if err := requireString("description"); err != nil {
			return err
		}
		rubric, ok := event["rubric"].(map[string]any)
		if !ok {
			return domain.Validation("rubric is required for user.define_outcome")
		}
		switch rubric["type"] {
		case "text":
			if err := validateObjectFields(
				rubric,
				map[string]bool{"type": true, "content": true},
				"text rubric",
			); err != nil {
				return err
			}
			if value, ok := rubric["content"].(string); !ok || value == "" {
				return domain.Validation("text rubric requires content")
			} else if utf8.RuneCountInString(value) > domain.MaxOutcomeRubricCharacters {
				return domain.Validation("text rubric content must contain at most 262144 characters")
			}
		case "file":
			if err := validateObjectFields(
				rubric,
				map[string]bool{"type": true, "file_id": true},
				"file rubric",
			); err != nil {
				return err
			}
			if value, ok := rubric["file_id"].(string); !ok || value == "" {
				return domain.Validation("file rubric requires file_id")
			}
		default:
			return domain.Validation("rubric type must be text or file")
		}
		if raw, present := event["max_iterations"]; present {
			value, ok := raw.(float64)
			if !ok || value != float64(int(value)) || value < 1 || value > 20 {
				return domain.Validation("max_iterations must be an integer from 1 to 20")
			}
		}
	}
	if raw, present := event["content"]; present {
		if _, ok := raw.([]any); !ok {
			return domain.Validation("content must be an array")
		}
	}
	if raw, present := event["is_error"]; present {
		if _, ok := raw.(bool); !ok {
			return domain.Validation("is_error must be a boolean")
		}
	}
	return nil
}

func validateClientContentBlock(block map[string]any, allowFileDocument bool) error {
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		if err := validateObjectFields(
			block,
			map[string]bool{"type": true, "text": true},
			"text content block",
		); err != nil {
			return err
		}
		if _, ok := block["text"].(string); !ok {
			return domain.Validation("text content blocks require text")
		}
	case "image":
		if err := validateObjectFields(
			block,
			map[string]bool{"type": true, "source": true},
			"image content block",
		); err != nil {
			return err
		}
		return validateClientContentSource(block, "image", false)
	case "document":
		if err := validateObjectFields(
			block,
			map[string]bool{
				"type": true, "source": true, "context": true, "title": true,
			},
			"document content block",
		); err != nil {
			return err
		}
		for _, key := range []string{"context", "title"} {
			if err := optionalString(block, key); err != nil {
				return err
			}
		}
		return validateClientContentSource(block, "document", allowFileDocument)
	case "search_result":
		return validateSearchResultBlock(block)
	default:
		return domain.Validation("content block type is required")
	}
	return nil
}

func validateClientContentSource(
	block map[string]any,
	blockType string,
	allowFile bool,
) error {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return domain.Validation(blockType + " content blocks require a source")
	}
	sourceType, _ := source["type"].(string)
	if sourceType == "" {
		return domain.Validation(blockType + " content source type is required")
	}

	allowed := map[string]bool{"type": true}
	requiredStrings := []string(nil)
	switch sourceType {
	case "base64":
		allowed["data"] = true
		allowed["media_type"] = true
		requiredStrings = []string{"data", "media_type"}
	case "url":
		allowed["url"] = true
		requiredStrings = []string{"url"}
	case "text":
		if blockType != "document" {
			return domain.Validation("image content source type must be base64, url, or file")
		}
		allowed["data"] = true
		allowed["media_type"] = true
		requiredStrings = []string{"data", "media_type"}
	case "file":
		allowed["file_id"] = true
		requiredStrings = []string{"file_id"}
	default:
		if blockType == "document" {
			return domain.Validation("document content source type must be base64, text, url, or file")
		}
		return domain.Validation("image content source type must be base64, url, or file")
	}
	if err := validateObjectFields(source, allowed, blockType+" content source"); err != nil {
		return err
	}
	for _, key := range requiredStrings {
		if _, ok := source[key].(string); !ok {
			return domain.Validation(fmt.Sprintf(
				"%s content source %s is required",
				blockType,
				key,
			))
		}
	}
	if sourceType == "text" && source["media_type"] != "text/plain" {
		return domain.Validation("text document source media_type must be text/plain")
	}
	if sourceType == "file" {
		if source["file_id"] == "" {
			return domain.Validation(blockType + " file source requires file_id")
		}
		if !allowFile {
			return domain.Unsupported(
				"file-sourced content is supported only for text documents in user.message",
			)
		}
	}
	return nil
}

func validateSearchResultBlock(block map[string]any) error {
	if err := validateObjectFields(
		block,
		map[string]bool{
			"type": true, "source": true, "title": true,
			"citations": true, "content": true,
		},
		"search_result content block",
	); err != nil {
		return err
	}
	for _, key := range []string{"source", "title"} {
		if _, ok := block[key].(string); !ok {
			return domain.Validation("search_result content blocks require " + key)
		}
	}
	citations, ok := block["citations"].(map[string]any)
	if !ok {
		return domain.Validation("search_result content blocks require citations")
	}
	if err := validateObjectFields(
		citations,
		map[string]bool{"enabled": true},
		"search_result citations",
	); err != nil {
		return err
	}
	if _, ok := citations["enabled"].(bool); !ok {
		return domain.Validation("search_result citations require enabled")
	}
	content, ok := block["content"].([]any)
	if !ok {
		return domain.Validation("search_result content must be an array")
	}
	for _, raw := range content {
		text, ok := raw.(map[string]any)
		if !ok {
			return domain.Validation("search_result content blocks must be objects")
		}
		if text["type"] != "text" {
			return domain.Validation("search_result content only accepts text blocks")
		}
		if err := validateClientContentBlock(text, false); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectFields(
	object map[string]any,
	allowed map[string]bool,
	objectName string,
) error {
	for key := range object {
		if !allowed[key] {
			return domain.Validation(fmt.Sprintf("unknown field %q for %s", key, objectName))
		}
	}
	return nil
}

func validateClientEventBatch(events []map[string]any) error {
	defineOutcomes := 0
	systemMessages := 0
	for i, event := range events {
		typeName, _ := event["type"].(string)
		if typeName == domain.EvUserDefineOutcome {
			defineOutcomes++
		}
		if typeName == domain.EvSystemMessage {
			systemMessages++
			if i != len(events)-1 || i == 0 {
				return domain.Validation("system.message must be the final event and immediately follow its accompanying user event")
			}
			previousType, _ := events[i-1]["type"].(string)
			switch previousType {
			case domain.EvUserMessage, domain.EvUserToolResult, domain.EvUserCustomToolResult:
			default:
				return domain.Validation("system.message must immediately follow user.message, user.tool_result, or user.custom_tool_result")
			}
		}
	}
	if defineOutcomes > 1 {
		return domain.Validation("events may contain at most one user.define_outcome")
	}
	if systemMessages > 1 {
		return domain.Validation("events may contain at most one system.message")
	}
	return nil
}

func optionalString(object map[string]any, key string) error {
	if value, present := object[key]; present {
		if _, ok := value.(string); !ok {
			return domain.Validation(key + " must be a string")
		}
	}
	return nil
}

func parseTimeParam(s string) (*time.Time, bool) {
	if s == "" {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, false
		}
	}
	return &t, true
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	deltaOptIn, err := parseEventDeltaOptIn(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if requestScope, _ := workspace.FromContext(r.Context()); requestScope.Session != nil {
		validator, ok := s.cfg.Authenticator.(SessionScopeValidator)
		if !ok {
			writeErrorEnvelope(w, http.StatusInternalServerError, "api_error",
				"session credential stream validation is unavailable")
			return
		}
		claim := *requestScope.Session
		if err := validator.ValidateSessionScope(r.Context(), claim); err != nil {
			writeError(w, err)
			return
		}
		streamCtx, cancelStream := context.WithCancel(r.Context())
		defer cancelStream()
		r = r.WithContext(streamCtx)
		go monitorSessionStreamLease(streamCtx, cancelStream, validator, claim)
	}

	// Subscribe before the existence check. Once Get succeeds, a concurrent
	// delete cannot slip between the check and subscription without delivering
	// the terminal session.deleted event to this stream.
	ch, cancel, err := s.deps.Stream.SubscribeContext(r.Context(), sessionID, deltaOptIn)
	if err != nil {
		writeError(w, err)
		return
	}
	defer cancel()
	if _, err := s.deps.Sessions.Get(r.Context(), sessionID); err != nil {
		writeError(w, err)
		return
	}
	writeEventStream(w, r, ch)
}

const sessionStreamLeaseCheckInterval = time.Second

func monitorSessionStreamLease(
	ctx context.Context,
	cancel context.CancelFunc,
	validator SessionScopeValidator,
	claim workspace.SessionScope,
) {
	ticker := time.NewTicker(sessionStreamLeaseCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := validator.ValidateSessionScope(ctx, claim); err != nil {
				cancel()
				return
			}
		}
	}
}
