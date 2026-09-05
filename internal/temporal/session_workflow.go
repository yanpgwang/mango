package temporal

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
)

// continueAsNewThreshold bounds how many turns one workflow history run drives
// before it carries its small cursor state into a fresh history via
// Continue-As-New.
const continueAsNewThreshold = 500

// loadBatchLimit bounds how many event references one LoadEvents call returns.
const loadBatchLimit = 100

// SessionWorkflow is the durable, long-lived orchestrator for one session. Its
// Workflow ID is the public session ID, so Signal-With-Start is idempotent.
//
// Design invariants:
//   - PostgreSQL is the event source of truth. Signals are wakeups carrying only
//     the highest known receipt sequence; the workflow loads authoritative events
//     after its own durable cursor.
//   - A pending client-action barrier is selected before ordinary receipt-order
//     work. Partial resolution remains parked; a full barrier resumes as one
//     logical model turn before queued messages.
//   - The durable cursor advances monotonically. Duplicate or out-of-order
//     wakeups are harmless because authoritative events at or below it are
//     ignored.
//   - State carried across Continue-As-New is small: just the cursor. Completed
//     model and tool Activity results enter Workflow history, while PostgreSQL
//     remains authoritative for the public event ledger and projection.
func SessionWorkflow(ctx workflow.Context, in SessionWorkflowInput) error {
	return sessionWorkflow(ctx, in, continueAsNewThreshold)
}

// sessionWorkflow contains the implementation behind SessionWorkflow. The
// threshold is an argument so workflow tests can exercise Continue-As-New after
// one turn without a mutable package variable. Production always passes the
// compile-time constant above, keeping the value deterministic across replay.
func sessionWorkflow(ctx workflow.Context, in SessionWorkflowInput, canThreshold int) error {
	logger := workflow.GetLogger(ctx)
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			// Keep infrastructure work durable through an operator-recoverable
			// outage. Permanent application failures remain visible as a stuck
			// Activity and require intervention; silently exhausting retries would
			// strand an admitted turn just as surely.
			MaximumAttempts: 0,
		},
	}
	actx := workflow.WithActivityOptions(ctx, ao)
	interruptsEnabled := workflow.GetVersion(
		ctx,
		durableInterruptChangeID,
		workflow.DefaultVersion,
		durableInterruptVersion,
	) == durableInterruptVersion
	cursor := in.StartCursor
	turnsThisRun := 0
	wakeupCh := workflow.GetSignalChannel(ctx, WakeupSignalName)

	// Deterministically consume every wakeup currently buffered in Workflow
	// history. Temporal rejects a close/Continue-As-New command when a Signal
	// arrived during the current Workflow Task. Consuming at both sides of
	// Activity-driven draining makes replay consume that now-visible Signal and
	// progress instead of proposing the same close forever.
	coalesceWakeups := func() bool {
		sawSignal := false
		for {
			var sig WakeupSignal
			if ok := wakeupCh.ReceiveAsync(&sig); !ok {
				return sawSignal
			}
			sawSignal = true
		}
	}

	terminated := false
	drain := func() (bool, error) {
		for {
			// A durable requires_action barrier always wins over ordinary events.
			// This prevents a queued message from overtaking a partially or fully
			// resolved tool round.
			var pending LoadPendingActionsResult
			if err := workflow.ExecuteActivity(
				actx,
				ActivityLoadPendingActions,
				LoadPendingActionsInput{SessionID: in.SessionID},
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
							ID:  action.ResolutionEventID,
							Seq: action.ResolutionEventSeq,
						}
					}
				}
				if interruptsEnabled {
					pendingInterrupt, err := loadInterruptAfter(
						actx,
						in.SessionID,
						"",
						cursor,
					)
					if err != nil {
						return false, err
					}
					if pendingInterrupt.Interrupt != nil &&
						(!fullyClaimed ||
							pendingInterrupt.Interrupt.Seq < trigger.Seq) {
						if _, err := acknowledgeIdleInterrupt(
							actx,
							in.SessionID,
							"",
							pendingInterrupt.Interrupt.ID,
						); err != nil {
							return false, err
						}
						// Do not advance cursor across a pending-action barrier:
						// lower-sequence ordinary messages may still be gated.
						continue
					}
				}
				if !fullyClaimed {
					return true, nil
				}

				var interrupts *turnInterruptWatcher
				if interruptsEnabled {
					interrupts = newTurnInterruptWatcher(
						actx,
						wakeupCh,
						in.SessionID,
						"",
						trigger.Seq,
					)
				}
				res, err := runWorkflowTurnInternal(
					actx, in.SessionID, trigger.ID, resolutionIDs, interrupts,
				)
				if err != nil {
					return false, err
				}
				turnsThisRun++
				switch res.Disposition {
				case TurnTerminated:
					logger.Info(
						"session terminated by pending-action resume; stopping",
						"session_id", in.SessionID,
						"trigger", trigger.ID,
					)
					terminated = true
					return false, nil
				case TurnParked:
					// The resumed model installed a fresh barrier atomically with
					// clearing the old one. Do not let queued ordinary messages
					// overtake it.
					return true, nil
				}
				// A resolution can have a higher public sequence than ordinary
				// messages admitted while the barrier was open. Do not advance the
				// receipt cursor to it; reload the ledger and consume the lower
				// ordinary work first.
				continue
			}

			var loaded LoadEventsResult
			if err := workflow.ExecuteActivity(actx, ActivityLoadEvents, LoadEventsInput{
				SessionID: in.SessionID,
				Cursor:    cursor,
				Limit:     loadBatchLimit,
			}).Get(actx, &loaded); err != nil {
				return false, err
			}
			if len(loaded.Events) == 0 {
				return false, nil
			}
			for _, event := range loaded.Events {
				if event.Seq <= cursor {
					continue
				}
				if event.Type == domain.EvSessionStatusTerminated {
					cursor = event.Seq
					terminated = true
					return false, nil
				}
				if event.Type == domain.EvUserMessage ||
					event.Type == domain.EvUserDefineOutcome ||
					event.Type == domain.EvAgentThreadMessageReceived {
					var interrupts *turnInterruptWatcher
					if interruptsEnabled {
						interrupts = newTurnInterruptWatcher(
							actx,
							wakeupCh,
							in.SessionID,
							"",
							event.Seq,
						)
					}
					res, err := runWorkflowTurnInternal(
						actx,
						in.SessionID,
						event.ID,
						nil,
						interrupts,
					)
					if err != nil {
						return false, err
					}
					turnsThisRun++
					cursor = event.Seq
					switch res.Disposition {
					case TurnTerminated:
						logger.Info(
							"session terminated by turn; stopping",
							"session_id", in.SessionID,
							"trigger", event.ID,
						)
						terminated = true
						return false, nil
					case TurnParked:
						return true, nil
					}
					continue
				}
				if event.Type == domain.EvUserInterrupt {
					if _, err := acknowledgeIdleInterrupt(
						actx,
						in.SessionID,
						"",
						event.ID,
					); err != nil {
						return false, err
					}
					cursor = event.Seq
					continue
				}
				cursor = event.Seq
			}
		}
	}

	drainRequested := false
	for {
		if coalesceWakeups() {
			drainRequested = true
		}
		if drainRequested {
			if _, err := drain(); err != nil {
				return err
			}
		}

		sawSignalDuringDrain := coalesceWakeups()
		if terminated {
			return nil
		}
		if sawSignalDuringDrain {
			drainRequested = true
			continue
		}

		info := workflow.GetInfo(ctx)
		if turnsThisRun >= canThreshold || info.GetContinueAsNewSuggested() {
			logger.Info(
				"continue-as-new",
				"session_id", in.SessionID,
				"cursor", cursor,
				"turns", turnsThisRun,
				"history_length", info.GetCurrentHistoryLength(),
				"history_size", info.GetCurrentHistorySize(),
			)
			return workflow.NewContinueAsNewError(ctx, SessionWorkflow, SessionWorkflowInput{
				SessionID:   in.SessionID,
				StartCursor: cursor,
			})
		}

		var sig WakeupSignal
		wakeupCh.Receive(ctx, &sig)
		drainRequested = true
	}
}
