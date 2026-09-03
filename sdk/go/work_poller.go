package mango

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	mrand "math/rand/v2"
	"os"
	"time"
)

const (
	defaultWorkPollBlockMs = int64(999)
	workPollRetryCap       = 5 * time.Second
)

// WorkPollerOptions configure a provider-neutral self-hosted Environment Work
// consumer. The configured Client credential authorizes every Work request.
type WorkPollerOptions struct {
	// EnvironmentID identifies the self-hosted Environment queue. Required.
	EnvironmentID string
	// WorkerID is an operational correlation identifier. An opaque, process-
	// unique value is generated when omitted; it is not an authorization token.
	WorkerID string
	// Drain ends iteration normally when Poll observes an empty queue. A drain
	// poll is non-blocking unless BlockMs is set explicitly.
	Drain bool
	// BlockMs overrides the server-side long-poll duration. It must be between
	// 1 and 999. Long-running pollers default to 999; drain pollers omit it.
	BlockMs Optional[int64]
	// ReclaimOlderThanMs asks the queue to reclaim stale, unacknowledged claims.
	ReclaimOlderThanMs Optional[int64]
}

// WorkPoller polls and acknowledges self-hosted Environment Work. It is a
// control-plane helper: it does not create a sandbox or execute tools.
//
// WorkPoller is not safe for concurrent use. It deliberately does not Stop a
// yielded item: the per-session runner that heartbeats the lease owns Stop.
type WorkPoller struct {
	ctx    context.Context
	client *Client
	opts   WorkPollerOptions

	current  *EnvironmentWork
	err      error
	closed   bool
	failures int

	sleep func(context.Context, time.Duration)
}

// NewWorkPoller creates a pull-style Work iterator. Configuration failures are
// reported by Err after the first Next call; the returned value is always safe
// to Close.
func NewWorkPoller(ctx context.Context, client *Client, opts WorkPollerOptions) *WorkPoller {
	poller := &WorkPoller{ctx: ctx, client: client, opts: opts, sleep: sleepWithContext}
	switch {
	case client == nil:
		poller.err = errors.New("mango: WorkPoller client is required")
	case opts.EnvironmentID == "":
		poller.err = errors.New("mango: WorkPollerOptions.EnvironmentID is required")
	case optionOutside(opts.BlockMs, 1, 999):
		poller.err = errors.New("mango: WorkPollerOptions.BlockMs must be from 1 through 999")
	case optionBelow(opts.ReclaimOlderThanMs, 0):
		poller.err = errors.New("mango: WorkPollerOptions.ReclaimOlderThanMs must be non-negative")
	}
	if poller.opts.WorkerID == "" {
		poller.opts.WorkerID = defaultWorkPollerID()
	}
	return poller
}

// Next polls until a new item is acknowledged. It returns false on drain
// completion, cancellation, Close, or a permanent error.
func (p *WorkPoller) Next() bool {
	p.current = nil
	if p.err != nil || p.closed {
		return false
	}

	params := PollEnvironmentWorkParams{WorkerID: Some(p.opts.WorkerID)}
	if block, ok := p.opts.BlockMs.Get(); ok {
		params.BlockMs = Some(block)
	} else if !p.opts.Drain {
		params.BlockMs = Some(defaultWorkPollBlockMs)
	}
	if reclaim, ok := p.opts.ReclaimOlderThanMs.Get(); ok {
		params.ReclaimOlderThanMs = Some(reclaim)
	}

	for {
		if p.ctx.Err() != nil {
			return false
		}
		response, err := p.client.PollEnvironmentWork(p.ctx, p.opts.EnvironmentID, params)
		if err != nil {
			if p.ctx.Err() != nil {
				return false
			}
			if !retryableWorkError(err) {
				p.err = fmt.Errorf("mango: poll Environment Work: %w", err)
				return false
			}
			p.failures++
			p.sleep(p.ctx, workRetryDelay(p.failures))
			continue
		}
		p.failures = 0
		work := response.EnvironmentWork
		if work == nil {
			if !isEmptyWorkPollResponse(response) {
				p.err = errors.New("mango: poll Environment Work returned an invalid response object")
				return false
			}
			if p.opts.Drain {
				return false
			}
			p.sleep(p.ctx, time.Second)
			continue
		}
		if err := validatePolledWork(*work, p.opts.EnvironmentID); err != nil {
			p.err = err
			return false
		}
		if p.ctx.Err() != nil {
			// Poll only tentatively claimed the item. Do not Ack after local
			// cancellation; the queue can reclaim the tentative claim.
			return false
		}

		acknowledged, err := p.client.AcknowledgeEnvironmentWork(
			p.ctx, p.opts.EnvironmentID, work.ID,
		)
		if err != nil {
			// Ack may have committed even when its response was lost. Stop is a
			// terminal state transition, not a safe claim release in Mango, so do
			// not issue it here. The starting-item TTL makes either outcome
			// reclaimable without terminating this activation or a newer owner.
			p.err = fmt.Errorf("mango: acknowledge Environment Work %s: %w", work.ID, err)
			return false
		}
		if err := validateAcknowledgedWork(acknowledged, *work); err != nil {
			// Ack already committed. Leave the item for TTL reclaim rather than
			// using unfenced Stop against an identity we cannot trust.
			p.err = err
			return false
		}
		// The server exposes the credential payload only on Poll. Preserve it
		// locally after validating the redacted Ack response.
		acknowledged.Secret = work.Secret
		p.current = &acknowledged
		return true
	}
}

// Current returns the most recently acknowledged Work item. It is valid after
// Next returns true and until the next Next call.
func (p *WorkPoller) Current() *EnvironmentWork { return p.current }

// Err returns the first permanent iteration error. Context cancellation and an
// empty drained queue are normal termination.
func (p *WorkPoller) Err() error { return p.err }

// Close ends iteration. It is idempotent and never changes Work state.
func (p *WorkPoller) Close() error {
	if p.closed {
		return p.err
	}
	p.closed = true
	return p.err
}

// All adapts the poller to Go's range-over-function iterator form.
func (p *WorkPoller) All() iter.Seq2[*EnvironmentWork, error] {
	return func(yield func(*EnvironmentWork, error) bool) {
		for p.Next() {
			if !yield(p.Current(), nil) {
				_ = p.Close()
				return
			}
		}
		if err := p.Err(); err != nil {
			yield(nil, err)
		}
	}
}

func retryableWorkError(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return true
	}
	return apiError.StatusCode == 409 || apiError.StatusCode == 429 || apiError.StatusCode >= 500
}

func workRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	ceiling := time.Duration(1<<(min(failures, 5)-1)) * 250 * time.Millisecond
	if ceiling > workPollRetryCap {
		ceiling = workPollRetryCap
	}
	floor := ceiling / 2
	if ceiling <= floor {
		return ceiling
	}
	return floor + time.Duration(mrand.Int64N(int64(ceiling-floor)))
}

func sleepWithContext(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func defaultWorkPollerID() string {
	var random [8]byte
	_, _ = crand.Read(random[:])
	suffix := hex.EncodeToString(random[:])
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "worker-" + suffix
	}
	return host + "-" + suffix
}

func optionOutside(value Optional[int64], low, high int64) bool {
	configured, ok := value.Get()
	return ok && (configured < low || configured > high)
}

func optionBelow(value Optional[int64], low int64) bool {
	configured, ok := value.Get()
	return ok && configured < low
}

func isEmptyWorkPollResponse(response PollEnvironmentWorkResponse) bool {
	return response.Object != nil && len(*response.Object) == 0 && len(response.Raw) == 0
}

func validatePolledWork(work EnvironmentWork, environmentID string) error {
	switch {
	case work.ID == "":
		return errors.New("mango: poll Environment Work returned an empty work ID")
	case work.EnvironmentID != environmentID:
		return fmt.Errorf(
			"mango: poll Environment Work returned environment %q for queue %q",
			work.EnvironmentID, environmentID,
		)
	case work.Data.Type != "session" || work.Data.ID == "":
		return fmt.Errorf(
			"mango: poll Environment Work returned invalid data identity %q/%q",
			work.Data.Type, work.Data.ID,
		)
	case work.State != EnvironmentWorkStateQueued:
		return fmt.Errorf("mango: poll Environment Work returned state %q", work.State)
	case work.Secret == nil || *work.Secret == "":
		return errors.New("mango: poll Environment Work returned no claim secret")
	default:
		return nil
	}
}

func validateAcknowledgedWork(acknowledged, polled EnvironmentWork) error {
	if acknowledged.ID != polled.ID ||
		acknowledged.EnvironmentID != polled.EnvironmentID ||
		acknowledged.Data != polled.Data {
		return fmt.Errorf(
			"mango: acknowledge Environment Work returned mismatched identity %q/%q",
			acknowledged.EnvironmentID, acknowledged.ID,
		)
	}
	if acknowledged.State != EnvironmentWorkStateStarting {
		return fmt.Errorf(
			"mango: acknowledge Environment Work returned state %q",
			acknowledged.State,
		)
	}
	if acknowledged.Secret != nil {
		return errors.New("mango: acknowledge Environment Work disclosed a credential payload")
	}
	return nil
}

var _ io.Closer = (*WorkPoller)(nil)
