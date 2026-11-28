//go:build live

package tests_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	cpaHost   = envOrDefault("CPA_HOST", "http://localhost:8317")
	cpaKey    = envOrDefault("CPA_KEY", "123")
	cpaPrefix = envOrDefault("CPA_PREFIX", "")
)

func envOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func modelID(name string) string {
	if cpaPrefix == "" {
		return name
	}
	return fmt.Sprintf("%s/%s", cpaPrefix, name)
}

type testCase struct {
	name       string
	clientPath string
	model      string
	stream     bool
	buildReq   func(t *testing.T, model string, stream bool) (*http.Request, error)
	verifyResp func(t *testing.T, resp *http.Response)
}

func TestLiveSmokeSuite(t *testing.T) {
	cases := []testCase{
		// ---- Non-Streaming Tests (3x3 = 9) ----
		{
			name:       "NonStream_Chat_GLM",
			clientPath: "/v1/chat/completions",
			model:      modelID("glm-5.3-flash"),
			stream:     false,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatNonStream,
		},
		{
			name:       "NonStream_Chat_Qwen",
			clientPath: "/v1/chat/completions",
			model:      modelID("mimo-v2.6-flash"),
			stream:     false,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatNonStream,
		},
		{
			name:       "NonStream_Chat_Luna",
			clientPath: "/v1/chat/completions",
			model:      modelID("gpt-6-luna"),
			stream:     false,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatNonStream,
		},
		{
			name:       "NonStream_Messages_GLM",
			clientPath: "/v1/messages",
			model:      modelID("glm-5.3-flash"),
			stream:     false,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesNonStream,
		},
		{
			name:       "NonStream_Messages_Qwen",
			clientPath: "/v1/messages",
			model:      modelID("mimo-v2.6-flash"),
			stream:     false,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesNonStream,
		},
		{
			name:       "NonStream_Messages_Luna",
			clientPath: "/v1/messages",
			model:      modelID("gpt-6-luna"),
			stream:     false,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesNonStream,
		},
		{
			name:       "NonStream_Responses_GLM",
			clientPath: "/v1/responses",
			model:      modelID("glm-5.3-flash"),
			stream:     false,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesNonStream,
		},
		{
			name:       "NonStream_Responses_Qwen",
			clientPath: "/v1/responses",
			model:      modelID("mimo-v2.6-flash"),
			stream:     false,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesNonStream,
		},
		{
			name:       "NonStream_Responses_Luna",
			clientPath: "/v1/responses",
			model:      modelID("gpt-6-luna"),
			stream:     false,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesNonStream,
		},

		// ---- Streaming Tests (3x3 = 9) ----
		{
			name:       "Stream_Chat_GLM",
			clientPath: "/v1/chat/completions",
			model:      modelID("glm-5.3-flash"),
			stream:     true,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatStream,
		},
		{
			name:       "Stream_Chat_Qwen",
			clientPath: "/v1/chat/completions",
			model:      modelID("mimo-v2.6-flash"),
			stream:     true,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatStream,
		},
		{
			name:       "Stream_Chat_Luna",
			clientPath: "/v1/chat/completions",
			model:      modelID("gpt-6-luna"),
			stream:     true,
			buildReq:   buildChatRequest,
			verifyResp: verifyChatStream,
		},
		{
			name:       "Stream_Messages_GLM",
			clientPath: "/v1/messages",
			model:      modelID("glm-5.3-flash"),
			stream:     true,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesStream,
		},
		{
			name:       "Stream_Messages_Qwen",
			clientPath: "/v1/messages",
			model:      modelID("mimo-v2.6-flash"),
			stream:     true,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesStream,
		},
		{
			name:       "Stream_Messages_Luna",
			clientPath: "/v1/messages",
			model:      modelID("gpt-6-luna"),
			stream:     true,
			buildReq:   buildMessagesRequest,
			verifyResp: verifyMessagesStream,
		},
		{
			name:       "Stream_Responses_GLM",
			clientPath: "/v1/responses",
			model:      modelID("glm-5.3-flash"),
			stream:     true,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesStream,
		},
		{
			name:       "Stream_Responses_Qwen",
			clientPath: "/v1/responses",
			model:      modelID("mimo-v2.6-flash"),
			stream:     true,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesStream,
		},
		{
			name:       "Stream_Responses_Luna",
			clientPath: "/v1/responses",
			model:      modelID("gpt-6-luna"),
			stream:     true,
			buildReq:   buildResponsesRequest,
			verifyResp: verifyResponsesStream,
		},
	}

	client := &http.Client{Timeout: 60 * time.Second}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			time.Sleep(500 * time.Millisecond) // rate pacing

			req, err := tc.buildReq(t, tc.model, tc.stream)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("HTTP request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
			}

			tc.verifyResp(t, resp)
		})
	}
}

// ---- Request Builders ----

func buildChatRequest(t *testing.T, model string, stream bool) (*http.Request, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 16,
	}
	if stream {
		payload["stream"] = true
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cpaKey)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func buildMessagesRequest(t *testing.T, model string, stream bool) (*http.Request, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 16,
	}
	if stream {
		payload["stream"] = true
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", cpaKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func buildResponsesRequest(t *testing.T, model string, stream bool) (*http.Request, error) {
	payload := map[string]any{
		"model":             model,
		"input":             "hi",
		"max_output_tokens": 16,
	}
	if stream {
		payload["stream"] = true
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cpaKey)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// ---- Response Verifiers ----

func verifyChatNonStream(t *testing.T, resp *http.Response) {
	var body struct {
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if len(body.Choices) == 0 {
		t.Fatalf("choices array is empty")
	}
}

func verifyMessagesNonStream(t *testing.T, resp *http.Response) {
	var body struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []any  `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if body.ID == "" {
		t.Fatalf("missing response ID in messages response")
	}
	if body.Type != "message" && body.Role != "assistant" {
		t.Fatalf("unexpected type/role: type=%q role=%q", body.Type, body.Role)
	}
}

func verifyResponsesNonStream(t *testing.T, resp *http.Response) {
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if body.ID == "" {
		t.Fatalf("missing response ID")
	}
}

func verifyChatStream(t *testing.T, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	sawDone := false
	chunkCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			chunkCount++
			if strings.Contains(line, "[DONE]") {
				sawDone = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if chunkCount == 0 {
		t.Fatalf("no data lines received")
	}
	if !sawDone {
		t.Fatalf("stream did not end with [DONE]")
	}
}

func verifyMessagesStream(t *testing.T, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	sawStart := false
	sawStop := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "message_start") {
			sawStart = true
		}
		if strings.Contains(line, "message_stop") {
			sawStop = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if !sawStart {
		t.Fatalf("missing message_start event")
	}
	if !sawStop {
		t.Fatalf("missing message_stop event")
	}
}

func verifyResponsesStream(t *testing.T, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	sawCreated := false
	sawCompleted := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "response.created") {
			sawCreated = true
		}
		if strings.Contains(line, "response.completed") {
			sawCompleted = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if !sawCreated {
		t.Fatalf("missing response.created event")
	}
	if !sawCompleted {
		t.Fatalf("missing response.completed event")
	}
}

// ============================================================================
// QA Test Suite: Tool Calling, Thinking/Reasoning, Edge Cases
// ============================================================================

// ---- 3. Tool Calling / Function Calling ----

func TestLiveToolCalling(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}

	t.Run("ChatEndpoint_Tools_MessagesUpstream", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("mimo-v2.6-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "What is the weather in Tokyo?"},
			},
			"tools": []map[string]any{
				{
					"type": "function",
					"function": map[string]any{
						"name":        "get_weather",
						"description": "Get current weather for a city",
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]string{"type": "string", "description": "City name"},
							},
							"required": []string{"location"},
						},
					},
				},
			},
			"max_tokens": 150,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cpaKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Choices []struct {
				Message struct {
					Role      string `json:"role"`
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Choices) == 0 {
			t.Fatalf("choices is empty")
		}
		msg := result.Choices[0].Message
		if len(msg.ToolCalls) == 0 && msg.Content == "" {
			t.Fatalf("response has neither tool_calls nor content: %#v", msg)
		}
		t.Logf("Chat tools response: content=%q, tool_calls=%+v", msg.Content, msg.ToolCalls)
	})

	t.Run("MessagesEndpoint_Tools_ChatUpstream", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("glm-5.3-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "What is the weather in Paris?"},
			},
			"tools": []map[string]any{
				{
					"name":        "get_weather",
					"description": "Get current weather for a city",
					"input_schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]string{"type": "string"},
						},
						"required": []string{"location"},
					},
				},
			},
			"max_tokens": 150,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("x-api-key", cpaKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text,omitempty"`
				Name  string         `json:"name,omitempty"`
				Input map[string]any `json:"input,omitempty"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Content) == 0 {
			t.Fatalf("content is empty")
		}
		t.Logf("Messages tools response: %+v", result.Content)
	})
}

// ---- 4. Thinking / Reasoning Output Handling ----

func TestLiveThinkingReasoning(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}

	t.Run("ChatEndpoint_Reasoning_Qwen", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("mimo-v2.6-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "Which number is larger: 9.9 or 9.11? Answer briefly in one sentence."},
			},
			"max_tokens": 300,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cpaKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Choices []struct {
				Message struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Choices) == 0 {
			t.Fatalf("choices is empty")
		}
		msg := result.Choices[0].Message
		if msg.Content == "" && msg.ReasoningContent == "" {
			t.Fatalf("expected content or reasoning_content, got empty")
		}
		t.Logf("Chat reasoning response: content=%q, reasoning_content_len=%d", msg.Content, len(msg.ReasoningContent))
	})

	t.Run("MessagesEndpoint_Thinking_Qwen", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("mimo-v2.6-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "Which number is larger: 9.9 or 9.11? Answer briefly in one sentence."},
			},
			"max_tokens": 300,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("x-api-key", cpaKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				Thinking string `json:"thinking,omitempty"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Content) == 0 {
			t.Fatalf("content is empty")
		}
		t.Logf("Messages thinking response: blocks=%d", len(result.Content))
	})
}

// ---- 5. Error & Edge Case Handling ----

func TestLiveEdgeCases(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}

	t.Run("NonExistentModel_Rejection", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("non-existent-model-xyz-123"),
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"max_tokens": 10,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cpaKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected non-200 for non-existent model, got %d", resp.StatusCode)
		}
		t.Logf("Non-existent model correctly rejected with HTTP %d", resp.StatusCode)
	})

	t.Run("MultiTurnContext_ChatToMessagesRoute", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("mimo-v2.6-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "My name is Alice and my secret word is Pineapple."},
				{"role": "assistant", "content": "Hello Alice! I have noted that your secret word is Pineapple."},
				{"role": "user", "content": "What is my secret word? Answer with only the word."},
			},
			"max_tokens": 300,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cpaKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Choices) == 0 {
			t.Fatalf("choices is empty")
		}
		content := result.Choices[0].Message.Content
		if !strings.Contains(strings.ToLower(content), "pineapple") {
			t.Fatalf("expected multi-turn recall of 'Pineapple', got: %q", content)
		}
		t.Logf("Multi-turn Chat->Messages recall success: %q", content)
	})

	t.Run("MultiTurnContext_MessagesToChatRoute", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model": modelID("glm-5.3-flash"),
			"messages": []map[string]string{
				{"role": "user", "content": "My favorite fruit is Mango."},
				{"role": "assistant", "content": "Got it! Your favorite fruit is Mango."},
				{"role": "user", "content": "What is my favorite fruit? Answer with only the fruit name."},
			},
			"max_tokens": 300,
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("x-api-key", cpaKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if len(result.Content) == 0 {
			t.Fatalf("content is empty")
		}
		text := result.Content[0].Text
		if !strings.Contains(strings.ToLower(text), "mango") {
			t.Fatalf("expected multi-turn recall of 'Mango', got: %q", text)
		}
		t.Logf("Multi-turn Messages->Chat recall success: %q", text)
	})

	t.Run("Messages_ClaudeToolResultImage_ChatRoute", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond)
		payload := map[string]any{
			"model":      modelID("deepseek-v4.1-flash"),
			"max_tokens": 16,
			"messages": []any{
				map[string]any{
					"role":    "user",
					"content": "Read test.png",
				},
				map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"id":    "toolu_123",
							"name":  "Read",
							"input": map[string]any{"file_path": "test.png"},
						},
					},
				},
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{
							"type":        "tool_result",
							"tool_use_id": "toolu_123",
							"content": []any{
								map[string]any{
									"type": "image",
									"source": map[string]any{
										"type":       "base64",
										"media_type": "image/png",
										"data":       "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
									},
								},
							},
						},
					},
				},
			},
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("x-api-key", cpaKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if result.Role != "assistant" {
			t.Fatalf("expected assistant role, got: %q", result.Role)
		}
		t.Logf("Claude tool result image successfully accepted and routed to Chat Completions")
	})
}
