package kubernetes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func dependencyProbeOutput(cmd *exec.Cmd) ([]byte, error) {
	// Compose plugins can retain their parent's pipes after it is cancelled.
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd.Output()
}

func dependencyStateSummary(raw []byte) (string, error) {
	var state struct {
		Running, OOMKilled bool
		ExitCode           int
		Health             *struct{ Status string }
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return "", err
	}
	health := "none"
	if state.Health != nil {
		health = "unknown"
		switch state.Health.Status {
		case "healthy", "unhealthy", "starting":
			health = state.Health.Status
		}
	}
	return fmt.Sprintf("running=%t oom_killed=%t exit_code=%d health=%s", state.Running, state.OOMKilled, state.ExitCode, health), nil
}

func (s *stateFixture) logDependencyStates() {
	s.cluster.t.Helper()
	// Bound diagnostics as a whole and keep command output private. A failed
	// probe must never prevent network/volume teardown or hide the first error.
	// Reserve time for the bounded pipe drain inside the ten-second budget.
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	probe := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = s.cluster.root
		cmd.Env = append(os.Environ(), "MANGO_KUBERNETES_MODEL_IMAGE="+s.cluster.model)
		return dependencyProbeOutput(cmd)
	}
	for _, service := range []string{"postgres", "temporal", "nats", "seaweedfs", "model"} {
		ids, err := probe("compose", "--project-name", s.project, "-f", "scripts/kubernetes/fixtures/compose.yaml", "ps", "--all", "--quiet", service)
		selected := strings.Fields(string(ids))
		if err == nil && len(selected) == 1 {
			raw, inspectErr := probe("inspect", "--format", "{{json .State}}", selected[0])
			if inspectErr == nil {
				if summary, summaryErr := dependencyStateSummary(raw); summaryErr == nil {
					s.cluster.t.Logf("fixture dependency state project=%s service=%s %s", s.project, service, summary)
					continue
				}
			}
		}
		s.cluster.t.Logf("fixture dependency state project=%s service=%s unavailable", s.project, service)
	}
}

func TestDependencyProbeBoundsInheritedOutputPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	// The Compose CLI plugin also inherits its parent's output descriptors.
	// Wait for a real child to hold the pipe before cancelling the parent.
	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 5 & echo $! > "$1"; wait`, "fixture", pidFile)
	done := make(chan error, 1)
	go func() {
		_, err := dependencyProbeOutput(cmd)
		done <- err
	}()
	var pid []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		pid, err = os.ReadFile(pidFile)
		if err == nil && bytes.HasSuffix(pid, []byte("\n")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, bytes.HasSuffix(pid, []byte("\n")), "child PID was not fully published")
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pid)))
	require.NoError(t, err)
	require.Greater(t, childPID, 1)
	child, err := os.FindProcess(childPID)
	require.NoError(t, err)
	defer func() { _ = child.Kill() }()
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic command waited for the inherited output pipe after cancellation")
	}
}

func TestDependencyDiagnosticsKeepOnlyLifecycleFacts(t *testing.T) {
	// Auth material and healthcheck output must remain private even when Docker
	// returns them alongside the lifecycle state needed to diagnose a failure.
	raw := []byte(`{"Running":false,"OOMKilled":true,"ExitCode":137,
		"Error":"private-engine-detail","Health":{"Status":"unhealthy",
		"Log":[{"Output":"private-healthcheck-output"}]},"future":"private-future-field"}`)
	summary, err := dependencyStateSummary(raw)
	require.NoError(t, err)
	require.Equal(t, "running=false oom_killed=true exit_code=137 health=unhealthy", summary)
	require.NotContains(t, summary, "private-")

	summary, err = dependencyStateSummary([]byte(`{"Running":true,"OOMKilled":false,"ExitCode":0}`))
	require.NoError(t, err)
	require.Equal(t, "running=true oom_killed=false exit_code=0 health=none", summary)

	summary, err = dependencyStateSummary([]byte(`{"Running":true,"Health":{"Status":"private-unexpected-status"}}`))
	require.NoError(t, err)
	require.Equal(t, "running=true oom_killed=false exit_code=0 health=unknown", summary)

	_, err = dependencyStateSummary([]byte(`not-json`))
	require.Error(t, err)
}
