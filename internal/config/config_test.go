package config

import (
	"strings"
	"testing"
	"time"
)

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// withKey is a minimal valid config: one channel, one key, every other field
// defaulted. It is the shape a first install pastes in.
const withKey = "upstreams:\n  - name: opencode-go\n    api-keys:\n      - value: sk-dummy\n"

func TestLoadMinimalAppliesAllDefaults(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal) failed: %v", err)
	}
	if len(c.Upstreams) != 1 {
		t.Fatalf("Upstreams = %d, want 1", len(c.Upstreams))
	}
	u := c.Upstreams[0]
	if u.Name != "opencode-go" || u.BaseURL != "https://opencode.ai/inference/go" {
		t.Errorf("channel = %+v", u)
	}
	if u.CatalogPath != "/v1/models" || u.QuotaPath != "/v1/usage" {
		t.Errorf("paths = %q/%q", u.CatalogPath, u.QuotaPath)
	}
	if !u.ModelPrefix.Enabled || u.ModelPrefix.Value != "opencode-go" {
		t.Errorf("ModelPrefix = %+v", u.ModelPrefix)
	}
	if len(u.APIKeys) != 1 || u.APIKeys[0].Value != "sk-dummy" {
		t.Errorf("APIKeys = %+v", u.APIKeys)
	}
	if u.MessagesAuth != MessagesAuthSchemeXAPIKey {
		t.Errorf("MessagesAuth = %q", u.MessagesAuth)
	}
	if c.Catalog.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if c.AllowHTTP {
		t.Errorf("AllowHTTP = true")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 67108864 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
	// The aggregate view mirrors the first channel so a reader that does not
	// resolve a channel still sees the same values.
	if c.BaseURL != u.BaseURL || c.ModelPrefix != u.ModelPrefix || len(c.APIKeys) != 1 {
		t.Errorf("aggregate view = %q / %+v / %+v", c.BaseURL, c.ModelPrefix, c.APIKeys)
	}
}

func TestLoadFullConfigHonorsEveryField(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "test-secret-value")
	doc := `
enabled: true
future-key: 1
allow-http: true
request-timeout: 4m
stream-idle-timeout: 90s
max-response-bytes: 1024
catalog-refresh-interval: 1h
metadata-url: https://metadata.test/api.json
upstreams:
  - name: myprefix
    base-url: http://localhost:9090/v1
    catalog-path: /models
    quota-path: /usage
    model-prefix:
      enabled: true
      value: myprefix
    messages-auth: bearer
    protocol-endpoints:
      chat-completions: /openai/v1/chat/completions
    fallback-routes:
      - {prefix: glm, protocol: chat-completions}
    models:
      gpt-5.6-luna:
        protocols: [responses, chat-completions]
        reasoning-shape: content
        endpoints:
          responses: /v1/responses-override
    api-keys:
      - value: ${TEST_OG_KEY}
        name: Personal
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load(full) failed: %v", err)
	}
	u := c.Upstreams[0]
	if u.BaseURL != "http://localhost:9090/v1" || u.CatalogPath != "/models" || u.QuotaPath != "/usage" {
		t.Errorf("channel paths = %+v", u)
	}
	if !u.ModelPrefix.Enabled || u.ModelPrefix.Value != "myprefix" {
		t.Errorf("ModelPrefix = %+v", u.ModelPrefix)
	}
	if u.MessagesAuth != MessagesAuthSchemeBearer {
		t.Errorf("MessagesAuth = %q", u.MessagesAuth)
	}
	if len(u.APIKeys) != 1 || u.APIKeys[0].Value != "test-secret-value" || u.APIKeys[0].Name != "Personal" {
		t.Errorf("APIKeys = %+v", u.APIKeys)
	}
	if got := u.ProtocolEndpoints["chat-completions"]; got != "/openai/v1/chat/completions" {
		t.Errorf("chat endpoint = %q", got)
	}
	if got := u.ProtocolEndpoints["responses"]; got != DefaultProtocolEndpoints["responses"] {
		t.Errorf("responses endpoint = %q", got)
	}
	if len(u.FallbackRoutes) != 1 || u.FallbackRoutes[0].Prefix != "glm" {
		t.Errorf("FallbackRoutes = %+v", u.FallbackRoutes)
	}
	mc, ok := u.Models["gpt-5.6-luna"]
	if !ok || mc.DefaultProtocol() != "responses" || mc.EndpointFor("responses") != "/v1/responses-override" || mc.ReasoningShape != ShapeContent {
		t.Errorf("Models = %+v", u.Models)
	}
	if c.Catalog.RefreshInterval != time.Hour {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if !c.AllowHTTP || c.RequestTimeout != 4*time.Minute || c.StreamIdleTimeout != 90*time.Second {
		t.Errorf("globals = %v/%v/%v", c.AllowHTTP, c.RequestTimeout, c.StreamIdleTimeout)
	}
	if c.MaxResponseBytes != 1024 || c.MetadataURL != "https://metadata.test/api.json" {
		t.Errorf("MaxResponseBytes/MetadataURL = %d/%q", c.MaxResponseBytes, c.MetadataURL)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "expanded-secret")
	c, err := Load([]byte("upstreams:\n  - name: opencode-go\n    api-keys:\n      - value: ${TEST_OG_KEY}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "expanded-secret" {
		t.Fatalf("APIKeys = %+v", c.APIKeys)
	}
}

// Unknown keys stay ignored so the host may pass newer fields, but the old
// top-level channel schema is not silently accepted: without an `upstreams`
// entry there is no channel, and that is an error rather than a config whose
// keys went nowhere.
func TestLoadIgnoresUnknownFieldsButRejectsOldTopLevelSchema(t *testing.T) {
	doc := "routing:\n  strategy: round-robin\n  max-retries-per-request: 9\n" +
		"future-key: 1\n" +
		"api-keys:\n  - value: sk-dummy\n    priority: 7\n"
	_, err := Load([]byte(doc))
	requireErrContains(t, err, "upstreams: at least one channel is required")

	if _, err := Load([]byte("future-key: 1\n" + withKey)); err != nil {
		t.Fatalf("unknown top-level key must be ignored: %v", err)
	}
}

func TestLoadCatalogPathDefaultsRelativeToBase(t *testing.T) {
	c, err := Load([]byte("upstreams:\n  - name: c\n    base-url: https://upstream.example.com/api\n    api-keys:\n      - value: k\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogPath != "/v1/models" {
		t.Errorf("CatalogPath = %q", c.CatalogPath)
	}
}

func TestLoadExplicitCatalogPathTrimsTrailingSlash(t *testing.T) {
	c, err := Load([]byte("upstreams:\n  - name: c\n    catalog-path: /api/models///\n    api-keys:\n      - value: k\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogPath != "/api/models" {
		t.Errorf("CatalogPath = %q, want trailing slashes trimmed", c.CatalogPath)
	}
}

func TestLoadEmptyCatalogPathFallsBackToDefault(t *testing.T) {
	c, err := Load([]byte("upstreams:\n  - name: c\n    catalog-path: \"\"\n    api-keys:\n      - value: k\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogPath != "/v1/models" {
		t.Errorf("CatalogPath = %q", c.CatalogPath)
	}
}

func TestPrefixDisabledSkipsValueValidation(t *testing.T) {
	doc := "upstreams:\n  - name: c\n    model-prefix:\n      enabled: false\n      value: \"has space\"\n    api-keys:\n      - value: k\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := PublicID(c, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID = %q", got)
	}
}

func TestPublicID(t *testing.T) {
	enabled := Config{ModelPrefix: ModelPrefix{Enabled: true, Value: "opencode-go"}}
	if got := PublicID(enabled, "glm-5.2"); got != "opencode-go/glm-5.2" {
		t.Errorf("PublicID(enabled) = %q", got)
	}
	disabled := Config{ModelPrefix: ModelPrefix{Enabled: false}}
	if got := PublicID(disabled, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID(disabled) = %q", got)
	}
}

func TestLoadRejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"invalid yaml syntax", "[unclosed", "decode config"},
		{"unknown anchor has no line info", "request-timeout: *nope\n", "decode config: invalid YAML structure"},
		{"bare-scalar api key never leaks", "upstreams:\n  - name: c\n    api-keys:\n      - plain-scalar-key\n", "decode config: invalid YAML structure"},
		{"no channel at all", "", "upstreams: at least one channel is required"},
		{"channel without keys", "upstreams:\n  - name: c\n    base-url: https://a.test\n", "api-keys: at least one key is required"},
		{"channel without name", "upstreams:\n  - base-url: https://a.test\n    api-keys:\n      - value: k\n", "name is required"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{
			"duplicate model keys",
			"upstreams:\n  - name: c\n    api-keys:\n      - value: k\n    models:\n" +
				"      gpt-5.6-luna:\n        protocols: [responses]\n" +
				"      gpt-5.6-luna:\n        protocols: [messages]\n",
			"decode config: invalid YAML structure near line",
		},
		{
			"key expands to empty",
			"upstreams:\n  - name: c\n    api-keys:\n      - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[0].value: expanded to empty",
		},
		{
			"second key expands to empty",
			"upstreams:\n  - name: c\n    api-keys:\n      - value: literal-ok\n      - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[1].value: expanded to empty",
		},
		{"explicitly empty base-url", "upstreams:\n  - name: c\n    base-url: \"\"\n    api-keys:\n      - value: k\n", "base-url: must not be empty"},
		{"malformed base-url", "upstreams:\n  - name: c\n    base-url: \"::::\"\n    api-keys:\n      - value: k\n", "base-url: invalid URL"},
		{"scheme-less base-url", "upstreams:\n  - name: c\n    base-url: opencode.ai/zen/go/v1\n    api-keys:\n      - value: k\n", "base-url: invalid URL"},
		{"catalog-path without leading slash", "upstreams:\n  - name: c\n    catalog-path: models\n    api-keys:\n      - value: k\n", "catalog-path: must start with /"},
		{"catalog-path given as full URL", "upstreams:\n  - name: c\n    catalog-path: https://example.com/models\n    api-keys:\n      - value: k\n", "catalog-path: must start with /"},
		{"http base-url without allow-http", "upstreams:\n  - name: c\n    base-url: http://localhost:9090/v1\n    api-keys:\n      - value: k\n", "base-url: http scheme requires allow-http"},
		{"ftp base-url", "upstreams:\n  - name: c\n    base-url: ftp://files.example.com/v1\n    api-keys:\n      - value: k\n", `base-url: unsupported scheme "ftp"`},
		{"ftp base-url with allow-http", "allow-http: true\nupstreams:\n  - name: c\n    base-url: ftp://files.example.com/v1\n    api-keys:\n      - value: k\n", `base-url: unsupported scheme "ftp"`},
		{"refresh-interval missing unit", "catalog-refresh-interval: 5\n" + withKey, "catalog-refresh-interval: invalid duration"},
		{"refresh-interval negative", "catalog-refresh-interval: -5m\n" + withKey, "catalog-refresh-interval: must be at least 1m"},
		{"refresh-interval below floor", "catalog-refresh-interval: 1ms\n" + withKey, "catalog-refresh-interval: must be at least 1m"},
		{"request-timeout unparsable", "request-timeout: bogus\n" + withKey, "request-timeout: invalid duration"},
		{"request-timeout explicitly empty", "request-timeout: \"\"\n" + withKey, "request-timeout: invalid duration"},
		{"userinfo credentials never echoed", "upstreams:\n  - name: c\n    base-url: https://user:sekret@/v1\n    api-keys:\n      - value: k\n", "base-url: invalid URL https://"},
		{"max-response-bytes zero rejected", "max-response-bytes: 0\n" + withKey, "max-response-bytes: must be positive"},
		{"max-response-bytes negative rejected", "max-response-bytes: -5\n" + withKey, "max-response-bytes: must be positive"},
		{
			"model with unsupported protocol",
			"upstreams:\n  - name: c\n    api-keys:\n      - value: k\n    models:\n      bad-model:\n        protocols: [grpc]\n",
			"models[bad-model].protocols: unsupported protocol",
		},
		{
			"model endpoint without leading slash",
			"upstreams:\n  - name: c\n    api-keys:\n      - value: k\n    models:\n      bad-model:\n        protocols: [responses]\n        endpoints:\n          responses: v1/x\n",
			"models[bad-model].endpoints[responses]: must start with /",
		},
		{"prefix enabled empty value", "upstreams:\n  - name: c\n    model-prefix:\n      enabled: true\n      value: \"\"\n    api-keys:\n      - value: k\n", "model-prefix.value: invalid provider-ID characters"},
		{"prefix with space", "upstreams:\n  - name: c\n    model-prefix:\n      value: has space\n    api-keys:\n      - value: k\n", "model-prefix.value: invalid provider-ID characters"},
		{"prefix leading dash", "upstreams:\n  - name: c\n    model-prefix:\n      value: -lead\n    api-keys:\n      - value: k\n", "model-prefix.value: invalid provider-ID characters"},
		{"invalid channel name", "upstreams:\n  - name: \"bad name\"\n    api-keys:\n      - value: k\n", "invalid upstream name"},
		{"messages-auth unknown scheme", "upstreams:\n  - name: c\n    messages-auth: basic\n    api-keys:\n      - value: k\n", "messages-auth: must be"},
		{"duplicate channel name", "upstreams:\n  - name: c\n    api-keys:\n      - value: k1\n  - name: c\n    api-keys:\n      - value: k2\n", "already used by another channel"},
		// Two channels publishing the same model IDs would make the host drop
		// one channel's models, so it is rejected at load.
		{"two bare-name channels", "upstreams:\n  - name: a\n    model-prefix:\n      enabled: false\n    api-keys:\n      - value: k1\n  - name: b\n    model-prefix:\n      enabled: false\n    api-keys:\n      - value: k2\n", "both publish bare model IDs"},
		{"two channels sharing a prefix", "upstreams:\n  - name: a\n    model-prefix:\n      value: same\n    api-keys:\n      - value: k1\n  - name: b\n    model-prefix:\n      value: same\n    api-keys:\n      - value: k2\n", "share the model-prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			requireErrContains(t, err, tt.wantErr)
			if tt.name == "key expands to empty" && strings.Contains(err.Error(), "${") {
				t.Errorf("error leaks environment reference: %q", err.Error())
			}
		})
	}
}

// TestLoadErrorsNeverLeakDecodedValues pins the security property: decode
// and validation failures must not echo raw scalars (a bare-scalar API key)
// or embedded userinfo credentials into the error the host logs.
func TestLoadErrorsNeverLeakDecodedValues(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		secret string
	}{
		{"bare-scalar api key", "upstreams:\n  - name: c\n    api-keys:\n      - plain-scalar-key\n", "plain-scalar-key"},
		{"duplicate api key values", "upstreams:\n  - name: c\n    api-keys:\n      - value: duplicate-key-value\n      - value: duplicate-key-value\n", "duplicate-key-value"},
		{"userinfo credentials", "upstreams:\n  - name: c\n    base-url: https://admin:p4ssw0rd@/v1\n    api-keys:\n      - value: k\n", "p4ssw0rd"},
	}
	for _, tc := range cases {
		_, err := Load([]byte(tc.yaml))
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("%s: error leaks secret %q: %v", tc.name, tc.secret, err)
		}
	}
}

// TestValidateURLRejectsQueryFragmentUserinfo pins the fix for bases that
// parse fine but break JoinUpstreamURL: a query or fragment truncates the
// joined path (…/v1?tenant=x/chat/completions), and userinfo leaks
// credentials into every upstream request URL.
func TestValidateURLRejectsQueryFragmentUserinfo(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		httpOK  bool
		wantErr string
	}{
		{"query rejected naming setting", "https://host/v1?tenant=x", false, "base-url: must not contain query"},
		{"fragment rejected", "https://host/v1#frag", false, "must not contain query, fragment, or userinfo"},
		{"userinfo rejected without echoing credentials", "https://user:key@host/v1", false, "must not contain query, fragment, or userinfo"},
		{"clean https passes", "https://host/v1", false, ""},
		{"http with allow-http passes", "http://localhost:9090/v1", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateURL("base-url", tt.raw, tt.httpOK)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateURL(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			requireErrContains(t, err, tt.wantErr)
			if strings.Contains(err.Error(), "key") && strings.Contains(tt.raw, ":key@") {
				t.Errorf("error echoes credentials: %q", err.Error())
			}
		})
	}
}

// --- several channels -------------------------------------------------------

func TestTwoChannelsKeepTheirOwnSettings(t *testing.T) {
	c, err := Load([]byte(`upstreams:
  - name: opencode-go
    base-url: https://primary.test/go
    protocol-endpoints:
      chat-completions: /openai/v1/chat/completions
    models:
      glm-5.3:
        protocols: [chat-completions]
    fallback-routes:
      - {prefix: glm, protocol: chat-completions}
    api-keys:
      - value: primary-key
  - name: od
    base-url: https://second.test/v1
    messages-auth: bearer
    api-keys:
      - value: second-key
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.Upstreams) != 2 {
		t.Fatalf("Upstreams = %d, want 2", len(c.Upstreams))
	}
	primary, extra := c.Upstreams[0], c.Upstreams[1]
	if primary.BaseURL != "https://primary.test/go" || extra.BaseURL != "https://second.test/v1" {
		t.Errorf("base URLs = %q / %q", primary.BaseURL, extra.BaseURL)
	}
	if len(primary.APIKeys) != 1 || primary.APIKeys[0].Value != "primary-key" {
		t.Errorf("primary keys = %+v", primary.APIKeys)
	}
	if len(extra.APIKeys) != 1 || extra.APIKeys[0].Value != "second-key" {
		t.Errorf("extra keys = %+v", extra.APIKeys)
	}
	// Nothing channel-scoped is inherited: the second vendor must not post to
	// another host's paths, inherit its model policy or its auth scheme.
	if got := extra.ProtocolEndpoints["chat-completions"]; got != DefaultProtocolEndpoints["chat-completions"] {
		t.Errorf("extra chat endpoint = %q, want default %q", got, DefaultProtocolEndpoints["chat-completions"])
	}
	if primary.ProtocolEndpoints["chat-completions"] != "/openai/v1/chat/completions" {
		t.Errorf("primary chat endpoint = %q", primary.ProtocolEndpoints["chat-completions"])
	}
	if len(extra.Models) != 0 {
		t.Errorf("extra inherited models: %+v", extra.Models)
	}
	if extra.FallbackRoutes != nil {
		t.Errorf("extra inherited fallback routes: %+v", extra.FallbackRoutes)
	}
	if extra.CatalogPath != DefaultCatalogPath || extra.QuotaPath != DefaultQuotaPath {
		t.Errorf("extra paths = %q/%q", extra.CatalogPath, extra.QuotaPath)
	}
	// Each channel publishes under its own prefix by default.
	if primary.ModelPrefix.Value != "opencode-go" || extra.ModelPrefix.Value != "od" {
		t.Errorf("prefixes = %+v / %+v", primary.ModelPrefix, extra.ModelPrefix)
	}
	// One catalogue snapshot per channel.
	if c.ForUpstream(extra).CatalogCachePath == c.ForUpstream(primary).CatalogCachePath {
		t.Error("channels share one catalog cache file")
	}
	if _, ok := c.UpstreamByName("od"); !ok {
		t.Error("UpstreamByName(od) missing")
	}
}

func TestChannelNeedsNoBaseURLForTheDefaultVendor(t *testing.T) {
	c, err := Load([]byte("upstreams:\n  - name: solo\n    api-keys:\n      - value: k\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.Upstreams[0].BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.Upstreams[0].BaseURL, DefaultBaseURL)
	}
}

func TestKeyEntriesFollowChannelOrder(t *testing.T) {
	c, err := Load([]byte(`upstreams:
  - name: opencode-go
    api-keys:
      - value: primary-key
  - name: od
    base-url: https://second.test/v1
    api-keys:
      - value: second-key
      - value: third-key
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	entries := c.KeyEntries()
	if len(entries) != 3 {
		t.Fatalf("KeyEntries = %d, want 3", len(entries))
	}
	if entries[0].Upstream != "opencode-go" || entries[1].Upstream != "od" || entries[2].Upstream != "od" {
		t.Fatalf("KeyEntries order/owner = %+v", entries)
	}
	if _, ok := c.UpstreamByName("nope"); ok {
		t.Error("UpstreamByName(nope) resolved")
	}
}

func TestMessagesAuthIsPerChannel(t *testing.T) {
	c, err := Load([]byte(`upstreams:
  - name: opencode-go
    api-keys:
      - value: primary-key
  - name: od
    base-url: https://second.test/v1
    messages-auth: bearer
    api-keys:
      - value: second-key
`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.Upstreams[0].MessagesAuth != MessagesAuthSchemeXAPIKey {
		t.Errorf("first channel messages-auth = %q", c.Upstreams[0].MessagesAuth)
	}
	if c.Upstreams[1].MessagesAuth != MessagesAuthSchemeBearer {
		t.Errorf("second channel messages-auth = %q", c.Upstreams[1].MessagesAuth)
	}
	if got := c.ForUpstream(c.Upstreams[0]).MessagesAuth; got != MessagesAuthSchemeXAPIKey {
		t.Errorf("view messages-auth = %q", got)
	}
}
