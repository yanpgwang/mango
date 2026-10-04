package temporal

import (
	"fmt"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/permission"
	"go.temporal.io/sdk/workflow"
)

func configuredAutoPermission(tools domain.ToolSet) bool {
	for _, name := range domain.BuiltinToolNames {
		if enabled, policy := tools.BuiltinEnabled(name); enabled && policy.Type == "auto" {
			return true
		}
	}
	for _, server := range tools.MCP {
		if server.DefaultEnabled && server.DefaultPolicy.Type == "auto" {
			return true
		}
		for _, config := range server.Configs {
			if enabled, policy := server.ToolEnabled(config.Name); enabled && policy.Type == "auto" {
				return true
			}
		}
	}
	return false
}

func (t *workflowTurnState) evaluateToolPermissions(prepared PrepareTurnResult, uses []domain.ContentBlock, tools map[string]TurnTool, steps map[string]PlannedToolStep, conversation []domain.Message) (map[string]domain.ToolPermissionDecision, bool, error) {
	hasAuto := false
	for _, use := range uses {
		if tools[use.ToolName].Permission.Type == "auto" {
			hasAuto = true
			break
		}
	}
	if !hasAuto {
		return nil, false, nil
	}
	owner := domain.ToolPermissionOwner{SessionID: t.sessionID, ThreadID: t.threadID, TriggerEventID: t.triggerEventID, AttemptID: prepared.AttemptID}
	if t.attemptID == "" {
		var established EnsureToolPermissionAttemptResult
		if err := workflow.ExecuteActivity(t.actx, ActivityEnsureToolPermissionAttempt, owner).Get(t.actx, &established); err != nil {
			return nil, false, err
		}
		if !established.Established {
			return nil, true, nil
		}
		t.attemptID = owner.AttemptID
	}
	schemas := make(map[string]model.ToolSchema, len(prepared.Request.Tools))
	for _, schema := range prepared.Request.Tools {
		schemas[schema.Name] = schema
	}
	decisions := make(map[string]domain.ToolPermissionDecision)
	for _, use := range uses {
		if tools[use.ToolName].Permission.Type != "auto" {
			continue
		}
		schema, ok := schemas[use.ToolName]
		if !ok {
			return nil, false, fmt.Errorf("automatic permission tool has no prepared schema")
		}
		input := EvaluateToolPermissionInput{Owner: owner, ToolUseEventID: steps[use.ToolUseID].ToolUseEventID, Model: domain.Model{ID: prepared.Request.Model}, Input: permission.Input{Intent: prepared.PermissionIntent, Tool: schema, Call: use, Conversation: conversation}}
		for {
			var evaluated EvaluateToolPermissionResult
			var outcome interruptibleActivityOutcome
			var err error
			if t.interrupts == nil {
				err = workflow.ExecuteActivity(t.actx, ActivityEvaluateToolPermission, input).Get(t.actx, &evaluated)
			} else {
				outcome, err = t.interrupts.executeActivity(ActivityEvaluateToolPermission, input, &evaluated)
			}
			if err != nil {
				return nil, false, err
			}
			if outcome.Interrupted || evaluated.Stopped {
				return nil, true, nil
			}
			if !evaluated.BudgetPaused {
				decisions[use.ToolUseID] = evaluated.Decision
				break
			}
			if t.interrupts == nil {
				return nil, false, fmt.Errorf("budget-paused permission judgment has no workflow wakeup channel")
			}
			if interrupted, err := t.waitPermissionBudgetWakeup(); err != nil || interrupted {
				return nil, interrupted, err
			}
		}
	}
	return decisions, false, nil
}

func (t *workflowTurnState) waitPermissionBudgetWakeup() (bool, error) {
	pending, err := loadInterruptAfter(t.actx, t.sessionID, t.threadID, t.interrupts.afterSeq)
	if err != nil {
		return false, err
	}
	if pending.Interrupt != nil {
		return true, nil
	}
	t.interrupts.waitForWakeup()
	pending, err = loadInterruptAfter(t.actx, t.sessionID, t.threadID, t.interrupts.afterSeq)
	return pending.Interrupt != nil, err
}
