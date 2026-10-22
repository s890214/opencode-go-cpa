package chatcompletions

import (
	"encoding/json"
	"testing"

	"opencode-go-cpa/internal/adapter/shared"
)

// Codex replays one model turn as a reasoning item, an assistant message and
// several function_call items. Chat must receive that as a single assistant
// message: a standalone text-only assistant turn is treated as complete, and
// thinking-mode upstreams reject a tool turn whose text message lost the
// reasoning_content.
func TestCodexTurnMergesCommentaryIntoCallMessage(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"check the disk"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"THINK"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will check."}]},
		{"type":"function_call","call_id":"c1","name":"exec","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"a"},
		{"type":"function_call_output","call_id":"c2","output":"b"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	var req struct {
		Messages []struct {
			Role             string  `json:"role"`
			Content          any     `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var assistant int
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		assistant++
		if len(m.ToolCalls) != 2 {
			t.Fatalf("assistant tool_calls = %d, want 2: %+v", len(m.ToolCalls), m)
		}
		if m.Content != "I will check." {
			t.Fatalf("commentary lost: %#v", m.Content)
		}
		if m.ReasoningContent == nil || *m.ReasoningContent != "THINK" {
			t.Fatalf("reasoning_content = %v, want THINK", m.ReasoningContent)
		}
	}
	if assistant != 1 {
		t.Fatalf("assistant messages = %d, want 1 (one turn must stay one message)", assistant)
	}
}

// Thinking-mode upstreams require a non-empty placeholder, never "" or null.
func TestToolCallPlaceholderIsNonEmpty(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	msg := assistantWithCalls(t, decodeMessages(t, out))
	if msg.ReasoningContent == nil || *msg.ReasoningContent != toolCallPlaceholder {
		t.Fatalf("reasoning_content = %v, want %q", msg.ReasoningContent, toolCallPlaceholder)
	}
}

// Responses may replay the commentary message after the calls. DeepSeek
// requires it folded back into the tool-call message; other providers keep
// the history untouched so their caches stay stable.
func TestCoalesceReplayedToolTurnOnlyForDeepSeek(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`)
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, merged assistant, tool): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].Content != "Checking." {
		t.Fatalf("assistant turn not coalesced: %+v", req.Messages[1])
	}

	shared.DefaultReasoningStore().Reset()
	out, eErr = BuildRequest("gpt-6-luna", "openai-response", body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest(gpt): %+v", eErr)
	}
	var other struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &other); err != nil {
		t.Fatalf("decode(gpt): %v", err)
	}
	if len(other.Messages) != 4 {
		t.Fatalf("gpt history rewritten: %d messages, want 4", len(other.Messages))
	}
}
