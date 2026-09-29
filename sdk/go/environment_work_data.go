package mango

// WorkType identifies the concrete Work payload, or returns empty for an
// unknown or ambiguous union. Consumers must reject unsupported payloads.
func (data EnvironmentWorkData) WorkType() string {
	if len(data.Raw) != 0 {
		return ""
	}
	if data.SessionWorkData != nil && data.HealthcheckWorkData == nil && data.SessionWorkData.Type == "session" {
		return "session"
	}
	if data.HealthcheckWorkData != nil && data.SessionWorkData == nil && data.HealthcheckWorkData.Type == "healthcheck" {
		return "healthcheck"
	}
	return ""
}

// SessionID is empty for healthchecks, which never create a Session.
func (data EnvironmentWorkData) SessionID() string {
	if data.WorkType() == "session" {
		return data.SessionWorkData.ID
	}
	return ""
}
