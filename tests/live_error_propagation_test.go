//go:build live

package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLiveErrorPropagation(t *testing.T) {
	client := &http.Client{Timeout: 10 * time.Second}

	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{name: "NonStream", stream: false},
		{name: "Stream", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqBody := map[string]any{
				"model":       modelID("glm-5.3-flash"),
				"messages":    []map[string]string{{"role": "user", "content": "hi"}},
				"stream":      tc.stream,
				"temperature": 999,
			}
			data, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(data))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if cpaKey != "" {
				req.Header.Set("Authorization", "Bearer "+cpaKey)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("client.Do: %v", err)
			}
			defer resp.Body.Close()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			bodyStr := string(bodyBytes)
			t.Logf("[%s] status: %d body: %s", tc.name, resp.StatusCode, bodyStr)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("[%s] want status 400, got %d (body: %s)", tc.name, resp.StatusCode, bodyStr)
			}

			var errResp struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    any    `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(bodyBytes, &errResp); err != nil {
				t.Fatalf("[%s] unmarshal error response: %v (body: %s)", tc.name, err, bodyStr)
			}

			if strings.Contains(errResp.Error.Message, "plugin call failed") {
				t.Fatalf("[%s] upstream error message discarded: got generic 'plugin call failed'", tc.name)
			}

			if !strings.Contains(strings.ToLower(errResp.Error.Message), "temperature") &&
				!strings.Contains(strings.ToLower(errResp.Error.Message), "invalid") {
				t.Fatalf("[%s] expected upstream explanation about temperature, got: %q", tc.name, errResp.Error.Message)
			}
		})
	}
}
