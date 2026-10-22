package chatcompletions

import "testing"

// Responses tool results may carry screenshots. Chat tool messages are
// text-only, so the media must move to a synthetic user message that follows
// the tool result and precedes the next ordinary item.
func TestResponsesToChatMovesToolMediaToUserMessage(t *testing.T) {
	body := `{"model":"m","stream":false,"input":[
		{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"text","text":"here:"},
			{"type":"input_image","image_url":"https://example.test/y.png"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}],
		"tools":[{"type":"function","name":"shot","parameters":{"type":"object","properties":{}}}]}`
	out := mustBuild(t, "openai-response", body, nil)
	rawMsgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages is %T", out["messages"])
	}
	if len(rawMsgs) != 4 {
		t.Fatalf("want 4 messages (assistant, tool, user-media, user-next), got %d: %v", len(rawMsgs), rawMsgs)
	}
	roles := make([]string, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		roles = append(roles, role)
	}
	want := []string{"assistant", "tool", "user", "user"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	// The tool message keeps the marker text, not the image.
	toolMsg, _ := rawMsgs[1].(map[string]any)
	text, _ := toolMsg["content"].(string)
	if text == "" || text == "here:" {
		t.Fatalf("tool content = %q, want marker appended to text", text)
	}
	// The synthetic user message carries the image part.
	mediaMsg, _ := rawMsgs[2].(map[string]any)
	parts, ok := mediaMsg["content"].([]any)
	if !ok || len(parts) < 2 {
		t.Fatalf("media content = %#v", mediaMsg["content"])
	}
	foundImage := false
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] == "image_url" {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("no image_url part in %#v", parts)
	}
}

// Text-only tool results must keep the legacy wire shape byte for byte.
func TestResponsesToChatTextOnlyToolResultUnchanged(t *testing.T) {
	body := `{"model":"m","stream":false,"input":[
		{"type":"function_call","call_id":"c1","name":"run","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"plain output"}],
		"tools":[{"type":"function","name":"run","parameters":{"type":"object","properties":{}}}]}`
	out := mustBuild(t, "openai-response", body, nil)
	rawMsgs, _ := out["messages"].([]any)
	if len(rawMsgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(rawMsgs))
	}
	toolMsg, _ := rawMsgs[1].(map[string]any)
	if toolMsg["content"] != "plain output" {
		t.Fatalf("tool content = %#v", toolMsg["content"])
	}
}
