package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

const geminiText = `{
  "responseId": "resp-1",
  "modelVersion": "gemini-4-argon",
  "candidates": [{"content": {"role": "model", "parts": [{"text": "hello "}, {"text": "world"}]}, "finishReason": "STOP"}],
  "usageMetadata": {"promptTokenCount": 11, "candidatesTokenCount": 7, "thoughtsTokenCount": 3, "cachedContentTokenCount": 5, "totalTokenCount": 21}
}`

const geminiToolCall = `{
  "candidates": [{"content": {"role": "model", "parts": [
    {"text": "checking"},
    {"functionCall": {"name": "get_time", "args": {"zone": "UTC"}}}
  ]}, "finishReason": "STOP"}],
  "usageMetadata": {"promptTokenCount": 4, "candidatesTokenCount": 2}
}`

func TestFromChatRequestMapsToolsAndSystem(t *testing.T) {
	body := []byte(`{
	  "messages": [
	    {"role": "system", "content": "be brief"},
	    {"role": "user", "content": "what time is it"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_1_ET_abc", "type": "function", "function": {"name": "get_time", "arguments": "{\"zone\":\"UTC\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1_ET_abc", "content": "12:00"}
	  ],
	  "tools": [{"type": "function", "function": {"name": "get_time", "description": "clock", "parameters": {"type": "object", "properties": {"zone": {"type": "string"}}, "required": ["zone"], "additionalProperties": false}}}],
	  "max_tokens": 64,
	  "reasoning_effort": "low"
	}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "be brief" {
		t.Fatalf("system instruction = %+v", req.SystemInstruction)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("contents = %d, want 3: %s", len(req.Contents), out)
	}
	if req.Contents[0].Role != "user" || req.Contents[0].Parts[0].Text != "what time is it" {
		t.Fatalf("user turn = %+v", req.Contents[0])
	}
	if req.Contents[1].Role != "model" || req.Contents[1].Parts[0].FunctionCall == nil || req.Contents[1].Parts[0].FunctionCall.Name != "get_time" {
		t.Fatalf("assistant turn = %+v", req.Contents[1])
	}
	resp := req.Contents[2].Parts[0].FunctionResponse
	if resp == nil || resp.Name != "get_time" {
		t.Fatalf("tool result = %+v", req.Contents[2])
	}
	if !strings.Contains(string(resp.Response), "12:00") {
		t.Fatalf("tool response payload = %s", resp.Response)
	}
	if len(req.Tools) != 1 || req.Tools[0].FunctionDeclarations[0].Name != "get_time" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if strings.Contains(string(req.Tools[0].FunctionDeclarations[0].Parameters), "additionalProperties") {
		t.Fatalf("schema not sanitized: %s", req.Tools[0].FunctionDeclarations[0].Parameters)
	}
	if req.GenerationConfig == nil || req.GenerationConfig.MaxOutputTokens != 64 {
		t.Fatalf("generation config = %+v", req.GenerationConfig)
	}
}

func TestFromChatRequestRequiresContent(t *testing.T) {
	if _, eErr := FromChatRequest([]byte(`{"messages":[]}`), nil); eErr == nil {
		t.Fatal("expected an error for an empty conversation")
	}
}

func TestFromChatRequestDataURLImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
	  {"type":"text","text":"what is this"},
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
	]}]}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	_ = json.Unmarshal(out, &req)
	parts := req.Contents[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "aGVsbG8=" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestConvertNonStreamChat(t *testing.T) {
	out, eErr := ConvertNonStreamResponse("openai", 200, []byte(geminiText))
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if resp.Choices[0].Message.Content != "hello world" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", resp.Choices[0])
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 10 || resp.Usage.TotalTokens != 21 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestConvertNonStreamClaudeToolUse(t *testing.T) {
	out, eErr := ConvertNonStreamResponse("claude", 200, []byte(geminiToolCall))
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	var resp struct {
		Type    string `json:"type"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if resp.Type != "message" || len(resp.Content) < 2 {
		t.Fatalf("claude message = %s", out)
	}
	var sawTool bool
	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == "get_time" && strings.Contains(string(block.Input), "UTC") {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("missing tool_use block: %s", out)
	}
}

func TestConvertNonStreamResponsesFunctionCall(t *testing.T) {
	out, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(geminiToolCall))
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	if !strings.Contains(string(out), `"type":"function_call"`) || !strings.Contains(string(out), `"name":"get_time"`) {
		t.Fatalf("responses payload = %s", out)
	}
}

func TestStreamConverterTranslatesChunks(t *testing.T) {
	conv := NewStreamConverter("openai")
	upstream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"he\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"llo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n"
	var events [][]byte
	// Feed in awkward slices to prove line buffering across chunks.
	for i := 0; i < len(upstream); i += 7 {
		end := i + 7
		if end > len(upstream) {
			end = len(upstream)
		}
		out, done, eErr := conv.Feed([]byte(upstream[i:end]))
		if eErr != nil {
			t.Fatalf("Feed: %v", eErr)
		}
		events = append(events, out...)
		if done {
			break
		}
	}
	events = append(events, conv.Flush()...)
	joined := string(bytesJoin(events))
	for _, want := range []string{`"content":"he"`, `"content":"llo"`, `"finish_reason":"stop"`, `"prompt_tokens":2`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream output missing %q: %s", want, joined)
		}
	}
}

func TestStreamConverterToolCall(t *testing.T) {
	conv := NewStreamConverter("claude")
	chunk := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"get_time\",\"args\":{\"zone\":\"UTC\"}}}]},\"finishReason\":\"STOP\"}]}\n\n"
	out, _, eErr := conv.Feed([]byte(chunk))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	out = append(out, conv.Flush()...)
	joined := string(bytesJoin(out))
	if !strings.Contains(joined, "tool_use") || !strings.Contains(joined, "get_time") {
		t.Fatalf("claude stream = %s", joined)
	}
}

func TestUpstreamURL(t *testing.T) {
	got := UpstreamURL("https://opencode.ai/inference/go/google/v1beta/models", "gemini-4-argon", true)
	want := "https://opencode.ai/inference/go/google/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse"
	if got != want {
		t.Fatalf("stream url = %s, want %s", got, want)
	}
	got = UpstreamURL("https://x/google/v1beta/models/", "models/gemini-4-argon", false)
	want = "https://x/google/v1beta/models/gemini-4-argon:generateContent"
	if got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
}

func bytesJoin(chunks [][]byte) []byte {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

func TestStreamChunkCarriesModelAndSkipsZeroUsage(t *testing.T) {
	conv := NewStreamConverter("openai")
	frame := []byte("data: " + `{"modelVersion":"gemini-4-argon","candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}}` + "\n")
	events, _, eErr := conv.Feed(frame)
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d: %q", len(events), events)
	}
	got := string(events[0])
	if !strings.Contains(got, `"model":"gemini-4-argon"`) {
		t.Fatalf("chunk lost the model: %s", got)
	}
	if strings.Contains(got, "usage") {
		t.Fatalf("zero-usage trailer must not be emitted: %s", got)
	}

	final := []byte("data: " + `{"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}` + "\n")
	events, _, eErr = conv.Feed(final)
	if eErr != nil {
		t.Fatalf("Feed final: %v", eErr)
	}
	var sawUsage bool
	for _, ev := range events {
		if strings.Contains(string(ev), `"usage":{"prompt_tokens":7`) {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatalf("final usage trailer missing: %q", events)
	}
}

// TestFromChatRequestMergesParallelToolResults pins the batch contract: all
// results of one parallel tool call must land in a single user turn, in the
// same order as the model turn's functionCall parts, each carrying the call
// id for exact upstream pairing.
func TestFromChatRequestMergesParallelToolResults(t *testing.T) {
	body := []byte(`{
	  "messages": [
	    {"role": "user", "content": "run three tools"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_a", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a\"}"}},
	      {"id": "call_b", "type": "function", "function": {"name": "grep", "arguments": "{\"pattern\":\"b\"}"}},
	      {"id": "call_c", "type": "function", "function": {"name": "echo", "arguments": "{\"x\":\"c\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_a", "content": "ra"},
	    {"role": "tool", "tool_call_id": "call_b", "content": "rb"},
	    {"role": "tool", "tool_call_id": "call_c", "content": "rc"}
	  ]
	}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("contents = %d, want 3 (user, model, one merged user): %s", len(req.Contents), out)
	}
	turn := req.Contents[2]
	if turn.Role != "user" || len(turn.Parts) != 3 {
		t.Fatalf("merged tool turn = %+v", turn)
	}
	for i, want := range []string{"read", "grep", "echo"} {
		fr := turn.Parts[i].FunctionResponse
		if fr == nil || fr.Name != want {
			t.Fatalf("part %d = %+v, want functionResponse %q", i, turn.Parts[i], want)
		}
	}
	if got := turn.Parts[0].FunctionResponse.ID; got != "call_a" {
		t.Fatalf("functionResponse id = %q, want call_a", got)
	}
	if call := req.Contents[1].Parts[1].FunctionCall; call == nil || call.ID != "call_b" {
		t.Fatalf("functionCall id not carried: %+v", req.Contents[1].Parts[1])
	}
}

// TestFromChatRequestReordersOutOfOrderToolResults pins slot alignment: a
// client may deliver parallel results in any order, Gemini pairs them by
// position with the preceding model turn.
func TestFromChatRequestReordersOutOfOrderToolResults(t *testing.T) {
	body := []byte(`{
	  "messages": [
	    {"role": "user", "content": "go"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "alpha", "arguments": "{}"}},
	      {"id": "call_2", "type": "function", "function": {"name": "beta", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_2", "content": "second"},
	    {"role": "tool", "tool_call_id": "call_1", "content": "first"}
	  ]
	}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	_ = json.Unmarshal(out, &req)
	parts := req.Contents[2].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d: %s", len(parts), out)
	}
	if parts[0].FunctionResponse.Name != "alpha" || parts[1].FunctionResponse.Name != "beta" {
		t.Fatalf("order = %q, %q; want alpha, beta", parts[0].FunctionResponse.Name, parts[1].FunctionResponse.Name)
	}
	if !strings.Contains(string(parts[0].FunctionResponse.Response), "first") {
		t.Fatalf("alpha payload = %s", parts[0].FunctionResponse.Response)
	}
}

// TestFromChatRequestKeepsUnmatchedToolResults pins that results without a
// counterpart are not dropped: they keep their order after the aligned ones
// so upstream validation still sees the full history.
func TestFromChatRequestKeepsUnmatchedToolResults(t *testing.T) {
	body := []byte(`{
	  "messages": [
	    {"role": "user", "content": "go"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "alpha", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_orphan", "content": "orphan"},
	    {"role": "tool", "tool_call_id": "call_1", "content": "matched"}
	  ]
	}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	_ = json.Unmarshal(out, &req)
	parts := req.Contents[2].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (matched then orphan): %s", len(parts), out)
	}
	if parts[0].FunctionResponse.ID != "call_1" || parts[1].FunctionResponse.ID != "call_orphan" {
		t.Fatalf("order = %q, %q; want call_1, call_orphan", parts[0].FunctionResponse.ID, parts[1].FunctionResponse.ID)
	}
}

// TestFromChatRequestToolTurnInterruptedByUser pins the boundary: a user
// message between tool results must flush the batch instead of merging across
// the interruption.
func TestFromChatRequestToolTurnInterruptedByUser(t *testing.T) {
	body := []byte(`{
	  "messages": [
	    {"role": "user", "content": "go"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "alpha", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "done"},
	    {"role": "user", "content": "next question"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_2", "type": "function", "function": {"name": "beta", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_2", "content": "done too"}
	  ]
	}`)
	out, eErr := FromChatRequest(body, nil)
	if eErr != nil {
		t.Fatalf("FromChatRequest: %v", eErr)
	}
	var req gRequest
	_ = json.Unmarshal(out, &req)
	roles := make([]string, 0, len(req.Contents))
	for _, c := range req.Contents {
		roles = append(roles, c.Role)
	}
	want := []string{"user", "model", "user", "user", "model", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v: %s", roles, want, out)
	}
	if len(req.Contents[2].Parts) != 1 || len(req.Contents[5].Parts) != 1 {
		t.Fatalf("interrupted batches must stay separate: %+v", req.Contents)
	}
}
