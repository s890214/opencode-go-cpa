package responses

import (
	"testing"

	"opencode-go-cpa/internal/adapter/shared"
)

const toolTurnBody = `{"input":[
	{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
	{"type":"function_call_output","call_id":"c1","output":"ok"}
]}`

// Thinking mode rejects a replayed tool call that has no reasoning ahead
// of it (400 "The `reasoning_content` in the thinking mode must be passed
// back"). Codex never sends one, so a placeholder must be inserted.
func TestFunctionCallGetsReasoningPlaceholder(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(toolTurnBody), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	items := inputItems(t, decodeReq(t, out))
	if len(items) != 4 {
		t.Fatalf("items = %d: %v", len(items), items)
	}
	if got := itemMap(t, items, 1); got["type"] != "reasoning" {
		t.Fatalf("item1 = %v, want reasoning placeholder", got)
	}
	if got := itemMap(t, items, 2); got["type"] != "function_call" {
		t.Fatalf("item2 = %v, want function_call", got)
	}
}

// Once the upstream's own thinking for that call was captured, the
// placeholder is replaced by the real text.
func TestFunctionCallReplaysCachedReasoning(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	shared.DefaultReasoningStore().Put("c1", "I need the lookup tool")
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(toolTurnBody), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	rs := itemMap(t, inputItems(t, decodeReq(t, out)), 1)
	summary, ok := rs["summary"].([]any)
	if !ok || len(summary) != 1 {
		t.Fatalf("summary = %v", rs["summary"])
	}
	part, ok := summary[0].(map[string]any)
	if !ok || part["text"] != "I need the lookup tool" {
		t.Fatalf("summary part = %v", summary[0])
	}
}

// A request that already carries reasoning only keeps it when a tool call
// follows; orphaned history items stay dropped.
func TestOrphanReasoningStillDropped(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"idle thought"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	for _, item := range inputItems(t, decodeReq(t, out)) {
		if im, ok := item.(map[string]any); ok && im["type"] == "reasoning" {
			t.Fatalf("orphan reasoning leaked: %v", item)
		}
	}
}

// Chat-sourced history has the same requirement on the Responses wire.
func TestChatToolCallsGetReasoningItem(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	items := inputItems(t, decodeReq(t, out))
	want := -1
	for i, item := range items {
		if im, ok := item.(map[string]any); ok && im["type"] == "reasoning" {
			want = i
		}
	}
	if want < 0 {
		t.Fatalf("no reasoning item in %v", items)
	}
	if after, ok := items[want+1].(map[string]any); !ok || after["type"] != "function_call" {
		t.Fatalf("reasoning must precede the call, got %v", items[want+1])
	}
}
