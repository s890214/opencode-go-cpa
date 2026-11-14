// Package config loads and validates the opencode-go plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL           = "https://opencode.ai/inference/go"
	DefaultModelPrefix       = "opencode-go"
	DefaultRefreshInterval   = 15 * time.Minute
	DefaultRequestTimeout    = 5 * time.Minute
	DefaultStreamIdleTimeout = 5 * time.Minute
	DefaultMaxResponseBytes  = int64(67108864) // 64 MiB
	// DefaultMetadataURL is models.dev's aggregated metadata feed. The
	// opencode-go section carries per-model limits, reasoning options and
	// modalities that the upstream /v1/models response omits.
	DefaultMetadataURL = "https://models.opencode.ai/api.json"
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

type APIKey struct {
	Name  string
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
	// Gemini gates the Google generateContent upstream route. It is an
	// upstream protocol only: inbound Gemini-native clients are not
	// registered, so this never changes what CPA may send us.
	Gemini bool
}

// Enabled reports whether one protocol switch is on. Pass-through honours
// the same switches the catalog build does, so a disabled protocol cannot
// sneak back in through protocol-preference.
func (p Protocols) Enabled(protocol string) bool {
	switch protocol {
	case "chat-completions":
		return p.ChatCompletions
	case "messages":
		return p.Messages
	case "responses":
		return p.Responses
	case "gemini":
		return p.Gemini
	}
	return false
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

// ReasoningShape selects the wire form used when a Responses request has to
// carry thinking back to the upstream. The upstreams disagree: measured on
// 2026-10-21, deepseek-v4-pro rejects the summary form with
// `400 reasoning_text must be passed back` while deepseek-v4.1-flash rejects
// the content form with `503 Endpoint is unavailable.`. An empty value means
// summary, which every model measured so far except v4-pro accepts.
type ReasoningShape string

const (
	ShapeSummary ReasoningShape = "summary"
	ShapeContent ReasoningShape = "content"
)

// FallbackRoute maps a model-name prefix to the protocol used when the model
// is not listed in `models`. It replaces the hardcoded family table, so an
// operator can teach the plugin a new model family without a release.
type FallbackRoute struct {
	Prefix   string `yaml:"prefix"`
	Protocol string `yaml:"protocol"`
}

// DefaultProtocolEndpoints are the paths each protocol lives at when the
// operator does not override them. They are configuration, not constants:
// an upstream can move a path without needing a new plugin build.
//
// gemini is a prefix: the model id and the action (":generateContent" /
// ":streamGenerateContent?alt=sse") are appended by the adapter, because the
// Google API puts both in the path.
var DefaultProtocolEndpoints = map[string]string{
	"chat-completions": "/v1/chat/completions",
	"messages":         "/v1/messages",
	"responses":        "/v1/responses",
	"gemini":           "/google/v1beta/models",
}

// ModelConfig is one upstream model's policy: which protocols it speaks
// natively (first entry = default route), the shape of the reasoning item it
// expects back, and any per-protocol endpoint overrides.
type ModelConfig struct {
	Protocols      []string
	ReasoningShape ReasoningShape
	// Endpoints overrides the default path of a protocol. Keyed by protocol
	// so one model can pin different custom paths for different protocols.
	Endpoints map[string]string
}

// DefaultProtocol returns the model's default route ("" when unconfigured).
func (m ModelConfig) DefaultProtocol() string {
	if len(m.Protocols) == 0 {
		return ""
	}
	return m.Protocols[0]
}

// EndpointFor returns the configured path for a protocol, or "" to use the
// protocol default.
func (m ModelConfig) EndpointFor(protocol string) string {
	if m.Endpoints == nil {
		return ""
	}
	return m.Endpoints[protocol]
}

type Config struct {
	BaseURL     string
	ModelPrefix ModelPrefix
	APIKeys     []APIKey
	Catalog     Catalog
	Protocols   Protocols
	// Models carries the per-model policy merged from the `models` block and
	// the legacy `protocol-preference` / `route-overrides` tables.
	Models map[string]ModelConfig
	// ProtocolEndpoints overrides each protocol's default path.
	ProtocolEndpoints map[string]string
	// CatalogPath is where the model catalog lives, relative to base-url.
	CatalogPath string
	// QuotaPath is where the usage endpoint lives, relative to base-url.
	QuotaPath string
	// FallbackRoutes resolves a model that `models` does not mention.
	FallbackRoutes   []FallbackRoute
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	// StreamIdleTimeout bounds the gap between two upstream stream chunks.
	// It replaces the old hard cap on total stream duration: a long answer
	// that keeps producing data is never cut, a stalled one still is.
	StreamIdleTimeout time.Duration
	// CatalogCachePath persists the last good upstream catalog so a fresh
	// process can serve models before (or without) a successful refresh.
	// Empty disables persistence.
	CatalogCachePath string
	// MetadataURL is models.dev-style metadata (limits, reasoning options,
	// modalities) merged underneath the upstream catalog. Empty disables it.
	MetadataURL string
	// MessagesAuth selects the Messages route authentication scheme of the
	// channel this view describes ("x-api-key" or "bearer").
	MessagesAuth string
	// Upstreams is the ordered list of upstream channels. The first entry is
	// the primary channel and keeps the legacy field values above.
	Upstreams []Upstream
	// UpstreamName identifies which upstream a single-upstream view belongs
	// to (set by ForUpstream); "" on the aggregate config.
	UpstreamName string
}

// DefaultUpstreamName labels the upstream described by the legacy top-level
// fields (base-url + api-keys). Extra channels must pick their own name.
const DefaultUpstreamName = "opencode-go"

// Upstream is one upstream channel: its own base URL, keys, catalog, quota
// endpoint, protocol paths and per-model policy. Everything not listed here
// (timeouts, allow-http, metadata feed, catalog refresh policy) is global.
type Upstream struct {
	Name              string
	BaseURL           string
	CatalogPath       string
	QuotaPath         string
	ModelPrefix       ModelPrefix
	APIKeys           []APIKey
	Models            map[string]ModelConfig
	ProtocolEndpoints map[string]string
	Protocols         Protocols
	FallbackRoutes    []FallbackRoute
	// MessagesAuth selects the header the Messages route authenticates with.
	// OpenCode Go expects x-api-key; an Anthropic-wire gateway that only
	// accepts bearer tokens (e.g. OpenDesign) sets "bearer".
	MessagesAuth string
}

// Messages auth schemes. MessagesAuthSchemeXAPIKey is the Anthropic default and
// what OpenCode Go itself expects.
const (
	MessagesAuthSchemeXAPIKey = "x-api-key"
	MessagesAuthSchemeBearer  = "bearer"
)

// PrimaryUpstream returns the first configured upstream. A Config built
// without an `upstreams` block — the legacy single-channel shape, or a struct
// literal used by callers that never ran Load — describes that one channel
// through the top-level fields, so they are promoted here instead of resolving
// to an empty channel that has no base URL and no keys.
func (c Config) PrimaryUpstream() Upstream {
	if len(c.Upstreams) == 0 {
		return Upstream{
			Name:              DefaultUpstreamName,
			BaseURL:           c.BaseURL,
			CatalogPath:       c.CatalogPath,
			QuotaPath:         c.QuotaPath,
			ModelPrefix:       c.ModelPrefix,
			APIKeys:           c.APIKeys,
			Models:            c.Models,
			ProtocolEndpoints: c.ProtocolEndpoints,
			Protocols:         c.Protocols,
			FallbackRoutes:    c.FallbackRoutes,
			MessagesAuth:      normalizeMessagesAuth(nil),
		}
	}
	return c.Upstreams[0]
}

// UpstreamByName resolves a configured upstream channel.
func (c Config) UpstreamByName(name string) (Upstream, bool) {
	name = strings.TrimSpace(name)
	if len(c.Upstreams) == 0 {
		// Single synthesised channel: the legacy name (or no name at all)
		// resolves to it, anything else is genuinely unknown.
		if name == "" || name == DefaultUpstreamName {
			return c.PrimaryUpstream(), true
		}
		return Upstream{}, false
	}
	if name == "" {
		return c.PrimaryUpstream(), true
	}
	for _, u := range c.Upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return Upstream{}, false
}

// KeyEntry pairs one configured key with the upstream that owns it.
type KeyEntry struct {
	Upstream string
	Key      APIKey
}

// KeyEntries flattens every upstream's keys in configuration order. Without an
// `upstreams` block the top-level keys are the primary channel's keys.
func (c Config) KeyEntries() []KeyEntry {
	if len(c.Upstreams) == 0 {
		out := make([]KeyEntry, 0, len(c.APIKeys))
		for _, k := range c.APIKeys {
			out = append(out, KeyEntry{Upstream: DefaultUpstreamName, Key: k})
		}
		return out
	}
	out := make([]KeyEntry, 0, len(c.APIKeys))
	for _, u := range c.Upstreams {
		for _, k := range u.APIKeys {
			out = append(out, KeyEntry{Upstream: u.Name, Key: k})
		}
	}
	return out
}

// ForUpstream returns a single-upstream view of the config. Consumers that
// used to read BaseURL/CatalogPath/QuotaPath/Protocols/ProtocolEndpoints/
// Models/FallbackRoutes from one config now get the values of one channel.
func (c Config) ForUpstream(u Upstream) Config {
	out := c
	out.UpstreamName = u.Name
	out.BaseURL = u.BaseURL
	out.APIKeys = u.APIKeys
	out.CatalogPath = u.CatalogPath
	out.QuotaPath = u.QuotaPath
	out.ModelPrefix = u.ModelPrefix
	out.Models = u.Models
	out.ProtocolEndpoints = u.ProtocolEndpoints
	out.Protocols = u.Protocols
	out.FallbackRoutes = u.FallbackRoutes
	out.MessagesAuth = u.MessagesAuth
	out.Upstreams = nil
	out.CatalogCachePath = upstreamCachePath(c.CatalogCachePath, u.Name, c.PrimaryUpstream().Name)
	return out
}

// upstreamCachePath keeps one snapshot file per channel. The primary channel
// keeps the legacy path so an upgrade does not cold-start the model list.
func upstreamCachePath(base, name, primary string) string {
	base = strings.TrimSpace(base)
	if base == "" || name == "" || name == primary {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if ext == "" {
		ext = ".json"
	}
	return stem + "-" + name + ext
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL            *string                   `yaml:"base-url"`
	CatalogPath        *string                   `yaml:"catalog-path"`
	ModelPrefix        rawPrefix                 `yaml:"model-prefix"`
	APIKeys            []rawKey                  `yaml:"api-keys"`
	Catalog            rawCatalog                `yaml:"catalog"`
	Protocols          rawProtocols              `yaml:"protocols"`
	Models             map[string]rawModelConfig `yaml:"models"`
	ProtocolEndpoints  map[string]string         `yaml:"protocol-endpoints"`
	QuotaPath          *string                   `yaml:"quota-path"`
	FallbackRoutes     []FallbackRoute           `yaml:"fallback-routes"`
	RouteOverrides     map[string]RouteOverride  `yaml:"route-overrides"`
	ProtocolPreference map[string][]string       `yaml:"protocol-preference"`
	MessagesAuth       *string                   `yaml:"messages-auth"`
	AllowHTTP          bool                      `yaml:"allow-http"`
	RequestTimeout     *string                   `yaml:"request-timeout"`
	StreamIdleTimeout  *string                   `yaml:"stream-idle-timeout"`
	MaxResponseBytes   *int64                    `yaml:"max-response-bytes"`
	CatalogCachePath   *string                   `yaml:"catalog-cache-path"`
	MetadataURL        *string                   `yaml:"metadata-url"`
	// Upstreams declares additional channels. When present, the top-level
	// base-url/api-keys describe the FIRST channel and become its defaults.
	Upstreams []rawUpstream `yaml:"upstreams"`
}

// rawUpstream mirrors one `upstreams[]` entry. Unset fields inherit the
// top-level values (which keeps the legacy single-channel config working).
type rawUpstream struct {
	Name               *string                   `yaml:"name"`
	BaseURL            *string                   `yaml:"base-url"`
	CatalogPath        *string                   `yaml:"catalog-path"`
	QuotaPath          *string                   `yaml:"quota-path"`
	ModelPrefix        rawPrefix                 `yaml:"model-prefix"`
	APIKeys            []rawKey                  `yaml:"api-keys"`
	Protocols          rawProtocols              `yaml:"protocols"`
	Models             map[string]rawModelConfig `yaml:"models"`
	ProtocolEndpoints  map[string]string         `yaml:"protocol-endpoints"`
	FallbackRoutes     []FallbackRoute           `yaml:"fallback-routes"`
	RouteOverrides     map[string]RouteOverride  `yaml:"route-overrides"`
	ProtocolPreference map[string][]string       `yaml:"protocol-preference"`
	MessagesAuth       *string                   `yaml:"messages-auth"`
}

// asRawConfig lets an upstream entry reuse mergeModels (which folds `models`
// plus the two legacy tables).
func (u rawUpstream) asRawConfig() rawConfig {
	return rawConfig{
		Models:             u.Models,
		RouteOverrides:     u.RouteOverrides,
		ProtocolPreference: u.ProtocolPreference,
	}
}

// rawModelConfig mirrors one entry of the `models` block.
type rawModelConfig struct {
	Protocols      []string          `yaml:"protocols"`
	ReasoningShape string            `yaml:"reasoning-shape"`
	Endpoints      map[string]string `yaml:"endpoints"`
	// Endpoint is the single-protocol shorthand; it lands under the model's
	// default protocol. Kept so a one-protocol model stays a two-line entry.
	Endpoint string `yaml:"endpoint"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
	Gemini          *bool `yaml:"gemini"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true, "gemini": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value), Name: strings.TrimSpace(k.Name)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	streamIdleTimeout, err := parseDuration("stream-idle-timeout", raw.StreamIdleTimeout, DefaultStreamIdleTimeout)
	if err != nil {
		return Config{}, err
	}
	if streamIdleTimeout <= 0 {
		return Config{}, fmt.Errorf("stream-idle-timeout: must be positive")
	}
	cachePath := ""
	if raw.CatalogCachePath != nil {
		cachePath = strings.TrimSpace(*raw.CatalogCachePath)
	}
	if cachePath == "" {
		// Persisting the last good catalog is the difference between "works
		// after a restart" and "no models until the upstream answers".
		cachePath = defaultCatalogCachePath()
	}
	metadataURL := DefaultMetadataURL
	if raw.MetadataURL != nil {
		metadataURL = strings.TrimSpace(*raw.MetadataURL)
	}
	// Fold the `models` block and the legacy tables into one policy map.
	models := mergeModels(raw)
	protocols := Protocols{
		ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
		Messages:        orDefault(raw.Protocols.Messages, true),
		Responses:       orDefault(raw.Protocols.Responses, true),
		Gemini:          orDefault(raw.Protocols.Gemini, true),
	}
	prefix := ModelPrefix{
		Enabled: orDefault(raw.ModelPrefix.Enabled, true),
		Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
	}
	primary := Upstream{
		Name:              DefaultUpstreamName,
		BaseURL:           orDefault(raw.BaseURL, DefaultBaseURL),
		CatalogPath:       catalogPath(raw.CatalogPath),
		QuotaPath:         quotaPath(raw.QuotaPath),
		ModelPrefix:       prefix,
		APIKeys:           keys,
		Models:            models,
		ProtocolEndpoints: protocolEndpoints(raw.ProtocolEndpoints),
		Protocols:         protocols,
		FallbackRoutes:    fallbackRoutes(raw.FallbackRoutes),
		MessagesAuth:      normalizeMessagesAuth(raw.MessagesAuth),
	}
	upstreams, err := buildUpstreams(raw, primary)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		AllowHTTP:         raw.AllowHTTP,
		RequestTimeout:    requestTimeout,
		StreamIdleTimeout: streamIdleTimeout,
		MaxResponseBytes:  orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
		CatalogCachePath:  cachePath,
		MetadataURL:       metadataURL,
		Upstreams:         upstreams,
	}
	// Legacy fields mirror the primary channel so existing readers keep
	// seeing the same values they saw before multiple upstreams existed.
	first := upstreams[0]
	c.BaseURL = first.BaseURL
	c.ModelPrefix = first.ModelPrefix
	c.APIKeys = first.APIKeys
	c.Protocols = first.Protocols
	c.Models = first.Models
	c.ProtocolEndpoints = first.ProtocolEndpoints
	c.MessagesAuth = first.MessagesAuth
	c.CatalogPath = first.CatalogPath
	c.QuotaPath = first.QuotaPath
	c.FallbackRoutes = first.FallbackRoutes
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// buildUpstreams expands the `upstreams` block. The primary channel always
// comes from the top-level fields; every `upstreams` entry is an ADDITIONAL
// channel and must declare its own name, base URL and keys. An entry never
// inherits the primary's keys or base URL — a mistyped entry must not be able
// to take over the primary credentials. Protocol policy (switch map, fallback
// routes) and global settings stay shared unless the entry overrides them.
func buildUpstreams(raw rawConfig, primary Upstream) ([]Upstream, error) {
	out := make([]Upstream, 0, 1+len(raw.Upstreams))
	out = append(out, primary)
	seen := map[string]struct{}{primary.Name: {}}
	for i, ru := range raw.Upstreams {
		name := ""
		if ru.Name != nil {
			name = strings.TrimSpace(*ru.Name)
		}
		if name == "" {
			return nil, fmt.Errorf("upstreams[%d].name: name is required for extra upstreams", i)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("upstreams[%d].name: %q is already used by another channel", i, name)
		}
		seen[name] = struct{}{}

		baseURL := ""
		if ru.BaseURL != nil {
			baseURL = strings.TrimSpace(*ru.BaseURL)
		}
		keys := make([]APIKey, 0, len(ru.APIKeys))
		for _, entry := range ru.APIKeys {
			keys = append(keys, APIKey{Value: os.ExpandEnv(entry.Value), Name: strings.TrimSpace(entry.Name)})
		}
		// Endpoints start from the shipped defaults: a second channel of the
		// same family needs no endpoint block, a different vendor overrides
		// every path it disagrees with.
		u := Upstream{
			Name:              name,
			BaseURL:           baseURL,
			CatalogPath:       catalogPath(ru.CatalogPath),
			QuotaPath:         quotaPath(ru.QuotaPath),
			ModelPrefix:       ModelPrefix{Enabled: true, Value: name},
			APIKeys:           keys,
			Models:            nil,
			ProtocolEndpoints: protocolEndpoints(nil),
			Protocols:         mergeProtocols(ru.Protocols, primary.Protocols),
			FallbackRoutes:    primary.FallbackRoutes,
			MessagesAuth:      normalizeMessagesAuth(ru.MessagesAuth),
		}
		// An extra channel cannot opt out of its prefix: two channels must
		// never publish the same model ID, because the host keeps one
		// executor per model and would drop this channel's models while
		// mixing credentials. `model-prefix.enabled: false` is ignored here
		// and only `value` is honoured.
		if ru.ModelPrefix.Value != nil && strings.TrimSpace(*ru.ModelPrefix.Value) != "" {
			u.ModelPrefix.Value = strings.TrimSpace(*ru.ModelPrefix.Value)
		}
		if len(ru.ProtocolEndpoints) > 0 {
			merged := protocolEndpoints(nil)
			for k, v := range protocolEndpoints(ru.ProtocolEndpoints) {
				merged[k] = v
			}
			u.ProtocolEndpoints = merged
		}
		if len(ru.Models) > 0 || len(ru.RouteOverrides) > 0 || len(ru.ProtocolPreference) > 0 {
			u.Models = mergeModels(ru.asRawConfig())
		}
		if ru.FallbackRoutes != nil {
			u.FallbackRoutes = fallbackRoutes(ru.FallbackRoutes)
		}
		out = append(out, u)
	}
	return out, nil
}

// mergeProtocols overlays explicitly-set protocol switches on the inherited
// defaults.
func mergeProtocols(raw rawProtocols, base Protocols) Protocols {
	if raw.ChatCompletions != nil {
		base.ChatCompletions = *raw.ChatCompletions
	}
	if raw.Messages != nil {
		base.Messages = *raw.Messages
	}
	if raw.Responses != nil {
		base.Responses = *raw.Responses
	}
	if raw.Gemini != nil {
		base.Gemini = *raw.Gemini
	}
	return base
}

// DefaultCatalogPath is where the model catalog sits under base-url. It is
// not a protocol endpoint, so it has its own setting.
const DefaultCatalogPath = "/v1/models"

// catalogPath resolves the catalog discovery path relative to base-url. An
// explicit trailing slash is trimmed so the joined URL never carries one.
func catalogPath(raw *string) string {
	if raw != nil {
		if v := strings.TrimRight(strings.TrimSpace(*raw), "/"); v != "" {
			return v
		}
	}
	return DefaultCatalogPath
}

// DefaultQuotaPath is where the usage endpoint sits under base-url. It is not
// a protocol endpoint, so it has its own setting.
const DefaultQuotaPath = "/v1/usage"

// quotaPath resolves the usage endpoint path.
func quotaPath(raw *string) string {
	if raw == nil {
		return DefaultQuotaPath
	}
	if v := strings.TrimSpace(*raw); v != "" {
		return v
	}
	return DefaultQuotaPath
}

// protocolEndpoints merges operator overrides onto the built-in paths.
func protocolEndpoints(raw map[string]string) map[string]string {
	out := make(map[string]string, len(DefaultProtocolEndpoints)+len(raw))
	for k, v := range DefaultProtocolEndpoints {
		out[k] = v
	}
	for k, v := range raw {
		key := normalizeProtocol(k)
		value := strings.TrimSpace(v)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// fallbackRoutes normalises the configured prefix table. An empty table is a
// valid choice: a model that `models` does not mention then has no route and
// the catalog reports it as unsupported instead of guessing.
func fallbackRoutes(raw []FallbackRoute) []FallbackRoute {
	if raw == nil {
		// Absent key: keep nil so the catalog can fall back to its built-in
		// family table. An explicit `fallback-routes: []` stays empty and
		// disables guessing, matching the documented contract.
		return nil
	}
	out := make([]FallbackRoute, 0, len(raw))
	for _, r := range raw {
		prefix := strings.ToLower(strings.TrimSpace(r.Prefix))
		proto := normalizeProtocol(r.Protocol)
		if prefix == "" || proto == "" {
			continue
		}
		out = append(out, FallbackRoute{Prefix: prefix, Protocol: proto})
	}
	return out
}

// mergeModels folds the `models` block and the two legacy tables into one
// policy map. Precedence per field: `models` wins, then `route-overrides`
// (whose protocol also becomes the default route), then
// `protocol-preference` (which only lists capabilities).
//
// The legacy shape forced operators to maintain the same model twice, with a
// single endpoint slot shared by every protocol it speaks — which is what made
// a model like qwen3.7-max look inconsistent. Merging here keeps existing
// configs working while the new block stays the single source of truth.
func mergeModels(raw rawConfig) map[string]ModelConfig {
	models := make(map[string]ModelConfig, len(raw.Models))

	for id, entry := range raw.Models {
		key := strings.TrimSpace(id)
		mc := ModelConfig{}
		seen := make(map[string]struct{}, len(entry.Protocols))
		for _, p := range entry.Protocols {
			v := normalizeProtocol(p)
			if v == "" {
				continue
			}
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			mc.Protocols = append(mc.Protocols, v)
		}
		if shape := normalizeProtocol(entry.ReasoningShape); shape != "" {
			mc.ReasoningShape = ReasoningShape(shape)
		}
		if len(entry.Endpoints) > 0 {
			mc.Endpoints = make(map[string]string, len(entry.Endpoints))
			for p, path := range entry.Endpoints {
				if v := normalizeProtocol(p); v != "" && strings.TrimSpace(path) != "" {
					mc.Endpoints[v] = strings.TrimSpace(path)
				}
			}
		}
		if path := strings.TrimSpace(entry.Endpoint); path != "" {
			if mc.Endpoints == nil {
				mc.Endpoints = map[string]string{}
			}
			if d := mc.DefaultProtocol(); d != "" {
				if _, exists := mc.Endpoints[d]; !exists {
					mc.Endpoints[d] = path
				}
			}
		}
		models[key] = mc
	}

	for id, protocols := range raw.ProtocolPreference {
		key := strings.TrimSpace(id)
		mc := models[key]
		if len(mc.Protocols) == 0 {
			seen := make(map[string]struct{}, len(protocols))
			for _, p := range protocols {
				v := normalizeProtocol(p)
				if v == "" {
					continue
				}
				if _, dup := seen[v]; dup {
					continue
				}
				seen[v] = struct{}{}
				mc.Protocols = append(mc.Protocols, v)
			}
		}
		models[key] = mc
	}

	for id, override := range raw.RouteOverrides {
		key := strings.TrimSpace(id)
		mc := models[key]
		protocol := normalizeProtocol(override.Protocol)
		if protocol != "" {
			mc.Protocols = promoteProtocol(mc.Protocols, protocol)
		}
		if path := strings.TrimSpace(override.Endpoint); path != "" && protocol != "" {
			if mc.Endpoints == nil {
				mc.Endpoints = map[string]string{}
			}
			mc.Endpoints[protocol] = path
		}
		models[key] = mc
	}

	return models
}

// promoteProtocol moves proto to the front, adding it when absent. An
// explicit route override states the default route, so it must win over
// whatever order protocol-preference happened to use.
func promoteProtocol(protocols []string, proto string) []string {
	out := make([]string, 0, len(protocols)+1)
	out = append(out, proto)
	for _, p := range protocols {
		if p != proto {
			out = append(out, p)
		}
	}
	return out
}

func normalizeProtocol(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	if c.MetadataURL != "" {
		if err := validateURL("metadata-url", c.MetadataURL, c.AllowHTTP); err != nil {
			return err
		}
	}
	if c.CatalogCachePath != "" && !filepath.IsAbs(c.CatalogCachePath) {
		return fmt.Errorf("catalog-cache-path: must be absolute")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("upstreams: at least one upstream is required")
	}
	names := make(map[string]bool, len(c.Upstreams))
	for _, u := range c.Upstreams {
		if !validUpstreamName(u.Name) {
			return fmt.Errorf("upstreams: invalid upstream name %q (letters, digits, dot, dash and underscore only)", u.Name)
		}
		if names[u.Name] {
			return fmt.Errorf("upstreams: duplicate upstream name %q", u.Name)
		}
		names[u.Name] = true
		if err := c.ForUpstream(u).validateUpstream(); err != nil {
			if len(c.Upstreams) == 1 {
				// Keep the historical single-channel messages unchanged.
				return err
			}
			return fmt.Errorf("upstreams[%s]: %w", u.Name, err)
		}
	}
	return nil
}

// normalizeMessagesAuth resolves the Messages authentication scheme. Unset and
// empty both mean the Anthropic default (x-api-key); anything else is returned
// lower-cased so validate() can reject it with a precise message.
func normalizeMessagesAuth(raw *string) string {
	if raw == nil {
		return MessagesAuthSchemeXAPIKey
	}
	switch v := strings.ToLower(strings.TrimSpace(*raw)); v {
	case "":
		return MessagesAuthSchemeXAPIKey
	default:
		return v
	}
}

// validUpstreamName keeps channel names usable in auth labels, file names and
// model prefixes.
func validUpstreamName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// validateUpstream checks one channel view (base URL, keys, paths, policy).
func (c Config) validateUpstream() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	switch c.MessagesAuth {
	case "", MessagesAuthSchemeXAPIKey, MessagesAuthSchemeBearer:
	default:
		return fmt.Errorf("messages-auth: must be %q or %q", MessagesAuthSchemeXAPIKey, MessagesAuthSchemeBearer)
	}
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.CatalogPath != "" && !strings.HasPrefix(c.CatalogPath, "/") {
		return fmt.Errorf("catalog-path: must start with /")
	}
	if c.QuotaPath != "" && !strings.HasPrefix(c.QuotaPath, "/") {
		return fmt.Errorf("quota-path: must start with /")
	}
	for protocol, path := range c.ProtocolEndpoints {
		if !validProtocols[protocol] {
			return fmt.Errorf("protocol-endpoints: unsupported protocol %q", protocol)
		}
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("protocol-endpoints[%s]: must start with /", protocol)
		}
	}
	for _, r := range c.FallbackRoutes {
		if !validProtocols[r.Protocol] {
			return fmt.Errorf("fallback-routes[%s]: unsupported protocol %q", r.Prefix, r.Protocol)
		}
	}
	for name, protocols := range c.Models {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("models: model id must not be empty")
		}
		if len(protocols.Protocols) == 0 {
			return fmt.Errorf("models[%s].protocols: at least one protocol is required", name)
		}
		for _, protocol := range protocols.Protocols {
			if !validProtocols[protocol] {
				return fmt.Errorf("models[%s].protocols: unsupported protocol %q", name, protocol)
			}
		}
		if protocols.ReasoningShape != "" {
			switch protocols.ReasoningShape {
			case ShapeSummary, ShapeContent:
			default:
				return fmt.Errorf("models[%s].reasoning-shape: unsupported value %q", name, protocols.ReasoningShape)
			}
		}
		for protocol, endpoint := range protocols.Endpoints {
			if !validProtocols[protocol] {
				return fmt.Errorf("models[%s].endpoints: unsupported protocol %q", name, protocol)
			}
			if !strings.HasPrefix(endpoint, "/") {
				return fmt.Errorf("models[%s].endpoints[%s]: must start with /", name, protocol)
			}
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

// defaultCatalogCachePath keeps the last good catalog snapshot inside the
// user cache directory, so a cold start reuses it with no configuration.
// An empty return (no usable HOME) simply disables persistence.
func defaultCatalogCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, "opencode-go", "catalog.json")
}
