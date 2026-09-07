package mango

// ModelID selects a model by its configured endpoint identifier.
func ModelID(id string) ModelInput { return ModelInput{String: Ptr(id)} }

// ModelSettings selects a model with effort or speed configuration.
func ModelSettings(config ModelInputObject) ModelInput {
	return ModelInput{Object: &config}
}

// AgentID resolves the latest active Agent version when creating a Session.
func AgentID(id string) SessionAgentInput { return SessionAgentInput{String: Ptr(id)} }

// AgentVersion pins a Session to one immutable Agent version.
func AgentVersion(id string, version int64) SessionAgentInput {
	return SessionAgentInput{AgentReference: &AgentReference{
		Type: "agent", ID: id, Version: Some(version),
	}}
}

// Coordinator configures the Agent roster. The server resolves and validates it.
func Coordinator(agents ...MultiagentRosterEntryInput) MultiagentInput {
	return &MultiagentInputValue{Type: "coordinator", Agents: agents}
}

// RosterAgent resolves the latest active version when saving the coordinator.
func RosterAgent(id string) MultiagentRosterEntryInput {
	return MultiagentRosterEntryInput{String: Ptr(id)}
}

// RosterAgentVersion pins a coordinator member to an immutable Agent version.
func RosterAgentVersion(id string, version int64) MultiagentRosterEntryInput {
	return MultiagentRosterEntryInput{MultiagentAgentReferenceInput: &MultiagentAgentReferenceInput{
		Type: "agent", ID: id, Version: Some(version),
	}}
}

// Self lets the coordinator spawn copies with its effective Session configuration.
func Self() MultiagentRosterEntryInput {
	return MultiagentRosterEntryInput{MultiagentSelfReferenceInput: &MultiagentSelfReferenceInput{Type: "self"}}
}

// Advisor configures the primary Agent's optional tool-free consultation model.
func Advisor(model string) MultiagentRosterEntryInput {
	return MultiagentRosterEntryInput{MultiagentAdvisor: &MultiagentAdvisor{Type: "advisor", Model: model}}
}

// Text creates a text content block. Other content variants remain available.
func Text(text string) MessageContentInput {
	return MessageContentInput{TextBlockInput: &TextBlockInput{Type: "text", Text: text}}
}

// UserMessage creates a text message for Sessions.Events.Send.
func UserMessage(text string) ClientSessionEventInput {
	return ClientSessionEventInput{UserMessageEventInput: &UserMessageEventInput{
		Type: "user.message", Content: []MessageContentInput{Text(text)},
	}}
}
