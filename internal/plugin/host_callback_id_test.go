package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host derives the upstream protocol from the request it proxies, so it has
// to tie each proxied call back to the execution that issued it. The plugin does
// that by echoing the host-issued execution id on every host call; without it the
// host cannot attribute the leg and the usage event loses its protocol.
func TestHostCallsEchoHostCallbackID(t *testing.T) {
	m, f := newExecManager(t)
	body, _ := json.Marshal(executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey},
			Model: "opencode-go/glm-5.3", SourceFormat: "openai",
			OriginalRequest: []byte(`{"model":"x","messages":[{"role":"user","content":"echo"}]}`),
		},
		HostCallbackID: "exec-7",
	})
	if resp, err := m.HandleCall("executor.execute", body); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("execute: %v %s", err, resp)
	}
	if got, _ := lastWire(t, f, pluginabi.MethodHostHTTPDo)["host_callback_id"].(string); got != "exec-7" {
		t.Fatalf("host.http.do host_callback_id = %q, want exec-7", got)
	}
}

func TestStreamHostCallsEchoHostCallbackID(t *testing.T) {
	m, f := newStreamManager(t, streamScript{upstreamID: "matrix-up"})
	body, _ := json.Marshal(executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey},
			Model: "opencode-go/glm-5.3", SourceFormat: "openai", Stream: true,
			OriginalRequest: []byte(`{"model":"x","messages":[{"role":"user","content":"echo"}],"stream":true}`),
		},
		StreamID:       "down-1",
		HostCallbackID: "exec-9",
	})
	if resp, err := m.HandleCall("executor.execute_stream", body); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("execute_stream: %v %s", err, resp)
	}
	if got, _ := lastWire(t, f, pluginabi.MethodHostHTTPDoStream)["host_callback_id"].(string); got != "exec-9" {
		t.Fatalf("host.http.do_stream host_callback_id = %q, want exec-9", got)
	}
}

// Executions without a host id (older hosts, direct calls) must keep working.
func TestHostCallsWithoutCallbackIDStayUnchanged(t *testing.T) {
	m, f := newExecManager(t)
	if resp, err := m.HandleCall("executor.execute", execReqBody("opencode-go/glm-5.3", "openai", []byte(`{"model":"x","messages":[{"role":"user","content":"echo"}]}`), false)); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("execute: %v %s", err, resp)
	}
	if _, present := lastWire(t, f, pluginabi.MethodHostHTTPDo)["host_callback_id"]; present {
		t.Fatal("host_callback_id must stay absent when the host did not issue one")
	}
}
