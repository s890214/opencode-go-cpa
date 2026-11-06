package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/errclass"
)

func searchRegistry(t *testing.T) (*shared.ResponseTools, string) {
	t.Helper()
	rt := shared.NewResponseTools()
	out, e := BuildRequest("m", "openai-response", []byte(`{"input":"find tools","tools":[{"type":"tool_search","execution":"client"}]}`), nil, rt)
	if e != nil {
		t.Fatal(e)
	}
	var req shared.ChatCompletionsRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("client search declaration lost: %s", out)
	}
	return rt, req.Tools[0].Function.Name
}

func TestChatToolSearchNonStream(t *testing.T) {
	rt, name := searchRegistry(t)
	for _, args := range []string{`{"query":"calendar"}`, `{"query":`, ""} {
		t.Run(args, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"s","function":{"name":%q,"arguments":%q}}]}}]}`, name, args)
			out, e := ConvertNonStreamResponse("openai-response", 200, []byte(body), rt)
			if e != nil {
				t.Fatal(e)
			}
			var resp struct {
				Output []map[string]any `json:"output"`
			}
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatal(err)
			}
			item := resp.Output[0]
			// Absent arguments decode to an empty object, which is a
			// well-formed call; only unparsable arguments are incomplete.
			status := "completed"
			if args == `{"query":` {
				status = "incomplete"
			}
			if item["type"] != "tool_search_call" || item["execution"] != "client" || item["call_id"] != "s" || item["id"] != "tsc_s" || item["status"] != status {
				t.Fatalf("wrong search item: %s", out)
			}
			obj, ok := item["arguments"].(map[string]any)
			if !ok {
				t.Fatalf("arguments must be object: %s", out)
			}
			if args == `{"query":"calendar"}` && obj["query"] != "calendar" {
				t.Fatalf("lost arguments: %s", out)
			}
			if _, exists := item["name"]; exists {
				t.Fatalf("shim name leaked: %s", out)
			}
		})
	}
}

func TestStreamToolSearchLifecycle(t *testing.T) {
	rt, name := searchRegistry(t)
	for _, args := range []string{`{"query":"calendar"}`, ""} {
		t.Run(args, func(t *testing.T) {
			sc := NewStreamConverter("openai-response", rt)
			first := fmt.Sprintf(`data: {"id":"r","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"s","function":{"name":%q,"arguments":""}}]}}]}`, name)
			middle := fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, args)
			events := feedAll(t, sc, first, middle, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`)
			var added, done, completed map[string]any
			for _, event := range events {
				if strings.HasPrefix(event.Name, "response.function_call_arguments.") {
					t.Fatalf("search emitted function event: %v", event)
				}
				switch event.Name {
				case "response.output_item.added":
					added = event.Data["item"].(map[string]any)
				case "response.output_item.done":
					done = event.Data["item"].(map[string]any)
				case "response.completed":
					completed = event.Data["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
				}
			}
			if added == nil || done == nil || completed == nil {
				t.Fatalf("missing search lifecycle: %v", events)
			}
			if added["type"] != "tool_search_call" || added["status"] != "in_progress" {
				t.Fatalf("wrong announcement: %v", added)
			}
			if done["type"] != "tool_search_call" || done["status"] != "completed" || done["id"] != added["id"] {
				t.Fatalf("wrong completion: %v", done)
			}
			a, _ := json.Marshal(done)
			b, _ := json.Marshal(completed)
			if string(a) != string(b) {
				t.Fatalf("done/completed differ: %s / %s", a, b)
			}
		})
	}
}

func TestChatToolSearchObjectHistoryAndNameCollision(t *testing.T) {
	body := `{"tools":[{"type":"function","name":"tool_search"},{"type":"tool_search","execution":"client"}],"tool_choice":{"type":"tool_search"},"input":[{"role":"user","content":"find"},{"type":"tool_search_call","call_id":"s","arguments":{"query":"calendar"}},{"type":"tool_search_output","call_id":"s","status":"completed","tools":[{"type":"namespace","name":"cal","tools":[{"type":"function","name":"list"}]}]}]}`
	rt := shared.NewResponseTools()
	out, e := BuildRequest("m", "openai-response", []byte(body), nil, rt)
	if e != nil {
		t.Fatal(e)
	}
	var req shared.ChatCompletionsRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 3 || req.Messages[1].ToolCalls[0].Function.Name != "tool_search_1" {
		t.Fatalf("collision/discovery failed: %s", out)
	}
	if !strings.Contains(string(req.ToolChoice), "tool_search_1") {
		t.Fatalf("wrong choice: %s", out)
	}
	for _, name := range []string{"tool_search", "tool_search_1", "cal__list"} {
		body := fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":%q,"arguments":"{}"}}]}}]}`, name)
		converted, e := ConvertNonStreamResponse("openai-response", 200, []byte(body), rt)
		if e != nil {
			t.Fatal(e)
		}
		want := "function_call"
		if name == "tool_search_1" {
			want = "tool_search_call"
		}
		if !strings.Contains(string(converted), `"type":"`+want+`"`) {
			t.Fatalf("wrong restoration: %s", converted)
		}
		if name == "cal__list" && !strings.Contains(string(converted), `"namespace":"cal"`) {
			t.Fatalf("lost namespace: %s", converted)
		}
	}
}

func TestChatToolSearchDeclarationKeepsClientShape(t *testing.T) {
	body := `{"tools":[
		{"type":"tool_search","description":"Search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}},
		{"type":"tool_search","execution":"server"}
	],"input":"hi"}`
	rt := shared.NewResponseTools()
	out, e := BuildRequest("m", "openai-response", []byte(body), nil, rt)
	if e != nil {
		t.Fatal(e)
	}
	var req shared.ChatCompletionsRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "tool_search" {
		t.Fatalf("absent execution must bridge as client search, server search must not: %s", out)
	}
	if req.Tools[0].Function.Description != "Search deferred tools" {
		t.Fatalf("client description lost: %s", out)
	}
	if !strings.Contains(string(req.Tools[0].Function.Parameters), `"limit"`) {
		t.Fatalf("client parameters narrowed: %s", out)
	}
	if len(rt.Losses()) != 1 {
		t.Fatalf("server-side search must be reported as one loss: %v", rt.Losses())
	}
}

func TestChatToolSearchChoiceWithoutDeclarationIsRejected(t *testing.T) {
	body := `{"tools":[{"type":"function","name":"other"}],"tool_choice":{"type":"tool_search"},"input":"hi"}`
	rt := shared.NewResponseTools()
	if _, e := BuildRequest("m", "openai-response", []byte(body), nil, rt); e == nil {
		t.Fatal("a tool_search choice without the declaration must stay unsupported")
	} else if e.Class != errclass.ClassTranslation {
		t.Fatalf("class = %v, want translation", e.Class)
	}
}
