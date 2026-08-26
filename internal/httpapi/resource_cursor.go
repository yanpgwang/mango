package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/yanpgwang/mango/internal/domain"
)

// resourceCursor is shared only by the two forward-only resource lists. Kind
// prevents a cursor from one endpoint being replayed against the other, while
// Filter binds it to the normalized filters that produced it.
type resourceCursor struct {
	Version   int    `json:"v"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
	Filter    string `json:"filter"`
}

type agentVersionCursor struct {
	Version      int    `json:"v"`
	Kind         string `json:"kind"`
	AgentID      string `json:"agent_id"`
	AfterVersion int    `json:"after_version"`
}

const (
	agentListCursorKind           = "agent_list"
	agentVersionCursorKind        = "agent_version_list"
	environmentListCursorKind     = "environment_list"
	sessionResourceListCursorKind = "session_resource_list"
	skillListCursorKind           = "skill_list"
	skillVersionListCursorKind    = "skill_version_list"
	memoryStoreListCursorKind     = "memory_store_list"
	memoryVersionListCursorKind   = "memory_version_list"
	vaultListCursorKind           = "vault_list"
	webhookListCursorKind         = "webhook_list"
	credentialListCursorKind      = "vault_credential_list"
	deploymentListCursorKind      = "deployment_list"
	deploymentRunListCursorKind   = "deployment_run_list"
	environmentWorkListCursorKind = "environment_work_list"
	sessionThreadListCursorKind   = "session_thread_list"
	resourceCursorPrefix          = "page_"
)

func encodeResourceCursor(cursor resourceCursor) string {
	cursor.Version = 1
	body, _ := json.Marshal(cursor)
	return resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(body)
}

func encodeAgentVersionCursor(agentID string, afterVersion int) string {
	body, _ := json.Marshal(agentVersionCursor{
		Version: 1, Kind: agentVersionCursorKind,
		AgentID: agentID, AfterVersion: afterVersion,
	})
	return resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(body)
}

func decodeAgentVersionCursor(token, agentID string) (int, bool) {
	encoded, ok := strings.CutPrefix(token, resourceCursorPrefix)
	if !ok {
		return 0, false
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, false
	}
	var cursor agentVersionCursor
	if err := json.Unmarshal(body, &cursor); err != nil {
		return 0, false
	}
	if cursor.Version != 1 || cursor.Kind != agentVersionCursorKind ||
		cursor.AgentID != agentID || cursor.AfterVersion < 1 ||
		cursor.AfterVersion > math.MaxInt32 {
		return 0, false
	}
	return cursor.AfterVersion, true
}

func decodeResourceCursor(token, kind string) (resourceCursor, bool) {
	encoded, ok := strings.CutPrefix(token, resourceCursorPrefix)
	if !ok {
		return resourceCursor{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return resourceCursor{}, false
	}
	var cursor resourceCursor
	if err := json.Unmarshal(body, &cursor); err != nil {
		return resourceCursor{}, false
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.CreatedAt == "" ||
		cursor.ID == "" || cursor.Filter == "" {
		return resourceCursor{}, false
	}
	return cursor, true
}

type agentCursorFilter struct {
	CreatedAtGte    string `json:"created_at_gte,omitempty"`
	CreatedAtLte    string `json:"created_at_lte,omitempty"`
	IncludeArchived bool   `json:"include_archived"`
}

func (filter agentCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

type environmentCursorFilter struct {
	IncludeArchived bool `json:"include_archived"`
}

type deploymentCursorFilter struct {
	AgentID         string `json:"agent_id,omitempty"`
	CreatedAtGte    string `json:"created_at_gte,omitempty"`
	CreatedAtLte    string `json:"created_at_lte,omitempty"`
	IncludeArchived bool   `json:"include_archived"`
	Status          string `json:"status,omitempty"`
}

type deploymentRunCursorFilter struct {
	CreatedAtGt  string  `json:"created_at_gt,omitempty"`
	CreatedAtGte string  `json:"created_at_gte,omitempty"`
	CreatedAtLt  string  `json:"created_at_lt,omitempty"`
	CreatedAtLte string  `json:"created_at_lte,omitempty"`
	DeploymentID *string `json:"deployment_id,omitempty"`
	HasError     *bool   `json:"has_error,omitempty"`
	TriggerType  string  `json:"trigger_type,omitempty"`
}

type environmentWorkCursorFilter struct {
	EnvironmentID string `json:"environment_id"`
}

func (filter deploymentCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func (filter deploymentRunCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func (filter environmentWorkCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

type sessionResourceCursorFilter struct {
	SessionID string `json:"session_id"`
}

type skillCursorFilter struct {
	Source string `json:"source,omitempty"`
}

type skillVersionCursorFilter struct {
	SkillID string `json:"skill_id"`
}

func (filter sessionResourceCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func (filter skillCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func (filter skillVersionCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func (filter environmentCursorFilter) fingerprint() string {
	return resourceFilterFingerprint(filter)
}

func resourceFilterFingerprint(filter any) string {
	body, _ := json.Marshal(filter)
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func parseResourceListLimit(raw string) (int, error) {
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return 0, domain.Validation("limit must be a positive integer")
	}
	return limit, nil
}

func parseResourceListBool(raw, field string) (bool, error) {
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, domain.Validation(field + " must be true or false")
	}
}
