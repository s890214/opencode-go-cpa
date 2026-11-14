package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

// authProvider is API-key only. OpenCode Go is a keyed service; the Console
// device-code login that 0.6.0 shipped was removed in 0.6.1, so the panel
// points at api-keys and nothing here talks to the Console.
type authProvider struct {
	cfg config.Config
}

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

// authRecord is one persisted credential. Only api_key records exist; the
// access/refresh token fields of 0.6.0 are gone, so a leftover OAuth file is
// rejected as "no api key" instead of being silently accepted.
type authRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	Label    string `json:"label,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	// Upstream names the channel this key belongs to. Absent on records
	// written before multi-upstream support; those belong to the primary
	// channel.
	Upstream string `json:"upstream,omitempty"`
}

func (p authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw authRecord
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	key := strings.TrimSpace(raw.APIKey)
	if key == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no api key (Console OAuth records are no longer supported)")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s upstream=%s", req.Provider, req.FileName, raw.ID, raw.Upstream)
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          raw.ID,
		FileName:    req.FileName,
		Label:       accountLabel(p.cfg, raw.Upstream, key, raw.Label, 0),
		StorageJSON: req.RawJSON,
		Attributes:  map[string]string{"api_key": key, "upstream": resolvedUpstreamName(p.cfg, raw.Upstream)},
		Metadata:    map[string]any{"auth_type": "api_key"},
	}}, nil
}

// resolvedUpstreamName falls back to the primary channel for records written
// before multi-upstream support.
func resolvedUpstreamName(cfg config.Config, upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if _, ok := cfg.UpstreamByName(upstream); ok && upstream != "" {
		return upstream
	}
	return cfg.PrimaryUpstream().Name
}

// errLoginUnsupported is returned by the two login entry points the
// AuthProvider interface still requires. Credentials are configured as API
// keys; there is no browser flow to start or poll.
var errLoginUnsupported = fmt.Errorf("opencode-go uses api keys; browser login is not supported")

func (p authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, errLoginUnsupported
}

func (p authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, errLoginUnsupported
}

// RefreshAuth echoes the record untouched: an API key never expires, and
// rewriting it would only churn the host's credential cache.
func (p authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
		Provider:    req.AuthProvider,
		ID:          req.AuthID,
		StorageJSON: req.StorageJSON,
		Metadata:    req.Metadata,
		Attributes:  req.Attributes,
	}}, nil
}

// Labels are presentation only. Stable auth IDs and stored host metadata are preserved.
// The label doubles as the credential file name, so the primary channel keeps
// its historical names and extra channels are named after their upstream.
func accountLabel(cfg config.Config, upstream, key, existing string, fallbackIndex int) string {
	view := cfg
	if u, ok := cfg.UpstreamByName(upstream); ok {
		view = cfg.ForUpstream(u)
	}
	index := fallbackIndex
	for i, entry := range view.APIKeys {
		if entry.Value == key {
			if name := strings.TrimSpace(entry.Name); name != "" {
				return name
			}
			index = i
			break
		}
	}
	label := strings.TrimSpace(existing)
	if label != "" && !strings.HasPrefix(label, "OpenCode Go credential ") && !strings.HasPrefix(label, "opencode-go-key-") {
		return label
	}
	display := "OpenCode Go"
	if name := strings.TrimSpace(upstream); name != "" && name != config.DefaultUpstreamName {
		display = name
	}
	if len(view.APIKeys) > 1 {
		return fmt.Sprintf("%s %d", display, index+1)
	}
	return display
}
