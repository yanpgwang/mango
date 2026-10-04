package temporal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/permission"
)

type permissionActivityStore struct {
	*fakeSource
	receipt      *domain.ToolPermissionReceipt
	admitted     int
	recordings   int
	stopped      bool
	failedWrites int
}

func (s *permissionActivityStore) EnsureToolPermissionAttempt(context.Context, domain.ToolPermissionOwner) error {
	return nil
}
func (s *permissionActivityStore) PermissionIntentThrough(context.Context, string, string) (domain.PermissionIntent, error) {
	return domain.PermissionIntent{}, nil
}
func (s *permissionActivityStore) GetToolPermissionReceipt(_ context.Context, _ domain.ToolPermissionReceipt) (*domain.ToolPermissionReceipt, error) {
	return s.receipt, nil
}
func (s *permissionActivityStore) AdmitToolPermissionEvaluation(context.Context, domain.ToolPermissionReceipt) (bool, error) {
	s.admitted++
	if s.stopped {
		return false, domain.Conflict("interrupted")
	}
	return true, nil
}
func (s *permissionActivityStore) RecordToolPermissionReceipt(_ context.Context, r domain.ToolPermissionReceipt) (domain.ToolPermissionReceipt, error) {
	s.recordings++
	if s.failedWrites > 0 {
		s.failedWrites--
		return domain.ToolPermissionReceipt{}, errors.New("temporary database outage")
	}
	s.receipt = &r
	return r, nil
}
func (s *permissionActivityStore) StartToolStepWithPermission(context.Context, string, string) error {
	return nil
}

type permissionEvaluatorFunc func(context.Context, domain.Model, permission.Input) (permission.Result, error)

func (f permissionEvaluatorFunc) Evaluate(ctx context.Context, m domain.Model, in permission.Input) (permission.Result, error) {
	return f(ctx, m, in)
}
func permissionActivityInput() EvaluateToolPermissionInput {
	return EvaluateToolPermissionInput{Owner: domain.ToolPermissionOwner{SessionID: "sesn_test", ThreadID: "sthr_test", TriggerEventID: "sevt_trigger", AttemptID: "ratm_test"}, ToolUseEventID: "sevt_tool", Model: domain.Model{ID: "claude-opus-4-8"}, Input: permission.Input{Call: domain.ContentBlock{Type: "tool_use", ToolName: "read", Input: map[string]any{"path": "README.md"}}, Intent: domain.PermissionIntent{Complete: true, Entries: []domain.PermissionIntentEntry{{Text: "Read README.md"}}}}}
}
func TestPermissionActivityCacheStillChecksOwnershipAndSkipsModel(t *testing.T) {
	source := &permissionActivityStore{fakeSource: newFakeSource(nil)}
	calls := 0
	acts := NewActivities(nil, source, nil, nil).WithPermissionEvaluator(permissionEvaluatorFunc(func(context.Context, domain.Model, permission.Input) (permission.Result, error) {
		calls++
		return permission.Result{Decision: domain.ToolPermissionDecision{Type: "allow"}, ResponseReceived: true, Usage: domain.TokenUsage{InputTokens: 31}, StopReason: "end_turn"}, nil
	}))
	first, err := acts.EvaluateToolPermission(context.Background(), permissionActivityInput())
	require.NoError(t, err)
	require.Equal(t, "allow", first.Decision.Type)
	second, err := acts.EvaluateToolPermission(context.Background(), permissionActivityInput())
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, source.recordings)
	require.Equal(t, 2, source.admitted)
	source.stopped = true
	stopped, err := acts.EvaluateToolPermission(context.Background(), permissionActivityInput())
	require.NoError(t, err)
	require.True(t, stopped.Stopped)
	require.Equal(t, 1, calls)
}
func TestPermissionActivityRetainsKnownCancelledResponseWithoutExecuting(t *testing.T) {
	source := &permissionActivityStore{fakeSource: newFakeSource(nil)}
	ctx, cancel := context.WithCancel(context.Background())
	acts := NewActivities(nil, source, nil, nil).WithPermissionEvaluator(permissionEvaluatorFunc(func(context.Context, domain.Model, permission.Input) (permission.Result, error) {
		cancel()
		return permission.Result{Decision: domain.ToolPermissionDecision{Type: "allow"}, ResponseReceived: true, Usage: domain.TokenUsage{InputTokens: 51}, StopReason: "end_turn"}, context.Canceled
	}))
	_, err := acts.EvaluateToolPermission(ctx, permissionActivityInput())
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, source.recordings)
	require.Equal(t, int64(51), source.receipt.Usage.InputTokens)
	require.True(t, source.receipt.ResponseReceived)
}
func TestPermissionActivityInvalidOrMissingEvaluatorPersistsAsk(t *testing.T) {
	for _, mode := range []string{"missing", "invalid", "error"} {
		t.Run(mode, func(t *testing.T) {
			source := &permissionActivityStore{fakeSource: newFakeSource(nil)}
			acts := NewActivities(nil, source, nil, nil).WithPermissionEvaluator(nil)
			if mode != "missing" {
				acts.WithPermissionEvaluator(permissionEvaluatorFunc(func(context.Context, domain.Model, permission.Input) (permission.Result, error) {
					if mode == "error" {
						return permission.Result{}, errors.New("transport")
					}
					return permission.Result{Decision: domain.ToolPermissionDecision{Type: "allow", ReasonCode: "invented"}}, nil
				}))
			}
			result, err := acts.EvaluateToolPermission(context.Background(), permissionActivityInput())
			require.NoError(t, err)
			require.Equal(t, domain.ToolPermissionDecision{Type: "ask", ReasonCode: "indeterminate"}, result.Decision)
			require.Equal(t, 1, source.recordings)
			require.False(t, source.receipt.ResponseReceived)
		})
	}
}

func TestPermissionActivityRetriesKnownFactsWithoutRepeatingModel(t *testing.T) {
	source := &permissionActivityStore{fakeSource: newFakeSource(nil), failedWrites: 1}
	calls := 0
	acts := NewActivities(nil, source, nil, nil).WithPermissionEvaluator(permissionEvaluatorFunc(func(context.Context, domain.Model, permission.Input) (permission.Result, error) {
		calls++
		return permission.Result{Decision: domain.ToolPermissionDecision{Type: "allow"}, ResponseReceived: true, Usage: domain.TokenUsage{InputTokens: 17}, StopReason: "end_turn"}, nil
	}))
	result, err := acts.EvaluateToolPermission(context.Background(), permissionActivityInput())
	require.NoError(t, err)
	require.Equal(t, "allow", result.Decision.Type)
	require.Equal(t, 1, calls)
	require.Equal(t, 2, source.recordings)
}
