//go:build live

package tests_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLiveCodexAdditionalTools_Accepted reproduces the Codex multi-turn
// failure where input contains {"type": "additional_tools", ...} and the
// server rejects it with HTTP 400.
func TestLiveCodexAdditionalTools_Accepted(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	payload := map[string]any{
		"model": modelID("glm-5.2"),
		"input": []map[string]any{
			{
				"type": "additional_tools",
				"tools": []map[string]any{
					{
						"type":        "function",
						"name":        "extra_search",
						"description": "extra tool carried in history",
						"parameters":  map[string]any{"type": "object"},
					},
				},
			},
			{"role": "user", "content": "say ok"},
		},
		"max_output_tokens": 16,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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
}

// TestLiveCodexNamespaceTools_NonStreamRoundTrip reproduces the Codex
// namespace-tool failure: a "namespace" tool wrapping spawn_agent plus a
// tool_choice targeting it must round-trip as a function_call with the
// namespace preserved.
func TestLiveCodexNamespaceTools_NonStreamRoundTrip(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	payload := map[string]any{
		"model": modelID("glm-5.2"),
		"input": "Call spawn_agent now",
		"tools": []map[string]any{
			{
				"type": "namespace",
				"name": "subagents",
				"tools": []map[string]any{
					{
						"type":        "function",
						"name":        "spawn_agent",
						"description": "Spawn a subagent",
						"parameters":  map[string]any{"type": "object"},
					},
				},
			},
		},
		"tool_choice": map[string]any{
			"type":      "function",
			"name":      "spawn_agent",
			"namespace": "subagents",
		},
		"max_output_tokens": 128,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	for _, item := range result.Output {
		if item.Type == "function_call" && item.Name == "spawn_agent" && item.Namespace == "subagents" {
			return
		}
	}
	dump, _ := json.Marshal(result.Output)
	t.Fatalf("missing function_call spawn_agent/subagents in output: %s", string(dump))
}

// TestLiveCodexNamespaceTools_StreamRoundTrip is the streaming variant: the
// SSE response.output_item.added / response.output_item.done events must
// carry the namespaced spawn_agent function call.
func TestLiveCodexNamespaceTools_StreamRoundTrip(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	payload := map[string]any{
		"model": modelID("glm-5.2"),
		"input": "Call spawn_agent now",
		"tools": []map[string]any{
			{
				"type": "namespace",
				"name": "subagents",
				"tools": []map[string]any{
					{
						"type":        "function",
						"name":        "spawn_agent",
						"description": "Spawn a subagent",
						"parameters":  map[string]any{"type": "object"},
					},
				},
			},
		},
		"tool_choice": map[string]any{
			"type":      "function",
			"name":      "spawn_agent",
			"namespace": "subagents",
		},
		"max_output_tokens": 128,
		"stream":            true,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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

	sawAdded := false
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	event := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			event = strings.TrimSpace(rest)
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if event != "response.output_item.added" && event != "response.output_item.done" {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &envelope); err != nil {
			continue
		}
		item, _ := envelope["item"].(map[string]any)
		if item == nil {
			item = envelope
		}
		name, _ := item["name"].(string)
		namespace, _ := item["namespace"].(string)
		if name == "spawn_agent" && namespace == "subagents" {
			if event == "response.output_item.added" {
				sawAdded = true
			} else {
				sawDone = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if !sawAdded {
		t.Fatalf("missing response.output_item.added for spawn_agent/subagents")
	}
	if !sawDone {
		t.Fatalf("missing response.output_item.done for spawn_agent/subagents")
	}
}

// TestLiveCodexGrokReasoningReplayAccepted reproduces the Codex multi-turn
// failure where the input contains a historical reasoning item with an
// encrypted_content blob from a prior turn.  When pooled across accounts,
// Grok cannot decrypt foreign blobs and returns HTTP 400 ("Could not decode
// the compaction blob").  The plugin must strip reasoning items containing
// encrypted_content before forwarding to non-GPT Responses models.
//
// RED: currently FAILS with HTTP 400 because the plugin preserves the item.
func TestLiveCodexGrokReasoningReplayAccepted(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	payload := map[string]any{
		"model": modelID("grok-4.6"),
		"input": []map[string]any{
			{
				"type":    "message",
				"role":    "user",
				"content": []map[string]any{{"type": "input_text", "text": "say ok"}},
			},
			{
				// Historical reasoning item from a prior turn — contains a
				// pooled encrypted blob that Grok cannot decrypt.
				"type":              "reasoning",
				"id":                "rs_1",
				"summary":           []any{},
				"encrypted_content": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
			},
			{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "ok"}},
			},
		},
		"max_output_tokens": 16,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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
}

// TestLiveCodexCustomToolAccepted asserts that client custom tools (e.g. Codex exec)
// and apply_patch are accepted rather than rejected with HTTP 400.
func TestLiveCodexCustomToolAccepted(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model": modelID("deepseek-v4.1-flash"),
		"input": "say ok",
		"tools": []map[string]any{
			{
				"type":        "custom",
				"name":        "exec",
				"description": "Run JavaScript code to orchestrate tools",
			},
			{
				"type": "custom",
				"name": "apply_patch",
			},
			{
				"type": "web_search",
			},
		},
		"max_output_tokens": 16,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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
}

// TestLiveClaudeToolReferenceToChatCompletions reproduces Issue #14: Claude Code
// ToolSearch emits tool_reference blocks inside tool_result, which must not fail
// with 500 when routed to Chat Completions models like DeepSeek.
func TestLiveClaudeToolReferenceToChatCompletions(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := cpaHost + "/v1/messages"

	body := map[string]any{
		"model":      modelID("deepseek-v4.1-flash"),
		"max_tokens": 500,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "search for weather",
			},
			{
				"role": "assistant",
				"content": []map[string]any{
					{
						"type":  "tool_use",
						"id":    "call_web_search_01",
						"name":  "web_search",
						"input": map[string]any{"query": "weather"},
					},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": "call_web_search_01",
						"content": []map[string]any{
							{
								"type":      "tool_reference",
								"tool_name": "web_search",
							},
						},
					},
					{
						"type": "text",
						"text": "here is the tool reference, please continue",
					},
				},
			},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cpaKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	if !strings.Contains(string(respBody), `"content"`) {
		t.Fatalf("response missing content block: %s", string(respBody))
	}
}
