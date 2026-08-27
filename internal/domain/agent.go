package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

type Model struct {
	ID     string
	Effort string
	Speed  string
	// EffortExplicit and SpeedExplicit distinguish an explicit Agent setting
	// from the Mango defaults echoed in the resolved resource. The
	// Messages adapter uses this distinction to avoid sending preview fields to
	// endpoints when the caller only accepted the platform default.
	EffortExplicit bool
	SpeedExplicit  bool
}

const (
	DefaultModelEffort = "high"
	DefaultModelSpeed  = "standard"
)

// NormalizeModel fills the defaults the Mango API exposes on a
// resolved Agent while preserving whether the user supplied each value.
func NormalizeModel(model Model) Model {
	if model.Effort == "" {
		model.Effort = DefaultModelEffort
	}
	if model.Speed == "" {
		model.Speed = DefaultModelSpeed
	}
	return model
}

// ValidateModel enforces the public model configuration enums. Model support
// for a particular effort/speed combination remains a provider concern.
func ValidateModel(model Model) error {
	if model.ID == "" {
		return Validation("model is required")
	}
	switch model.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return Validation("model effort must be low, medium, high, xhigh, or max")
	}
	switch model.Speed {
	case "", "standard", "fast":
	default:
		return Validation("model speed must be standard or fast")
	}
	return nil
}

type Agent struct {
	ID          string
	Version     int
	Name        string
	Model       Model
	System      *string
	Description *string
	Tools       []any
	MCPServers  []any
	Skills      []SkillReference
	Multiagent  *Multiagent
	Metadata    map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  *time.Time
}

type NullableString struct {
	Set   bool
	Value *string
}

type AgentPatch struct {
	Name            *string
	Model           *Model
	System          *NullableString
	Description     *NullableString
	Tools           *[]any
	MCPServers      *[]any
	Skills          *[]SkillReference
	Multiagent      *NullableMultiagent
	Metadata        map[string]any
	ExpectedVersion *int
}

// Multiagent is the immutable, version-pinned coordinator roster stored on an
// Agent Version. New writes contain only concrete agent references. The custom
// roster-entry decoder keeps Agents written by older Mango releases readable;
// application admission never treats those unresolved legacy entries as an
// executable roster.
type Multiagent struct {
	Type      string           `json:"type"`
	Agents    []AgentReference `json:"agents"`
	legacyRaw json.RawMessage
}

// AgentReference accepts the documented roster input forms while giving the
// application service one type to resolve. A new persisted reference is always
// the object form with Type=agent and a positive Version. StringForm preserves
// the string union variant until resolution and across legacy storage reads.
type AgentReference struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	Version    int    `json:"version,omitempty"`
	Model      string `json:"model,omitempty"`
	StringForm bool   `json:"-"`
}

const AdvisorAgentName = "anthropic.advisor"

// Advisor is the resolved public form used by an automatically terminating
// consultation Thread. It is deliberately not an Agent: it has no mutable
// resource identity, tools, prompt, or coordinator messaging surface.
type Advisor struct {
	Type  string `json:"type"`
	Model string `json:"model"`
}

func (r AgentReference) MarshalJSON() ([]byte, error) {
	if r.StringForm {
		return json.Marshal(r.ID)
	}
	type wire AgentReference
	return json.Marshal(wire(r))
}

func (r *AgentReference) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &r.ID); err != nil {
			return err
		}
		r.Type = "agent"
		r.StringForm = true
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return Validation("invalid multiagent roster entry")
	}
	var entryType string
	if value, ok := object["type"]; !ok || json.Unmarshal(value, &entryType) != nil {
		return Validation("invalid multiagent roster entry type")
	}
	if entryType == "self" {
		if len(object) != 1 {
			return Validation("invalid multiagent self entry")
		}
		*r = AgentReference{Type: "self"}
		return nil
	}
	if entryType == "advisor" {
		for field := range object {
			if field != "type" && field != "model" {
				return Validation("invalid multiagent advisor entry field")
			}
		}
		var model string
		if value, ok := object["model"]; !ok || json.Unmarshal(value, &model) != nil ||
			strings.TrimSpace(model) == "" {
			return Validation("invalid multiagent advisor model")
		}
		*r = AgentReference{Type: "advisor", Model: model}
		return nil
	}
	if entryType != "agent" {
		return Validation("invalid multiagent roster entry type")
	}
	for field := range object {
		if field != "type" && field != "id" && field != "version" {
			return Validation("invalid multiagent roster entry field")
		}
	}
	var id string
	if value, ok := object["id"]; !ok || json.Unmarshal(value, &id) != nil || id == "" {
		return Validation("invalid multiagent agent ID")
	}
	decoded := AgentReference{Type: "agent", ID: id}
	if value, ok := object["version"]; ok {
		if err := json.Unmarshal(value, &decoded.Version); err != nil || decoded.Version < 1 {
			return Validation("invalid multiagent agent version")
		}
	}
	*r = decoded
	return nil
}

func (m Multiagent) MarshalJSON() ([]byte, error) {
	if len(m.legacyRaw) > 0 {
		return append([]byte(nil), m.legacyRaw...), nil
	}
	type wire struct {
		Type   string           `json:"type"`
		Agents []AgentReference `json:"agents"`
	}
	return json.Marshal(wire{Type: m.Type, Agents: m.Agents})
}

func (m *Multiagent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return Validation("invalid stored multiagent configuration")
	}
	for field := range object {
		if field != "type" && field != "agents" {
			m.legacyRaw = append([]byte(nil), trimmed...)
			return nil
		}
	}
	var topologyType string
	if value, ok := object["type"]; !ok || json.Unmarshal(value, &topologyType) != nil ||
		topologyType != "coordinator" {
		m.legacyRaw = append([]byte(nil), trimmed...)
		return nil
	}
	var agents []AgentReference
	if value, ok := object["agents"]; !ok || json.Unmarshal(value, &agents) != nil ||
		len(agents) < 1 || len(agents) > 20 {
		m.legacyRaw = append([]byte(nil), trimmed...)
		return nil
	}
	m.Type = topologyType
	m.Agents = agents
	return nil
}

func (m *Multiagent) IsLegacy() bool {
	return m != nil && len(m.legacyRaw) > 0
}

func (m *Multiagent) IsResolved() bool {
	if m == nil {
		return true
	}
	if m.IsLegacy() || m.Type != "coordinator" || len(m.Agents) < 1 || len(m.Agents) > 20 {
		return false
	}
	seen := make(map[string]struct{}, len(m.Agents))
	seenAdvisor := false
	for _, reference := range m.Agents {
		switch reference.Type {
		case "agent":
			if reference.ID == "" || reference.Version < 1 || reference.Model != "" || reference.StringForm {
				return false
			}
			if _, duplicate := seen[reference.ID]; duplicate {
				return false
			}
			seen[reference.ID] = struct{}{}
		case "advisor":
			if seenAdvisor || reference.ID != "" || reference.Version != 0 ||
				strings.TrimSpace(reference.Model) == "" || reference.StringForm {
				return false
			}
			seenAdvisor = true
		default:
			return false
		}
	}
	return true
}

func (m *Multiagent) Clone() *Multiagent {
	if m == nil {
		return nil
	}
	clone := &Multiagent{Type: m.Type}
	clone.Agents = append([]AgentReference(nil), m.Agents...)
	clone.legacyRaw = append(json.RawMessage(nil), m.legacyRaw...)
	return clone
}

// RebindAgentVersion returns a copy whose reference to the owning coordinator
// points at version. Direct references to the owner are rejected during roster
// resolution, so an owner-ID entry can only have originated from {"type":"self"}.
func (m *Multiagent) RebindAgentVersion(ownerID string, version int) *Multiagent {
	clone := m.Clone()
	for i := range clone.Agents {
		if clone.Agents[i].ID == ownerID {
			clone.Agents[i].Type = "agent"
			clone.Agents[i].Version = version
			clone.Agents[i].StringForm = false
		}
	}
	return clone
}

// HasExternalAgent reports whether the resolved roster contains an Agent other
// than the owning coordinator. Session model overrides also apply to self
// copies, but never to independently referenced Agents, so callers use this to
// enforce roster-wide model constraints without resolving immutable versions a
// second time.
func (m *Multiagent) HasExternalAgent(ownerID string) bool {
	if m == nil {
		return false
	}
	for _, reference := range m.Agents {
		if reference.Type == "agent" && reference.ID != ownerID {
			return true
		}
	}
	return false
}

// Advisor returns the single resolved advisor roster entry, if configured.
func (m *Multiagent) Advisor() *Advisor {
	if m == nil {
		return nil
	}
	for _, reference := range m.Agents {
		if reference.Type == "advisor" {
			return &Advisor{Type: "advisor", Model: reference.Model}
		}
	}
	return nil
}

// HasCallableAgents reports whether the roster contains any ordinary Agent
// that may be listed or messaged by the coordinator toolset. Advisors are
// intentionally excluded from that private protocol.
func (m *Multiagent) HasCallableAgents() bool {
	if m == nil {
		return false
	}
	for _, reference := range m.Agents {
		if reference.Type == "agent" {
			return true
		}
	}
	return false
}

// NullableMultiagent preserves the update tri-state: an omitted patch leaves
// the roster unchanged, Value=nil clears it, and a non-nil Value replaces it.
type NullableMultiagent struct {
	Value *Multiagent
}

func (a Agent) Apply(p AgentPatch) (Agent, bool, error) {
	if p.ExpectedVersion != nil && *p.ExpectedVersion != a.Version {
		return a, false, Conflict("agent version mismatch")
	}
	next := a
	next.Multiagent = a.Multiagent.Clone()
	// deep-copy source metadata only if present
	if a.Metadata != nil {
		next.Metadata = make(map[string]any, len(a.Metadata))
		for k, v := range a.Metadata {
			next.Metadata[k] = v
		}
	}
	if p.Name != nil {
		next.Name = *p.Name
	}
	if p.Model != nil {
		model := *p.Model
		// Mango treats effort as the one sticky model field: updating
		// the same model id without effort preserves the stored level. Changing
		// ids resets an omitted effort to that model's default. Other omitted
		// model fields, including speed, take their defaults.
		if model.ID == a.Model.ID && !model.EffortExplicit {
			model.Effort = a.Model.Effort
			model.EffortExplicit = a.Model.EffortExplicit
		}
		next.Model = NormalizeModel(model)
	}
	if p.System != nil {
		next.System = p.System.Value
	}
	if p.Description != nil {
		next.Description = p.Description.Value
	}
	if p.Tools != nil {
		next.Tools = *p.Tools
	}
	if p.MCPServers != nil {
		next.MCPServers = *p.MCPServers
	}
	if p.Skills != nil {
		next.Skills = *p.Skills
	}
	if p.Multiagent != nil {
		next.Multiagent = p.Multiagent.Value.Clone()
	}
	// apply metadata patch (allocate lazily if source was nil)
	for k, v := range p.Metadata {
		if v == nil {
			delete(next.Metadata, k) // safe on nil map
		} else {
			if next.Metadata == nil {
				next.Metadata = map[string]any{}
			}
			next.Metadata[k] = v
		}
	}
	// normalize: empty non-nil map != nil under DeepEqual
	if len(next.Metadata) == 0 {
		next.Metadata = nil
	}
	changed := !agentFieldsEqual(a, next)
	return next, changed, nil
}

func agentFieldsEqual(a, b Agent) bool {
	a.Version, b.Version = 0, 0
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// SessionSnapshotJSON is the public resolved-agent projection embedded in a
// Session (`session.agent`) and in a `session.updated` event. It omits the
// agent-resource bookkeeping (created_at/updated_at/archived_at) that the
// session agent object does not carry. It lives here because the HTTP layer and
// the durable event ledger must publish exactly the same shape.
func (a Agent) SessionSnapshotJSON() map[string]any {
	model := map[string]any{"id": a.Model.ID}
	if a.Model.Effort != "" {
		model["effort"] = map[string]any{"type": a.Model.Effort}
	}
	if a.Model.Speed != "" {
		model["speed"] = a.Model.Speed
	}
	system, description := "", ""
	if a.System != nil {
		system = *a.System
	}
	if a.Description != nil {
		description = *a.Description
	}
	return map[string]any{
		"id": a.ID, "type": "agent", "version": a.Version, "name": a.Name,
		"model": model, "system": system, "description": description,
		"multiagent": a.Multiagent,
		"tools":      orEmptyList(a.Tools), "mcp_servers": orEmptyList(a.MCPServers),
		"skills": orEmptyList(a.Skills),
	}
}

func orEmptyList[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// AgentOverrides expresses per-session agent configuration overrides
// (agent_with_overrides). Each field is applied only when Set is true. For list
// fields, a nil slice with Set=true clears the field; model is never clearable.
type AgentOverrides struct {
	Model      *Model
	System     *NullableString
	Tools      *[]any
	MCPServers *[]any
	Skills     *[]SkillReference
}

// WithOverrides returns a copy of the agent with session-local overrides
// applied. It does not change version or identity; the returned snapshot still
// reports the base agent's id and version so a session traces back to it. Each
// provided field replaces (never merges) the agent's value.
func (a Agent) WithOverrides(o AgentOverrides) Agent {
	next := a
	if o.Model != nil {
		model := *o.Model
		// Per-session model overrides may select a model and speed, but the
		// official API does not apply effort supplied inside the override. The
		// Agent's resolved effort remains authoritative for the Session.
		model.Effort = a.Model.Effort
		model.EffortExplicit = a.Model.EffortExplicit
		next.Model = NormalizeModel(model)
	}
	if o.System != nil {
		next.System = o.System.Value
	}
	if o.Tools != nil {
		next.Tools = *o.Tools
	}
	if o.MCPServers != nil {
		next.MCPServers = *o.MCPServers
	}
	if o.Skills != nil {
		next.Skills = *o.Skills
	}
	return next
}
