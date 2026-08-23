package mango

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	managedAgentsBeta = "managed-agents-2026-04-01"
	anthropicVersion  = "2023-06-01"
)

type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// RequestTimeout applies to finite JSON requests. Streaming requests use
	// their attachment context and are not cut off by this timeout.
	RequestTimeout time.Duration
}

type Client struct {
	baseURL        string
	apiKey         string
	http           *http.Client
	requestTimeout time.Duration
}

type APIError struct {
	StatusCode int
	Status     string
	Detail     string
}

func (e *APIError) Error() string { return fmt.Sprintf("Mango API %s: %s", e.Status, e.Detail) }

func New(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Mango URL %q", baseURL)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("invalid Mango URL %q: credentials do not belong in the endpoint URL", baseURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid Mango URL %q: query parameters and fragments are not supported", baseURL)
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	return &Client{
		baseURL: baseURL, apiKey: strings.TrimSpace(config.APIKey), http: httpClient,
		requestTimeout: requestTimeout,
	}, nil
}

func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	base := "/v1/sessions?limit=100&order=desc"
	var sessions []Session
	for path := base; path != ""; {
		var page struct {
			Data     []Session `json:"data"`
			NextPage *string   `json:"next_page"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		sessions = append(sessions, page.Data...)
		path = nextPath(base, page.NextPage)
	}
	return sessions, nil
}

func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	base := "/v1/agents?limit=100"
	var agents []Agent
	for path := base; path != ""; {
		var page struct {
			Data     []Agent `json:"data"`
			NextPage *string `json:"next_page"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		agents = append(agents, page.Data...)
		path = nextPath(base, page.NextPage)
	}
	return agents, nil
}

func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	base := "/v1/environments?limit=100"
	var environments []Environment
	for path := base; path != ""; {
		var page struct {
			Data     []Environment `json:"data"`
			NextPage *string       `json:"next_page"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		environments = append(environments, page.Data...)
		path = nextPath(base, page.NextPage)
	}
	return environments, nil
}

func (c *Client) CreateAgent(ctx context.Context, input CreateAgentInput) (Agent, error) {
	body := map[string]any{
		"name": input.Name, "model": input.Model, "system": input.System,
	}
	if len(input.Tools) > 0 {
		body["tools"] = input.Tools
	}
	if len(input.MCPServers) > 0 {
		body["mcp_servers"] = input.MCPServers
	}
	if input.Multiagent != nil {
		body["multiagent"] = map[string]any{
			"type": input.Multiagent.Type, "agents": input.Multiagent.Agents,
		}
	}
	var agent Agent
	err := c.do(ctx, http.MethodPost, "/v1/agents", body, &agent)
	return agent, err
}

func (c *Client) CreateEnvironment(ctx context.Context, input CreateEnvironmentInput) (Environment, error) {
	var environment Environment
	err := c.do(ctx, http.MethodPost, "/v1/environments", map[string]any{
		"name": input.Name, "description": input.Description,
		"config": map[string]any{"type": "cloud"},
	}, &environment)
	return environment, err
}

func (c *Client) CreateSession(ctx context.Context, input CreateSessionInput) (Session, error) {
	body := map[string]any{
		"agent": input.AgentID, "environment_id": input.EnvironmentID, "title": input.Title,
	}
	if strings.TrimSpace(input.InitialPrompt) != "" {
		body["initial_events"] = []map[string]any{{
			"type":    "user.message",
			"content": []map[string]any{{"type": "text", "text": input.InitialPrompt}},
		}}
	}
	var session Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions", body, &session)
	return session, err
}

func (c *Client) UpdateSessionTitle(ctx context.Context, sessionID, title string) (Session, error) {
	var session Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID),
		map[string]any{"title": strings.TrimSpace(title)}, &session)
	return session, err
}

func (c *Client) ArchiveSession(ctx context.Context, sessionID string) (Session, error) {
	var session Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/archive", nil, &session)
	return session, err
}

func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), nil, nil)
}

func (c *Client) Refresh(ctx context.Context, sessionID string) (Summary, error) {
	var summary Summary
	var err error
	summary.Session, err = c.getSession(ctx, sessionID)
	if err != nil {
		return Summary{}, err
	}
	summary.Threads, err = c.listThreads(ctx, sessionID)
	return summary, err
}

func (c *Client) Attach(parent context.Context, sessionID string) (Attachment, error) {
	ctx, cancel := context.WithCancel(parent)
	summary, err := c.Refresh(ctx, sessionID)
	if err != nil {
		cancel()
		return Attachment{}, err
	}
	if len(summary.Threads) == 0 {
		cancel()
		return Attachment{}, errors.New("Mango returned a Session without a primary Thread")
	}

	updates, ready := c.subscribe(ctx, sessionID, summary.Threads)
	for range summary.Threads {
		if streamErr := <-ready; streamErr != nil {
			cancel()
			return Attachment{}, streamErr
		}
	}

	history := make(map[string][]Event, len(summary.Threads))
	for _, thread := range summary.Threads {
		events, listErr := c.listThreadEvents(ctx, sessionID, thread.ID)
		if listErr != nil {
			cancel()
			return Attachment{}, listErr
		}
		history[thread.ID] = events
	}
	return Attachment{
		Session: summary.Session, Threads: summary.Threads, Events: history,
		Updates: updates, Cancel: cancel,
	}, nil
}

func (c *Client) SendMessage(ctx context.Context, sessionID, text string) ([]Event, error) {
	return c.sendEvents(ctx, sessionID, []map[string]any{{
		"type":    "user.message",
		"content": []map[string]any{{"type": "text", "text": text}},
	}})
}

func (c *Client) Interrupt(ctx context.Context, sessionID, threadID string) ([]Event, error) {
	event := map[string]any{"type": "user.interrupt"}
	if threadID != "" {
		event["session_thread_id"] = threadID
	}
	return c.sendEvents(ctx, sessionID, []map[string]any{event})
}

func (c *Client) ResolveAction(
	ctx context.Context,
	sessionID string,
	action Action,
	response ActionResponse,
) ([]Event, error) {
	var event map[string]any
	switch action.Kind {
	case ActionConfirmation:
		event = map[string]any{
			"type": "user.tool_confirmation", "tool_use_id": action.ID,
			"result": response.Result,
		}
		if response.Result == "deny" && strings.TrimSpace(response.DenyMessage) != "" {
			event["deny_message"] = strings.TrimSpace(response.DenyMessage)
		}
	case ActionCustomResult:
		event = resultEvent("user.custom_tool_result", "custom_tool_use_id", action.ID, response)
	case ActionToolResult:
		event = resultEvent("user.tool_result", "tool_use_id", action.ID, response)
	default:
		return nil, fmt.Errorf("unknown action kind %q", action.Kind)
	}
	if action.ThreadID != "" {
		event["session_thread_id"] = action.ThreadID
	}
	return c.sendEvents(ctx, sessionID, []map[string]any{event})
}

func resultEvent(typ, reference string, id string, response ActionResponse) map[string]any {
	event := map[string]any{
		"type": typ, reference: id,
		"content": []map[string]any{{"type": "text", "text": response.Content}},
	}
	if response.IsError {
		event["is_error"] = true
	}
	return event
}

func (c *Client) getSession(ctx context.Context, sessionID string) (Session, error) {
	var session Session
	err := c.get(ctx, "/v1/sessions/"+url.PathEscape(sessionID), &session)
	return session, err
}

func (c *Client) listThreads(ctx context.Context, sessionID string) ([]Thread, error) {
	base := "/v1/sessions/" + url.PathEscape(sessionID) + "/threads?limit=1000"
	var threads []Thread
	for path := base; path != ""; {
		var page struct {
			Data     []Thread `json:"data"`
			NextPage *string  `json:"next_page"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		threads = append(threads, page.Data...)
		path = nextPath(base, page.NextPage)
	}
	return threads, nil
}

func (c *Client) listThreadEvents(ctx context.Context, sessionID, threadID string) ([]Event, error) {
	base := "/v1/sessions/" + url.PathEscape(sessionID) + "/threads/" +
		url.PathEscape(threadID) + "/events?limit=1000"
	var events []Event
	for path := base; path != ""; {
		var page struct {
			Data     []Event `json:"data"`
			NextPage *string `json:"next_page"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		events = append(events, page.Data...)
		path = nextPath(base, page.NextPage)
	}
	return events, nil
}

func (c *Client) sendEvents(ctx context.Context, sessionID string, events []map[string]any) ([]Event, error) {
	var response struct {
		Data []Event `json:"data"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/events",
		map[string]any{"events": events}, &response)
	return response.Data, err
}

func (c *Client) get(ctx context.Context, path string, destination any) error {
	return c.do(ctx, http.MethodGet, path, nil, destination)
}

func (c *Client) do(ctx context.Context, method, path string, body, destination any) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := c.newRequest(ctx, method, path, reader)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	if destination == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return fmt.Errorf("decode Mango response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("anthropic-beta", managedAgentsBeta)
	request.Header.Set("anthropic-version", anthropicVersion)
	if c.apiKey != "" {
		request.Header.Set("x-api-key", c.apiKey)
	}
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	return request, nil
}

func nextPath(base string, cursor *string) string {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		return ""
	}
	separator := "&"
	if !strings.Contains(base, "?") {
		separator = "?"
	}
	return base + separator + "page=" + url.QueryEscape(*cursor)
}

func responseError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	detail := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		detail = envelope.Error.Message
	}
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return &APIError{StatusCode: response.StatusCode, Status: response.Status, Detail: detail}
}
