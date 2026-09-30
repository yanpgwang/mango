// Command hitl-gate is a standalone Mango SDK application, not a runtime runner.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCommand(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("Stopped; state is preserved. Use resume or cleanup with the same -state path.")
			return
		}
		_, _ = fmt.Fprintln(os.Stderr, "HITL example:", err)
		os.Exit(1)
	}
}

func runCommand(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || (args[0] != "start" && args[0] != "resume" && args[0] != "cleanup") {
		return errors.New("usage: hitl-gate {start|resume|cleanup} [-state .mango/hitl-gate.json] [-stop-after-first-result]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("state", ".mango/hitl-gate.json", "private local resource and decision journal")
	stopAfterFirst := flags.Bool("stop-after-first-result", false, "exit after one result submission to try resume")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	apiKey := os.Getenv("MANGO_API_KEY")
	if apiKey == "" {
		apiKey = "sk-mango-local-development"
	}
	// #region client
	client, err := mango.New(mango.Config{
		BaseURL: baseURL, APIKey: apiKey, RequestTimeout: 30 * time.Second,
	})
	// #endregion client
	if err != nil {
		return err
	}
	app := gateApp{client: client, path: *path, output: output}
	if args[0] == "start" {
		model := strings.TrimSpace(os.Getenv("MANGO_EXAMPLE_MODEL_ID"))
		if model == "" {
			return errors.New("start requires MANGO_EXAMPLE_MODEL_ID, configured on the Mango worker")
		}
		app.state = gateState{BaseURL: baseURL, Decisions: map[string]decision{}}
		if err := writeState(app.path, app.state, true); err != nil {
			return fmt.Errorf("create state (use resume or another -state path): %w", err)
		}
		if err := app.start(ctx, model); err != nil {
			return fmt.Errorf("%w; state kept at %s; use cleanup for known resources", err, app.path)
		}
	} else {
		app.state, err = readState(app.path)
		if err != nil {
			return err
		}
		if app.state.BaseURL != baseURL {
			return fmt.Errorf("state belongs to server %s, not server %s", app.state.BaseURL, baseURL)
		}
		if args[0] == "cleanup" {
			return app.cleanup(ctx)
		}
		if app.state.Cleaning || app.state.SessionID == "" || app.state.Creating != "" {
			return errors.New("setup or cleanup is incomplete; use cleanup with the same state file")
		}
	}
	_, _ = fmt.Fprintf(output, "Session %s; state: %s\n", app.state.SessionID, app.path)
	return app.follow(ctx, bufio.NewReader(input), *stopAfterFirst)
}

type gateApp struct {
	client *mango.Client
	path   string
	state  gateState
	output io.Writer
}

func (a *gateApp) save() error { return writeState(a.path, a.state, false) }

func (a *gateApp) start(ctx context.Context, model string) error {
	// Record each creation attempt and returned ID separately. Creation has no
	// idempotency key: a lost response can require manual resource inspection.
	a.state.Creating = "Environment"
	if err := a.save(); err != nil {
		return err
	}
	environment, err := a.client.Environments.New(ctx, mango.EnvironmentCreateRequest{Name: "HITL expense example"})
	if err := a.created("Environment", environment.ID, &a.state.EnvironmentID, err); err != nil {
		return err
	}
	a.state.Creating = "Agent"
	if err := a.save(); err != nil {
		return err
	}
	// #region agent
	agent, err := a.client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "HITL expense gate", Model: mango.ModelID(model),
		System: mango.SomePtr("Follow the expense policy. Call exactly one tool per receipt in one response. After both results arrive, summarize them without calling more tools."),
		Tools: mango.Some([]mango.AgentTool{
			customTool("decide", "Record an approve or reject decision for a clear expense.",
				`{"receipt_id":{"type":"string"},"action":{"type":"string","enum":["approve","reject"]},"reason":{"type":"string"}}`, `["receipt_id","action","reason"]`),
			customTool("escalate", "Request a human decision for an ambiguous expense.",
				`{"receipt_id":{"type":"string"},"question":{"type":"string"}}`, `["receipt_id","question"]`),
		}),
	})
	// #endregion agent
	if err := a.created("Agent", agent.ID, &a.state.AgentID, err); err != nil {
		return err
	}
	a.state.Creating = "Session"
	if err := a.save(); err != nil {
		return err
	}
	// #region session
	session, err := a.client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent: mango.AgentID(a.state.AgentID), EnvironmentID: a.state.EnvironmentID,
		Title: mango.Some("Interactive expense review"),
	})
	// #endregion session
	if err := a.created("Session", session.ID, &a.state.SessionID, err); err != nil {
		return err
	}
	// Only start sends this message. Resume never creates or starts another turn.
	_, err = a.client.Sessions.Events.Send(ctx, session.ID, mango.SendSessionEventsRequest{
		Events: []mango.ClientSessionEventInput{mango.UserMessage(
			"Apply this expense policy: office supplies up to USD 100 with a receipt are approved; expenses above USD 500 without an itemized receipt require human review. " +
				"Process exactly two receipts in one response. r01: USD 12 for office pencils with an itemized receipt; call decide with action approve. " +
				"r02: USD 900 for an unspecified team activity without an itemized receipt; call escalate with a useful reviewer question. " +
				"Call exactly one tool for each receipt now, with no prose before the two calls.")},
	})
	if err != nil {
		return fmt.Errorf("initial message response failed; use resume to inspect history, never resend blindly: %w", err)
	}
	return nil
}

func customTool(name, description, properties, required string) mango.AgentTool {
	return mango.AgentTool{CustomTool: &mango.CustomTool{
		Type: "custom", Name: name, Description: description,
		InputSchema: mango.CustomToolInputSchema{Type: "object", AdditionalProperties: map[string]json.RawMessage{
			"properties": json.RawMessage(properties), "required": json.RawMessage(required),
		}},
	}}
}

func (a *gateApp) created(kind, id string, target *string, err error) error {
	if err != nil {
		var apiErr *mango.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			a.state.Creating = "" // Explicit admission rejection created no resource.
			return errors.Join(fmt.Errorf("create %s: %w", kind, err), a.save())
		}
		return fmt.Errorf("create %s outcome unknown; inspect its resource list for HITL entries before retrying: %w", kind, err)
	}
	if id == "" {
		return fmt.Errorf("create %s response omitted ID; inspect its resource list", kind)
	}
	_, _ = fmt.Fprintf(a.output, "Created %s %s\n", kind, id)
	*target = id
	a.state.Creating = ""
	if err := a.save(); err != nil {
		return fmt.Errorf("save %s %s: %w; retain this ID for manual cleanup", kind, id, err)
	}
	return nil
}

func (a *gateApp) follow(ctx context.Context, reader *bufio.Reader, stopAfterFirst bool) error {
	for {
		view, err := a.history(ctx)
		if err != nil {
			return err
		}
		if !view.started {
			return errors.New("no persisted initial message; setup may have stopped before sending; inspect the Session and use cleanup, not a repeated start")
		}
		if view.ended {
			_, _ = fmt.Fprintln(a.output, "\nAgent final response:\n"+view.answer)
			_, _ = fmt.Fprintln(a.output, "Resources retained. Use cleanup with the same -state path when finished.")
			return nil
		}
		for _, action := range view.pending {
			// #region decision
			result, saved := a.state.Decisions[action.ID]
			if !saved {
				result, err = resolveAction(ctx, reader, a.output, action)
				if err != nil {
					return err
				}
				a.state.Decisions[action.ID] = result
				// This local record is the simulated application's only business effect.
				// Persist it BEFORE submitting; retries reuse the recorded decision.
				if err := a.save(); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(a.output, "Recorded %s for %s (%s).\n", result.Decision, result.ReceiptID, result.DecidedBy)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return err
			}
			_, err = a.client.Sessions.Events.Send(ctx, a.state.SessionID, mango.SendSessionEventsRequest{
				Events: []mango.ClientSessionEventInput{{UserCustomToolResultEventInput: &mango.UserCustomToolResultEventInput{
					Type: "user.custom_tool_result", CustomToolUseID: action.ID,
					Content: mango.Some([]mango.ResultContentInput{{TextBlockInput: &mango.TextBlockInput{Type: "text", Text: string(encoded)}}}),
				}}},
			})
			if err != nil {
				return fmt.Errorf("result %s response failed; decision saved; resume reconciles history before retrying: %w", action.ID, err)
			}
			// #endregion decision
			_, _ = fmt.Fprintf(a.output, "Result persisted for %s.\n", action.ID)
			if stopAfterFirst {
				_, _ = fmt.Fprintln(a.output, "Stopped after one result; use resume with the same -state path.")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type historyView struct {
	started, ended bool
	answer         string
	pending        []mango.AgentCustomToolUseEvent
}

func (a *gateApp) history(ctx context.Context) (historyView, error) {
	view := historyView{}
	actions := map[string]mango.AgentCustomToolUseEvent{}
	answered := map[string]bool{}
	var idle *mango.SessionStatusIdleEvent
	// #region history
	pages := a.client.Sessions.Events.ListAutoPaging(ctx, a.state.SessionID, mango.ListSessionEventsParams{
		Order: mango.Some("asc"), Limit: mango.Some(int64(100)),
	})
	for pages.Next() {
		event := pages.Value()
		if event.PersistedUserMessageEvent != nil {
			view.started = true
		}
		if action := event.AgentCustomToolUseEvent; action != nil {
			actions[action.ID] = *action
		}
		if result := event.PersistedUserCustomToolResultEvent; result != nil {
			answered[result.CustomToolUseID] = true
		}
		if event.SessionStatusRunningEvent != nil || event.SessionStatusRescheduledEvent != nil {
			idle = nil
		}
		if event.SessionStatusIdleEvent != nil {
			idle = event.SessionStatusIdleEvent
		}
		if event.SessionStatusTerminatedEvent != nil || event.SessionDeletedEvent != nil {
			return view, errors.New("session terminated or deleted; use cleanup")
		}
		if failure := event.SessionErrorEvent; failure != nil {
			// Automatic retries remain in history even after the turn recovers.
			// Only an explicit retrying status lets this tutorial keep following.
			if retry, ok := failure.Error.RetryStatus.Get(); !ok || retry.Type != "retrying" {
				return view, fmt.Errorf("session error: %s", failure.Error.Message)
			}
		}
		if message := event.AgentMessageEvent; message != nil {
			var text []string
			for _, block := range message.Content {
				text = append(text, block.Text)
			}
			view.answer = strings.Join(text, "\n")
		}
	}
	if err := pages.Err(); err != nil {
		return view, err
	}
	if idle == nil {
		return view, nil
	}
	if barrier := idle.StopReason.SessionRequiresAction; barrier != nil {
		if err := validateActions(actions); err != nil {
			return view, err
		}
		for _, id := range barrier.EventIDs {
			action, ok := actions[id]
			if !ok {
				return view, fmt.Errorf("barrier references missing custom action %s", id)
			}
			if !answered[id] {
				view.pending = append(view.pending, action)
			}
		}
		sort.Slice(view.pending, func(i, j int) bool {
			return stringInput(view.pending[i], "receipt_id") < stringInput(view.pending[j], "receipt_id")
		})
	} else if idle.StopReason.SessionEndTurn != nil {
		view.ended = view.answer != "" && len(answered) == 2
		if !view.ended {
			return view, errors.New("session ended without both expense results and a final answer")
		}
	} else {
		return view, errors.New("session stopped for another reason; inspect history before cleanup")
	}
	// #endregion history
	return view, nil
}

func validateActions(actions map[string]mango.AgentCustomToolUseEvent) error {
	want := map[string]string{"r01": "decide", "r02": "escalate"}
	if len(actions) != 2 {
		return fmt.Errorf("model requested %d actions; this tutorial expects exactly two", len(actions))
	}
	for _, action := range actions {
		receipt := stringInput(action, "receipt_id")
		if want[receipt] != action.Name || action.Name == "" {
			return fmt.Errorf("unexpected %s action for %q", action.Name, receipt)
		}
		delete(want, receipt)
	}
	return nil
}

func resolveAction(ctx context.Context, reader *bufio.Reader, output io.Writer, action mango.AgentCustomToolUseEvent) (decision, error) {
	result := decision{Recorded: true, ReceiptID: stringInput(action, "receipt_id"), Decision: stringInput(action, "action"), DecidedBy: "application"}
	if action.Name == "escalate" {
		result.DecidedBy = "human"
		_, _ = fmt.Fprintf(output, "\nHuman review for %s: %s\n", result.ReceiptID, stringInput(action, "question"))
		for {
			if _, err := fmt.Fprint(output, "Decision [approve/reject]: "); err != nil {
				return decision{}, err
			}
			line, err := readLine(ctx, reader)
			if err != nil {
				return decision{}, err
			}
			result.Decision = strings.ToLower(strings.TrimSpace(line))
			if result.Decision == "approve" || result.Decision == "reject" {
				break
			}
			_, _ = fmt.Fprintln(output, "Enter approve or reject.")
		}
	}
	if result.Decision != "approve" && result.Decision != "reject" {
		return decision{}, fmt.Errorf("invalid decision %q", result.Decision)
	}
	return result, nil
}

func stringInput(action mango.AgentCustomToolUseEvent, key string) string {
	var value string
	_ = json.Unmarshal(action.Input[key], &value)
	return value
}

func readLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	type lineResult struct {
		line string
		err  error
	}
	ready := make(chan lineResult, 1)
	// Stdin may block forever. Cancellation lets main exit without waiting for it;
	// no resource or decision changes happen in this goroutine.
	go func() { line, err := reader.ReadString('\n'); ready <- lineResult{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ready:
		if result.err == io.EOF && strings.TrimSpace(result.line) != "" {
			result.err = nil
		}
		return result.line, result.err
	}
}

func (a *gateApp) cleanup(ctx context.Context) error {
	a.state.Cleaning = true
	if err := a.save(); err != nil {
		return err
	}
	steps := []struct {
		kind   string
		id     *string
		remove func(context.Context, string) error
	}{
		{"Session", &a.state.SessionID, func(ctx context.Context, id string) error { _, err := a.client.Sessions.Delete(ctx, id); return err }},
		{"Agent", &a.state.AgentID, func(ctx context.Context, id string) error { _, err := a.client.Agents.Archive(ctx, id); return err }},
		{"Environment", &a.state.EnvironmentID, func(ctx context.Context, id string) error {
			_, err := a.client.Environments.Delete(ctx, id)
			return err
		}},
	}
	for _, step := range steps {
		if *step.id == "" {
			continue
		}
		err := step.remove(ctx, *step.id)
		var apiErr *mango.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.StatusCode != 404) {
			return fmt.Errorf("cleanup %s %s: %w; state retained; retry cleanup", step.kind, *step.id, err)
		}
		_, _ = fmt.Fprintf(a.output, "Cleaned %s %s\n", step.kind, *step.id)
		*step.id = ""
		if err := a.save(); err != nil {
			return err
		}
	}
	if a.state.Creating != "" {
		return fmt.Errorf("known resources cleaned, but %s creation outcome is unknown; inspect its HITL resource entries and clean manually, then remove %s; state retained", a.state.Creating, a.path)
	}
	if err := os.Remove(a.path); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.output, "Cleanup complete; state file removed.")
	return nil
}
