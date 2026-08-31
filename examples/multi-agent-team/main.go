// Command multi-agent-team runs a real-model coordinator, specialist, Advisor,
// and persistent follow-up workflow against Mango's public HTTP API. This
// client currently uses net/http; first-party SDKs expose the same operations.
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
	scenarioTimeout = 12 * time.Minute
)

type apiClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type resource struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type eventList struct {
	Data []event `json:"data"`
}

type event struct {
	ID                  string         `json:"id"`
	Type                string         `json:"type"`
	AgentName           string         `json:"agent_name"`
	SessionThreadID     string         `json:"session_thread_id"`
	FromAgentName       string         `json:"from_agent_name"`
	FromSessionThreadID string         `json:"from_session_thread_id"`
	Content             []contentBlock `json:"content"`
	StopReason          *stopReason    `json:"stop_reason"`
	Error               any            `json:"error"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type stopReason struct {
	Type string `json:"type"`
}

type sessionThreadList struct {
	Data []sessionThread `json:"data"`
}

type sessionThread struct {
	ID             string      `json:"id"`
	ParentThreadID *string     `json:"parent_thread_id"`
	Agent          threadAgent `json:"agent"`
	Status         string      `json:"status"`
	Usage          threadUsage `json:"usage"`
}

type threadAgent struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type threadUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type roundResult struct {
	Events []event
	Answer string
}

type createdResources struct {
	EnvironmentID string
	AgentIDs      []string
	SessionID     string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "multi-agent example failed:", err)
		os.Exit(1)
	}
}

func run() error {
	modelID := strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_MODEL_ID"))
	if modelID == "" {
		return errors.New("MANGO_EXAMPLE_MODEL_ID is required; set it to the model configured on the Mango worker")
	}
	advisorModelID := strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_ADVISOR_MODEL_ID"))
	if advisorModelID == "" {
		advisorModelID = modelID
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

	signalContext, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
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

	resources := createdResources{}
	succeeded := false
	defer func() {
		if keepResources || (!succeeded && resources.SessionID != "") {
			if !succeeded && resources.SessionID != "" {
				fmt.Fprintf(
					os.Stderr,
					"Keeping failed Session %s for inspection; delete it after diagnosis.\n",
					resources.SessionID,
				)
			}
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cleanupCancel()
		client.cleanup(cleanupCtx, resources)
	}()

	fmt.Println("Creating two specialists, a coordinator, and a Session through the Mango HTTP API...")
	environment, err := client.create(ctx, "/v1/environments", map[string]any{
		"name":   "Multi-agent release review example",
		"config": map[string]any{"type": "cloud"},
	})
	if err != nil {
		return fmt.Errorf("create Environment: %w", err)
	}
	resources.EnvironmentID = environment.ID

	reliability, err := client.create(ctx, "/v1/agents", map[string]any{
		"name":        "reliability_reviewer",
		"description": "Reviews rollout safety, failure modes, observability, and rollback plans.",
		"model":       modelID,
		"system": strings.Join([]string{
			"You are the reliability specialist on a release-review team.",
			"Analyze only the task sent by the coordinator.",
			"Return a concise report beginning with RELIABILITY_REPORT: and include release blockers, rollback conditions, and the smallest safe rollout.",
			"Do not address the end user and do not claim to have changed any system.",
		}, " "),
	})
	if err != nil {
		return fmt.Errorf("create reliability Agent: %w", err)
	}
	resources.AgentIDs = append(resources.AgentIDs, reliability.ID)

	security, err := client.create(ctx, "/v1/agents", map[string]any{
		"name":        "security_reviewer",
		"description": "Reviews authentication, secret handling, auditability, and abuse paths.",
		"model":       modelID,
		"system": strings.Join([]string{
			"You are the security specialist on a release-review team.",
			"Analyze only the task sent by the coordinator.",
			"Return a concise report beginning with SECURITY_REPORT: and include threat assumptions, credential risks, audit requirements, and release blockers.",
			"Do not address the end user and do not claim to have changed any system.",
		}, " "),
	})
	if err != nil {
		return fmt.Errorf("create security Agent: %w", err)
	}
	resources.AgentIDs = append(resources.AgentIDs, security.ID)

	coordinator, err := client.create(ctx, "/v1/agents", map[string]any{
		"name":  "release_review_coordinator",
		"model": modelID,
		"system": strings.Join([]string{
			"You coordinate release-readiness reviews and must use the team rather than doing specialist analysis yourself.",
			"For the first user request, immediately send one self-contained task to reliability_reviewer and one to security_reviewer; these independent tasks may run in parallel.",
			"Call advisor exactly once for an independent challenge while the specialists run or after they report. Return one final decision only after both specialist reports and the advice arrive, with headings Decision, Reliability, Security, Advisor challenge, and Next steps.",
			"Do not invent reports and do not give the user a final decision while required team work is still running.",
			"For a later message beginning FOLLOW-UP:, call list_agents, find the existing reliability_reviewer Thread, and send the new constraint to that exact Thread using session_thread_id.",
			"Do not start a second reliability Thread and do not consult the advisor again for the follow-up. Wait for the follow-up report, then revise the decision.",
		}, " "),
		"multiagent": map[string]any{
			"type": "coordinator",
			"agents": []any{
				map[string]any{
					"type": "agent", "id": reliability.ID,
					"version": reliability.Version,
				},
				map[string]any{
					"type": "agent", "id": security.ID,
					"version": security.Version,
				},
				map[string]any{"type": "advisor", "model": advisorModelID},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create coordinator Agent: %w", err)
	}
	resources.AgentIDs = append(resources.AgentIDs, coordinator.ID)

	session, err := client.create(ctx, "/v1/sessions", map[string]any{
		"agent": map[string]any{
			"type": "agent", "id": coordinator.ID,
			"version": coordinator.Version,
		},
		"environment_id": environment.ID,
		"title":          "Release readiness team review",
	})
	if err != nil {
		return fmt.Errorf("create Session: %w", err)
	}
	resources.SessionID = session.ID
	fmt.Printf(
		"Session %s is using worker model %s and Advisor model %s.\n",
		session.ID, modelID, advisorModelID,
	)

	initialHistory, err := client.events(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("read initial Session history: %w", err)
	}
	initialPrompt := strings.Join([]string{
		"Review whether we should release a payments API migration from static API keys to 15-minute access tokens tomorrow.",
		"Facts: 15 client applications are in scope; a 5% canary is available; rollback restores the previous gateway in about 20 minutes; audit logs exclude token values; one legacy client can rotate credentials only by taking a two-hour outage; there is no automatic client-side fallback.",
		"Use the required specialist-team process and give a go, conditional-go, or no-go decision.",
	}, " ")
	if err := client.sendUserMessage(ctx, session.ID, initialPrompt); err != nil {
		return fmt.Errorf("send initial user message: %w", err)
	}

	fmt.Println("Waiting for both real specialist models and the real Advisor consultation...")
	initialRound, err := client.waitForRound(
		ctx,
		session.ID,
		len(initialHistory),
		[]string{
			"reliability_reviewer",
			"security_reviewer",
			"anthropic.advisor",
		},
	)
	if err != nil {
		return fmt.Errorf("initial team review: %w", err)
	}
	if err := validateInitialRound(initialRound.Events); err != nil {
		return err
	}
	initialThreads, err := client.threads(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("list Session Threads: %w", err)
	}
	threadIDs, initialUsage, err := validateInitialThreads(initialThreads)
	if err != nil {
		return err
	}

	printReports(initialRound.Events)
	fmt.Println("\nCoordinator decision:")
	fmt.Println(initialRound.Answer)

	reader := bufio.NewReader(os.Stdin)
	defaultFollowUp := "Assume rollback now takes 45 minutes and the legacy client has no maintenance window this week. Revisit the recommendation."
	fmt.Println("\nAdd a release constraint for the existing reliability specialist.")
	fmt.Printf("Follow-up [%s]: ", defaultFollowUp)
	line, readErr := reader.ReadString('\n')
	followUp := strings.TrimSpace(line)
	if followUp == "" {
		followUp = defaultFollowUp
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("read follow-up: %w", readErr)
	}

	followHistory, err := client.events(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("read history before follow-up: %w", err)
	}
	if err := client.sendUserMessage(
		ctx,
		session.ID,
		"FOLLOW-UP: "+followUp,
	); err != nil {
		return fmt.Errorf("send follow-up user message: %w", err)
	}

	fmt.Println("Waiting for a follow-up on the existing reliability Thread...")
	followRound, err := client.waitForRound(
		ctx,
		session.ID,
		len(followHistory),
		[]string{"reliability_reviewer"},
	)
	if err != nil {
		return fmt.Errorf("follow-up review: %w", err)
	}
	if err := validateFollowUpRound(
		followRound.Events,
		threadIDs["reliability_reviewer"],
	); err != nil {
		return err
	}
	followThreads, err := client.threads(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("list Threads after follow-up: %w", err)
	}
	if err := validateFollowUpThreads(
		followThreads,
		threadIDs,
		initialUsage,
	); err != nil {
		return err
	}

	printReports(followRound.Events)
	fmt.Println("\nRevised coordinator decision:")
	fmt.Println(followRound.Answer)
	fmt.Println("\nVerified: two specialist Threads, one Advisor consultation, real usage, and persistent reliability follow-up.")

	succeeded = true
	if keepResources {
		fmt.Printf("\nKeeping Session %s for inspection.\n", session.ID)
		fmt.Printf("History: GET /v1/sessions/%s/events\n", session.ID)
		fmt.Printf("Threads: GET /v1/sessions/%s/threads\n", session.ID)
		return nil
	}
	fmt.Println("\nSet MANGO_EXAMPLE_KEEP_RESOURCES=1 on a later run to inspect its durable history.")
	return nil
}

func (c *apiClient) waitForRound(
	ctx context.Context,
	sessionID string,
	start int,
	requiredReports []string,
) (roundResult, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		history, err := c.events(ctx, sessionID)
		if err != nil {
			return roundResult{}, fmt.Errorf("list events: %w", err)
		}
		if len(history) < start {
			return roundResult{}, errors.New("session event history moved backwards")
		}
		round := history[start:]
		if failure := firstEvent(round, "session.error"); failure != nil {
			encoded, _ := json.Marshal(failure)
			return roundResult{}, fmt.Errorf("session failed: %s", encoded)
		}
		idle := lastEvent(round, "session.status_idle")
		if idle != nil && idle.StopReason != nil && idle.StopReason.Type == "end_turn" {
			for _, name := range requiredReports {
				report := lastReport(round, name)
				if report == nil {
					return roundResult{}, fmt.Errorf("session reached end_turn without a report from %s", name)
				}
				if strings.TrimSpace(eventText(*report)) == "" {
					return roundResult{}, fmt.Errorf("session reached end_turn with an empty report from %s", name)
				}
			}
			message := lastEvent(round, "agent.message")
			if message == nil || strings.TrimSpace(eventText(*message)) == "" {
				return roundResult{}, errors.New("session ended without a non-empty coordinator message")
			}
			return roundResult{Events: round, Answer: strings.TrimSpace(eventText(*message))}, nil
		}
		select {
		case <-ctx.Done():
			return roundResult{}, fmt.Errorf("wait for end_turn: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateInitialRound(events []event) error {
	created := map[string]int{}
	for _, item := range events {
		switch item.Type {
		case "session.thread_created":
			created[item.AgentName]++
		}
	}
	for _, name := range []string{
		"reliability_reviewer", "security_reviewer", "anthropic.advisor",
	} {
		if created[name] != 1 {
			return fmt.Errorf("thread_created count for %s = %d, want 1", name, created[name])
		}
	}
	if lastReport(events, "anthropic.advisor") == nil {
		return errors.New("advisor report was not projected onto the primary event history")
	}
	answer := eventText(*lastEvent(events, "agent.message"))
	for _, term := range []string{"reliab", "security", "advisor"} {
		if !strings.Contains(strings.ToLower(answer), term) {
			return fmt.Errorf("coordinator answer did not incorporate %s evidence", term)
		}
	}
	return nil
}

func validateInitialThreads(
	threads []sessionThread,
) (map[string]string, map[string]int64, error) {
	if len(threads) != 4 {
		return nil, nil, fmt.Errorf("session has %d Threads, want primary, two specialists, and one Advisor", len(threads))
	}
	threadIDs := map[string]string{}
	usage := map[string]int64{}
	advisorCount := 0
	for _, thread := range threads {
		label := thread.Agent.Name
		if thread.Agent.Type == "advisor" {
			label = "anthropic.advisor"
			advisorCount++
			if thread.Status != "terminated" {
				return nil, nil, fmt.Errorf(
					"advisor Thread status = %s, want terminated", thread.Status,
				)
			}
		} else if thread.Status != "idle" {
			return nil, nil, fmt.Errorf(
				"agent Thread %s status = %s, want idle", thread.ID, thread.Status,
			)
		}
		tokens := thread.Usage.InputTokens + thread.Usage.OutputTokens
		if tokens <= 0 {
			return nil, nil, fmt.Errorf("thread %s (%s) has no provider token usage", thread.ID, label)
		}
		usage[label] = tokens
		if thread.ParentThreadID != nil && thread.Agent.Type != "advisor" {
			if _, exists := threadIDs[label]; exists {
				return nil, nil, fmt.Errorf("more than one child Thread exists for %s", label)
			}
			threadIDs[label] = thread.ID
		}
	}
	if advisorCount != 1 {
		return nil, nil, fmt.Errorf("advisor Thread count = %d, want 1", advisorCount)
	}
	for _, name := range []string{"reliability_reviewer", "security_reviewer"} {
		if threadIDs[name] == "" {
			return nil, nil, fmt.Errorf("missing child Thread for %s", name)
		}
	}
	return threadIDs, usage, nil
}

func validateFollowUpRound(events []event, reliabilityThreadID string) error {
	if created := firstEvent(events, "session.thread_created"); created != nil {
		return fmt.Errorf("follow-up created an unexpected new Thread for %s", created.AgentName)
	}
	if advisor := lastReport(events, "anthropic.advisor"); advisor != nil {
		return errors.New("follow-up made an unexpected second Advisor consultation")
	}
	report := lastReport(events, "reliability_reviewer")
	if report == nil {
		return errors.New("follow-up returned no reliability report")
	}
	if report.FromSessionThreadID != reliabilityThreadID {
		return fmt.Errorf(
			"follow-up report came from Thread %s, want persistent Thread %s",
			report.FromSessionThreadID,
			reliabilityThreadID,
		)
	}
	return nil
}

func validateFollowUpThreads(
	threads []sessionThread,
	initialThreadIDs map[string]string,
	initialUsage map[string]int64,
) error {
	if len(threads) != 4 {
		return fmt.Errorf("follow-up changed Thread count to %d, want 4", len(threads))
	}
	reliabilityCount := 0
	advisorCount := 0
	for _, thread := range threads {
		if thread.Agent.Type == "advisor" {
			advisorCount++
			if thread.Status != "terminated" {
				return fmt.Errorf(
					"advisor Thread status after follow-up = %s, want terminated",
					thread.Status,
				)
			}
			continue
		}
		if thread.ParentThreadID == nil || thread.Agent.Name != "reliability_reviewer" {
			continue
		}
		reliabilityCount++
		if thread.Status != "idle" {
			return fmt.Errorf(
				"reliability Thread status after follow-up = %s, want idle",
				thread.Status,
			)
		}
		if thread.ID != initialThreadIDs["reliability_reviewer"] {
			return fmt.Errorf("follow-up replaced reliability Thread %s with %s", initialThreadIDs["reliability_reviewer"], thread.ID)
		}
		tokens := thread.Usage.InputTokens + thread.Usage.OutputTokens
		if tokens <= initialUsage["reliability_reviewer"] {
			return fmt.Errorf("persistent reliability Thread usage did not increase: %d <= %d", tokens, initialUsage["reliability_reviewer"])
		}
	}
	if reliabilityCount != 1 {
		return fmt.Errorf("reliability child Thread count = %d, want 1", reliabilityCount)
	}
	if advisorCount != 1 {
		return fmt.Errorf("advisor Thread count after follow-up = %d, want 1", advisorCount)
	}
	return nil
}

func printReports(events []event) {
	reports := make([]event, 0, 3)
	for _, item := range events {
		if item.Type == "agent.thread_message_received" {
			reports = append(reports, item)
		}
	}
	sort.SliceStable(reports, func(i, j int) bool {
		return reports[i].FromAgentName < reports[j].FromAgentName
	})
	for _, report := range reports {
		fmt.Printf("\n[%s, Thread %s]\n%s\n", report.FromAgentName, report.FromSessionThreadID, strings.TrimSpace(eventText(report)))
	}
}

func eventText(item event) string {
	parts := make([]string, 0, len(item.Content))
	for _, block := range item.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, "\n")
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

func lastReport(events []event, agentName string) *event {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == "agent.thread_message_received" &&
			events[index].FromAgentName == agentName {
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

func (c *apiClient) threads(ctx context.Context, sessionID string) ([]sessionThread, error) {
	var output sessionThreadList
	if err := c.get(ctx, "/v1/sessions/"+sessionID+"/threads?limit=1000", &output); err != nil {
		return nil, err
	}
	return output.Data, nil
}

func (c *apiClient) sendUserMessage(ctx context.Context, sessionID, text string) error {
	return c.post(ctx, "/v1/sessions/"+sessionID+"/events", map[string]any{
		"events": []any{map[string]any{
			"type": "user.message",
			"content": []any{map[string]any{
				"type": "text", "text": text,
			}},
		}},
	}, nil)
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

func (c *apiClient) cleanup(ctx context.Context, resources createdResources) {
	if resources.SessionID == "" && resources.EnvironmentID == "" &&
		len(resources.AgentIDs) == 0 {
		return
	}
	fmt.Println("Cleaning up example resources...")
	if resources.SessionID != "" {
		if err := c.do(ctx, http.MethodDelete, "/v1/sessions/"+resources.SessionID, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup Session failed: %v\n", err)
		}
	}
	for index := len(resources.AgentIDs) - 1; index >= 0; index-- {
		if err := c.do(ctx, http.MethodPost, "/v1/agents/"+resources.AgentIDs[index]+"/archive", nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup Agent %s failed: %v\n", resources.AgentIDs[index], err)
		}
	}
	if resources.EnvironmentID != "" {
		if err := c.do(ctx, http.MethodDelete, "/v1/environments/"+resources.EnvironmentID, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup Environment failed: %v\n", err)
		}
	}
}
