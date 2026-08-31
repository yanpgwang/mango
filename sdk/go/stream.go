package mango

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SSEEvent is a complete server-sent event. Data can be decoded into the
// generated EventStreamFrame union; EventStart/EventDelta are live-only previews.
type SSEEvent struct {
	Event string
	ID    string
	Data  json.RawMessage
	Retry time.Duration
}

func (e SSEEvent) Decode(output any) error { return json.Unmarshal(e.Data, output) }

// EventStream is a live-only SSE iterator. Next is not safe for concurrent calls;
// Close is safe while Next blocks. A disconnect does not reconnect automatically.
// Reconcile durable history with ListSessionEvents/ListSessionThreadEvents.
type EventStream struct {
	scanner   *bufio.Scanner
	body      io.ReadCloser
	cancel    context.CancelFunc
	once      sync.Once
	event     SSEEvent
	err       error
	lastID    string
	retry     time.Duration
	firstLine bool
}

// This exceeds Mango's maximum admitted input-event body size so valid durable
// user events are not rejected simply because they are delivered over SSE.
const maxSSEEventBytes = 64 << 20

func (c *Client) stream(ctx context.Context, method, path string, query url.Values, auth bool) (*EventStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	response, err := c.request(ctx, method, path, query, nil, "", "text/event-stream", auth)
	if err != nil {
		cancel()
		return nil, err
	}
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || mediaType != "text/event-stream" {
		_ = response.Body.Close()
		cancel()
		return nil, errors.New("mango: SSE response must use text/event-stream")
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxSSEEventBytes)
	scanner.Split(splitSSELines)
	return &EventStream{scanner: scanner, body: response.Body, cancel: cancel, firstLine: true}, nil
}

func splitSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, value := range data {
		if value == '\n' {
			return index + 1, data[:index], nil
		}
		if value == '\r' {
			if index+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance = index + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:index], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (s *EventStream) Next() bool {
	if s.err != nil {
		return false
	}
	eventType := ""
	var data []byte
	hasData := false
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if s.firstLine {
			line = strings.TrimPrefix(line, "\uFEFF")
			s.firstLine = false
		}
		if line == "" {
			if !hasData {
				eventType = ""
				continue
			}
			if eventType == "" {
				eventType = "message"
			}
			s.event = SSEEvent{Event: eventType, ID: s.lastID, Data: append(json.RawMessage(nil), bytes.TrimSuffix(data, []byte{'\n'})...), Retry: s.retry}
			return true
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if len(data)+len(value) > maxSSEEventBytes {
				s.err = errors.New("mango: SSE event exceeds 64 MiB")
				_ = s.Close()
				return false
			}
			data = append(data, value...)
			data = append(data, '\n')
			hasData = true
		case "event":
			eventType = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				s.lastID = value
			}
		case "retry":
			if duration, ok := ParseRetryMilliseconds(value); ok {
				s.retry = duration
			}
		}
	}
	s.err = s.scanner.Err()
	_ = s.Close()
	// An event without its terminating blank line is incomplete and discarded.
	return false
}

func (s *EventStream) Event() SSEEvent { return s.event }
func (s *EventStream) Err() error      { return s.err }
func (s *EventStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}
