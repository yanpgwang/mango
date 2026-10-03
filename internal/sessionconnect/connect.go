// Package sessionconnect implements the operator CLI over Mango's native SDK.
package sessionconnect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	mango "github.com/yanpgwang/mango/sdk/go"
)

type Options struct {
	Client         *mango.Client
	SessionID      string
	Input          io.ReadCloser
	Output         io.Writer
	ReadOnly       bool
	Verbose        bool
	ReconnectDelay time.Duration
}

const help = "Text sends a message. Commands: /allow EVENT_ID, /deny EVENT_ID [reason], /interrupt (all threads), /message TEXT, /help, /quit. Ctrl+C detaches without interrupting the Session."

var errDetach = errors.New("detach")
var errUnconfirmedWrite = errors.New("send was not confirmed; reconnect and inspect history before resending")

type inputLine struct {
	text string
	err  error
}

type streamItem struct {
	event mango.SessionEvent
	err   error
}

type eventView struct {
	ID                  string          `json:"id"`
	Type                string          `json:"type"`
	Name                string          `json:"name"`
	EvaluatedPermission string          `json:"evaluated_permission"`
	SessionThreadID     string          `json:"session_thread_id"`
	ToolUseID           string          `json:"tool_use_id"`
	MCPToolUseID        string          `json:"mcp_tool_use_id"`
	CustomToolUseID     string          `json:"custom_tool_use_id"`
	Content             json.RawMessage `json:"content"`
	Input               json.RawMessage `json:"input"`
	MCPServerName       string          `json:"mcp_server_name"`
	StopReason          json.RawMessage `json:"stop_reason"`
	Error               json.RawMessage `json:"error"`
}

type view struct {
	options    Options
	seen       map[string]bool
	pending    map[string]eventView
	resolved   map[string]bool
	terminated bool
	announced  map[string]bool
}

// Run owns Input until it returns and closes it on detach. Read-only connections
// do not consume Input. It never retries a mutation or interrupts on detach.
func Run(ctx context.Context, options Options) error {
	if options.Client == nil || options.SessionID == "" || options.Output == nil || (!options.ReadOnly && options.Input == nil) {
		return errors.New("connect requires a client, Session ID, output, and input unless read-only")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if options.Input != nil {
		defer func() { _ = options.Input.Close() }()
	}
	if options.ReconnectDelay <= 0 {
		options.ReconnectDelay = time.Second
	}
	state := &view{options: options, seen: map[string]bool{}, pending: map[string]eventView{}, resolved: map[string]bool{}, announced: map[string]bool{}}
	session, err := options.Client.Sessions.Get(ctx, options.SessionID)
	if err != nil {
		return contextResult(ctx, err)
	}
	if err := state.print("Session %s [%s]\n", session.ID, session.Status); err != nil {
		return err
	}
	if session.Status == mango.SessionStatusTerminated {
		return contextResult(ctx, state.replay(ctx))
	}
	var input <-chan inputLine
	if !options.ReadOnly {
		lines := make(chan inputLine)
		input = lines
		go readInput(ctx, options.Input, lines)
		if err := state.print("%s\n", help); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		err := state.connection(ctx, input)
		if errors.Is(err, errUnconfirmedWrite) {
			return err
		}
		if errors.Is(err, errDetach) || ctx.Err() != nil {
			return nil
		}
		if !transient(err) {
			return err
		}
		if err := state.print("Disconnected; reconnecting and checking durable history.\n"); err != nil {
			return err
		}
		timer := time.NewTimer(options.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func contextResult(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (v *view) connection(ctx context.Context, input <-chan inputLine) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := v.options.Client.Sessions.Events.Stream(ctx, v.options.SessionID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	live := make(chan streamItem, 32)
	go readStream(ctx, stream, live)
	// The stream is connected first. Its bounded queue covers events accepted
	// while every history page is read; IDs deduplicate the overlap.
	if err := v.replay(ctx); err != nil {
		return err
	}
	if v.terminated {
		return errDetach
	}
	if err := v.refreshChildApprovals(ctx); err != nil {
		return err
	}
	if err := v.showApprovals(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return errDetach
		case line := <-input:
			if line.err != nil {
				if errors.Is(line.err, io.EOF) {
					return errDetach
				}
				return line.err
			}
			if err := v.command(ctx, line.text); err != nil {
				return err
			}
		case item := <-live:
			if item.err != nil {
				return item.err
			}
			if err := v.apply(item.event); err != nil {
				return err
			}
			if v.terminated {
				return errDetach
			}
			if err := v.refreshChildApprovals(ctx); err != nil {
				return err
			}
			if err := v.showApprovals(); err != nil {
				return err
			}
		}
	}
}

func readInput(ctx context.Context, input io.Reader, lines chan<- inputLine) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		select {
		case lines <- inputLine{text: scanner.Text()}:
		case <-ctx.Done():
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case lines <- inputLine{err: err}:
	case <-ctx.Done():
	}
}

func readStream(ctx context.Context, stream *mango.EventStream, out chan<- streamItem) {
	send := func(item streamItem) bool {
		select {
		case out <- item:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for stream.Next() {
		var event mango.SessionEvent
		if err := stream.Event().Decode(&event); err != nil {
			send(streamItem{err: fmt.Errorf("decode event stream: %w", err)})
			return
		}
		if !send(streamItem{event: event}) {
			return
		}
	}
	err := stream.Err()
	if err == nil {
		err = io.EOF
	}
	send(streamItem{err: err})
}

func (v *view) replay(ctx context.Context) error {
	pager := v.options.Client.Sessions.Events.ListAutoPaging(ctx, v.options.SessionID, mango.ListSessionEventsParams{Order: mango.Some("asc"), Limit: mango.Some(int64(1000))})
	for pager.Next() {
		if err := v.apply(pager.Value()); err != nil {
			return err
		}
	}
	return pager.Err()
}

func decode(event any) (eventView, []byte, error) {
	var item eventView
	body, err := json.Marshal(event)
	if err == nil {
		err = json.Unmarshal(body, &item)
	}
	if err == nil && (item.ID == "" || item.Type == "") {
		err = errors.New("durable event is missing its ID or type")
	}
	return item, body, err
}

func (v *view) apply(event any) error {
	item, body, err := decode(event)
	if err != nil {
		return err
	}
	if v.seen[item.ID] {
		return nil
	}
	v.seen[item.ID] = true
	v.resolve(item)
	if (item.Type == "agent.tool_use" || item.Type == "agent.mcp_tool_use") && item.EvaluatedPermission == "ask" && !v.resolved[item.ID] {
		v.pending[item.ID] = item
	}
	if item.Type == "session.status_terminated" || item.Type == "session.deleted" {
		v.terminated = true
	}
	if err := v.print("[%s] %s %s\n", item.Type, item.ID, item.Name); err != nil {
		return err
	}
	if v.options.Verbose {
		return v.print("%s\n", body)
	}
	if item.MCPServerName != "" {
		if err := v.print("MCP server: %s\n", item.MCPServerName); err != nil {
			return err
		}
	}
	for _, detail := range []struct {
		label string
		value json.RawMessage
	}{{"Input", item.Input}, {"Stop reason", item.StopReason}, {"Error", item.Error}} {
		if len(detail.value) != 0 {
			if err := v.print("%s: %s\n", detail.label, detail.value); err != nil {
				return err
			}
		}
	}
	var content []struct {
		Text string `json:"text"`
	}
	if len(item.Content) != 0 && json.Unmarshal(item.Content, &content) == nil {
		for _, block := range content {
			if block.Text != "" {
				if err := v.print("%s\n", block.Text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (v *view) resolve(item eventView) {
	for _, id := range []string{item.ToolUseID, item.MCPToolUseID, item.CustomToolUseID} {
		if id != "" {
			delete(v.pending, id)
			delete(v.announced, id)
			v.resolved[id] = true
		}
	}
	if item.Type == "user.interrupt" || item.Type == "session.thread_status_terminated" {
		for id, pending := range v.pending {
			if item.SessionThreadID == "" || pending.SessionThreadID == item.SessionThreadID {
				delete(v.pending, id)
				delete(v.announced, id)
				v.resolved[id] = true
			}
		}
	}
}

// Child confirmations are persisted on the child, even when the action is
// cross-posted to the primary stream. Read that ledger before offering approval.
func (v *view) refreshChildApprovals(ctx context.Context) error {
	threads := map[string]bool{}
	for _, pending := range v.pending {
		if pending.SessionThreadID != "" {
			threads[pending.SessionThreadID] = true
		}
	}
	for thread := range threads {
		// Reduce each ledger in its own order. An interrupt from an older child turn
		// must not cancel a later action that is already present in primary history.
		child := &view{pending: map[string]eventView{}, resolved: map[string]bool{}}
		pager := v.options.Client.Sessions.Threads.Events.ListAutoPaging(ctx, v.options.SessionID, thread, mango.ListSessionThreadEventsParams{Limit: mango.Some(int64(1000))})
		for pager.Next() {
			item, _, err := decode(pager.Value())
			if err != nil {
				return err
			}
			if item.SessionThreadID == "" {
				item.SessionThreadID = thread
			}
			child.resolve(item)
			if (item.Type == "agent.tool_use" || item.Type == "agent.mcp_tool_use") && item.EvaluatedPermission == "ask" && !child.resolved[item.ID] {
				child.pending[item.ID] = item
			}
		}
		if err := pager.Err(); err != nil {
			return err
		}
		for id := range child.resolved {
			if pending, ok := v.pending[id]; ok && pending.SessionThreadID == thread {
				delete(v.pending, id)
				delete(v.announced, id)
				v.resolved[id] = true
			}
		}
	}
	return nil
}

func (v *view) showApprovals() error {
	if v.options.ReadOnly {
		return nil
	}
	ids := make([]string, 0, len(v.pending))
	for id := range v.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if v.announced[id] {
			continue
		}
		if err := v.print("Approval pending: %s (%s). /allow %s or /deny %s [reason]\n", id, v.pending[id].Name, id, id); err != nil {
			return err
		}
		v.announced[id] = true
	}
	return nil
}

func (v *view) command(ctx context.Context, line string) error {
	text := strings.TrimSpace(line)
	if text == "" {
		return nil
	}
	var event mango.ClientSessionEventInput
	command, rest, _ := strings.Cut(text, " ")
	switch command {
	case "/quit":
		return errDetach
	case "/help":
		return v.print("%s\n", help)
	case "/interrupt":
		event.UserInterruptEventInput = &mango.UserInterruptEventInput{Type: "user.interrupt"}
	case "/allow", "/deny":
		id, reason, _ := strings.Cut(strings.TrimSpace(rest), " ")
		if err := v.replay(ctx); err != nil {
			return err
		}
		if v.terminated {
			return errDetach
		}
		if err := v.refreshChildApprovals(ctx); err != nil {
			return err
		}
		pending, ok := v.pending[id]
		if !ok {
			return v.print("No pending approval for %s; no decision sent.\n", id)
		}
		confirmation := &mango.UserToolConfirmationEventInput{Type: "user.tool_confirmation", ToolUseID: id, Result: strings.TrimPrefix(command, "/")}
		if pending.SessionThreadID != "" {
			confirmation.SessionThreadID = mango.Some(pending.SessionThreadID)
		}
		if command == "/deny" && reason != "" {
			confirmation.DenyMessage = mango.Some(reason)
		}
		event.UserToolConfirmationEventInput = confirmation
	case "/message":
		if strings.TrimSpace(rest) == "" {
			return v.print("Usage: /message TEXT\n")
		}
		event = mango.UserMessage(rest)
	default:
		if strings.HasPrefix(text, "/") {
			return v.print("Unknown command. Use /help or /message TEXT.\n")
		}
		event = mango.UserMessage(line)
	}
	batch, err := v.options.Client.Sessions.Events.Send(ctx, v.options.SessionID, mango.SendSessionEventsRequest{Events: []mango.ClientSessionEventInput{event}})
	if err != nil {
		// A lost response may follow a committed admission. Never reconnect and
		// replay a write; the operator must inspect history before resubmitting.
		return errors.Join(errUnconfirmedWrite, err)
	}
	if len(batch.Data) != 1 {
		return errUnconfirmedWrite
	}
	for _, accepted := range batch.Data {
		if err := v.apply(accepted); err != nil {
			return errors.Join(errUnconfirmedWrite, err)
		}
	}
	return nil
}

func (v *view) print(format string, args ...any) error {
	text := fmt.Sprintf(format, args...)
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
	_, err := io.WriteString(v.options.Output, text)
	return err
}

func transient(err error) bool {
	if errors.Is(err, errUnconfirmedWrite) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiErr *mango.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}
