package messages

import "testing"

func blocksOf(msgs []anthropicMessage, i int) []anthropicBlock {
	return anthropicBlocksOf(msgs[i].Content)
}

// sanitizeAnthropicMessages enforces the invariants Anthropic rejects at
// request time. These cases pin each one independently, because a regression
// here surfaces as an opaque 400 on a client path nobody tests by hand.
func TestSanitizeInsertsLeadingUser(t *testing.T) {
	in := []anthropicMessage{
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "hi"},
	}
	out := sanitizeAnthropicMessages(in)
	if len(out) != 3 || out[0].Role != "user" {
		t.Fatalf("leading user not inserted: %#v", out)
	}
}

func TestSanitizeDropsEmptyMessages(t *testing.T) {
	in := []anthropicMessage{
		{Role: "user", Content: ""},
		{Role: "user", Content: "real"},
	}
	out := sanitizeAnthropicMessages(in)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("empty message kept: %#v", out)
	}
}

func TestSanitizeDropsIncompleteToolTurn(t *testing.T) {
	in := []anthropicMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: []anthropicBlock{
			{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{}},
		}},
		// No tool_result follows: the tool_use turn would 400.
		{Role: "user", Content: "next"},
	}
	out := sanitizeAnthropicMessages(in)
	for _, msg := range out {
		for _, blk := range anthropicBlocksOf(msg.Content) {
			if blk["type"] == "tool_use" {
				t.Fatalf("unmatched tool_use survived: %#v", out)
			}
		}
	}
}

func TestSanitizeKeepsCompleteToolPair(t *testing.T) {
	in := []anthropicMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: []anthropicBlock{
			{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{}},
		}},
		{Role: "user", Content: []anthropicBlock{
			{"type": "tool_result", "tool_use_id": "c1", "content": "ok"},
		}},
	}
	out := sanitizeAnthropicMessages(in)
	if len(out) != 3 {
		t.Fatalf("complete pair dropped: %#v", out)
	}
	if len(blocksOf(out, 1)) != 1 || blocksOf(out, 1)[0]["type"] != "tool_use" {
		t.Fatalf("tool_use missing: %#v", out)
	}
}

func TestSanitizeDropsUnsignedThinkingOnToolTurn(t *testing.T) {
	in := []anthropicMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: []anthropicBlock{
			{"type": "thinking", "thinking": "unsigned"},
			{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{}},
		}},
		{Role: "user", Content: []anthropicBlock{
			{"type": "tool_result", "tool_use_id": "c1", "content": "ok"},
		}},
	}
	out := sanitizeAnthropicMessages(in)
	for _, blk := range blocksOf(out, 1) {
		if blk["type"] == "thinking" {
			t.Fatalf("unsigned thinking survived: %#v", out)
		}
	}
}
