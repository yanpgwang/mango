package mango

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultEnvironmentLeaseTTL         = 30 * time.Second
	defaultEnvironmentHeartbeatFloor   = 100 * time.Millisecond
	defaultEnvironmentHeartbeatCeiling = 30 * time.Second
	defaultEnvironmentStopTimeout      = 15 * time.Second
	maxEnvironmentWorkSecretBytes      = 16 << 10
	noEnvironmentHeartbeat             = "NO_HEARTBEAT"
)

var ErrEnvironmentWorkLeaseLost = errors.New("mango: Environment Work lease lost")

// EnvironmentWorkerOptions configure the provider-neutral self-hosted worker.
// The Client passed to NewEnvironmentWorker is the trusted supervisor client
// used only for Poll and Ack. Every request after Ack uses the scoped token
// carried by the Work secret.
type EnvironmentWorkerOptions struct {
	EnvironmentID      string
	WorkerID           string
	Drain              bool
	BlockMs            Optional[int64]
	ReclaimOlderThanMs Optional[int64]

	// DesiredTTLSeconds, when set, is requested on every heartbeat. The server
	// remains authoritative and its returned TTL bounds result-send recovery.
	DesiredTTLSeconds Optional[int64]

	Tools       []SessionTool
	MaxIdle     *time.Duration
	ToolTimeout time.Duration
	SendTimeout time.Duration
	Logger      *slog.Logger
}

// EnvironmentWorkerHandleItemOptions identifies one already-acknowledged Work
// item. Empty values fall back to the matching MANGO_* environment variables,
// allowing a launcher to pass only this narrow identity into a sandbox.
type EnvironmentWorkerHandleItemOptions struct {
	WorkID        string
	EnvironmentID string
	SessionID     string
	WorkSecret    string
}

// EnvironmentWorker composes WorkPoller and SessionToolRunner. It owns Work
// heartbeat and final Stop, but it does not create a sandbox, download inputs,
// or own registered tools. Provider launchers remain separate from this loop.
type EnvironmentWorker struct {
	client *Client
	opts   EnvironmentWorkerOptions
	logger *slog.Logger

	initialLeaseTTL  time.Duration
	heartbeatFloor   time.Duration
	heartbeatCeiling time.Duration
	stopTimeout      time.Duration
	now              func() time.Time
	retryDelay       func(int) time.Duration
	sleep            func(context.Context, time.Duration)
}

// NewEnvironmentWorker returns a worker bound to a trusted supervisor client.
// Configuration errors are returned by Run or HandleItem before any request.
func NewEnvironmentWorker(client *Client, opts EnvironmentWorkerOptions) *EnvironmentWorker {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &EnvironmentWorker{
		client: client, opts: opts, logger: logger.With("component", "environment-worker"),
		initialLeaseTTL: defaultEnvironmentLeaseTTL, heartbeatFloor: defaultEnvironmentHeartbeatFloor,
		heartbeatCeiling: defaultEnvironmentHeartbeatCeiling, stopTimeout: defaultEnvironmentStopTimeout,
		now: time.Now, retryDelay: workRetryDelay, sleep: sleepWithContext,
	}
}

// Run polls and services one acknowledged Session at a time until cancellation,
// drain completion, or a permanent Poll/Ack error. An individual item failure
// is logged and does not terminate the Environment queue consumer.
func (w *EnvironmentWorker) Run(ctx context.Context) error {
	if err := w.validate(true); err != nil {
		return fmt.Errorf("mango: EnvironmentWorker.Run: %w", err)
	}
	poller := NewWorkPoller(ctx, w.client, WorkPollerOptions{
		EnvironmentID: w.opts.EnvironmentID, WorkerID: w.opts.WorkerID,
		Drain: w.opts.Drain, BlockMs: w.opts.BlockMs,
		ReclaimOlderThanMs: w.opts.ReclaimOlderThanMs,
	})
	defer poller.Close()
	for poller.Next() {
		work := poller.Current()
		if work == nil {
			continue
		}
		if err := w.handleWork(ctx, *work); err != nil && ctx.Err() == nil {
			w.logger.Warn("Environment Work item failed", "work_id", work.ID, "session_id", work.Data.ID, "error", err)
		}
	}
	if err := poller.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// HandleItem services one already-acknowledged Work item. It is intended for a
// short-lived sandbox process launched after an external supervisor Polls and
// Acks. A valid Work secret is mandatory; Mango never falls back to the parent
// Client credential for item execution.
func (w *EnvironmentWorker) HandleItem(ctx context.Context, opts EnvironmentWorkerHandleItemOptions) error {
	if err := w.validate(false); err != nil {
		return fmt.Errorf("mango: EnvironmentWorker.HandleItem: %w", err)
	}
	workID := firstNonEmpty(opts.WorkID, os.Getenv("MANGO_WORK_ID"))
	environmentID := firstNonEmpty(opts.EnvironmentID, w.opts.EnvironmentID, os.Getenv("MANGO_ENVIRONMENT_ID"))
	sessionID := firstNonEmpty(opts.SessionID, os.Getenv("MANGO_SESSION_ID"))
	secret := firstNonEmpty(opts.WorkSecret, os.Getenv("MANGO_WORK_SECRET"))
	for _, required := range []struct{ name, value string }{
		{"work ID", workID}, {"environment ID", environmentID},
		{"session ID", sessionID}, {"Work secret", secret},
	} {
		if required.value == "" {
			return fmt.Errorf("mango: EnvironmentWorker.HandleItem: %s is required", required.name)
		}
	}
	return w.handleWork(ctx, EnvironmentWork{
		ID: workID, EnvironmentID: environmentID,
		Data:  EnvironmentWorkData{Type: "session", ID: sessionID},
		State: EnvironmentWorkStateStarting, Secret: &secret,
	})
}

func (w *EnvironmentWorker) validate(requireEnvironment bool) error {
	switch {
	case w.client == nil:
		return errors.New("client is required")
	case requireEnvironment && w.opts.EnvironmentID == "":
		return errors.New("EnvironmentID is required")
	case optionOutside(w.opts.BlockMs, 1, 999):
		return errors.New("BlockMs must be from 1 through 999")
	case optionBelow(w.opts.ReclaimOlderThanMs, 0):
		return errors.New("ReclaimOlderThanMs must be non-negative")
	case optionOutside(w.opts.DesiredTTLSeconds, 1, 300):
		return errors.New("DesiredTTLSeconds must be from 1 through 300")
	case w.opts.ToolTimeout < 0:
		return errors.New("ToolTimeout must be non-negative")
	case w.opts.SendTimeout < 0:
		return errors.New("SendTimeout must be non-negative")
	}
	return nil
}

func (w *EnvironmentWorker) handleWork(ctx context.Context, work EnvironmentWork) error {
	if work.ID == "" || work.EnvironmentID == "" || work.Data.Type != "session" || work.Data.ID == "" {
		return errors.New("mango: Environment Work has an invalid identity")
	}
	if work.State != EnvironmentWorkStateStarting {
		return fmt.Errorf("mango: Environment Work must be acknowledged before handling; state is %q", work.State)
	}
	if work.Secret == nil {
		return errors.New("mango: Environment Work has no scoped secret")
	}
	token, err := sessionTokenFromWorkSecret(*work.Secret)
	if err != nil {
		return fmt.Errorf("mango: Environment Work scoped secret is invalid: %w", err)
	}
	itemClient := w.client.withAPIKey(token)
	log := w.logger.With("work_id", work.ID, "session_id", work.Data.ID)

	sessionCtx, cancelSession := context.WithCancel(ctx)
	start := make(chan environmentHeartbeatStart, 1)
	heartbeatDone := make(chan environmentHeartbeatEnd, 1)
	go func() {
		end := w.runHeartbeat(sessionCtx, itemClient, work, start, log)
		heartbeatDone <- end
		cancelSession()
	}()

	startup := <-start
	var runnerErr error
	if startup.ready {
		runner := NewSessionToolRunner(sessionCtx, itemClient, work.Data.ID, SessionToolRunnerOptions{
			Tools: w.opts.Tools, MaxIdle: w.opts.MaxIdle,
			ToolTimeout: w.opts.ToolTimeout, SendTimeout: w.opts.SendTimeout,
			SendRetryWindow: startup.ttl, Logger: log,
		})
		for runner.Next() {
			call := runner.Current()
			log.Info("dispatched tool", "tool", call.Name, "tool_use_id", call.ToolUseID,
				"is_error", call.IsError, "posted", call.Posted)
		}
		runnerErr = runner.Err()
		if err := runner.Close(); err != nil && runnerErr == nil {
			runnerErr = err
		}
	} else {
		runnerErr = startup.end.err
	}

	cancelSession()
	heartbeatEnd := <-heartbeatDone
	leaseLost := heartbeatEnd.lost() || errors.Is(runnerErr, ErrSessionLeaseLost)
	if leaseLost {
		if runnerErr != nil && !errors.Is(runnerErr, ErrSessionLeaseLost) {
			log.Warn("session runner ended while Work lease was lost", "error", runnerErr)
		}
		cause := heartbeatEnd.err
		if errors.Is(runnerErr, ErrSessionLeaseLost) {
			cause = runnerErr
		}
		if cause != nil {
			return fmt.Errorf("%w: work %s: %v", ErrEnvironmentWorkLeaseLost, work.ID, cause)
		}
		return fmt.Errorf("%w: work %s", ErrEnvironmentWorkLeaseLost, work.ID)
	}

	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), w.stopTimeout)
	stopErr := itemClient.StopEnvironmentWork(stopCtx, work.EnvironmentID, work.ID, EnvironmentWorkStopRequest{Force: Some(true)})
	cancelStop()
	if isAPIStatus(stopErr, http.StatusConflict) {
		stopErr = nil
	}
	if isLeaseLossStatus(stopErr) {
		return fmt.Errorf("%w while stopping work %s: %v", ErrEnvironmentWorkLeaseLost, work.ID, stopErr)
	}
	if stopErr != nil {
		log.Warn("force Stop failed", "error", stopErr)
	}

	if ctx.Err() != nil {
		return nil
	}
	if runnerErr != nil && !errors.Is(runnerErr, ErrSessionTerminated) && !errors.Is(runnerErr, ErrIdleTimeout) {
		return runnerErr
	}
	if heartbeatEnd.reason == environmentHeartbeatRejected && heartbeatEnd.err != nil {
		return heartbeatEnd.err
	}
	return stopErr
}

type environmentHeartbeatReason int

const (
	environmentHeartbeatRunnerDone environmentHeartbeatReason = iota
	environmentHeartbeatControlStop
	environmentHeartbeatLeaseLost
	environmentHeartbeatAssumedLost
	environmentHeartbeatRejected
)

type environmentHeartbeatStart struct {
	ready bool
	ttl   time.Duration
	end   environmentHeartbeatEnd
}

type environmentHeartbeatEnd struct {
	reason environmentHeartbeatReason
	err    error
}

func (e environmentHeartbeatEnd) lost() bool {
	return e.reason == environmentHeartbeatLeaseLost || e.reason == environmentHeartbeatAssumedLost
}

func (w *EnvironmentWorker) runHeartbeat(
	ctx context.Context,
	client *Client,
	work EnvironmentWork,
	start chan<- environmentHeartbeatStart,
	log *slog.Logger,
) (end environmentHeartbeatEnd) {
	ready := false
	defer func() {
		if !ready {
			start <- environmentHeartbeatStart{end: end}
		}
	}()

	ttl := w.initialLeaseTTL
	lastSuccess := w.now()
	lastHeartbeat := noEnvironmentHeartbeat
	failures := 0
	for {
		remaining := ttl - w.now().Sub(lastSuccess)
		if remaining <= 0 {
			return environmentHeartbeatEnd{reason: environmentHeartbeatAssumedLost,
				err: fmt.Errorf("%w: heartbeat stale for %s", ErrEnvironmentWorkLeaseLost, ttl)}
		}
		requestTimeout := min(w.heartbeatInterval(ttl), remaining)
		beatCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		response, err := client.HeartbeatEnvironmentWork(beatCtx, work.EnvironmentID, work.ID, HeartbeatEnvironmentWorkParams{
			ExpectedLastHeartbeat: Some(lastHeartbeat), DesiredTTLSeconds: w.opts.DesiredTTLSeconds,
		})
		cancel()
		if err != nil {
			if isLeaseLossStatus(err) {
				return environmentHeartbeatEnd{reason: environmentHeartbeatLeaseLost, err: err}
			}
			if ctx.Err() != nil {
				return environmentHeartbeatEnd{reason: environmentHeartbeatRunnerDone}
			}
			if !retryableWorkError(err) {
				return environmentHeartbeatEnd{reason: environmentHeartbeatRejected,
					err: fmt.Errorf("mango: heartbeat Environment Work %s: %w", work.ID, err)}
			}
			failures++
			remaining = ttl - w.now().Sub(lastSuccess)
			if remaining <= 0 {
				return environmentHeartbeatEnd{reason: environmentHeartbeatAssumedLost,
					err: fmt.Errorf("%w: heartbeat stale for %s: %v", ErrEnvironmentWorkLeaseLost, ttl, err)}
			}
			delay := min(w.retryDelay(failures), remaining)
			log.Warn("transient Work heartbeat failure", "attempt", failures, "delay", delay, "error", err)
			w.sleep(ctx, delay)
			continue
		}

		if err := validateEnvironmentHeartbeat(response); err != nil {
			return environmentHeartbeatEnd{reason: environmentHeartbeatRejected, err: err}
		}
		failures = 0
		lastHeartbeat = response.LastHeartbeat
		lastSuccess = w.now()
		ttl = time.Duration(response.TTLSeconds) * time.Second
		if response.State == EnvironmentWorkStateStopping || response.State == EnvironmentWorkStateStopped || !response.LeaseExtended {
			return environmentHeartbeatEnd{reason: environmentHeartbeatControlStop}
		}
		if !ready {
			ready = true
			start <- environmentHeartbeatStart{ready: true, ttl: ttl}
		}
		w.sleep(ctx, w.heartbeatInterval(ttl))
		if ctx.Err() != nil {
			return environmentHeartbeatEnd{reason: environmentHeartbeatRunnerDone}
		}
	}
}

func (w *EnvironmentWorker) heartbeatInterval(ttl time.Duration) time.Duration {
	return max(w.heartbeatFloor, min(ttl/2, w.heartbeatCeiling))
}

func validateEnvironmentHeartbeat(heartbeat EnvironmentWorkHeartbeat) error {
	if heartbeat.Type != "work_heartbeat" || heartbeat.LastHeartbeat == "" ||
		heartbeat.TTLSeconds < 1 || heartbeat.TTLSeconds > 300 {
		return errors.New("mango: Environment Work heartbeat returned an invalid response")
	}
	switch heartbeat.State {
	case EnvironmentWorkStateActive, EnvironmentWorkStateStopping, EnvironmentWorkStateStopped:
		return nil
	default:
		return fmt.Errorf("mango: Environment Work heartbeat returned state %q", heartbeat.State)
	}
}

func sessionTokenFromWorkSecret(secret string) (string, error) {
	if secret == "" {
		return "", errors.New("payload is empty")
	}
	if len(secret) > maxEnvironmentWorkSecretBytes {
		return "", errors.New("payload is too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return "", errors.New("payload is not base64url")
	}
	var payload struct {
		SessionsToken string `json:"sessions_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errors.New("payload is not JSON")
	}
	if payload.SessionsToken == "" {
		return "", errors.New("sessions_token is missing or invalid")
	}
	for index := range len(payload.SessionsToken) {
		if payload.SessionsToken[index] <= 0x20 || payload.SessionsToken[index] >= 0x7f {
			return "", errors.New("sessions_token is missing or invalid")
		}
	}
	return payload.SessionsToken, nil
}

func (c *Client) withAPIKey(apiKey string) *Client {
	clone := *c
	clone.apiKey = apiKey
	return &clone
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isAPIStatus(err error, status int) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == status
}

func isLeaseLossStatus(err error) bool {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusGone, http.StatusPreconditionFailed,
	} {
		if isAPIStatus(err, status) {
			return true
		}
	}
	return false
}
