//go:build live

package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestLiveCodexNamespaceToolRejected reproduces BUG-06 where Codex sends
// a client-side namespace grouping tool in the tools array to /v1/responses.
func TestLiveCodexNamespaceToolRejected(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model": modelID("glm-5.3-flash"),
		"input": "say ok",
		"tools": []map[string]any{
			{
				"type":        "function",
				"name":        "get_weather",
				"description": "Get weather",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{"type": "string"},
					},
				},
			},
			{
				"type":  "namespace",
				"name":  "subagents",
				"tools": []any{},
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
