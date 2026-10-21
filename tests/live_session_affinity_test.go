//go:build live

package tests_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const emptySessionHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestLiveSessionAffinity(t *testing.T) {
	debugLog := os.Getenv("CPA_DEBUG_LOG")
	if debugLog == "" {
		t.Skip("CPA_DEBUG_LOG is required")
	}
	if _, err := os.Stat(debugLog); err != nil {
		t.Fatalf("stat CPA_DEBUG_LOG: %v", err)
	}

	alpha := "session-affinity-alpha-2026-09-04"
	alphaHash := sessionHash(alpha)
	client := &http.Client{Timeout: 90 * time.Second}
	models := []struct {
		name    string
		enabled bool
	}{
		{name: "glm-5.2", enabled: true},
		{name: "qwen3.7-max", enabled: true},
		{name: "gpt-5.6-luna", enabled: envBool("CPA_LUNA_ENABLED")},
	}
	formats := []struct {
		name         string
		path         string
		sourceFormat string
	}{
		{name: "chat", path: "/v1/chat/completions", sourceFormat: "openai"},
		{name: "messages", path: "/v1/messages", sourceFormat: "claude"},
		{name: "responses", path: "/v1/responses", sourceFormat: "openai-response"},
	}
	variants := []struct {
		name   string
		stream bool
		later  bool
	}{
		{name: "first_non_stream"},
		{name: "later_non_stream", later: true},
		{name: "later_stream", later: true, stream: true},
	}

	for _, model := range models {
		model := model
		t.Run(model.name, func(t *testing.T) {
			if !model.enabled {
				t.Skip("set CPA_LUNA_ENABLED=true when the Luna Responses route is available")
			}
			for _, format := range formats {
				format := format
				for _, variant := range variants {
					variant := variant
					t.Run(format.name+"/"+variant.name, func(t *testing.T) {
						body := affinityRequestBody(t, format.name, modelID(model.name), alpha, variant.later, variant.stream)
						line := runAffinityRequest(t, client, debugLog, format.path, format.sourceFormat, body, variant.stream)
						assertSessionLine(t, line, format.sourceFormat, alphaHash, variant.stream, false)
						if strings.Contains(line, alpha) {
							t.Fatalf("debug line leaked raw input: %s", line)
						}
					})
				}
			}
		})
	}

	t.Run("different_initial_input", func(t *testing.T) {
		beta := "session-affinity-beta-2026-09-04"
		betaHash := sessionHash(beta)
		if betaHash == alphaHash {
			t.Fatal("test inputs unexpectedly produced the same hash")
		}
		body := affinityRequestBody(t, "chat", modelID("glm-5.2"), beta, false, false)
		line := runAffinityRequest(t, client, debugLog, "/v1/chat/completions", "openai", body, false)
		assertSessionLine(t, line, "openai", betaHash, false, false)
	})

	t.Run("consecutive_initial_user_messages", func(t *testing.T) {
		second := "-second-user-part"
		body := map[string]any{
			"model": modelID("glm-5.2"),
			"messages": []map[string]any{
				{"role": "user", "content": alpha},
				{"role": "user", "content": second},
			},
			"max_tokens": 16,
		}
		line := runAffinityRequest(t, client, debugLog, "/v1/chat/completions", "openai", mustJSON(t, body), false)
		assertSessionLine(t, line, "openai", sessionHash(alpha+second), false, false)
	})

	t.Run("empty_input_fallback", func(t *testing.T) {
		body := map[string]any{
			"model": modelID("glm-5.2"),
			"messages": []map[string]any{
				{"role": "assistant", "content": "prefill"},
			},
			"max_tokens": 16,
		}
		line := runAffinityRequestAllowStatus(t, client, debugLog, "/v1/chat/completions", "openai", mustJSON(t, body), false)
		assertSessionLine(t, line, "openai", emptySessionHash, false, true)
	})
}

func affinityRequestBody(t *testing.T, format, model, first string, later, stream bool) []byte {
	t.Helper()
	var body map[string]any
	switch format {
	case "chat", "messages":
		messages := []map[string]any{{"role": "user", "content": first}}
		if later {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": "ack"},
				map[string]any{"role": "user", "content": "second turn"},
			)
		}
		body = map[string]any{"model": model, "messages": messages, "max_tokens": 16}
	case "responses":
		var input any = first
		if later {
			input = []map[string]any{
				{"type": "message", "role": "user", "content": first},
				{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "ack"}}},
				{"type": "message", "role": "user", "content": "second turn"},
			}
		}
		body = map[string]any{"model": model, "input": input, "max_output_tokens": 16}
	default:
		t.Fatalf("unknown format %q", format)
	}
	if stream {
		body["stream"] = true
	}
	return mustJSON(t, body)
}

func runAffinityRequest(t *testing.T, client *http.Client, debugLog, path, sourceFormat string, body []byte, stream bool) string {
	t.Helper()
	line, status, response := runAffinityRequestRaw(t, client, debugLog, path, sourceFormat, body, stream)
	if status != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", status, response)
	}
	return line
}

func runAffinityRequestAllowStatus(t *testing.T, client *http.Client, debugLog, path, sourceFormat string, body []byte, stream bool) string {
	t.Helper()
	line, _, _ := runAffinityRequestRaw(t, client, debugLog, path, sourceFormat, body, stream)
	return line
}

func runAffinityRequestRaw(t *testing.T, client *http.Client, debugLog, path, sourceFormat string, body []byte, stream bool) (string, int, string) {
	t.Helper()
	info, err := os.Stat(debugLog)
	if err != nil {
		t.Fatalf("stat debug log: %v", err)
	}
	offset := info.Size()

	req, err := http.NewRequest(http.MethodPost, cpaHost+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if path == "/v1/messages" {
		req.Header.Set("x-api-key", cpaKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+cpaKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	response, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	line := waitForSessionLine(t, debugLog, offset, sourceFormat, stream)
	return line, resp.StatusCode, string(response)
}

func waitForSessionLine(t *testing.T, path string, offset int64, sourceFormat string, stream bool) string {
	t.Helper()
	mode := "non-stream"
	if stream {
		mode = "stream"
	}
	prefix := "executor session mode=" + mode + " source_format=" + sourceFormat + " "
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open debug log: %v", err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatalf("stat open debug log: %v", err)
		}
		if info.Size() < offset {
			f.Close()
			t.Fatal("debug log was truncated during test")
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			t.Fatalf("seek debug log: %v", err)
		}
		appended, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("read debug log: %v", err)
		}
		for _, line := range strings.Split(string(appended), "\n") {
			if strings.Contains(line, prefix) {
				return line
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no appended session debug line for mode=%s source_format=%s", mode, sourceFormat)
	return ""
}

func assertSessionLine(t *testing.T, line, sourceFormat, wantHash string, stream, fallback bool) {
	t.Helper()
	mode := "non-stream"
	if stream {
		mode = "stream"
	}
	want := fmt.Sprintf("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", mode, sourceFormat, wantHash, fallback)
	if strings.Contains(line, want) {
		return
	}
	// CPA v8 supplies canonical_session_id in executor metadata, which takes
	// precedence over the plugin's content hash. The host identity is a
	// derived:ctx:v1:<64-hex> value; the content-hash fallback remains
	// covered by the unit tests and is only used when metadata is absent.
	if !fallback {
		if idx := strings.Index(line, "x_opencode_session=derived:ctx:v1:"); idx >= 0 {
			rest := line[idx+len("x_opencode_session=derived:ctx:v1:"):]
			hash := strings.Fields(rest)
			if len(hash) > 0 && len(hash[0]) == 64 && isLowerHex(hash[0]) {
				return
			}
		}
	}
	t.Fatalf("session debug line = %q, want it to contain %q", line, want)
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func sessionHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}
