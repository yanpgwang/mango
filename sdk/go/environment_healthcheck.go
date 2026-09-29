package mango

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HealthcheckExecutionTimeout bounds the fixed provider execution probe.
const HealthcheckExecutionTimeout = 10 * time.Second

func (w *EnvironmentWorker) handleHealthcheck(ctx context.Context, client *Client, work EnvironmentWork) error {
	beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	beat, err := client.Environments.Work.Heartbeat(beatCtx, work.EnvironmentID, work.ID, HeartbeatEnvironmentWorkParams{
		ExpectedLastHeartbeat: Some(noEnvironmentHeartbeat), DesiredTTLSeconds: Some[int64](30),
	})
	cancel()
	if err != nil {
		return fmt.Errorf("mango: healthcheck heartbeat: %w", err)
	}
	if err := validateEnvironmentHeartbeat(beat); err != nil {
		return err
	}
	if !beat.LeaseExtended || beat.State != EnvironmentWorkStateActive {
		return ErrEnvironmentWorkLeaseLost
	}
	probeCtx, cancel := context.WithTimeout(ctx, HealthcheckExecutionTimeout)
	probeErr := errors.New("worker has no healthcheck executor")
	if w.opts.Healthcheck != nil {
		probeErr = w.opts.Healthcheck(probeCtx, w.opts.Workdir)
	}
	if probeCtx.Err() != nil {
		probeErr = probeCtx.Err()
	}
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	result := EnvironmentWorkResultRequest{Status: "succeeded", Message: "Sandbox process and workspace check passed"}
	if probeErr != nil {
		result.Status = "failed"
		message := []rune(strings.ToValidUTF8("Sandbox check failed: "+probeErr.Error(), "�"))
		result.Message = string(message[:min(len(message), 1024)])
	}
	// A transport failure may follow a committed result. Retry exactly the
	// same payload using the server's short terminal replay grant.
	resultCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		_, err = client.Environments.Work.Complete(resultCtx, work.EnvironmentID, work.ID, result)
		if err == nil {
			return nil
		}
		if resultCtx.Err() != nil || isAPIStatus(err, http.StatusConflict) || !retryableWorkError(err) {
			return err
		}
		w.sleep(resultCtx, w.retryDelay(attempt))
	}
}
