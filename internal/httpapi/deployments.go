package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

type deploymentScheduleRequest struct {
	Expression string `json:"expression"`
	Timezone   string `json:"timezone"`
	Type       string `json:"type"`
}

type deploymentCreateRequest struct {
	Agent         json.RawMessage                              `json:"agent"`
	Budget        optionalJSONField[json.RawMessage]           `json:"budget"`
	EnvironmentID string                                       `json:"environment_id"`
	InitialEvents []map[string]any                             `json:"initial_events"`
	Name          string                                       `json:"name"`
	Description   optionalJSONField[string]                    `json:"description"`
	Metadata      optionalJSONField[map[string]string]         `json:"metadata"`
	Resources     optionalJSONField[[]json.RawMessage]         `json:"resources"`
	Schedule      optionalJSONField[deploymentScheduleRequest] `json:"schedule"`
	VaultIDs      optionalJSONField[[]string]                  `json:"vault_ids"`
}

type deploymentUpdateRequest struct {
	Agent         optionalJSONField[json.RawMessage]           `json:"agent"`
	Budget        optionalJSONField[json.RawMessage]           `json:"budget"`
	EnvironmentID optionalJSONField[string]                    `json:"environment_id"`
	InitialEvents optionalJSONField[[]map[string]any]          `json:"initial_events"`
	Name          optionalJSONField[string]                    `json:"name"`
	Description   optionalJSONField[string]                    `json:"description"`
	Metadata      optionalJSONField[map[string]*string]        `json:"metadata"`
	Resources     optionalJSONField[[]json.RawMessage]         `json:"resources"`
	Schedule      optionalJSONField[deploymentScheduleRequest] `json:"schedule"`
	VaultIDs      optionalJSONField[[]string]                  `json:"vault_ids"`
}

func (s *Server) deploymentsConfigured(w http.ResponseWriter) bool {
	if s.deps.Deployments != nil {
		return true
	}
	writeError(w, domain.Unsupported("Deployments are unavailable for the configured server"))
	return false
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	var body deploymentCreateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	var budget *domain.SessionBudget
	var err error
	if body.Budget.Present && !body.Budget.Null {
		budget, err = parseSessionBudget(body.Budget.Value)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	ref, err := parseDeploymentAgent(body.Agent)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := validateDeploymentInitialEvents(body.InitialEvents); err != nil {
		writeError(w, err)
		return
	}
	resources, err := parseDeploymentResources(body.Resources)
	if err != nil {
		writeError(w, err)
		return
	}
	schedule, err := parseDeploymentSchedule(body.Schedule)
	if err != nil {
		writeError(w, err)
		return
	}
	if body.Metadata.Null || body.Resources.Null || body.VaultIDs.Null {
		writeError(w, domain.Validation("metadata, resources, and vault_ids cannot be null on create"))
		return
	}
	description := ""
	if body.Description.Present && !body.Description.Null {
		description = body.Description.Value
	}
	var metadata map[string]string
	if body.Metadata.Present {
		metadata = body.Metadata.Value
	}
	var vaultIDs []string
	if body.VaultIDs.Present {
		vaultIDs = body.VaultIDs.Value
	}
	item, err := s.deps.Deployments.Create(r.Context(), app.DeploymentCreateInput{
		AgentID: ref.ID, AgentVersion: ref.Version,
		EnvironmentID: body.EnvironmentID, Name: body.Name,
		Description: description, InitialEvents: toDrafts(body.InitialEvents),
		Resources: resources, VaultIDs: vaultIDs, Metadata: metadata, Schedule: schedule,
		Budget: budget,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(item))
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	item, err := s.deps.Deployments.Get(r.Context(), r.PathValue("deployment_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(item))
}

func (s *Server) updateDeployment(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	var body deploymentUpdateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	patch := domain.DeploymentPatch{}
	if body.Budget.Present {
		patch.BudgetSet = true
		if !body.Budget.Null {
			budget, err := parseSessionBudget(body.Budget.Value)
			if err != nil {
				writeError(w, err)
				return
			}
			patch.Budget = budget
		}
	}
	if body.Agent.Present {
		if body.Agent.Null {
			writeError(w, domain.Validation("agent cannot be null"))
			return
		}
		ref, err := parseDeploymentAgent(body.Agent.Value)
		if err != nil {
			writeError(w, err)
			return
		}
		patch.AgentID, patch.AgentVersion = &ref.ID, ref.Version
	}
	if body.EnvironmentID.Present {
		if body.EnvironmentID.Null || body.EnvironmentID.Value == "" {
			writeError(w, domain.Validation("environment_id cannot be null or empty"))
			return
		}
		patch.EnvironmentID = &body.EnvironmentID.Value
	}
	if body.Name.Present {
		if body.Name.Null || body.Name.Value == "" {
			writeError(w, domain.Validation("name cannot be null or empty"))
			return
		}
		patch.Name = &body.Name.Value
	}
	if body.Description.Present {
		value := body.Description.Value
		if body.Description.Null {
			value = ""
		}
		patch.Description = &value
	}
	if body.InitialEvents.Present {
		if body.InitialEvents.Null {
			writeError(w, domain.Validation("initial_events cannot be null"))
			return
		}
		if err := validateDeploymentInitialEvents(body.InitialEvents.Value); err != nil {
			writeError(w, err)
			return
		}
		value := toDrafts(body.InitialEvents.Value)
		patch.InitialEvents = &value
	}
	if body.Resources.Present {
		var value []domain.DeploymentResource
		if !body.Resources.Null {
			parsed, err := parseDeploymentResources(body.Resources)
			if err != nil {
				writeError(w, err)
				return
			}
			value = parsed
		}
		patch.Resources = &value
	}
	if body.VaultIDs.Present {
		value := []string{}
		if !body.VaultIDs.Null {
			value = body.VaultIDs.Value
		}
		patch.VaultIDs = &value
	}
	if body.Metadata.Present {
		if body.Metadata.Null {
			current, err := s.deps.Deployments.Get(r.Context(), r.PathValue("deployment_id"))
			if err != nil {
				writeError(w, err)
				return
			}
			patch.Metadata = make(map[string]*string, len(current.Metadata))
			for key := range current.Metadata {
				patch.Metadata[key] = nil
			}
		} else {
			patch.Metadata = body.Metadata.Value
		}
	}
	if body.Schedule.Present {
		patch.ScheduleSet = true
		if !body.Schedule.Null {
			schedule, err := parseDeploymentSchedule(body.Schedule)
			if err != nil {
				writeError(w, err)
				return
			}
			patch.Schedule = schedule
		}
	}
	item, err := s.deps.Deployments.Update(
		r.Context(), r.PathValue("deployment_id"), patch,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(item))
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	query, filter, err := parseDeploymentListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Deployments.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]any, 0, len(page.Deployments))
	for _, item := range page.Deployments {
		data = append(data, deploymentToJSON(item))
	}
	var next any
	if page.HasMore && len(page.Deployments) > 0 {
		last := page.Deployments[len(page.Deployments)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: deploymentListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": next})
}

func (s *Server) archiveDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentLifecycle(w, r, s.deps.Deployments.Archive)
}

func (s *Server) pauseDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentLifecycle(w, r, s.deps.Deployments.Pause)
}

func (s *Server) unpauseDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentLifecycle(w, r, s.deps.Deployments.Unpause)
}

func (s *Server) deploymentLifecycle(
	w http.ResponseWriter,
	r *http.Request,
	operation func(context.Context, string) (domain.Deployment, error),
) {
	if !s.deploymentsConfigured(w) {
		return
	}
	item, err := operation(r.Context(), r.PathValue("deployment_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(item))
}

func (s *Server) runDeployment(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	run, err := s.deps.Deployments.Run(r.Context(), r.PathValue("deployment_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentRunToJSON(run))
}

func (s *Server) getDeploymentRun(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	run, err := s.deps.Deployments.GetRun(r.Context(), r.PathValue("deployment_run_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentRunToJSON(run))
}

func (s *Server) listDeploymentRuns(w http.ResponseWriter, r *http.Request) {
	if !s.deploymentsConfigured(w) {
		return
	}
	query, filter, err := parseDeploymentRunListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Deployments.ListRuns(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]any, 0, len(page.Runs))
	for _, run := range page.Runs {
		data = append(data, deploymentRunToJSON(run))
	}
	var next any
	if page.HasMore && len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: deploymentRunListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": next})
}

func parseDeploymentAgent(raw json.RawMessage) (agentRef, error) {
	ref, err := parseAgentRef(raw)
	if err != nil {
		return agentRef{}, err
	}
	if ref.Overrides != nil {
		return agentRef{}, domain.Validation("deployment agent cannot include overrides")
	}
	return ref, nil
}

func parseDeploymentSchedule(
	field optionalJSONField[deploymentScheduleRequest],
) (*domain.DeploymentSchedule, error) {
	if !field.Present || field.Null {
		return nil, nil
	}
	if field.Value.Type != "cron" {
		return nil, domain.Validation("schedule type must be cron")
	}
	return &domain.DeploymentSchedule{
		Expression: field.Value.Expression, Timezone: field.Value.Timezone,
	}, nil
}

func parseDeploymentResources(
	field optionalJSONField[[]json.RawMessage],
) ([]domain.DeploymentResource, error) {
	if !field.Present || field.Null || len(field.Value) == 0 {
		return nil, nil
	}
	raw := field.Value
	files, memories, repositories, err := parseSessionResourceInputs(&raw)
	if err != nil {
		return nil, err
	}
	out := make([]domain.DeploymentResource, 0, len(raw))
	for _, file := range files {
		out = append(out, domain.DeploymentResource{
			Type: domain.SessionResourceTypeFile, FileID: file.FileID, MountPath: file.MountPath,
		})
	}
	for _, memory := range memories {
		out = append(out, domain.DeploymentResource{
			Type:          domain.SessionResourceTypeMemoryStore,
			MemoryStoreID: memory.MemoryStoreID, Access: memory.Access,
			Instructions: memory.Instructions,
		})
	}
	for _, repository := range repositories {
		resource := domain.DeploymentResource{
			Type:          domain.SessionResourceTypeGitRepository,
			RepositoryURL: repository.URL, MountPath: repository.MountPath,
		}
		if repository.Checkout != nil {
			resource.RepositoryCheckoutType = repository.Checkout.Type
			resource.RepositoryCheckoutValue = repository.Checkout.Value
		}
		out = append(out, resource)
	}
	return out, nil
}

func validateDeploymentInitialEvents(events []map[string]any) error {
	if len(events) == 0 || len(events) > app.MaxDeploymentInitialEvents {
		return domain.Validation("initial_events must contain between 1 and 50 events")
	}
	for _, event := range events {
		if err := validateClientEvent(event); err != nil {
			return err
		}
		typeName, _ := event["type"].(string)
		if !domain.IsInitialEventType(typeName) && typeName != domain.EvSystemMessage {
			return domain.Validation("deployment initial_events contains an unsupported event type")
		}
	}
	return validateClientEventBatch(events)
}

func deploymentToJSON(item domain.Deployment) map[string]any {
	events := make([]any, 0, len(item.InitialEvents))
	for _, event := range item.InitialEvents {
		value := map[string]any{"type": event.Type}
		for key, field := range event.Payload {
			if key == "id" || key == "type" || key == "processed_at" ||
				strings.HasPrefix(key, "__") {
				continue
			}
			value[key] = field
		}
		events = append(events, value)
	}
	resources := make([]any, 0, len(item.Resources))
	for _, resource := range item.Resources {
		switch resource.Type {
		case domain.SessionResourceTypeFile:
			value := map[string]any{
				"type": "file", "file_id": resource.FileID, "mount_path": nil,
			}
			if resource.MountPath != nil {
				value["mount_path"] = *resource.MountPath
			}
			resources = append(resources, value)
		case domain.SessionResourceTypeMemoryStore:
			value := map[string]any{
				"type": "memory_store", "memory_store_id": resource.MemoryStoreID,
				"access": nil, "instructions": nil,
			}
			if resource.Access != "" {
				value["access"] = resource.Access
			}
			if resource.Instructions != "" {
				value["instructions"] = resource.Instructions
			}
			resources = append(resources, value)
		case domain.SessionResourceTypeGitRepository:
			var checkout any
			switch resource.RepositoryCheckoutType {
			case domain.GitRepositoryCheckoutBranch:
				checkout = map[string]any{
					"type": domain.GitRepositoryCheckoutBranch,
					"name": resource.RepositoryCheckoutValue,
				}
			case domain.GitRepositoryCheckoutCommit:
				checkout = map[string]any{
					"type": domain.GitRepositoryCheckoutCommit,
					"sha":  resource.RepositoryCheckoutValue,
				}
			}
			value := map[string]any{
				"type": domain.SessionResourceTypeGitRepository,
				"url":  resource.RepositoryURL, "checkout": checkout, "mount_path": nil,
			}
			if resource.MountPath != nil {
				value["mount_path"] = *resource.MountPath
			}
			resources = append(resources, value)
		}
	}
	budget := any(nil)
	if item.Budget != nil {
		budget = item.Budget.JSON()
	}
	out := map[string]any{
		"id": item.ID, "type": "deployment",
		"budget": budget,
		"agent": map[string]any{
			"type": "agent", "id": item.AgentID, "version": item.AgentVersion,
		},
		"environment_id": item.EnvironmentID, "name": item.Name,
		"description": item.Description, "initial_events": events,
		"resources": resources, "vault_ids": append([]string{}, item.VaultIDs...),
		"metadata": item.Metadata, "status": item.Status,
		"created_at": item.CreatedAt.Format(timeFmt), "updated_at": item.UpdatedAt.Format(timeFmt),
		"archived_at": nil, "paused_reason": nil, "schedule": nil,
	}
	if item.ArchivedAt != nil {
		out["archived_at"] = item.ArchivedAt.Format(timeFmt)
	}
	if item.PausedReason != nil {
		reason := map[string]any{"type": item.PausedReason.Type}
		if item.PausedReason.Type == "error" {
			reason["error"] = map[string]any{"type": item.PausedReason.ErrorType}
		}
		out["paused_reason"] = reason
	}
	if item.Schedule != nil {
		upcoming := make([]string, 0, len(item.Schedule.UpcomingRunsAt))
		for _, value := range item.Schedule.UpcomingRunsAt {
			upcoming = append(upcoming, value.Format(timeFmt))
		}
		schedule := map[string]any{
			"type": "cron", "expression": item.Schedule.Expression,
			"timezone": item.Schedule.Timezone, "last_run_at": nil,
			"upcoming_runs_at": upcoming,
		}
		if item.Schedule.LastRunAt != nil {
			schedule["last_run_at"] = item.Schedule.LastRunAt.Format(timeFmt)
		}
		out["schedule"] = schedule
	}
	return out
}

func deploymentRunToJSON(run domain.DeploymentRun) map[string]any {
	out := map[string]any{
		"id": run.ID, "type": "deployment_run", "deployment_id": run.DeploymentID,
		"agent": map[string]any{
			"type": "agent", "id": run.AgentID, "version": run.AgentVersion,
		},
		"created_at": run.CreatedAt.Format(timeFmt), "session_id": nil, "error": nil,
	}
	if run.SessionID != nil {
		out["session_id"] = *run.SessionID
	}
	if run.ErrorType != "" {
		out["error"] = map[string]any{"type": run.ErrorType, "message": run.ErrorMessage}
	}
	trigger := map[string]any{"type": run.TriggerType}
	if run.ScheduledAt != nil {
		trigger["scheduled_at"] = run.ScheduledAt.Format(timeFmt)
	}
	out["trigger_context"] = trigger
	return out
}

func parseDeploymentListQuery(
	r *http.Request,
) (app.DeploymentListQuery, deploymentCursorFilter, error) {
	values := r.URL.Query()
	query := app.DeploymentListQuery{Limit: app.DefaultDeploymentListLimit}
	filter := deploymentCursorFilter{AgentID: values.Get("agent_id"), Status: values.Get("status")}
	query.AgentID, query.Status = filter.AgentID, filter.Status
	if query.Status != "" && query.Status != domain.DeploymentStatusActive &&
		query.Status != domain.DeploymentStatusPaused {
		return query, filter, domain.Validation("status must be active or paused")
	}
	if values.Has("include_archived") {
		parsed, err := parseResourceListBool(values.Get("include_archived"), "include_archived")
		if err != nil {
			return query, filter, err
		}
		query.IncludeArchived, filter.IncludeArchived = parsed, parsed
	}
	if query.IncludeArchived && query.Status != "" {
		return query, filter, domain.Validation("status cannot be combined with include_archived")
	}
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxDeploymentListLimit {
			return query, filter, domain.Validation("limit must be between 1 and 100")
		}
		query.Limit = limit
	}
	for _, bound := range []struct {
		key  string
		dst  **time.Time
		norm *string
	}{
		{"created_at[gte]", &query.CreatedAtGte, &filter.CreatedAtGte},
		{"created_at[lte]", &query.CreatedAtLte, &filter.CreatedAtLte},
	} {
		if !values.Has(bound.key) {
			continue
		}
		parsed, ok := parseTimeParam(values.Get(bound.key))
		if !ok {
			return query, filter, domain.Validation(bound.key + " must be an RFC 3339 timestamp")
		}
		*bound.dst = parsed
		*bound.norm = parsed.UTC().Format(timeFmt)
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), deploymentListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return query, filter, domain.Validation("invalid deployment page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return query, filter, domain.Validation("invalid deployment page cursor")
		}
		query.Boundary = &app.DeploymentListBoundary{CreatedAt: *createdAt, ID: cursor.ID}
	}
	return query, filter, nil
}

func parseDeploymentRunListQuery(
	r *http.Request,
) (app.DeploymentRunListQuery, deploymentRunCursorFilter, error) {
	values := r.URL.Query()
	query := app.DeploymentRunListQuery{Limit: app.DefaultDeploymentRunListLimit}
	filter := deploymentRunCursorFilter{TriggerType: values.Get("trigger_type")}
	query.TriggerType = filter.TriggerType
	if query.TriggerType != "" && query.TriggerType != domain.DeploymentTriggerManual &&
		query.TriggerType != domain.DeploymentTriggerSchedule {
		return query, filter, domain.Validation("trigger_type must be manual or schedule")
	}
	if values.Has("deployment_id") {
		value := values.Get("deployment_id")
		query.DeploymentID, filter.DeploymentID = &value, &value
	}
	if values.Has("has_error") {
		value, err := strconv.ParseBool(values.Get("has_error"))
		if err != nil {
			return query, filter, domain.Validation("has_error must be true or false")
		}
		query.HasError, filter.HasError = &value, &value
	}
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxDeploymentRunListLimit {
			return query, filter, domain.Validation("limit must be between 1 and 1000")
		}
		query.Limit = limit
	}
	for _, bound := range []struct {
		key  string
		dst  **time.Time
		norm *string
	}{
		{"created_at[gt]", &query.CreatedAtGt, &filter.CreatedAtGt},
		{"created_at[gte]", &query.CreatedAtGte, &filter.CreatedAtGte},
		{"created_at[lt]", &query.CreatedAtLt, &filter.CreatedAtLt},
		{"created_at[lte]", &query.CreatedAtLte, &filter.CreatedAtLte},
	} {
		if !values.Has(bound.key) {
			continue
		}
		parsed, ok := parseTimeParam(values.Get(bound.key))
		if !ok {
			return query, filter, domain.Validation(bound.key + " must be an RFC 3339 timestamp")
		}
		*bound.dst = parsed
		*bound.norm = parsed.UTC().Format(timeFmt)
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), deploymentRunListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return query, filter, domain.Validation("invalid deployment run page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return query, filter, domain.Validation("invalid deployment run page cursor")
		}
		query.Boundary = &app.DeploymentRunListBoundary{CreatedAt: *createdAt, ID: cursor.ID}
	}
	return query, filter, nil
}
