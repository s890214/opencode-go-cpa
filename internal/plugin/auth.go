package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

const (
	oauthClientID   = "opencode-cli"
	oauthUserAgent  = "opencode/1.0.0"
	oauthGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	oauthRefreshTTL = 5 * time.Minute
)

type authProvider struct {
	cfg    config.Config
	bridge *HostBridge
}

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

// do prefers the host client CPA injects into login/refresh requests and falls
// back to the bridge, which uses the same host transport.
func (p authProvider) do(ctx context.Context, client pluginapi.HostHTTPClient, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if client != nil {
		return client.Do(ctx, req)
	}
	if p.bridge != nil {
		return p.bridge.Do(ctx, req)
	}
	return pluginapi.HTTPResponse{}, fmt.Errorf("no http transport available")
}

// consoleURL derives the Console origin from the configured inference
// base-url, so a mirror deployment needs no second setting.
func consoleURL(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://opencode.ai/console"
	}
	return u.Scheme + "://" + u.Host + "/console"
}

// authRecord is one persisted credential. API-key records carry api_key;
// Console OAuth records carry access_token/refresh_token plus the workspace
// binding the inference endpoint requires.
type authRecord struct {
	Type         string `json:"type"`
	Provider     string `json:"provider,omitempty"`
	ID           string `json:"id,omitempty"`
	Label        string `json:"label,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ConsoleURL   string `json:"console_url,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	OrgName      string `json:"org_name,omitempty"`
	Email        string `json:"email,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Plan         string `json:"plan,omitempty"`
}

func (r authRecord) isOAuth() bool { return strings.TrimSpace(r.AccessToken) != "" }

// credential returns the bearer value sent upstream and, for OAuth, the
// workspace header that goes with it.
func (r authRecord) credential() (token, org string) {
	if key := strings.TrimSpace(r.APIKey); key != "" {
		return key, ""
	}
	return strings.TrimSpace(r.AccessToken), strings.TrimSpace(r.OrgID)
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
	if strings.TrimSpace(raw.APIKey) == "" && strings.TrimSpace(raw.AccessToken) == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no api key or access token")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	attrs := map[string]string{}
	metadata := map[string]any{"auth_type": "api_key"}
	if raw.isOAuth() {
		token, org := raw.credential()
		attrs["access_token"] = token
		attrs["org_id"] = org
		metadata["auth_type"] = "oauth"
		if org != "" {
			metadata["org_id"] = org
		}
		if raw.Email != "" {
			metadata["email"] = raw.Email
		}
		if raw.Plan != "" {
			metadata["plan"] = raw.Plan
		}
	} else {
		attrs["api_key"] = strings.TrimSpace(raw.APIKey)
	}
	label := accountLabel(p.cfg, raw.APIKey, raw.Label, 0)
	if raw.isOAuth() {
		label = firstNonBlank(raw.Label, raw.Email, raw.OrgName, "OpenCode Go (Console)")
	}
	nextRefresh := time.Time{}
	if raw.isOAuth() && raw.ExpiresAt > 0 {
		nextRefresh = time.UnixMilli(raw.ExpiresAt).Add(-oauthRefreshTTL)
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s oauth=%t credential_present=%t", req.Provider, req.FileName, raw.ID, raw.isOAuth(), len(attrs) > 0)
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider:         ProviderID,
		ID:               raw.ID,
		FileName:         req.FileName,
		Label:            label,
		StorageJSON:      req.RawJSON,
		Attributes:       attrs,
		Metadata:         metadata,
		NextRefreshAfter: nextRefresh,
	}}, nil
}

// StartLogin runs the Console device-code flow. OpenCode Go itself is keyed,
// but a Console account yields an access token that the inference endpoint
// accepts for the Go plan and that can be refreshed without pasting a key.
func (p authProvider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	base := consoleURL(p.cfg.BaseURL)
	body, _ := json.Marshal(map[string]any{
		"client_id":          oauthClientID,
		"supports_org_scope": true,
	})
	resp, err := p.do(ctx, req.HTTPClient, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    base + "/auth/device/code",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{oauthUserAgent},
		},
		Body: body,
	})
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("request OpenCode device code failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("OpenCode device code HTTP %d", resp.StatusCode)
	}
	var code struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(resp.Body, &code); err != nil || strings.TrimSpace(code.DeviceCode) == "" {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("OpenCode device code response was invalid")
	}
	verify := firstNonBlank(code.VerificationURIComplete, code.VerificationURI)
	if strings.HasPrefix(verify, "/") {
		verify = strings.TrimSuffix(base, "/console") + verify
	}
	if verify == "" {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("OpenCode device code response had no verification URL")
	}
	expiresIn := code.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderID,
		URL:       verify,
		State:     oauthState(code.DeviceCode, code.UserCode),
		ExpiresAt: time.Now().UTC().Add(time.Duration(expiresIn) * time.Second),
		Metadata: map[string]any{
			"device_code": code.DeviceCode,
			"user_code":   code.UserCode,
			"interval":    code.Interval,
			"console_url": base,
		},
	}, nil
}

// PollLogin exchanges an approved device code for tokens. Pending approval is
// reported as pending with the user code, so the panel keeps the instruction
// instead of showing an error.
func (p authProvider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	deviceCode := strings.TrimSpace(req.State)
	userCode := ""
	base := consoleURL(p.cfg.BaseURL)
	if req.Metadata != nil {
		if v, ok := req.Metadata["device_code"].(string); ok && strings.TrimSpace(v) != "" {
			deviceCode = strings.TrimSpace(v)
		}
		if v, ok := req.Metadata["user_code"].(string); ok {
			userCode = v
		}
		if v, ok := req.Metadata["console_url"].(string); ok && strings.TrimSpace(v) != "" {
			base = strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	if deviceCode == "" {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "invalid login state"}, nil
	}
	body, _ := json.Marshal(map[string]any{
		"grant_type":  oauthGrantType,
		"device_code": deviceCode,
		"client_id":   oauthClientID,
	})
	resp, err := p.do(ctx, req.HTTPClient, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    base + "/auth/device/token",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{oauthUserAgent},
		},
		Body: body,
	})
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for browser approval"}, nil
	}
	var token struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    int     `json:"expires_in"`
		OrgID        *string `json:"org_id"`
		Error        string  `json:"error"`
	}
	_ = json.Unmarshal(resp.Body, &token)
	if strings.TrimSpace(token.AccessToken) == "" {
		switch strings.ToLower(strings.TrimSpace(token.Error)) {
		case "", "authorization_pending", "slow_down":
			message := "waiting for browser approval"
			if userCode != "" {
				message = "waiting for approval (code: " + userCode + ")"
			}
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: message}, nil
		default:
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "device authorization failed: " + token.Error}, nil
		}
	}
	record, err := p.completeLogin(ctx, req.HTTPClient, base, token.AccessToken, token.RefreshToken, token.ExpiresIn, token.OrgID)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: err.Error()}, nil
	}
	storage, _ := json.Marshal(record)
	authData := pluginapi.AuthData{
		Provider:         ProviderID,
		ID:               record.ID,
		Label:            firstNonBlank(record.Email, record.OrgName, "OpenCode Go (Console)"),
		StorageJSON:      storage,
		Attributes:       map[string]string{"access_token": record.AccessToken, "org_id": record.OrgID},
		Metadata:         map[string]any{"auth_type": "oauth", "org_id": record.OrgID, "email": record.Email, "plan": record.Plan},
		NextRefreshAfter: time.UnixMilli(record.ExpiresAt).Add(-oauthRefreshTTL),
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "OpenCode Go login complete", Auth: authData}, nil
}

// completeLogin resolves the account identity (best effort) and builds the
// persisted record. Identity failures are not fatal: a working token is what
// the subscription needs; the email is only a label.
func (p authProvider) completeLogin(ctx context.Context, client pluginapi.HostHTTPClient, base, accessToken, refreshToken string, expiresIn int, orgID *string) (authRecord, error) {
	if strings.TrimSpace(accessToken) == "" {
		return authRecord{}, fmt.Errorf("login returned no access token")
	}
	headers := http.Header{
		"Authorization": []string{"Bearer " + accessToken},
		"Accept":        []string{"application/json"},
		"User-Agent":    []string{oauthUserAgent},
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if resp, err := p.do(ctx, client, pluginapi.HTTPRequest{Method: http.MethodGet, URL: base + "/api/user", Headers: headers}); err == nil && resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(resp.Body, &user)
	}
	var orgs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if resp, err := p.do(ctx, client, pluginapi.HTTPRequest{Method: http.MethodGet, URL: base + "/api/orgs", Headers: headers}); err == nil && resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(resp.Body, &orgs)
	}
	selectedID, selectedName := "", ""
	if orgID != nil {
		selectedID = strings.TrimSpace(*orgID)
	}
	for _, org := range orgs {
		if org.ID == selectedID {
			selectedName = org.Name
			break
		}
	}
	if selectedID == "" && len(orgs) > 0 {
		selectedID, selectedName = orgs[0].ID, orgs[0].Name
	}
	if expiresIn <= 0 {
		expiresIn = 2592000
	}
	record := authRecord{
		Type:         ProviderID,
		Provider:     ProviderID,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
		ConsoleURL:   base,
		OrgID:        selectedID,
		OrgName:      selectedName,
		Email:        user.Email,
		AccountID:    user.ID,
	}
	record.ID = oauthRecordID(record)
	if record.RefreshToken == "" && record.AccountID == "" {
		return authRecord{}, fmt.Errorf("login returned no refresh token")
	}
	return record, nil
}

// RefreshAuth renews an OAuth record. API-key records are returned unchanged:
// they have nothing to refresh, and rewriting them would churn the host cache.
func (p authProvider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), mapKeys(req.Attributes), mapKeys(req.Metadata))
	var record authRecord
	if err := json.Unmarshal(req.StorageJSON, &record); err != nil {
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
	}
	if strings.TrimSpace(record.RefreshToken) == "" {
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
	}
	base := strings.TrimRight(firstNonBlank(record.ConsoleURL, consoleURL(p.cfg.BaseURL)), "/")
	body, _ := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": record.RefreshToken,
		"client_id":     oauthClientID,
	})
	resp, err := p.do(ctx, req.HTTPClient, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    base + "/auth/device/token",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{oauthUserAgent},
		},
		Body: body,
	})
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("refresh OpenCode token failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("refresh OpenCode token HTTP %d", resp.StatusCode)
	}
	var token struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    int     `json:"expires_in"`
		OrgID        *string `json:"org_id"`
	}
	if err := json.Unmarshal(resp.Body, &token); err != nil || strings.TrimSpace(token.AccessToken) == "" {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("invalid OpenCode refresh response")
	}
	record.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		record.RefreshToken = token.RefreshToken
	}
	if token.ExpiresIn <= 0 {
		token.ExpiresIn = 2592000
	}
	record.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli()
	if token.OrgID != nil && strings.TrimSpace(*token.OrgID) != "" {
		record.OrgID = strings.TrimSpace(*token.OrgID)
	}
	storage, _ := json.Marshal(record)
	authData := pluginapi.AuthData{
		Provider:         req.AuthProvider,
		ID:               req.AuthID,
		StorageJSON:      storage,
		Metadata:         req.Metadata,
		Attributes:       map[string]string{"access_token": record.AccessToken, "org_id": record.OrgID},
		NextRefreshAfter: time.UnixMilli(record.ExpiresAt).Add(-oauthRefreshTTL),
	}
	return pluginapi.AuthRefreshResponse{Auth: authData, NextRefreshAfter: authData.NextRefreshAfter}, nil
}

func oauthRecordID(record authRecord) string {
	seed := firstNonBlank(record.AccountID, record.RefreshToken, record.AccessToken)
	sum := sha256.Sum256([]byte(seed))
	return "opencode-go-oauth-" + hex.EncodeToString(sum[:16])
}

func oauthState(deviceCode, userCode string) string {
	raw := strings.TrimSpace(deviceCode)
	if raw == "" {
		raw = "oc-" + strings.TrimSpace(userCode)
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 96 {
		out = out[:96]
	}
	if out == "" {
		out = fmt.Sprintf("oc-%d", time.Now().UnixNano())
	}
	return out
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// Labels are presentation only. Stable auth IDs and stored host metadata are preserved.
func accountLabel(cfg config.Config, key, existing string, fallbackIndex int) string {
	index := fallbackIndex
	for i, entry := range cfg.APIKeys {
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
	if len(cfg.APIKeys) > 1 {
		return fmt.Sprintf("OpenCode Go %d", index+1)
	}
	return "OpenCode Go"
}
