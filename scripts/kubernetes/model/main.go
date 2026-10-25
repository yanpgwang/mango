// Command model is an explicitly simulated Messages endpoint for isolated
// Kubernetes acceptance. It is never included in either Mango runtime image.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type block struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   any    `json:"content"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool      `json:"stream"`
			Messages []message `json:"messages"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var instruction string
		for _, msg := range request.Messages {
			for _, b := range msg.Content {
				if msg.Role == "user" && b.Type == "text" && strings.HasPrefix(b.Text, "alpha:") {
					instruction = b.Text
				}
			}
		}
		id, name := "", ""
		var input map[string]any
		switch instruction {
		case "alpha:write":
			id, name = "provider_write", "bash"
			input = map[string]any{"command": "grep -q alpha-skill-bundle skills/alpha-skill/SKILL.md && printf 'alpha-proof\\n' >> proof.txt && printf alpha-memory > /mnt/memory/alpha-memory/proof.txt && cat proof.txt /mnt/memory/alpha-memory/proof.txt && printf ' alpha-skill-bundle'"}
		case "alpha:read":
			id, name = "provider_read", "bash"
			input = map[string]any{"command": "cat proof.txt /mnt/memory/alpha-memory/proof.txt && grep alpha-skill-bundle skills/alpha-skill/SKILL.md"}
		case "alpha:custom":
			id, name = "provider_custom", "checkpoint"
			input = map[string]any{"label": "alpha-proof"}
		default:
			http.Error(w, "unknown fixture instruction", http.StatusUnprocessableEntity)
			return
		}
		content := map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
		stop := "tool_use"
		for _, msg := range request.Messages {
			for _, b := range msg.Content {
				if msg.Role != "user" || b.Type != "tool_result" || b.ToolUseID != id {
					continue
				}
				raw, err := json.Marshal(b.Content)
				valid := err == nil && !b.IsError && strings.Contains(string(raw), "alpha-proof")
				if name == "bash" {
					valid = valid && strings.Contains(string(raw), "alpha-skill-bundle") && strings.Contains(string(raw), "alpha-memory")
				}
				if !valid {
					http.Error(w, "invalid correlated fixture result", http.StatusUnprocessableEntity)
					return
				}
				content = map[string]any{"type": "text", "text": "completed " + id}
				stop = "end_turn"
			}
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{content}, "stop_reason": stop, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 10}}},
			{"type": "content_block_start", "index": 0, "content_block": content},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 5}},
			{"type": "message_stop"},
		} {
			data, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data); err != nil {
				return
			}
		}
	})
	return mux
}

func main() {
	server := &http.Server{Addr: ":8081", Handler: handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
