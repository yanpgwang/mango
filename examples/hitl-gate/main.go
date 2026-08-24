// Command hitl-gate runs the documented human-in-the-loop custom-tool example
// against a live Mango HTTP API. It intentionally uses net/http instead of an
// SDK so every public operation in the example is visible.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBaseURL  = "http://localhost:8080"
	defaultAPIKey   = "sk-mango-local-development"
	requestTimeout  = 30 * time.Second
	scenarioTimeout = 6 * time.Minute
)

type apiClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type resource struct {
	ID string `json:"id"`
}

type eventList struct {
	Data []event `json:"data"`
}

type event struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Input      map[string]any `json:"input"`
	Content    []contentBlock `json:"content"`
	StopReason *stopReason    `json:"stop_reason"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type stopReason struct {
	Type     string   `json:"type"`
	EventIDs []string `json:"event_ids"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "HITL example failed:", err)
		os.Exit(1)
	}
}

func run() error {
	modelID := strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_MODEL_ID"))
	if modelID == "" {
		return errors.New("MANGO_EXAMPLE_MODEL_ID is required; set it to the model configured on the Mango worker")
	}
	baseURL := strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	apiKey := strings.TrimSpace(os.Getenv("MANGO_API_KEY"))
	if apiKey == "" {
		apiKey = defaultAPIKey
	}
	keepResources := os.Getenv("MANGO_EXAMPLE_KEEP_RESOURCES") == "1"

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, scenarioTimeout)
	defer cancel()

	client := &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: requestTimeout},
	}
	if err := client.get(ctx, "/readyz", nil); err != nil {
		return fmt.Errorf("mango is not ready at %s: %w", client.baseURL, err)
	}

	fmt.Println("Creating Environment, Agent, and Session through the Mango HTTP API...")
	environment, err := client.create(ctx, "/v1/environments", map[string]any{
		"name":   "HITL expense example",
		"config": map[string]any{"type": "cloud"},
	})
	if err != nil {
		return fmt.Errorf("create Environment: %w", err)
	}

	agent, err := client.create(ctx, "/v1/agents", map[string]any{
		"name":   "HITL expense gate",
		"model":  modelID,
		"system": "You process expense receipts through application-owned tools. Follow the supplied policy and call exactly one tool per receipt. After tool results arrive, summarize the recorded outcomes without calling another tool.",
		"tools": []any{
			map[string]any{
				"type": "custom", "name": "decide",
				"description": "Record a final approve or reject decision for a clear expense.",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"receipt_id": map[string]any{"type": "string"},
						"action":     map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
						"reason":     map[string]any{"type": "string"},
					},
					"required": []string{"receipt_id", "action", "reason"},
				},
			},
			map[string]any{
				"type": "custom", "name": "escalate",
				"description": "Request a human decision for an ambiguous expense.",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"receipt_id": map[string]any{"type": "string"},
						"question":   map[string]any{"type": "string"},
					},
					"required": []string{"receipt_id", "question"},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create Agent: %w", err)
	}

	session, err := client.create(ctx, "/v1/sessions", map[string]any{
		"agent":          agent.ID,
		"environment_id": environment.ID,
		"title":          "Interactive expense review",
	})
	if err != nil {
		return fmt.Errorf("create Session: %w", err)
	}
	fmt.Printf("Session %s is processing two receipts with model %s.\n", session.ID, modelID)

	prompt := strings.Join([]string{
		"Apply this expense policy: office supplies at or below USD 100 with a receipt are approved; expenses above USD 500 without an itemized receipt require human review.",
		"Process exactly two receipts in one response.",
		"Receipt r01 is USD 12 for office pencils and has an itemized receipt. Call decide with action approve.",
		"Receipt r02 is USD 900 for an unspecified team activity and has no itemized receipt. Call escalate with a useful reviewer question.",
		"Call exactly one tool for each receipt now, with no prose before the two tool calls.",
	}, " ")
	if err := client.post(ctx, "/v1/sessions/"+session.ID+"/events", map[string]any{
		"events": []any{map[string]any{
			"type":    "user.message",
			"content": []any{map[string]any{"type": "text", "text": prompt}},
		}},
	}, nil); err != nil {
		return fmt.Errorf("send user message: %w", err)
	}

	actions, err := client.waitForActions(ctx, session.ID)
	if err != nil {
		return err
	}
	fmt.Println("The real model requested these application-owned actions:")
	for _, action := range actions {
		encoded, _ := json.Marshal(action.Input)
		fmt.Printf("  %s %s\n", action.Name, encoded)
	}

	reader := bufio.NewReader(os.Stdin)
	for index, action := range actions {
		result, err := resolveAction(reader, os.Stdout, action)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode result for %s: %w", action.ID, err)
		}
		if err := client.post(ctx, "/v1/sessions/"+session.ID+"/events", map[string]any{
			"events": []any{map[string]any{
				"type":               "user.custom_tool_result",
				"custom_tool_use_id": action.ID,
				"content": []any{map[string]any{
					"type": "text", "text": string(encoded),
				}},
			}},
		}, nil); err != nil {
			return fmt.Errorf("submit result for %s: %w", action.ID, err)
		}
		if index == 0 {
			fmt.Println("First result persisted; the Session remains at the incomplete barrier.")
		}
	}

	answer, err := client.waitForCompletion(ctx, session.ID)
	if err != nil {
		return err
	}
	fmt.Println("\nAgent final response:")
	fmt.Println(answer)

	if keepResources {
		fmt.Printf(
			"\nKeeping Session %s, Agent %s, and Environment %s for inspection.\n",
			session.ID,
			agent.ID,
			environment.ID,
		)
		fmt.Printf("History: GET /v1/sessions/%s/events\n", session.ID)
		return nil
	}
	fmt.Println("\nSet MANGO_EXAMPLE_KEEP_RESOURCES=1 on a later run to inspect its durable history.")
	client.cleanup(ctx, session.ID, agent.ID, environment.ID)
	return nil
}

func (c *apiClient) waitForActions(ctx context.Context, sessionID string) ([]event, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		history, err := c.events(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("list events while waiting for actions: %w", err)
		}
		if failure := firstEvent(history, "session.error"); failure != nil {
			return nil, fmt.Errorf("session failed before the gate: %+v", *failure)
		}
		idle := lastEvent(history, "session.status_idle")
		if idle != nil && idle.StopReason != nil && idle.StopReason.Type == "requires_action" {
			byID := make(map[string]event, len(history))
			for _, item := range history {
				byID[item.ID] = item
			}
			actions := make([]event, 0, len(idle.StopReason.EventIDs))
			for _, id := range idle.StopReason.EventIDs {
				action, ok := byID[id]
				if !ok || action.Type != "agent.custom_tool_use" {
					return nil, fmt.Errorf("requires_action referenced missing custom-tool event %s", id)
				}
				actions = append(actions, action)
			}
			if err := validateScenarioActions(actions); err != nil {
				return nil, err
			}
			sort.Slice(actions, func(i, j int) bool {
				return stringInput(actions[i], "receipt_id") < stringInput(actions[j], "receipt_id")
			})
			return actions, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for requires_action: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *apiClient) waitForCompletion(ctx context.Context, sessionID string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		history, err := c.events(ctx, sessionID)
		if err != nil {
			return "", fmt.Errorf("list events while waiting for completion: %w", err)
		}
		if failure := firstEvent(history, "session.error"); failure != nil {
			return "", fmt.Errorf("session failed after the gate: %+v", *failure)
		}
		idle := lastEvent(history, "session.status_idle")
		if idle != nil && idle.StopReason != nil && idle.StopReason.Type == "end_turn" {
			message := lastEvent(history, "agent.message")
			if message == nil {
				return "", errors.New("session ended without an agent.message")
			}
			parts := make([]string, 0, len(message.Content))
			for _, block := range message.Content {
				if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
					parts = append(parts, strings.TrimSpace(block.Text))
				}
			}
			if len(parts) == 0 {
				return "", errors.New("final agent.message contained no text")
			}
			return strings.Join(parts, "\n"), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for end_turn: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateScenarioActions(actions []event) error {
	want := map[string]string{"r01": "decide", "r02": "escalate"}
	if len(actions) != len(want) {
		return fmt.Errorf("model emitted %d actions, want exactly %d", len(actions), len(want))
	}
	seen := make(map[string]bool, len(actions))
	for _, action := range actions {
		receiptID := stringInput(action, "receipt_id")
		if receiptID == "" || want[receiptID] != action.Name || seen[receiptID] {
			return fmt.Errorf("unexpected action %s for receipt %q: %+v", action.Name, receiptID, action.Input)
		}
		seen[receiptID] = true
	}
	return nil
}

func resolveAction(reader *bufio.Reader, writer io.Writer, action event) (map[string]any, error) {
	receiptID := stringInput(action, "receipt_id")
	switch action.Name {
	case "decide":
		decision := stringInput(action, "action")
		if decision != "approve" && decision != "reject" {
			return nil, fmt.Errorf("decide action has invalid decision %q", decision)
		}
		if _, err := fmt.Fprintf(writer, "\nApplication records %s for %s.\n", decision, receiptID); err != nil {
			return nil, fmt.Errorf("write application decision: %w", err)
		}
		return map[string]any{
			"recorded": true, "receipt_id": receiptID, "decision": decision,
		}, nil
	case "escalate":
		if _, err := fmt.Fprintf(
			writer,
			"\nHuman review required for %s: %s\n",
			receiptID,
			stringInput(action, "question"),
		); err != nil {
			return nil, fmt.Errorf("write review question: %w", err)
		}
		for {
			if _, err := fmt.Fprint(writer, "Decision [approve/reject]: "); err != nil {
				return nil, fmt.Errorf("write review prompt: %w", err)
			}
			line, err := reader.ReadString('\n')
			decision := strings.ToLower(strings.TrimSpace(line))
			if decision == "approve" || decision == "reject" {
				return map[string]any{
					"recorded": true, "receipt_id": receiptID, "human_decision": decision,
				}, nil
			}
			if err != nil {
				return nil, fmt.Errorf("read human decision: %w", err)
			}
			if _, err := fmt.Fprintln(writer, "Enter approve or reject."); err != nil {
				return nil, fmt.Errorf("write review guidance: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported custom tool %q", action.Name)
	}
}

func stringInput(action event, key string) string {
	value, _ := action.Input[key].(string)
	return value
}

func firstEvent(events []event, eventType string) *event {
	for index := range events {
		if events[index].Type == eventType {
			return &events[index]
		}
	}
	return nil
}

func lastEvent(events []event, eventType string) *event {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == eventType {
			return &events[index]
		}
	}
	return nil
}

func (c *apiClient) events(ctx context.Context, sessionID string) ([]event, error) {
	var history eventList
	if err := c.get(ctx, "/v1/sessions/"+sessionID+"/events?order=asc&limit=1000", &history); err != nil {
		return nil, err
	}
	return history.Data, nil
}

func (c *apiClient) create(ctx context.Context, endpoint string, input any) (resource, error) {
	var output resource
	if err := c.post(ctx, endpoint, input, &output); err != nil {
		return resource{}, err
	}
	if output.ID == "" {
		return resource{}, errors.New("response omitted id")
	}
	return output, nil
}

func (c *apiClient) get(ctx context.Context, endpoint string, output any) error {
	return c.do(ctx, http.MethodGet, endpoint, nil, output)
}

func (c *apiClient) post(ctx context.Context, endpoint string, input, output any) error {
	return c.do(ctx, http.MethodPost, endpoint, input, output)
}

func (c *apiClient) do(ctx context.Context, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s: %s", method, endpoint, response.Status, strings.TrimSpace(string(data)))
	}
	if output == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
	}
	return nil
}

func (c *apiClient) cleanup(ctx context.Context, sessionID, agentID, environmentID string) {
	fmt.Println("Cleaning up example resources...")
	steps := []struct {
		method   string
		endpoint string
	}{
		{http.MethodDelete, "/v1/sessions/" + sessionID},
		{http.MethodPost, "/v1/agents/" + agentID + "/archive"},
		{http.MethodDelete, "/v1/environments/" + environmentID},
	}
	for _, step := range steps {
		if err := c.do(ctx, step.method, step.endpoint, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup %s failed: %v\n", step.endpoint, err)
		}
	}
}
