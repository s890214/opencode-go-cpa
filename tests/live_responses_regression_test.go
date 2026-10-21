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

// TestLiveResponsesUntypedMessageItem verifies that a Responses request
// whose input array omits the type field on role-bearing items succeeds (HTTP 200)
// instead of returning HTTP 500 (Issue #1).
func TestLiveResponsesUntypedMessageItem(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model":             modelID("glm-5.2"),
		"max_output_tokens": 16,
		"input": []map[string]any{
			{"role": "user", "content": "say ok"},
		},
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

// TestLiveResponsesUnsupportedItemDoesNotCoolDownAuth verifies that client-fault
// errors (such as an unsupported item type) return HTTP 400 rather than
// causing CPA to cool down the credential and take the provider offline (Issue #2).
func TestLiveResponsesUnsupportedItemDoesNotCoolDownAuth(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}

	// Step 1: Malformed input item type must yield HTTP 400 Bad Request.
	badPayload := map[string]any{
		"model":             modelID("glm-5.2"),
		"max_output_tokens": 16,
		"input": []map[string]any{
			{"type": "web_search"},
		},
	}
	bb, err := json.Marshal(badPayload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	badReq, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(bb))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	badReq.Header.Set("Authorization", "Bearer "+cpaKey)
	badReq.Header.Set("Content-Type", "application/json")

	badResp, err := client.Do(badReq)
	if err != nil {
		t.Fatalf("bad request failed: %v", err)
	}
	defer badResp.Body.Close()

	if badResp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(badResp.Body)
		t.Fatalf("expected HTTP 400 for unsupported item type, got %d: %s", badResp.StatusCode, string(body))
	}

	// Step 2: Immediately send a valid request; auth must remain available (HTTP 200).
	time.Sleep(500 * time.Millisecond)
	validPayload := map[string]any{
		"model":      modelID("glm-5.2"),
		"messages":   []map[string]string{{"role": "user", "content": "say ok"}},
		"max_tokens": 16,
	}
	vb, err := json.Marshal(validPayload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	validReq, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(vb))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	validReq.Header.Set("Authorization", "Bearer "+cpaKey)
	validReq.Header.Set("Content-Type", "application/json")

	validResp, err := client.Do(validReq)
	if err != nil {
		t.Fatalf("valid request failed: %v", err)
	}
	defer validResp.Body.Close()

	if validResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(validResp.Body)
		t.Fatalf("expected HTTP 200 on subsequent request, got %d: %s (auth was cooled down!)", validResp.StatusCode, string(body))
	}
}

// TestLiveResponsesStreamDoneEvents verifies that a streaming Responses
// request emits item done events before response.completed, as required by
// downstream clients like OpenAI Codex CLI.
func TestLiveResponsesStreamDoneEvents(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	payload := map[string]any{
		"model":             modelID("glm-5.2"),
		"max_output_tokens": 256,
		"stream":            true,
		"input": []map[string]any{
			{"role": "user", "content": "say ok"},
		},
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

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, strings.TrimSpace(rest))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	indexOf := func(name string) int {
		for i, e := range events {
			if e == name {
				return i
			}
		}
		return -1
	}
	completed := indexOf("response.completed")
	if completed < 0 {
		t.Fatalf("missing response.completed event (got %v)", events)
	}
	for _, want := range []string{"response.output_text.done", "response.content_part.done", "response.output_item.done"} {
		idx := indexOf(want)
		if idx < 0 {
			t.Fatalf("missing %s event before response.completed (got %v)", want, events)
		}
		if idx > completed {
			t.Fatalf("%s arrived after response.completed (events %v)", want, events)
		}
	}
}

// TestLiveInHistorySystemRoleInMessages verifies that a Messages request
// containing an in-history system message inside the messages array
// succeeds (HTTP 200) instead of failing validation.
func TestLiveInHistorySystemRoleInMessages(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model": modelID("glm-5.2"),
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
			{"role": "system", "content": []map[string]string{{"type": "text", "text": "environment reminder"}}},
			{"role": "user", "content": "say ok"},
		},
		"max_tokens": 16,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
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
}

// TestLiveResponsesMultipartToolOutput verifies that a Responses request with
// an array tool output (function_call_output.output containing content parts)
// decodes successfully without failing unmarshaling (Issue BUG-03).
func TestLiveResponsesMultipartToolOutput(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model":             modelID("glm-5.2"),
		"max_output_tokens": 16,
		"input": []map[string]any{
			{
				"type":      "function_call",
				"call_id":   "c1",
				"name":      "read",
				"arguments": "{}",
			},
			{
				"type":    "function_call_output",
				"call_id": "c1",
				"output": []map[string]string{
					{"type": "input_text", "text": "file contents"},
				},
			},
		},
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
