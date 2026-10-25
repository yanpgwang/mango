package kubernetes_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/domain"
)

const workDiagnosticBytes = 64 << 10

var diagnosticID = regexp.MustCompile(`^(?:sevt|work|env|sesn)_[a-zA-Z0-9]{1,64}$`)

func diagnosticEnum(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "unknown"
}

func diagnosticPublicID(value string) string {
	if diagnosticID.MatchString(value) {
		return value
	}
	return "unknown"
}

func diagnosticEventPage(raw []byte) (string, error) {
	var page struct {
		Data []struct {
			ID              string                `json:"id"`
			Type            string                `json:"type"`
			ProcessedAt     json.RawMessage       `json:"processed_at"`
			ToolUseID       string                `json:"tool_use_id"`
			CustomToolUseID string                `json:"custom_tool_use_id"`
			Result          string                `json:"result"`
			StopReason      struct{ Type string } `json:"stop_reason"`
		} `json:"data"`
		NextPage *string `json:"next_page"`
	}
	if len(raw) > workDiagnosticBytes || json.Unmarshal(raw, &page) != nil {
		return "", errors.New("invalid or oversized diagnostic event page")
	}
	var result strings.Builder
	// The request selects the latest thirty events in descending order. Show
	// them chronologically, without copying any content, input, or error body.
	for index := min(len(page.Data), 30) - 1; index >= 0; index-- {
		event := page.Data[index]
		kind := diagnosticEnum(event.Type,
			domain.EvUserMessage, domain.EvUserInterrupt, domain.EvUserToolConfirmation,
			domain.EvUserToolResult, domain.EvUserCustomToolResult, domain.EvSystemMessage,
			domain.EvAgentMessage, domain.EvAgentThinking, domain.EvAgentToolUse,
			domain.EvAgentToolResult, domain.EvAgentCustomToolUse, domain.EvAgentMcpToolUse,
			domain.EvAgentMcpToolResult, domain.EvSessionStatusIdle, domain.EvSessionStatusRunning,
			domain.EvSessionStatusTerminated, domain.EvSessionStatusRescheduling,
			domain.EvSessionError, domain.EvSessionUsage, domain.EvSessionUpdated,
			domain.EvSpanModelRequestStart, domain.EvSpanModelRequestEnd)
		fmt.Fprintf(&result, "id=%s type=%s processed=%t", diagnosticPublicID(event.ID), kind,
			len(event.ProcessedAt) != 0 && string(event.ProcessedAt) != "null")
		if kind == domain.EvUserToolConfirmation || kind == domain.EvUserToolResult {
			fmt.Fprintf(&result, " tool_use_id=%s", diagnosticPublicID(event.ToolUseID))
		}
		if kind == domain.EvUserCustomToolResult {
			fmt.Fprintf(&result, " custom_tool_use_id=%s", diagnosticPublicID(event.CustomToolUseID))
		}
		if kind == domain.EvUserToolConfirmation {
			fmt.Fprintf(&result, " result=%s", diagnosticEnum(event.Result, "allow", "deny"))
		}
		if kind == domain.EvSessionStatusIdle {
			fmt.Fprintf(&result, " stop_reason=%s", diagnosticEnum(event.StopReason.Type,
				"end_turn", "requires_action", "max_tokens", "refusal", "content_capacity"))
		}
		result.WriteByte('\n')
	}
	fmt.Fprintf(&result, "has_more=%t", page.NextPage != nil || len(page.Data) > 30)
	return result.String(), nil
}

func diagnosticWorkPage(raw []byte, environment, session string) (string, error) {
	var page struct {
		Data []struct {
			ID            string                    `json:"id"`
			EnvironmentID string                    `json:"environment_id"`
			State         string                    `json:"state"`
			Data          struct{ Type, ID string } `json:"data"`
			Heartbeat     *time.Time                `json:"latest_heartbeat_at"`
		} `json:"data"`
		NextPage *string `json:"next_page"`
	}
	if len(raw) > workDiagnosticBytes || json.Unmarshal(raw, &page) != nil {
		return "", errors.New("invalid or oversized diagnostic Work page")
	}
	var result strings.Builder
	for _, work := range page.Data[:min(len(page.Data), 30)] {
		if work.EnvironmentID != environment || work.Data.Type != "session" || work.Data.ID != session {
			continue
		}
		heartbeat := "none"
		if work.Heartbeat != nil {
			heartbeat = work.Heartbeat.UTC().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(&result, "id=%s state=%s latest_heartbeat_at=%s\n", diagnosticPublicID(work.ID),
			diagnosticEnum(work.State, "queued", "starting", "active", "stopping", "stopped"), heartbeat)
	}
	fmt.Fprintf(&result, "has_more=%t", page.NextPage != nil || len(page.Data) > 30)
	return result.String(), nil
}

// Container and supervisor errors may embed HTTP bodies, credentials, or tool
// arguments. Recognize fixed lifecycle messages; discard ALL dynamic fields.
// Even a forged log line can therefore emit only these public constant words.
func diagnosticLifecycleLogs(raw []byte) string {
	if len(raw) > workDiagnosticBytes {
		raw = raw[len(raw)-workDiagnosticBytes:]
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.SplitN(line, " ", 4)
		if len(fields) != 4 {
			continue
		}
		if _, err := time.Parse("2006/01/02 15:04:05", fields[0]+" "+fields[1]); err != nil {
			continue
		}
		level := diagnosticEnum(fields[2], "DEBUG", "INFO", "WARN", "ERROR")
		if level == "unknown" {
			continue
		}
		for _, message := range []string{
			"started Work container", "Work container failed", "mango worker stopped", "dispatched tool",
			"transient Session input preparation failure", "transient Work heartbeat failure",
			"event stream connection failed; retrying", "event history reconciliation failed; retrying",
			"event stream read failed; reconnecting", "event stream closed; reconnecting",
			"MCP output preparation failed; reconnecting", "tool result send failed; retrying",
			"tool is not registered; leaving result pending", "session runner ended while Work lease was lost",
			"force Stop failed", "Session Memory Store flush reached its timeout",
			"final Session Memory Store sync reached its timeout",
		} {
			if fields[3] == message || strings.HasPrefix(fields[3], message+" ") {
				lines = append(lines, fields[0]+" "+fields[1]+" "+level+" "+message)
				break
			}
		}
	}
	if len(lines) == 0 {
		return "no allowlisted lifecycle messages"
	}
	return strings.Join(lines[max(0, len(lines)-20):], "\n")
}

// Do not let a log frame's untrusted length allocate an unbounded buffer.
func diagnosticDockerLogPayload(raw []byte) []byte {
	var payload []byte
	for len(raw) >= 8 {
		size := uint64(binary.BigEndian.Uint32(raw[4:8]))
		if (raw[0] != 1 && raw[0] != 2) || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 || size > uint64(len(raw)-8) {
			break
		}
		payload = append(payload, raw[8:8+int(size)]...)
		raw = raw[8+int(size):]
	}
	return payload
}

func collectOwnedWorkDiagnostics(ctx context.Context, engine *client.Client, environment, session, image string, emit func(string)) {
	if environment == "" || session == "" || image == "" {
		emit("item diagnostics unavailable: missing fixture identity")
		return
	}
	filters := make(client.Filters).Add("label", "io.mango.self-hosted-worker=true", "io.mango.environment-id="+environment, "io.mango.session-id="+session)
	listed, err := engine.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		emit("item diagnostics unavailable: container list failed")
		return
	}
	found := false
	for _, item := range listed.Items[:min(len(listed.Items), 8)] {
		inspected, err := engine.ContainerInspect(ctx, item.ID, client.ContainerInspectOptions{})
		if err != nil || inspected.Container.Config == nil || inspected.Container.State == nil {
			continue
		}
		config, state := inspected.Container.Config, inspected.Container.State
		labels := config.Labels
		if config.Image != image || labels["io.mango.self-hosted-worker"] != "true" ||
			labels["io.mango.environment-id"] != environment || labels["io.mango.session-id"] != session || labels["io.mango.work-id"] == "" {
			continue
		}
		found = true
		emit(fmt.Sprintf("item work_id=%s running=%t paused=%t oom_killed=%t exit_code=%d",
			diagnosticPublicID(labels["io.mango.work-id"]), state.Running, state.Paused, state.OOMKilled, state.ExitCode))
		logs, err := engine.ContainerLogs(ctx, item.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "80"})
		if err != nil {
			emit("item lifecycle logs unavailable")
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(logs, workDiagnosticBytes))
		_ = logs.Close()
		if err == nil {
			if !config.Tty {
				raw = diagnosticDockerLogPayload(raw)
			}
			emit("item lifecycle logs (bounded; dynamic fields omitted):\n" + diagnosticLifecycleLogs(raw))
		} else {
			emit("item lifecycle logs unavailable")
		}
	}
	if !found {
		emit("no exact-owned Work container found")
	}
}

func (s *stateFixture) logWorkFailure(environment, session, supervisorLog string) {
	// Run BEFORE supervisor cancellation removes the item. Keep all reads under
	// one bound; a failed probe reports no raw error/body and cannot stop cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	emit := func(text string) { s.cluster.t.Log("fixture Work diagnostic:", text) }
	for _, probe := range []struct {
		path      string
		summarize func([]byte) (string, error)
	}{
		{"/v1/sessions/" + session + "/events?limit=30&order=desc", diagnosticEventPage},
		{"/v1/environments/" + environment + "/work?limit=30", func(raw []byte) (string, error) { return diagnosticWorkPage(raw, environment, session) }},
	} {
		request, err := http.NewRequestWithContext(ctx, "GET", s.baseURL+probe.path, nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer fixture-workspace-only")
			response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(request)
			if requestErr == nil {
				raw, readErr := io.ReadAll(io.LimitReader(response.Body, workDiagnosticBytes+1))
				_ = response.Body.Close()
				if readErr == nil && response.StatusCode == http.StatusOK {
					if summary, summaryErr := probe.summarize(raw); summaryErr == nil {
						emit(summary)
						continue
					}
				}
			}
		}
		emit("public lifecycle metadata unavailable")
	}
	engine, err := client.New(client.FromEnv)
	if err == nil {
		defer func() { _ = engine.Close() }()
		_, err = engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
		if err == nil {
			collectOwnedWorkDiagnostics(ctx, engine, environment, session, s.cluster.workerImage, emit)
		}
	}
	if err != nil {
		emit("Docker lifecycle metadata unavailable")
	}
	if file, err := os.Open(supervisorLog); err == nil {
		defer func() { _ = file.Close() }()
		if info, statErr := file.Stat(); statErr == nil {
			_, _ = file.Seek(max(0, info.Size()-workDiagnosticBytes), io.SeekStart)
		}
		raw, _ := io.ReadAll(io.LimitReader(file, workDiagnosticBytes))
		emit("supervisor lifecycle logs (bounded; dynamic fields omitted):\n" + diagnosticLifecycleLogs(raw))
	}
}
