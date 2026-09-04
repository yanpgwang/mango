package mango

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const (
	DefaultMaxIdle = 60 * time.Second

	defaultSessionToolTimeout     = 120 * time.Second
	defaultSessionSendTimeout     = 15 * time.Second
	defaultSessionSendRetryWindow = 30 * time.Second
	sessionStreamRetryCap         = 5 * time.Second
	sessionRunnerBuffer           = 32
)

var (
	// ErrSessionTerminated reports a durable Session terminal event, or a
	// terminal response from an events endpoint.
	ErrSessionTerminated = errors.New("mango: session terminated")
	// ErrIdleTimeout reports that an end_turn idle remained quiet for MaxIdle.
	ErrIdleTimeout = errors.New("mango: session idle after end_turn")
	// ErrSessionLeaseLost reports that the credential no longer owns the
	// self-hosted Work lease. No further tool results are submitted.
	ErrSessionLeaseLost = errors.New("mango: session execution lease lost")
)

// SessionToolCall is passed to a local tool. ToolUseID is the stable event ID
// and should be used as the idempotency key for external side effects.
type SessionToolCall struct {
	SessionID       string
	ToolUseID       string
	Name            string
	Input           json.RawMessage
	Custom          bool
	SessionThreadID Optional[string]
}

// SessionTool is a locally executable tool registered with SessionToolRunner.
// Execute must honor ctx. A successful call returns Sessions result blocks;
// returning an error posts a text error result to the Session.
type SessionTool interface {
	Name() string
	Execute(context.Context, SessionToolCall) ([]ResultContentInput, error)
}

// SessionToolRunnerOptions configure a provider-neutral runner for one Session.
type SessionToolRunnerOptions struct {
	Tools []SessionTool
	// MaxIdle starts after session.status_idle{stop_reason:end_turn}. nil uses
	// DefaultMaxIdle; a non-positive value disables it.
	MaxIdle *time.Duration
	Logger  *slog.Logger
	// ToolTimeout and SendTimeout use their defaults when zero. Negative values
	// are rejected. Tools must still cooperate with context cancellation.
	ToolTimeout time.Duration
	SendTimeout time.Duration
	// SendRetryWindow bounds retries of ambiguous or transient result writes.
	// Zero uses the default Work lease TTL of 30 seconds.
	SendRetryWindow time.Duration
}

// DispatchedToolCall describes a local dispatch attempt. Denied and unowned
// calls are observable but have no Result and are never posted.
type DispatchedToolCall struct {
	Custom        bool
	Confirmation  string
	Owned         bool
	ToolUse       *AgentToolUseEvent
	CustomToolUse *AgentCustomToolUseEvent
	Result        *UserToolResultEventInput
	CustomResult  *UserCustomToolResultEventInput
	ToolUseID     string
	Name          string
	IsError       bool
	Posted        bool
}

// SessionToolRunner attaches to one Session, reconciles durable history after
// every connection, executes owned local tools serially, and posts matching
// result events. MCP tool calls remain server-side. It neither heartbeats Work
// nor creates a sandbox; EnvironmentWorker composes those responsibilities.
//
// Iterator methods are not safe for concurrent use. Close is safe while Next
// blocks. The runner never closes registered tools.
type SessionToolRunner struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	client    *Client
	sessionID string
	opts      SessionToolRunnerOptions
	logger    *slog.Logger
	tools     map[string]SessionTool

	lifecycleMu sync.Mutex
	started     bool
	closed      bool
	results     chan DispatchedToolCall
	done        chan struct{}
	current     DispatchedToolCall

	errMu sync.Mutex
	err   error

	state *sessionToolState
	idle  *sessionIdleClock

	sleep      func(context.Context, time.Duration)
	retryDelay func(int) time.Duration
}

type sessionToolState struct {
	mu       sync.Mutex
	seen     map[string]struct{}
	claimed  map[string]struct{}
	resolved map[string]struct{}
}

type pendingSessionToolCall struct {
	custom        bool
	toolUse       *AgentToolUseEvent
	customToolUse *AgentCustomToolUseEvent
	confirmation  string
}

func (p pendingSessionToolCall) id() string {
	if p.custom {
		return p.customToolUse.ID
	}
	return p.toolUse.ID
}

func (p pendingSessionToolCall) name() string {
	if p.custom {
		return p.customToolUse.Name
	}
	return p.toolUse.Name
}

func (p pendingSessionToolCall) input() map[string]json.RawMessage {
	if p.custom {
		return p.customToolUse.Input
	}
	return p.toolUse.Input
}

func (p pendingSessionToolCall) threadID() Optional[string] {
	if p.custom {
		return p.customToolUse.SessionThreadID
	}
	return p.toolUse.SessionThreadID
}

// NewSessionToolRunner returns an iterator bound to client and sessionID.
// Configuration errors are reported by Err after Next returns false.
func NewSessionToolRunner(ctx context.Context, client *Client, sessionID string, opts SessionToolRunnerOptions) *SessionToolRunner {
	internalCtx, cancel := context.WithCancelCause(ctx)
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner := &SessionToolRunner{
		ctx:       internalCtx,
		cancel:    cancel,
		client:    client,
		sessionID: sessionID,
		opts:      opts,
		logger:    logger.With("component", "session-tool-runner", "session_id", sessionID),
		tools:     make(map[string]SessionTool, len(opts.Tools)),
		state: &sessionToolState{
			seen:     make(map[string]struct{}),
			claimed:  make(map[string]struct{}),
			resolved: make(map[string]struct{}),
		},
		sleep:      sleepWithContext,
		retryDelay: sessionToolRetryDelay,
	}
	runner.idle = newSessionIdleClock(runner.maxIdle())
	switch {
	case client == nil:
		runner.err = errors.New("mango: SessionToolRunner client is required")
	case sessionID == "":
		runner.err = errors.New("mango: SessionToolRunner session ID is required")
	case opts.ToolTimeout < 0:
		runner.err = errors.New("mango: SessionToolRunnerOptions.ToolTimeout must be non-negative")
	case opts.SendTimeout < 0:
		runner.err = errors.New("mango: SessionToolRunnerOptions.SendTimeout must be non-negative")
	case opts.SendRetryWindow < 0:
		runner.err = errors.New("mango: SessionToolRunnerOptions.SendRetryWindow must be non-negative")
	}
	for _, tool := range opts.Tools {
		if runner.err != nil {
			break
		}
		if tool == nil || tool.Name() == "" {
			runner.err = errors.New("mango: SessionToolRunner tools must have non-empty names")
			break
		}
		if _, exists := runner.tools[tool.Name()]; exists {
			runner.err = fmt.Errorf("mango: SessionToolRunner tool name %q is duplicated", tool.Name())
			break
		}
		runner.tools[tool.Name()] = tool
	}
	return runner
}

// Next advances to the next observed dispatch.
func (r *SessionToolRunner) Next() bool {
	if r.Err() != nil {
		return false
	}
	results, started := r.start()
	if !started {
		return false
	}
	call, ok := <-results
	if !ok {
		return false
	}
	r.current = call
	return true
}

func (r *SessionToolRunner) Current() DispatchedToolCall { return r.current }

// Err returns the first terminal runner error. Caller cancellation and Close
// are normal completion and leave Err nil.
func (r *SessionToolRunner) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

func (r *SessionToolRunner) Close() error {
	r.lifecycleMu.Lock()
	if r.closed {
		started, done := r.started, r.done
		r.lifecycleMu.Unlock()
		if started {
			<-done
		}
		return nil
	}
	r.closed = true
	started, done := r.started, r.done
	r.cancel(context.Canceled)
	r.lifecycleMu.Unlock()
	if started {
		<-done
	}
	return nil
}

func (r *SessionToolRunner) All() iter.Seq2[DispatchedToolCall, error] {
	return func(yield func(DispatchedToolCall, error) bool) {
		for r.Next() {
			if !yield(r.Current(), nil) {
				_ = r.Close()
				return
			}
		}
		if err := r.Err(); err != nil {
			yield(DispatchedToolCall{}, err)
		}
	}
}

func (r *SessionToolRunner) start() (<-chan DispatchedToolCall, bool) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return nil, false
	}
	if r.started {
		return r.results, true
	}
	if r.Err() != nil {
		return nil, false
	}
	r.started = true
	r.results = make(chan DispatchedToolCall, sessionRunnerBuffer)
	r.done = make(chan struct{})
	queue := make(chan pendingSessionToolCall, sessionRunnerBuffer)
	componentDone := make(chan error, 3)
	go func() { componentDone <- r.ingest(r.ctx, queue) }()
	go func() { componentDone <- r.dispatch(r.ctx, queue) }()
	go func() { componentDone <- r.watchIdle(r.ctx) }()
	go func() {
		var terminal error
		for range 3 {
			err := <-componentDone
			if err != nil && terminal == nil {
				terminal = err
				r.cancel(err)
			}
		}
		r.setErr(terminal)
		close(r.results)
		close(r.done)
	}()
	return r.results, true
}

func (r *SessionToolRunner) setErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	r.errMu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.errMu.Unlock()
}

func (r *SessionToolRunner) ingest(ctx context.Context, queue chan<- pendingSessionToolCall) error {
	confirmations := make(map[string]string)
	awaiting := make(map[string]pendingSessionToolCall)
	failures := 0
	for ctx.Err() == nil {
		streamCtx, stopStream := context.WithCancel(ctx)
		stream, err := r.client.StreamSessionEvents(streamCtx, r.sessionID, StreamSessionEventsParams{})
		if err != nil {
			stopStream()
			if terminal := classifySessionRequestError("open event stream", err); terminal != nil {
				return terminal
			}
			failures++
			delay := r.retryDelay(failures)
			r.logger.Warn("event stream connection failed; retrying", "delay", delay, "error", err)
			r.sleep(ctx, delay)
			continue
		}

		live := make(chan sessionStreamItem, sessionRunnerBuffer)
		go readSessionStream(streamCtx, stream, live)
		if err := r.reconcile(ctx, queue, confirmations, awaiting); err != nil {
			stopStream()
			_ = stream.Close()
			if terminal := classifySessionRequestError("reconcile event history", err); terminal != nil {
				return terminal
			}
			failures++
			delay := r.retryDelay(failures)
			r.logger.Warn("event history reconciliation failed; retrying", "delay", delay, "error", err)
			r.sleep(ctx, delay)
			continue
		}

		connectedAt := time.Now()
		for {
			select {
			case <-ctx.Done():
				stopStream()
				_ = stream.Close()
				return nil
			case item, ok := <-live:
				if !ok {
					stopStream()
					_ = stream.Close()
					if time.Since(connectedAt) >= 5*time.Second {
						failures = 0
					} else {
						failures++
					}
					delay := r.retryDelay(failures)
					r.logger.Debug("event stream closed; reconnecting", "delay", delay)
					r.sleep(ctx, delay)
					goto reconnect
				}
				if item.err != nil {
					stopStream()
					_ = stream.Close()
					var decodeError *sessionStreamDecodeError
					if errors.As(item.err, &decodeError) {
						return item.err
					}
					if terminal := classifySessionRequestError("read event stream", item.err); terminal != nil {
						return terminal
					}
					failures++
					delay := r.retryDelay(failures)
					r.logger.Warn("event stream read failed; reconnecting", "delay", delay, "error", item.err)
					r.sleep(ctx, delay)
					goto reconnect
				}
				if item.event == nil {
					continue
				}
				if err := r.handleLiveEvent(ctx, queue, confirmations, awaiting, *item.event); err != nil {
					stopStream()
					_ = stream.Close()
					return err
				}
			}
		}
	reconnect:
	}
	return nil
}

type sessionStreamItem struct {
	event *SessionEvent
	err   error
}

type sessionStreamDecodeError struct{ err error }

func (e *sessionStreamDecodeError) Error() string {
	return "mango: decode Session event stream: " + e.err.Error()
}
func (e *sessionStreamDecodeError) Unwrap() error { return e.err }

func readSessionStream(ctx context.Context, stream *EventStream, out chan<- sessionStreamItem) {
	defer close(out)
	for stream.Next() {
		var frame EventStreamFrame
		if err := stream.Event().Decode(&frame); err != nil {
			select {
			case out <- sessionStreamItem{err: &sessionStreamDecodeError{err: err}}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case out <- sessionStreamItem{event: frame.SessionEvent}:
		case <-ctx.Done():
			return
		}
	}
	if err := stream.Err(); err != nil {
		select {
		case out <- sessionStreamItem{err: err}:
		case <-ctx.Done():
		}
	}
}

func (r *SessionToolRunner) reconcile(
	ctx context.Context,
	queue chan<- pendingSessionToolCall,
	confirmations map[string]string,
	awaiting map[string]pendingSessionToolCall,
) error {
	r.idle.disarm()
	var pending []pendingSessionToolCall
	var last SessionEvent
	hasLast := false
	pager := r.client.ListSessionEventsAutoPaging(ctx, r.sessionID, ListSessionEventsParams{
		Limit: Some(int64(1000)), Order: Some("asc"),
	})
	for pager.Next() {
		event := pager.Value()
		last, hasLast = event, true
		r.markSeen(sessionEventID(event))
		switch {
		case event.AgentToolUseEvent != nil:
			copy := *event.AgentToolUseEvent
			pending = append(pending, pendingSessionToolCall{toolUse: &copy})
		case event.AgentCustomToolUseEvent != nil:
			copy := *event.AgentCustomToolUseEvent
			pending = append(pending, pendingSessionToolCall{custom: true, customToolUse: &copy})
		case event.PersistedUserToolConfirmationEvent != nil:
			confirmation := event.PersistedUserToolConfirmationEvent
			confirmations[confirmation.ToolUseID] = confirmation.Result
		case event.PersistedUserToolResultEvent != nil:
			r.resolve(event.PersistedUserToolResultEvent.ToolUseID)
			delete(awaiting, event.PersistedUserToolResultEvent.ToolUseID)
		case event.PersistedUserCustomToolResultEvent != nil:
			r.resolve(event.PersistedUserCustomToolResultEvent.CustomToolUseID)
			delete(awaiting, event.PersistedUserCustomToolResultEvent.CustomToolUseID)
		case event.SessionStatusTerminatedEvent != nil, event.SessionDeletedEvent != nil:
			return ErrSessionTerminated
		}
	}
	if err := pager.Err(); err != nil {
		return err
	}

	for _, call := range pending {
		r.route(ctx, queue, confirmations, awaiting, call)
	}
	for id, call := range awaiting {
		if verdict, ok := confirmations[id]; ok {
			r.applyConfirmation(ctx, queue, awaiting, call, verdict)
		}
	}
	if hasLast {
		r.idle.note(last)
	}
	return nil
}

func (r *SessionToolRunner) handleLiveEvent(
	ctx context.Context,
	queue chan<- pendingSessionToolCall,
	confirmations map[string]string,
	awaiting map[string]pendingSessionToolCall,
	event SessionEvent,
) error {
	id := sessionEventID(event)
	if id != "" && !r.markSeen(id) {
		return nil
	}
	r.idle.note(event)
	switch {
	case event.AgentToolUseEvent != nil:
		copy := *event.AgentToolUseEvent
		r.route(ctx, queue, confirmations, awaiting, pendingSessionToolCall{toolUse: &copy})
	case event.AgentCustomToolUseEvent != nil:
		copy := *event.AgentCustomToolUseEvent
		r.route(ctx, queue, confirmations, awaiting, pendingSessionToolCall{custom: true, customToolUse: &copy})
	case event.PersistedUserToolConfirmationEvent != nil:
		confirmation := event.PersistedUserToolConfirmationEvent
		confirmations[confirmation.ToolUseID] = confirmation.Result
		if call, ok := awaiting[confirmation.ToolUseID]; ok {
			r.applyConfirmation(ctx, queue, awaiting, call, confirmation.Result)
		}
	case event.PersistedUserToolResultEvent != nil:
		r.resolve(event.PersistedUserToolResultEvent.ToolUseID)
		delete(awaiting, event.PersistedUserToolResultEvent.ToolUseID)
	case event.PersistedUserCustomToolResultEvent != nil:
		r.resolve(event.PersistedUserCustomToolResultEvent.CustomToolUseID)
		delete(awaiting, event.PersistedUserCustomToolResultEvent.CustomToolUseID)
	case event.SessionStatusTerminatedEvent != nil, event.SessionDeletedEvent != nil:
		return ErrSessionTerminated
	}
	return nil
}

func (r *SessionToolRunner) route(
	ctx context.Context,
	queue chan<- pendingSessionToolCall,
	confirmations map[string]string,
	awaiting map[string]pendingSessionToolCall,
	call pendingSessionToolCall,
) {
	if !r.claim(call.id()) {
		return
	}
	r.idle.block(call.id())
	if call.custom {
		r.enqueue(ctx, queue, call)
		return
	}
	permission, present := call.toolUse.EvaluatedPermission.Get()
	switch {
	case !present || permission == EvaluatedPermissionAllow:
		r.enqueue(ctx, queue, call)
	case permission == EvaluatedPermissionDeny:
		r.deny(ctx, call)
	default:
		awaiting[call.id()] = call
		if verdict, ok := confirmations[call.id()]; ok {
			r.applyConfirmation(ctx, queue, awaiting, call, verdict)
		}
	}
}

func (r *SessionToolRunner) applyConfirmation(
	ctx context.Context,
	queue chan<- pendingSessionToolCall,
	awaiting map[string]pendingSessionToolCall,
	call pendingSessionToolCall,
	verdict string,
) {
	switch verdict {
	case "allow":
		delete(awaiting, call.id())
		call.confirmation = verdict
		r.enqueue(ctx, queue, call)
	case "deny":
		delete(awaiting, call.id())
		r.deny(ctx, call)
	}
}

func (r *SessionToolRunner) deny(ctx context.Context, pending pendingSessionToolCall) {
	if !r.resolve(pending.id()) {
		return
	}
	call := r.describe(pending)
	call.Owned = r.tools[pending.name()] != nil
	call.Confirmation = "deny"
	r.yield(ctx, call)
}

func (r *SessionToolRunner) enqueue(ctx context.Context, queue chan<- pendingSessionToolCall, call pendingSessionToolCall) {
	select {
	case queue <- call:
	case <-ctx.Done():
	}
}

func (r *SessionToolRunner) dispatch(ctx context.Context, queue <-chan pendingSessionToolCall) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case pending := <-queue:
			if ctx.Err() != nil {
				return nil
			}
			if r.isResolved(pending.id()) {
				continue
			}
			call, err := r.execute(ctx, pending)
			if call.ToolUseID != "" {
				r.yield(ctx, call)
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
	}
}

func (r *SessionToolRunner) execute(ctx context.Context, pending pendingSessionToolCall) (DispatchedToolCall, error) {
	call := r.describe(pending)
	tool, owned := r.tools[pending.name()]
	call.Owned = owned
	if !owned {
		r.release(pending.id())
		r.logger.Info("tool is not registered; leaving result pending", "tool", pending.name(), "tool_use_id", pending.id())
		return call, nil
	}
	rawInput, err := json.Marshal(pending.input())
	if err != nil {
		call.IsError = true
		return r.postResult(ctx, call, pending.threadID(), textSessionToolResult(fmt.Sprintf("tool input could not be encoded: %v", err)))
	}

	toolCtx, cancel := context.WithTimeout(ctx, r.toolTimeout())
	blocks, runErr := tool.Execute(toolCtx, SessionToolCall{
		SessionID: r.sessionID, ToolUseID: pending.id(), Name: pending.name(),
		Input: rawInput, Custom: pending.custom, SessionThreadID: pending.threadID(),
	})
	timedOut := errors.Is(toolCtx.Err(), context.DeadlineExceeded)
	cancel()
	switch {
	case timedOut:
		call.IsError = true
		blocks = textSessionToolResult(fmt.Sprintf("tool %q timed out after %s", pending.name(), r.toolTimeout()))
	case runErr != nil:
		call.IsError = true
		blocks = textSessionToolResult(runErr.Error())
	case len(blocks) == 0:
		blocks = textSessionToolResult("(no output)")
	}
	resultCtx := ctx
	cancelResult := func() {}
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		// Cancellation must stop the local side effect before Work is released,
		// but the matching error result still needs a short, independent window
		// to cross the durable Session boundary. Keep context values while
		// deliberately detaching only cancellation and deadline propagation.
		resultCtx, cancelResult = context.WithTimeout(context.WithoutCancel(ctx), r.sendRetryWindow())
	}
	defer cancelResult()
	return r.postResult(resultCtx, call, pending.threadID(), blocks)
}

func (r *SessionToolRunner) postResult(
	ctx context.Context,
	call DispatchedToolCall,
	threadID Optional[string],
	blocks []ResultContentInput,
) (DispatchedToolCall, error) {
	// Another client may have answered while this tool was running. The local
	// side effect cannot be undone, but a second result must not be appended.
	if r.isResolved(call.ToolUseID) {
		return call, nil
	}
	var input ClientSessionEventInput
	if call.Custom {
		result := UserCustomToolResultEventInput{
			Type: "user.custom_tool_result", CustomToolUseID: call.ToolUseID,
			Content: Some(blocks), IsError: Some(call.IsError), SessionThreadID: threadID,
		}
		call.CustomResult = &result
		input.UserCustomToolResultEventInput = &result
	} else {
		result := UserToolResultEventInput{
			Type: "user.tool_result", ToolUseID: call.ToolUseID,
			Content: Some(blocks), IsError: Some(call.IsError), SessionThreadID: threadID,
		}
		call.Result = &result
		input.UserToolResultEventInput = &result
	}
	err := r.sendResult(ctx, call.ToolUseID, input)
	if err == nil {
		call.Posted = true
		r.resolve(call.ToolUseID)
		return call, nil
	}
	r.release(call.ToolUseID)
	return call, err
}

func (r *SessionToolRunner) sendResult(ctx context.Context, toolUseID string, event ClientSessionEventInput) error {
	request := SendSessionEventsRequest{Events: []ClientSessionEventInput{event}}
	if _, err := json.Marshal(request); err != nil {
		return fmt.Errorf("mango: encode tool result %s: %w", toolUseID, err)
	}
	retryCtx, cancelRetry := context.WithTimeout(ctx, r.sendRetryWindow())
	defer cancelRetry()
	retryDeadline, _ := retryCtx.Deadline()
	var lastSendErr error
	for attempt := 1; ; attempt++ {
		if attempt > 1 && r.isResolved(toolUseID) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if retryCtx.Err() != nil {
			cause := lastSendErr
			if cause == nil {
				cause = retryCtx.Err()
			}
			return fmt.Errorf("mango: send tool result %s: retry window exhausted: %w", toolUseID, cause)
		}
		sendCtx, cancel := context.WithTimeout(retryCtx, r.sendTimeout())
		_, err := r.client.SendSessionEvents(sendCtx, r.sessionID, request)
		cancel()
		if err == nil {
			return nil
		}
		lastSendErr = err
		if terminal := classifySessionRequestError("send tool result", err); terminal != nil {
			return terminal
		}
		found, checkErr := r.resultExists(retryCtx, toolUseID)
		if checkErr != nil {
			if terminal := classifySessionRequestError("reconcile ambiguous tool result", checkErr); terminal != nil {
				return terminal
			}
		} else if found {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		remaining := time.Until(retryDeadline)
		if remaining <= 0 {
			return fmt.Errorf("mango: send tool result %s: retry window exhausted: %w", toolUseID, err)
		}
		delay := min(r.retryDelay(attempt), remaining)
		r.logger.Warn("tool result send failed; retrying", "tool_use_id", toolUseID, "attempt", attempt, "delay", delay, "error", err)
		r.sleep(retryCtx, delay)
	}
}

func (r *SessionToolRunner) resultExists(ctx context.Context, toolUseID string) (bool, error) {
	pager := r.client.ListSessionEventsAutoPaging(ctx, r.sessionID, ListSessionEventsParams{
		Limit: Some(int64(1000)), Order: Some("asc"),
		Types: Some([]CoreSessionEventType{
			CoreSessionEventTypeUserToolResult,
			CoreSessionEventTypeUserCustomToolResult,
		}),
	})
	for pager.Next() {
		event := pager.Value()
		if event.PersistedUserToolResultEvent != nil && event.PersistedUserToolResultEvent.ToolUseID == toolUseID {
			return true, nil
		}
		if event.PersistedUserCustomToolResultEvent != nil && event.PersistedUserCustomToolResultEvent.CustomToolUseID == toolUseID {
			return true, nil
		}
	}
	return false, pager.Err()
}

func (r *SessionToolRunner) describe(pending pendingSessionToolCall) DispatchedToolCall {
	return DispatchedToolCall{
		Custom: pending.custom, Confirmation: pending.confirmation,
		ToolUse: pending.toolUse, CustomToolUse: pending.customToolUse,
		ToolUseID: pending.id(), Name: pending.name(),
	}
}

func (r *SessionToolRunner) yield(ctx context.Context, call DispatchedToolCall) {
	select {
	case r.results <- call:
	case <-ctx.Done():
	}
}

func (r *SessionToolRunner) markSeen(id string) bool {
	if id == "" {
		return true
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if _, exists := r.state.seen[id]; exists {
		return false
	}
	r.state.seen[id] = struct{}{}
	return true
}

func (r *SessionToolRunner) claim(id string) bool {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if _, done := r.state.resolved[id]; done {
		return false
	}
	if _, busy := r.state.claimed[id]; busy {
		return false
	}
	r.state.claimed[id] = struct{}{}
	return true
}

func (r *SessionToolRunner) resolve(id string) bool {
	r.state.mu.Lock()
	_, existed := r.state.resolved[id]
	r.state.resolved[id] = struct{}{}
	delete(r.state.claimed, id)
	r.state.mu.Unlock()
	r.idle.unblock(id)
	return !existed
}

func (r *SessionToolRunner) release(id string) {
	r.state.mu.Lock()
	delete(r.state.claimed, id)
	r.state.mu.Unlock()
}

func (r *SessionToolRunner) isResolved(id string) bool {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	_, ok := r.state.resolved[id]
	return ok
}

func (r *SessionToolRunner) maxIdle() time.Duration {
	if r.opts.MaxIdle == nil {
		return DefaultMaxIdle
	}
	return *r.opts.MaxIdle
}

func (r *SessionToolRunner) toolTimeout() time.Duration {
	if r.opts.ToolTimeout == 0 {
		return defaultSessionToolTimeout
	}
	return r.opts.ToolTimeout
}

func (r *SessionToolRunner) sendTimeout() time.Duration {
	if r.opts.SendTimeout == 0 {
		return defaultSessionSendTimeout
	}
	return r.opts.SendTimeout
}

func (r *SessionToolRunner) sendRetryWindow() time.Duration {
	if r.opts.SendRetryWindow == 0 {
		return defaultSessionSendRetryWindow
	}
	return r.opts.SendRetryWindow
}

func textSessionToolResult(text string) []ResultContentInput {
	return []ResultContentInput{{TextBlockInput: &TextBlockInput{Type: "text", Text: text}}}
}

func classifySessionRequestError(action string, err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return nil
	}
	switch apiError.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %s: %v", ErrSessionLeaseLost, action, err)
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: %s: %v", ErrSessionTerminated, action, err)
	}
	if apiError.StatusCode >= 400 && apiError.StatusCode < 500 &&
		apiError.StatusCode != http.StatusRequestTimeout &&
		apiError.StatusCode != http.StatusConflict &&
		apiError.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("mango: %s: %w", action, err)
	}
	return nil
}

func sessionToolRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	ceiling := time.Duration(1<<(min(failures, 5)-1)) * 250 * time.Millisecond
	ceiling = min(ceiling, sessionStreamRetryCap)
	floor := ceiling / 2
	return floor + time.Duration(mrand.Int64N(int64(ceiling-floor)))
}

func sessionEventID(event SessionEvent) string {
	switch {
	case event.PersistedUserMessageEvent != nil:
		return event.PersistedUserMessageEvent.ID
	case event.PersistedUserInterruptEvent != nil:
		return event.PersistedUserInterruptEvent.ID
	case event.PersistedUserToolConfirmationEvent != nil:
		return event.PersistedUserToolConfirmationEvent.ID
	case event.PersistedUserCustomToolResultEvent != nil:
		return event.PersistedUserCustomToolResultEvent.ID
	case event.PersistedUserToolResultEvent != nil:
		return event.PersistedUserToolResultEvent.ID
	case event.PersistedUserDefineOutcomeEvent != nil:
		return event.PersistedUserDefineOutcomeEvent.ID
	case event.PersistedSystemMessageEvent != nil:
		return event.PersistedSystemMessageEvent.ID
	case event.AgentMessageEvent != nil:
		return event.AgentMessageEvent.ID
	case event.AgentThinkingEvent != nil:
		return event.AgentThinkingEvent.ID
	case event.AgentCustomToolUseEvent != nil:
		return event.AgentCustomToolUseEvent.ID
	case event.AgentToolUseEvent != nil:
		return event.AgentToolUseEvent.ID
	case event.AgentToolResultEvent != nil:
		return event.AgentToolResultEvent.ID
	case event.AgentMCPToolUseEvent != nil:
		return event.AgentMCPToolUseEvent.ID
	case event.AgentMCPToolResultEvent != nil:
		return event.AgentMCPToolResultEvent.ID
	case event.SessionStatusIdleEvent != nil:
		return event.SessionStatusIdleEvent.ID
	case event.SessionStatusRunningEvent != nil:
		return event.SessionStatusRunningEvent.ID
	case event.SessionStatusTerminatedEvent != nil:
		return event.SessionStatusTerminatedEvent.ID
	case event.SessionStatusRescheduledEvent != nil:
		return event.SessionStatusRescheduledEvent.ID
	case event.SessionUsageEvent != nil:
		return event.SessionUsageEvent.ID
	case event.SessionErrorEvent != nil:
		return event.SessionErrorEvent.ID
	case event.SessionUpdatedEvent != nil:
		return event.SessionUpdatedEvent.ID
	case event.SessionDeletedEvent != nil:
		return event.SessionDeletedEvent.ID
	case event.SessionThreadCreatedEvent != nil:
		return event.SessionThreadCreatedEvent.ID
	case event.SessionThreadStatusIdleEvent != nil:
		return event.SessionThreadStatusIdleEvent.ID
	case event.SessionThreadStatusRunningEvent != nil:
		return event.SessionThreadStatusRunningEvent.ID
	case event.SessionThreadStatusRescheduledEvent != nil:
		return event.SessionThreadStatusRescheduledEvent.ID
	case event.SessionThreadStatusTerminatedEvent != nil:
		return event.SessionThreadStatusTerminatedEvent.ID
	case event.AgentThreadMessageReceivedEvent != nil:
		return event.AgentThreadMessageReceivedEvent.ID
	case event.AgentThreadMessageSentEvent != nil:
		return event.AgentThreadMessageSentEvent.ID
	case event.AgentThreadContextCompactedEvent != nil:
		return event.AgentThreadContextCompactedEvent.ID
	case event.SpanOutcomeEvaluationStartEvent != nil:
		return event.SpanOutcomeEvaluationStartEvent.ID
	case event.SpanOutcomeEvaluationOngoingEvent != nil:
		return event.SpanOutcomeEvaluationOngoingEvent.ID
	case event.SpanOutcomeEvaluationEndEvent != nil:
		return event.SpanOutcomeEvaluationEndEvent.ID
	case event.SpanModelRequestStartEvent != nil:
		return event.SpanModelRequestStartEvent.ID
	case event.SpanModelRequestEndEvent != nil:
		return event.SpanModelRequestEndEvent.ID
	default:
		return ""
	}
}

type sessionIdleClock struct {
	duration time.Duration
	wake     chan struct{}
	mu       sync.Mutex
	deadline time.Time
	pending  bool
	blocked  map[string]struct{}
}

func newSessionIdleClock(duration time.Duration) *sessionIdleClock {
	return &sessionIdleClock{duration: duration, wake: make(chan struct{}, 1), blocked: make(map[string]struct{})}
}

func (c *sessionIdleClock) note(event SessionEvent) {
	if event.PersistedUserToolConfirmationEvent != nil {
		return
	}
	if event.SessionStatusIdleEvent != nil && event.SessionStatusIdleEvent.StopReason.SessionEndTurn != nil {
		c.arm()
		return
	}
	c.disarm()
}

func (c *sessionIdleClock) arm() {
	if c.duration <= 0 {
		return
	}
	c.mu.Lock()
	if len(c.blocked) > 0 {
		c.pending = true
		c.deadline = time.Time{}
	} else {
		c.pending = false
		c.deadline = time.Now().Add(c.duration)
	}
	c.mu.Unlock()
	c.signal()
}

func (c *sessionIdleClock) disarm() {
	c.mu.Lock()
	c.pending = false
	c.deadline = time.Time{}
	c.mu.Unlock()
	c.signal()
}

func (c *sessionIdleClock) block(id string) {
	c.mu.Lock()
	c.blocked[id] = struct{}{}
	if !c.deadline.IsZero() {
		c.pending = true
		c.deadline = time.Time{}
	}
	c.mu.Unlock()
	c.signal()
}

func (c *sessionIdleClock) unblock(id string) {
	c.mu.Lock()
	delete(c.blocked, id)
	if len(c.blocked) == 0 && c.pending {
		c.pending = false
		c.deadline = time.Now().Add(c.duration)
	}
	c.mu.Unlock()
	c.signal()
}

func (c *sessionIdleClock) snapshot() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func (c *sessionIdleClock) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (r *SessionToolRunner) watchIdle(ctx context.Context) error {
	if r.idle.duration <= 0 {
		<-ctx.Done()
		return nil
	}
	var timer *time.Timer
	for {
		deadline := r.idle.snapshot()
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			delay := time.Until(deadline)
			if delay <= 0 {
				return ErrIdleTimeout
			}
			if timer == nil {
				timer = time.NewTimer(delay)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(delay)
			}
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		case <-r.idle.wake:
		case <-timerC:
			if current := r.idle.snapshot(); !current.IsZero() && !time.Now().Before(current) {
				return ErrIdleTimeout
			}
		}
	}
}

var _ io.Closer = (*SessionToolRunner)(nil)
