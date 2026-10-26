package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveGeminiUpstream is an opt-in end-to-end check against the real
// OpenCode Go gateway. It is skipped unless OPENCODE_LIVE=1 and
// OPENCODE_GO_API_KEY are set, so the normal test suite stays hermetic.
//
// Run:
//
//	OPENCODE_LIVE=1 OPENCODE_GO_API_KEY=... go test ./internal/adapter/gemini -run TestLive -v
func TestLiveGeminiUpstream(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE") == "" {
		t.Skip("set OPENCODE_LIVE=1 to call the real upstream")
	}
	key := strings.TrimSpace(os.Getenv("OPENCODE_GO_API_KEY"))
	if key == "" {
		t.Skip("OPENCODE_GO_API_KEY is not set")
	}
	const model = "gemini-4-argon"
	base := "https://opencode.ai/inference/go/google/v1beta/models"
	claudeBody := []byte(`{"model":"x","max_tokens":64,"messages":[{"role":"user","content":"Reply with exactly: pong"}]}`)

	// ---- non-stream ----
	reqBody, eErr := BuildRequest(model, "claude", claudeBody, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	respBody, status := livePost(t, key, UpstreamURL(base, model, false), reqBody)
	if status != http.StatusOK {
		t.Fatalf("non-stream status=%d body=%s", status, truncate(respBody))
	}
	converted, eErr := ConvertNonStreamResponse("claude", status, respBody)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	var msg struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(converted, &msg); err != nil {
		t.Fatalf("decode claude response: %v (%s)", err, truncate(converted))
	}
	text := ""
	for _, block := range msg.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("model did not answer pong: %q", text)
	}
	t.Logf("non-stream ok: %q tokens=%d/%d", strings.TrimSpace(text), msg.Usage.InputTokens, msg.Usage.OutputTokens)

	// ---- stream ----
	conv := NewStreamConverter("claude")
	streamReq, eErr := BuildRequest(model, "claude", claudeBody, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest stream: %v", eErr)
	}
	streamBody, status := liveStream(t, key, UpstreamURL(base, model, true), streamReq, conv)
	if status != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", status, truncate(streamBody))
	}
	joined := string(streamBody)
	if !strings.Contains(joined, "message_stop") {
		t.Fatalf("claude stream never terminated: %s", truncate(streamBody))
	}
	if !strings.Contains(strings.ToLower(joined), "pong") {
		t.Fatalf("claude stream missing answer: %s", truncate(streamBody))
	}
	t.Logf("stream ok: %d bytes", len(streamBody))
}

func livePost(t *testing.T, key, url string, body []byte) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/1.0.0")
	req.Header.Set("x-opencode-session", "live-gemini-test")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return out, resp.StatusCode
}

func liveStream(t *testing.T, key, url string, body []byte, conv *StreamConverter) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/1.0.0")
	req.Header.Set("x-opencode-session", "live-gemini-test")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream post: %v", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReaderSize(resp.Body, 32<<10)
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			events, _, eErr := conv.Feed(buf[:n])
			if eErr != nil {
				t.Fatalf("Feed: %v", eErr)
			}
			for _, event := range events {
				out = append(out, event...)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatalf("read stream: %v", readErr)
		}
	}
	for _, event := range conv.Flush() {
		out = append(out, event...)
	}
	return out, resp.StatusCode
}

func truncate(body []byte) string {
	if len(body) > 800 {
		return string(body[:800]) + "..."
	}
	return string(body)
}
