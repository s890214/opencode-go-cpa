package responses

import "testing"

// Responses function tools must carry strict:false. Without it some upstreams
// promote every optional property into required, which silently changes the
// tool contract the client declared.
func TestChatToolsCarryStrictFalse(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`
	out, eErr := BuildRequest("m", "openai", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	m := decodeReq(t, out)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", m["tools"])
	}
	tm, _ := tools[0].(map[string]any)
	v, present := tm["strict"]
	if !present || v != false {
		t.Fatalf("strict = %#v (present=%v), want false", v, present)
	}
}

// Claude tools take the same path; the flag must survive both request adapters.
func TestClaudeToolsCarryStrictFalse(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{}}}]}`
	out, eErr := BuildRequest("m", "claude", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("build: %v", eErr)
	}
	m := decodeReq(t, out)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", m["tools"])
	}
	tm, _ := tools[0].(map[string]any)
	if v, present := tm["strict"]; !present || v != false {
		t.Fatalf("strict = %#v (present=%v), want false", v, present)
	}
}
