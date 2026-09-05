package temporal

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	childPendingActionRoutingChangeID = "child-pending-action-routing"
	childPendingActionRoutingVersion  = 1
	childParkedDrainBoundaryChangeID  = "child-message-park-drain-boundary"
	childParkedDrainBoundaryVersion   = 1
	childThreadInterruptsChangeID     = "child-thread-interrupts"
	childThreadInterruptsVersion      = 1
)

// SessionThreadWorkflow is the independent durable loop for one child Agent.
// PostgreSQL owns its Thread identity and ledger; the Workflow owns only the
// in-flight cursor and model/tool command sequence.
func SessionThreadWorkflow(
	ctx workflow.Context,
	in SessionThreadWorkflowInput,
) error {
	return sessionThreadWorkflow(ctx, in, continueAsNewThreshold)
}

func sessionThreadWorkflow(
	ctx workflow.Context,
	in SessionThreadWorkflowInput,
	continueThreshold int,
) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2,
			MaximumInterval: time.Minute, MaximumAttempts: 0,
		},
	}
	actx := workflow.WithActivityOptions(ctx, ao)
	wakeupCh := workflow.GetSignalChannel(ctx, WakeupSignalName)
	cursor := in.StartCursor
	turns := 0
	pendingActionRouting := workflow.GetVersion(
		ctx,
		childPendingActionRoutingChangeID,
		workflow.DefaultVersion,
		childPendingActionRoutingVersion,
	) == childPendingActionRoutingVersion
	parkedMessageDrainBoundary := workflow.GetVersion(
		ctx,
		childParkedDrainBoundaryChangeID,
		workflow.DefaultVersion,
		childParkedDrainBoundaryVersion,
	) == childParkedDrainBoundaryVersion
	interruptsEnabled := workflow.GetVersion(
		ctx,
		childThreadInterruptsChangeID,
		workflow.DefaultVersion,
		childThreadInterruptsVersion,
	) == childThreadInterruptsVersion

	coalesce := func() bool {
		saw := false
		for {
			var signal WakeupSignal
			if !wakeupCh.ReceiveAsync(&signal) {
				return saw
			}
			saw = true
		}
	}
	drain := func() (bool, error) {
		for {
			if pendingActionRouting {
				var pending LoadPendingActionsResult
				if err := workflow.ExecuteActivity(
					actx,
					ActivityLoadPendingActions,
					LoadPendingActionsInput{
						SessionID: in.SessionID, ThreadID: in.ThreadID,
					},
				).Get(actx, &pending); err != nil {
					return false, err
				}
				if len(pending.Actions) > 0 {
					fullyClaimed := true
					trigger := EventRef{}
					resolutionIDs := make([]string, 0, len(pending.Actions))
					for _, action := range pending.Actions {
						if action.ResolutionEventID == "" {
							fullyClaimed = false
							break
						}
						resolutionIDs = append(resolutionIDs, action.ResolutionEventID)
						if action.ResolutionEventSeq > trigger.Seq ||
							(action.ResolutionEventSeq == trigger.Seq &&
								action.ResolutionEventID > trigger.ID) {
							trigger = EventRef{
								ID: action.ResolutionEventID, Seq: action.ResolutionEventSeq,
							}
						}
					}
					if interruptsEnabled {
						pendingInterrupt, err := loadInterruptAfter(
							actx, in.SessionID, in.ThreadID, cursor,
						)
						if err != nil {
							return false, err
						}
						if pendingInterrupt.Interrupt != nil &&
							(!fullyClaimed || pendingInterrupt.Interrupt.Seq < trigger.Seq) {
							if _, err := acknowledgeIdleInterrupt(
								actx, in.SessionID, in.ThreadID,
								pendingInterrupt.Interrupt.ID,
							); err != nil {
								return false, err
							}
							// Keep the cursor below the barrier so queued messages that
							// predate its resolution remain visible after it closes.
							continue
						}
					}
					if !fullyClaimed {
						// The Thread remains parked until every action in this model
						// round has a client result or an interrupt is acknowledged.
						return false, nil
					}
					var interrupts *turnInterruptWatcher
					if interruptsEnabled {
						interrupts = newTurnInterruptWatcher(
							actx, wakeupCh, in.SessionID, in.ThreadID, trigger.Seq,
						)
					}
					completed, err := runWorkflowTurnInternal(
						actx, in.SessionID, trigger.ID, resolutionIDs, interrupts,
					)
					if err != nil {
						return false, err
					}
					turns++
					if stop, terminated := threadDrainBoundary(
						completed.Disposition, true,
					); stop {
						return terminated, nil
					}
					// Completion may have installed a new barrier. Re-evaluate it
					// before consuming any queued follow-up message.
					continue
				}
			}

			var loaded LoadEventsResult
			if err := workflow.ExecuteActivity(
				actx,
				ActivityLoadEvents,
				LoadEventsInput{
					SessionID: in.SessionID, ThreadID: in.ThreadID,
					Cursor: cursor, Limit: loadBatchLimit,
				},
			).Get(actx, &loaded); err != nil {
				return false, err
			}
			if len(loaded.Events) == 0 {
				return false, nil
			}
			for _, event := range loaded.Events {
				if event.Seq <= cursor {
					continue
				}
				if event.Type == domain.EvSessionThreadStatusTerminated ||
					event.Type == domain.EvSessionStatusTerminated {
					cursor = event.Seq
					return true, nil
				}
				if event.Type == domain.EvAgentThreadMessageReceived {
					var interrupts *turnInterruptWatcher
					if interruptsEnabled {
						interrupts = newTurnInterruptWatcher(
							actx, wakeupCh, in.SessionID, in.ThreadID, event.Seq,
						)
					}
					completed, err := runWorkflowTurnInternal(
						actx, in.SessionID, event.ID, nil, interrupts,
					)
					if err != nil {
						return false, err
					}
					turns++
					// A committed message turn owns this receipt even when it parks.
					// Advance before returning so barrier resume does not replay an
					// already-completed PrepareTurn Activity.
					cursor = event.Seq
					if stop, terminated := threadDrainBoundary(
						completed.Disposition, parkedMessageDrainBoundary,
					); stop {
						return terminated, nil
					}
				}
				if event.Type == domain.EvUserInterrupt && interruptsEnabled {
					if _, err := acknowledgeIdleInterrupt(
						actx, in.SessionID, in.ThreadID, event.ID,
					); err != nil {
						return false, err
					}
				}
				cursor = event.Seq
			}
		}
	}

	drainRequested := false
	for {
		if coalesce() {
			drainRequested = true
		}
		if drainRequested {
			terminated, err := drain()
			if err != nil {
				return err
			}
			if terminated {
				return nil
			}
		}
		if coalesce() {
			drainRequested = true
			continue
		}
		info := workflow.GetInfo(ctx)
		if turns >= continueThreshold || info.GetContinueAsNewSuggested() {
			return workflow.NewContinueAsNewError(
				ctx,
				SessionThreadWorkflow,
				SessionThreadWorkflowInput{
					SessionID: in.SessionID, ThreadID: in.ThreadID,
					StartCursor: cursor,
				},
			)
		}
		wakeupCh.Receive(ctx, nil)
		drainRequested = true
	}
}

// threadDrainBoundary centralizes the two dispositions that may stop the
// current child-ledger drain. stopOnPark is version-gated for message turns so
// histories recorded before that boundary keep their original command shape;
// pending-action resumes have always stopped when they install a new barrier.
func threadDrainBoundary(
	disposition TurnDisposition,
	stopOnPark bool,
) (stop bool, terminated bool) {
	switch disposition {
	case TurnTerminated:
		return true, true
	case TurnParked:
		return stopOnPark, false
	default:
		return false, false
	}
}

func sessionThreadWorkflowID(threadID string) string {
	return "session-thread:" + threadID
}
