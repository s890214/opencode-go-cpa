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
