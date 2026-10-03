package sessionconnect_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sessionconnect"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func runConnect(t *testing.T, handler http.HandlerFunc, input io.ReadCloser, configure func(*sessionconnect.Options)) (string, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	options := sessionconnect.Options{Client: client, SessionID: "sesn_test", Input: input, Output: &output, ReconnectDelay: time.Millisecond}
	if configure != nil {
		configure(&options)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = sessionconnect.Run(ctx, options)
	if ctx.Err() != nil {
		t.Fatalf("connect did not finish: %v", ctx.Err())
	}
	return output.String(), err
}

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func writeFrame(w http.ResponseWriter, event string) {
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", event)
	w.(http.Flusher).Flush()
}

func jsonReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func TestConnectReplaysEveryPageWithoutRevivingResolvedApproval(t *testing.T) {
	var pages, sends atomic.Int32
	var subscribed atomic.Bool
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Error("missing Mango authentication")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			subscribed.Store(true)
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			jsonReply(w, `{"data":[]}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			pages.Add(1)
			if !subscribed.Load() || r.URL.Query().Get("order") != "asc" {
				t.Error("history must follow subscription and request ascending order")
			}
			if r.URL.Query().Get("page") == "" {
				jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_old","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask"},{"processed_at":null,"id":"sevt_message1","type":"user.message","content":[{"type":"text","text":"first-page message"}]}],"next_page":"page-two"}`)
			} else {
				jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_confirm","type":"user.tool_confirmation","tool_use_id":"sevt_old","result":"allow"},{"processed_at":null,"id":"sevt_message2","type":"agent.message","content":[{"type":"text","text":"second-page message"}]}],"next_page":null}`)
			}
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/allow sevt_old\n/quit\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pages.Load() < 2 || !strings.Contains(output, "first-page message") || !strings.Contains(output, "second-page message") {
		t.Fatalf("incomplete paginated history: %s", output)
	}
	if sends.Load() != 0 || strings.Contains(output, "Approval pending: sevt_old") {
		t.Fatalf("resolved approval was revived: sends=%d, output=%s", sends.Load(), output)
	}
}

func TestConnectReconcilesDisconnectGapAndDeduplicatesLiveHistoryOverlap(t *testing.T) {
	var streams, histories atomic.Int32
	input, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			n := streams.Add(1)
			streamHeaders(w)
			if n == 1 {
				writeFrame(w, `{"processed_at":null,"id":"sevt_two","type":"agent.message","content":[{"type":"text","text":"live-two"}]}`)
				return
			}
			writeFrame(w, `{"processed_at":null,"id":"sevt_gap","type":"agent.message","content":[{"type":"text","text":"disconnect-gap"}]}`)
			writeFrame(w, `{"processed_at":null,"id":"sevt_end","type":"session.status_terminated"}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			if histories.Add(1) == 1 {
				jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_one","type":"agent.message","content":[{"type":"text","text":"history-one"}]}],"next_page":null}`)
			} else {
				jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_one","type":"agent.message","content":[{"type":"text","text":"history-one"}]},{"processed_at":null,"id":"sevt_two","type":"agent.message","content":[{"type":"text","text":"live-two"}]},{"processed_at":null,"id":"sevt_gap","type":"agent.message","content":[{"type":"text","text":"disconnect-gap"}]}],"next_page":null}`)
			}
		default:
			jsonReply(w, `{"id":"sesn_test","status":"running"}`)
		}
	}, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"history-one", "live-two", "disconnect-gap"} {
		if strings.Count(output, text) != 1 {
			t.Fatalf("%q must appear exactly once: %s", text, output)
		}
	}
	if streams.Load() < 2 {
		t.Fatal("stream was not reconnected")
	}
}

func TestConnectCommandsUseMangoEventsAndRouteChildApproval(t *testing.T) {
	var sent []map[string]any
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			var request struct {
				Events []map[string]any `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			sent = append(sent, request.Events...)
			for i := range request.Events {
				request.Events[i]["processed_at"] = nil
				request.Events[i]["id"] = fmt.Sprintf("sevt_sent_%d", len(sent)+i)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": request.Events})
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_primary","type":"agent.tool_use","name":"bash","input":{"command":"echo safe"},"evaluated_permission":"ask"},{"processed_at":null,"id":"sevt_child","type":"agent.mcp_tool_use","name":"lookup","mcp_server_name":"catalog","input":{"query":"catalog item"},"evaluated_permission":"ask","session_thread_id":"sthr_child"}],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("hello agent\n/allow sevt_primary\n/deny sevt_child avoid production\n/interrupt\n/quit\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "echo safe") || !strings.Contains(output, "catalog item") || !strings.Contains(output, "catalog") {
		t.Fatalf("approval lacks tool arguments: %s", output)
	}
	if len(sent) != 4 {
		t.Fatalf("sent %d events, output=%s", len(sent), output)
	}
	if sent[0]["type"] != "user.message" || sent[1]["tool_use_id"] != "sevt_primary" || sent[1]["result"] != "allow" ||
		sent[2]["type"] != "user.tool_confirmation" || sent[2]["tool_use_id"] != "sevt_child" || sent[2]["session_thread_id"] != "sthr_child" || sent[2]["result"] != "deny" || sent[2]["deny_message"] != "avoid production" || sent[3]["type"] != "user.interrupt" {
		t.Fatalf("incorrect HTTP command mapping: %+v", sent)
	}
}

func TestConnectDoesNotRetryAmbiguousMutation(t *testing.T) {
	var sends atomic.Int32
	_, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			http.Error(w, "upstream lost its response", http.StatusBadGateway)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("one message\none message again\n/quit\n")), nil)
	if err == nil || sends.Load() != 1 {
		t.Fatalf("ambiguous mutation must stop without retry: sends=%d, err=%v", sends.Load(), err)
	}
}

func TestConnectReadOnlyAndTerminalSanitization(t *testing.T) {
	var sends atomic.Int32
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			sends.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			streamHeaders(w)
			writeFrame(w, `{"processed_at":null,"id":"sevt_end","type":"session.status_terminated"}`)
		} else if strings.HasSuffix(r.URL.Path, "/events") {
			jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_text","type":"agent.message","content":[{"type":"text","text":"safe\u001b[2J\rtext"}]}],"next_page":null}`)
		} else {
			jsonReply(w, `{"id":"sesn_test","status":"running"}`)
		}
	}, io.NopCloser(strings.NewReader("should not send\n/interrupt\n")), func(o *sessionconnect.Options) { o.ReadOnly = true })
	if err != nil || sends.Load() != 0 || strings.ContainsAny(output, "\x1b\r") || !strings.Contains(output, "safe") {
		t.Fatalf("read-only/sanitization failed: sends=%d, err=%v, output=%q", sends.Load(), err, output)
	}
}

func TestConnectDoesNotReviveApprovalConfirmedInChildHistory(t *testing.T) {
	var sends atomic.Int32
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			jsonReply(w, `{"data":[]}`)
		case strings.Contains(r.URL.Path, "/threads/sthr_child/events"):
			jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_confirm_child","type":"user.tool_confirmation","tool_use_id":"sevt_child","result":"allow"}],"next_page":null}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[{"processed_at":null,"id":"sevt_child","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":"sthr_child"}],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/allow sevt_child\n/quit\n")), nil)
	if err != nil || sends.Load() != 0 || strings.Contains(output, "Approval pending: sevt_child") {
		t.Fatalf("child approval was revived: sends=%d, err=%v, output=%s", sends.Load(), err, output)
	}
}

func TestConnectOldChildInterruptDoesNotCancelNewApproval(t *testing.T) {
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case strings.Contains(r.URL.Path, "/threads/sthr_child/events"):
			jsonReply(w, `{"data":[{"id":"sevt_old_interrupt","type":"user.interrupt","processed_at":null},{"id":"sevt_new","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","processed_at":null}],"next_page":null}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[{"id":"sevt_new","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":"sthr_child","processed_at":null}],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/quit\n")), nil)
	if err != nil || !strings.Contains(output, "Approval pending: sevt_new") {
		t.Fatalf("old interrupt canceled new approval: err=%v, output=%s", err, output)
	}
}

func TestConnectTerminatedChildDoesNotOfferApproval(t *testing.T) {
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case strings.Contains(r.URL.Path, "/threads/sthr_child/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[{"id":"sevt_new","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":"sthr_child","processed_at":null},{"id":"sevt_end","type":"session.thread_status_terminated","session_thread_id":"sthr_child","agent_name":"child","processed_at":null}],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/quit\n")), nil)
	if err != nil || strings.Contains(output, "Approval pending: sevt_new") {
		t.Fatalf("terminated child approval revived: err=%v, output=%s", err, output)
	}
}

func TestConnectRejectsUnconfirmedSuccessReceipt(t *testing.T) {
	var sends atomic.Int32
	_, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			jsonReply(w, `{"data":[]}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("one\none again\n/quit\n")), nil)
	if err == nil || sends.Load() != 1 {
		t.Fatalf("invalid receipt allowed further writes: sends=%d, err=%v", sends.Load(), err)
	}
}

func TestConnectDoesNotReconnectAuthorizationFailure(t *testing.T) {
	var streams atomic.Int32
	_, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			streams.Add(1)
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/quit\n")), nil)
	if err == nil || streams.Load() != 1 {
		t.Fatalf("authorization failure retried: streams=%d, err=%v", streams.Load(), err)
	}
}

func TestConnectCancellationDetachesWithoutInterrupt(t *testing.T) {
	var sends atomic.Int32
	ready := make(chan struct{})
	input, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			close(ready)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}))
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-ready:
			cancel()
		case <-ctx.Done():
		}
	}()
	err = sessionconnect.Run(ctx, sessionconnect.Options{Client: client, SessionID: "sesn_test", Input: input, Output: io.Discard})
	if err != nil || ctx.Err() != context.Canceled || sends.Load() != 0 {
		t.Fatalf("cancel did not detach: sends=%d, err=%v, context=%v", sends.Load(), err, ctx.Err())
	}
	if _, err := writer.Write([]byte("ignored")); err != io.ErrClosedPipe {
		t.Fatalf("blocked stdin was not closed: %v", err)
	}
}

func TestConnectCancellationDuringWriteReportsUncertainReceipt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			// The server has received the request; its admission outcome is unknown.
			cancel()
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}))
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	err = sessionconnect.Run(ctx, sessionconnect.Options{Client: client, SessionID: "sesn_test", Input: io.NopCloser(strings.NewReader("message\n/quit\n")), Output: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "send was not confirmed") || sends.Load() != 1 {
		t.Fatalf("uncertain canceled write hidden: sends=%d, err=%v", sends.Load(), err)
	}
}

func TestConnectInterruptPreservesPrimaryAndChildApprovals(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%v", child), func(t *testing.T) {
			var sent []map[string]any
			action := map[string]any{"id": "sevt_wait", "type": "agent.tool_use", "name": "bash", "input": map[string]any{}, "evaluated_permission": "ask", "processed_at": nil}
			if child {
				action["session_thread_id"] = "sthr_child"
				action["source_event_id"] = "sevt_local"
			}
			history, _ := json.Marshal(map[string]any{"data": []any{action, map[string]any{"id": "sevt_idle_interrupt", "type": "user.interrupt", "processed_at": nil}}, "next_page": nil})
			output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/stream"):
					streamHeaders(w)
					<-r.Context().Done()
				case r.Method == "POST":
					var body struct {
						Events []map[string]any `json:"events"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					sent = append(sent, body.Events...)
					body.Events[0]["id"] = fmt.Sprintf("sevt_receipt_%d", len(sent))
					body.Events[0]["processed_at"] = nil
					_ = json.NewEncoder(w).Encode(map[string]any{"data": body.Events})
				case strings.Contains(r.URL.Path, "/threads/sthr_child/events"):
					jsonReply(w, `{"data":[{"id":"sevt_local","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","processed_at":null},{"id":"sevt_child_idle_interrupt","type":"user.interrupt","processed_at":null}],"next_page":null}`)
				case strings.HasSuffix(r.URL.Path, "/events"):
					jsonReply(w, string(history))
				default:
					jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
				}
			}, io.NopCloser(strings.NewReader("/interrupt\n/allow sevt_wait\n/quit\n")), nil)
			if err != nil || len(sent) != 2 || sent[1]["tool_use_id"] != "sevt_wait" || sent[1]["result"] != "allow" {
				t.Fatalf("interrupt hid pending approval: sent=%v, err=%v, output=%s", sent, err, output)
			}
		})
	}
}

func TestConnectCorrelatesChildLocalApprovalWithRelayedAction(t *testing.T) {
	var sends atomic.Int32
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			jsonReply(w, `{"data":[]}`)
		case strings.Contains(r.URL.Path, "/threads/sthr_child/events"):
			jsonReply(w, `{"data":[{"id":"sevt_local","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","processed_at":null},{"id":"sevt_child_allow","type":"user.tool_confirmation","tool_use_id":"sevt_local","result":"allow","processed_at":null}],"next_page":null}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[{"id":"sevt_relay","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":"sthr_child","source_event_id":"sevt_local","processed_at":null}],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/allow sevt_relay\n/quit\n")), nil)
	if err != nil || sends.Load() != 0 || strings.Contains(output, "Approval pending: sevt_relay") {
		t.Fatalf("local child confirmation not reconciled: sends=%d, err=%v, output=%s", sends.Load(), err, output)
	}
}

func TestConnectReconcilesQueuedEventMovingBehindHistoryCursor(t *testing.T) {
	var passes atomic.Int32
	queued := func(id int) map[string]any {
		return map[string]any{"id": fmt.Sprintf("sevt_%d", id), "type": "user.message", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("queued-%d", id)}}, "processed_at": nil}
	}
	output, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			// All 1001 rows predate subscription; changing processed_at does not
			// allocate a new sequence and therefore emits no live event for row 1001.
			streamHeaders(w)
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/events"):
			var data []any
			var next any
			if r.URL.Query().Get("page") == "" {
				n := passes.Add(1)
				if n > 1 {
					event := queued(1001)
					event["processed_at"] = "2026-10-03T00:00:00Z"
					data = append(data, event)
				}
				for id := 1; len(data) < 1000; id++ {
					data = append(data, queued(id))
				}
				next = "null-cursor"
			} else if passes.Load() > 1 {
				data = append(data, queued(1000))
			}
			if data == nil {
				data = []any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "next_page": next})
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/quit\n")), nil)
	if err != nil || strings.Count(output, "queued-1001\n") != 1 || strings.Count(output, "[user.message]") != 1001 {
		t.Fatalf("queued row missed or duplicated: passes=%d, err=%v, displayed=%d", passes.Load(), err, strings.Count(output, "[user.message]"))
	}
}

func TestConnectAcceptsInterruptNoopReceipt(t *testing.T) {
	var sends atomic.Int32
	_, err := runConnect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			streamHeaders(w)
			<-r.Context().Done()
		case r.Method == "POST":
			sends.Add(1)
			jsonReply(w, `{"data":[]}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonReply(w, `{"data":[],"next_page":null}`)
		default:
			jsonReply(w, `{"id":"sesn_test","status":"idle"}`)
		}
	}, io.NopCloser(strings.NewReader("/interrupt\n/quit\n")), nil)
	if err != nil || sends.Load() != 1 {
		t.Fatalf("valid interrupt no-op rejected: sends=%d,err=%v", sends.Load(), err)
	}
}
