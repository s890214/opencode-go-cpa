package chatcompletions

import (
	"encoding/json"
	"testing"

	"opencode-go-cpa/internal/adapter/shared"
)

type replayedMessage struct {
	Role             string  `json:"role"`
	ReasoningContent *string `json:"reasoning_content"`
	ToolCalls        []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

func decodeMessages(t *testing.T, raw []byte) []replayedMessage {
	t.Helper()
	var req struct {
		Messages []replayedMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode outbound request: %v", err)
	}
	return req.Messages
}

func assistantWithCalls(t *testing.T, msgs []replayedMessage) replayedMessage {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			return m
		}
	}
	t.Fatalf("no assistant tool-call message in %+v", msgs)
	return replayedMessage{}
}

// A Responses client (Codex) replays only the call and its output. Without
// reasoning_content the upstream answers 400 in thinking mode, so the field
// must exist even when the text is unknown.
func TestResponsesToolCallGetsReasoningContent(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	msg := assistantWithCalls(t, decodeMessages(t, out))
	if msg.ReasoningContent == nil {
		t.Fatalf("reasoning_content missing: %+v", msg)
	}
	if *msg.ReasoningContent != "" {
		t.Fatalf("unknown call should send an empty placeholder, got %q", *msg.ReasoningContent)
	}
}

// A captured upstream answer is replayed verbatim.
func TestResponsesToolCallReplaysCapturedReasoning(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	shared.DefaultReasoningStore().Put("c1", "the user wants weather")
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	msg := assistantWithCalls(t, decodeMessages(t, out))
	if msg.ReasoningContent == nil || *msg.ReasoningContent != "the user wants weather" {
		t.Fatalf("reasoning_content = %v", msg.ReasoningContent)
	}
}

// A native Chat Completions client loses the field as well when it trims
// history; the same backfill applies on the passthrough leg.
func TestOpenAIHistoryGetsReasoningContent(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c9","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c9","content":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	msg := assistantWithCalls(t, decodeMessages(t, out))
	if msg.ReasoningContent == nil {
		t.Fatalf("reasoning_content missing: %+v", msg)
	}
}

// An existing reasoning_content is never overwritten.
func TestExistingReasoningContentPreserved(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	shared.DefaultReasoningStore().Put("c9", "cached")
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"reasoning_content":"from client","tool_calls":[{"id":"c9","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c9","content":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v", eErr)
	}
	msg := assistantWithCalls(t, decodeMessages(t, out))
	if msg.ReasoningContent == nil || *msg.ReasoningContent != "from client" {
		t.Fatalf("reasoning_content = %v, want the client value", msg.ReasoningContent)
	}
}
