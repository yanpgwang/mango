package domain

// SkillReference is the resolved Skill configuration stored on an Agent or
// Session snapshot. Requests may omit Version or send "latest"; application
// services replace either form with the concrete immutable Version before the
// resource crosses a persistence boundary.
type SkillReference struct {
	Type    string `json:"type"`
	SkillID string `json:"skill_id"`
	Version string `json:"version,omitempty"`
}
