// Thinking-mode pass-back capture.
//
// The upstream runs tool-calling turns in thinking mode and refuses the
// follow-up turn when the assistant message that produced the call no
// longer carries its reasoning (400 "The `reasoning_content` in the
// thinking mode must be passed back to the API"). Clients speaking the
// OpenAI Responses protocol only replay the call item and its output, and
// the upstream Responses endpoint does not hand them the reasoning
// (reasoning_tokens is 0), so nothing is left to send back.
//
// This file records what the upstream actually said while answering, keyed
// by tool call id, so the request translators in the adapters can replay
// it. Capture is deliberately best-effort and side-effect free: a body
// that does not parse is simply not cached, never an execution error.
package plugin

import (
	"encoding/json"
	"strings"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/catalog"
	"opencode-go-cpa/internal/reasoning"
)

// captureNonStreamReasoning records thinking text from a complete upstream
// response body (before translation, so every client protocol benefits).
func captureNonStreamReasoning(route catalog.Route, body []byte) {
	if len(body) == 0 {
		return
	}
	switch route {
	case catalog.RouteChatCompletions:
		captureChatBody(body)
	case catalog.RouteResponses:
		captureResponsesBody(body)
	}
}

func captureChatBody(body []byte) {
	var env struct {
		Choices []struct {
			Message struct {
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &env) != nil {
		return
	}
	store := shared.DefaultReasoningStore()
	for _, choice := range env.Choices {
		if choice.Message.ReasoningContent == nil || len(choice.Message.ToolCalls) == 0 {
			continue
		}
		for _, call := range choice.Message.ToolCalls {
			store.Put(call.ID, *choice.Message.ReasoningContent)
		}
	}
}

func captureResponsesBody(body []byte) {
	var env struct {
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Output) == 0 {
		return
	}
	captureResponsesItems(env.Output)
}

// captureResponsesItems binds the reasoning text of one response to every
// tool call that response contains. A Responses answer may interleave
// message, reasoning, and function_call items, so both sides are collected
// before the bind.
func captureResponsesItems(items []json.RawMessage) {
	var text strings.Builder
	var callIDs []string
	for _, item := range items {
		var meta struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &meta) != nil {
			continue
		}
		switch meta.Type {
		case "reasoning":
			text.WriteString(reasoning.TextFromResponseItem(item))
		case "function_call", "custom_tool_call":
			if meta.CallID != "" {
				callIDs = append(callIDs, meta.CallID)
			}
		}
	}
	if len(callIDs) == 0 {
		return
	}
	store := shared.DefaultReasoningStore()
	for _, id := range callIDs {
		store.Put(id, text.String())
	}
}

// reasoningTee accumulates thinking text and tool call ids from an
// upstream SSE stream so the same binding can happen for streaming
// requests, where the executor only ever sees raw chunks.
type reasoningTee struct {
	route  catalog.Route
	active bool
	text   strings.Builder
	calls  []string
	seen   map[string]struct{}
}

func newReasoningTee(route catalog.Route) *reasoningTee {
	return &reasoningTee{route: route, active: true, seen: make(map[string]struct{})}
}

// Feed consumes one upstream chunk (possibly several SSE lines). Errors are
// swallowed by design: capture must never fail a live request.
func (t *reasoningTee) Feed(chunk []byte) {
	if t == nil || !t.active || len(chunk) == 0 {
		return
	}
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		switch t.route {
		case catalog.RouteChatCompletions:
			t.feedChatChunk(payload)
		case catalog.RouteResponses:
			t.feedResponsesChunk(payload)
		}
	}
}

func (t *reasoningTee) feedChatChunk(payload string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.ReasoningContent != nil {
			t.text.WriteString(*choice.Delta.ReasoningContent)
		}
		for _, call := range choice.Delta.ToolCalls {
			t.addCall(call.ID)
		}
	}
}

func (t *reasoningTee) feedResponsesChunk(payload string) {
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(payload), &event) != nil {
		return
	}
	switch event.Type {
	case "response.output_item.done", "response.output_item.added":
		if len(event.Item) > 0 {
			var meta struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(event.Item, &meta) == nil {
				if meta.Type == "reasoning" {
					t.text.WriteString(reasoning.TextFromResponseItem(event.Item))
				}
				if meta.Type == "function_call" || meta.Type == "custom_tool_call" {
					t.addCall(meta.CallID)
				}
			}
		}
	case "response.completed":
		// The terminal event carries the authoritative item list; prefer
		// it when the incremental events were sparse.
		if len(event.Response.Output) > 0 {
			for _, item := range event.Response.Output {
				var meta struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
				}
				if json.Unmarshal(item, &meta) != nil {
					continue
				}
				switch meta.Type {
				case "reasoning":
					if t.text.Len() == 0 {
						t.text.WriteString(reasoning.TextFromResponseItem(item))
					}
				case "function_call", "custom_tool_call":
					t.addCall(meta.CallID)
				}
			}
		}
	}
}

func (t *reasoningTee) addCall(id string) {
	if id == "" {
		return
	}
	if _, dup := t.seen[id]; dup {
		return
	}
	t.seen[id] = struct{}{}
	t.calls = append(t.calls, id)
}

// Flush binds the accumulated text to every call seen in the stream. It is
// safe to call more than once; the tee deactivates after the first bind so
// a late duplicate flush cannot double-write.
func (t *reasoningTee) Flush() {
	if t == nil || !t.active {
		return
	}
	t.active = false
	if len(t.calls) == 0 {
		return
	}
	text := t.text.String()
	store := shared.DefaultReasoningStore()
	for _, id := range t.calls {
		store.Put(id, text)
	}
}
