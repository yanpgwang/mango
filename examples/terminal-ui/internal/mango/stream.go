package mango

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

func (c *Client) subscribe(
	ctx context.Context,
	sessionID string,
	threads []Thread,
) (<-chan StreamUpdate, <-chan error) {
	streamCtx, cancel := context.WithCancel(ctx)
	updates := make(chan StreamUpdate, max(64, len(threads)*16))
	ready := make(chan error, len(threads))
	var workers sync.WaitGroup
	workers.Add(len(threads))
	for _, thread := range threads {
		threadID, primary := thread.ID, thread.Primary()
		go func() {
			defer workers.Done()
			c.subscribeThread(streamCtx, sessionID, threadID, primary, updates, ready)
			// The UI presents one aggregate managed Session. If a single child
			// subscription ends, stop its siblings so the complete roster is
			// reattached instead of silently losing one Agent forever.
			cancel()
		}()
	}
	go func() {
		workers.Wait()
		cancel()
		close(updates)
	}()
	return updates, ready
}

func (c *Client) subscribeThread(
	ctx context.Context,
	sessionID, threadID string,
	primary bool,
	updates chan<- StreamUpdate,
	ready chan<- error,
) {
	query := url.Values{}
	query.Add("event_deltas[]", "agent.message")
	query.Add("event_deltas[]", "agent.thinking")
	// Primary-Agent previews are published on the Session-level live subject;
	// child previews are scoped to their durable Thread. Persisted events appear
	// on both HTTP shapes, which made using the Thread route for every Agent look
	// correct while silently dropping every primary event_start/event_delta.
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/events/stream?" + query.Encode()
	if !primary {
		path = "/v1/sessions/" + url.PathEscape(sessionID) + "/threads/" +
			url.PathEscape(threadID) + "/stream?" + query.Encode()
	}
	request, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		ready <- err
		return
	}
	request.Header.Set("accept", "text/event-stream")
	response, err := c.http.Do(request)
	if err != nil {
		ready <- err
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err = responseError(response)
		ready <- err
		return
	}
	ready <- nil

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			return true
		}
		var frame Event
		err := json.Unmarshal([]byte(data.String()), &frame)
		data.Reset()
		return sendUpdate(ctx, updates, StreamUpdate{ThreadID: threadID, Frame: frame, Err: err})
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if line == "" && !flush() {
			return
		}
	}
	if !flush() {
		return
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		sendUpdate(ctx, updates, StreamUpdate{ThreadID: threadID, Err: err})
	}
}

func sendUpdate(ctx context.Context, destination chan<- StreamUpdate, update StreamUpdate) bool {
	select {
	case destination <- update:
		return true
	case <-ctx.Done():
		return false
	}
}
