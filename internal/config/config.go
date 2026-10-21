// Package config loads and validates the opencode-go plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL          = "https://opencode.ai/inference/go/v1"
	DefaultModelPrefix      = "opencode-go"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
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
	CatalogURL  string
	ModelPrefix ModelPrefix
	APIKeys     []APIKey
	Catalog     Catalog
	Protocols   Protocols
	// Models carries the per-model policy merged from the `models` block and
	// the legacy `protocol-preference` / `route-overrides` tables.
	Models           map[string]ModelConfig
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL            *string                   `yaml:"base-url"`
	CatalogURL         *string                   `yaml:"catalog-url"`
	ModelPrefix        rawPrefix                 `yaml:"model-prefix"`
	APIKeys            []rawKey                  `yaml:"api-keys"`
	Catalog            rawCatalog                `yaml:"catalog"`
	Protocols          rawProtocols              `yaml:"protocols"`
	Models             map[string]rawModelConfig `yaml:"models"`
	RouteOverrides     map[string]RouteOverride  `yaml:"route-overrides"`
	ProtocolPreference map[string][]string       `yaml:"protocol-preference"`
	AllowHTTP          bool                      `yaml:"allow-http"`
	RequestTimeout     *string                   `yaml:"request-timeout"`
	MaxResponseBytes   *int64                    `yaml:"max-response-bytes"`
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
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
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
	// Fold the `models` block and the legacy tables into one policy map.
	models := mergeModels(raw)
	c := Config{
		BaseURL: orDefault(raw.BaseURL, DefaultBaseURL),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
		},
		Models:           models,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
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
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
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
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
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
