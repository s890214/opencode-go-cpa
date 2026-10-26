// Package catalog fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; M3 wires the refresh cadence.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through (FR-004).
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
	// RouteGemini is the Google generateContent protocol. Its endpoint is a
	// prefix: the model and action live in the path, so the executor appends
	// them (see gemini.UpstreamURL).
	RouteGemini Route = "gemini"
)

// EndpointPath returns the upstream endpoint path for the route (FR-004).
// routeEndpoints maps each route to its upstream endpoint path.
// routeEndpoints is the last-resort default of each protocol's path, used
// only when neither `models.*.endpoints` nor `protocol-endpoints` override it.
// base-url is the service root (…/inference/go) and these are appended
// verbatim. The values are the upstream's *generic* paths, which serve all
// three protocols; its documented layout is /openai/v1/… and /anthropic/v1/…,
// and a real deployment pins those in `protocol-endpoints`.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
	RouteGemini:          "/google/v1beta/models",
}

// EndpointPath returns the route's upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

func routeFromString(s string) (Route, bool) {
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry (arch §7, FR-003).
type ModelRecord struct {
	PublicID    string
	UpstreamID  string
	DisplayName string
	Protocol    Route
	// SupportedProtocols lists the upstream protocols this model accepts
	// natively, in priority order (first = default route).
	SupportedProtocols []Route
	// ReasoningShape is the wire form this model expects when thinking is
	// passed back on the Responses wire.
	ReasoningShape config.ReasoningShape
	// EndpointPaths maps every supported protocol to its resolved path, so a
	// pass-through to any protocol lands on the configured endpoint instead
	// of the protocol default.
	EndpointPaths map[string]string
	EndpointPath  string
	ContextLimit  int64
	OutputLimit   int64
	InputModes    []string
	OutputModes   []string
	Thinking      *pluginapi.ThinkingSupport
}

// retiredModels lists upstream ids that are still advertised by the catalog
// endpoint but answer 410 Gone. They are excluded from the routable set so a
// client cannot pick a model that always fails.
var retiredModels = map[string]string{
	"gemini-4-argon-contributor": "retired by upstream (HTTP 410)",
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics (FR-004, open questions §10).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry; unknown fields —
// including protocol/endpoint fields the catalog does not provide and
// reasoning/tool_calling flags the plugin does not act on — are ignored via
// json tags. Modality metadata accepts both spellings.
type rawModel struct {
	ID               string       `json:"id"`
	DisplayName      string       `json:"display_name"`
	ContextLength    int64        `json:"context_length"`
	MaxOutputTokens  int64        `json:"max_output_tokens"`
	InputModes       []string     `json:"input_modes"`
	OutputModes      []string     `json:"output_modes"`
	InputModalities  []string     `json:"input_modalities"`
	OutputModalities []string     `json:"output_modalities"`
	Thinking         *rawThinking `json:"thinking"`
}

// rawThinking tolerantly decodes the optional per-model thinking object;
// pluginapi.ThinkingSupport itself is untagged, so snake_case upstream keys
// are decoded here and mapped field-by-field (partial objects decode to a
// partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// defaultFallbackRoutes covers model families the operator has not listed.
// It is only consulted when `fallback-routes` is empty — the configured table
// (and `models`) always wins, so a new family needs a config edit, not a
// release. Order matters: the first matching prefix wins.
var defaultFallbackRoutes = []config.FallbackRoute{
	{Prefix: "muse-spark", Protocol: string(RouteResponses)},
	{Prefix: "deepseek", Protocol: string(RouteChatCompletions)},
	{Prefix: "minimax", Protocol: string(RouteMessages)},
	{Prefix: "longcat", Protocol: string(RouteChatCompletions)},
	{Prefix: "grok", Protocol: string(RouteResponses)},
	{Prefix: "gpt", Protocol: string(RouteResponses)},
	{Prefix: "kimi", Protocol: string(RouteChatCompletions)},
	{Prefix: "qwen", Protocol: string(RouteMessages)},
	{Prefix: "glm", Protocol: string(RouteChatCompletions)},
	{Prefix: "mimo", Protocol: string(RouteChatCompletions)},
	{Prefix: "hy", Protocol: string(RouteChatCompletions)},
}

// fallbackRoute resolves an unlisted model by family prefix.
func (m *Manager) fallbackRoute(upstreamID string) (Route, bool) {
	routes := m.cfg.FallbackRoutes
	if routes == nil {
		// nil means "not configured": keep the built-in family table.
		// An explicitly empty list means the operator disabled guessing.
		routes = defaultFallbackRoutes
	}
	lower := strings.ToLower(upstreamID)
	for _, r := range routes {
		if strings.HasPrefix(lower, r.Prefix) {
			return Route(r.Protocol), true
		}
	}
	return "", false
}

// endpointPath resolves a protocol's path: the configured one when present,
// the protocol default otherwise.
func (m *Manager) endpointPath(route Route) string {
	if path := m.cfg.ProtocolEndpoints[string(route)]; path != "" {
		return path
	}
	return route.EndpointPath()
}

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// metadata fills gaps the upstream catalog leaves (models.dev feed).
	metadata map[string]ModelMetadata
	// cachePath persists the last good entries; empty disables persistence.
	cachePath string
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client.
func New(cfg config.Config, client HostClient) *Manager {
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate (arch §5).
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-path/
// protocols/prefix (F5: a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success (FR-002).
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    strings.TrimRight(m.cfg.BaseURL, "/") + m.cfg.CatalogPath,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	}
	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// clear like an empty plan (swap(nil)) but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return m.fail("invalid json")
	}
	m.swap(entries, warns...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone (FR-002). Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns = nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded entries and installs it
// atomically under the mutex (FR-010 dedup, arch §5 route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) swap(entries []rawModel, extraWarns ...string) {
	models := make([]ModelRecord, 0, len(entries))
	index := make(map[string]ModelRecord, len(entries)*2)
	var unsup []UnsupportedModel
	var warns []string
	warns = append(warns, extraWarns...)
	seen := make(map[string]bool, len(entries))
modelLoop:
	for _, e := range entries {
		if e.ID == "" {
			unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
			continue
		}
		if seen[e.ID] {
			warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
			continue
		}
		seen[e.ID] = true
		if reason, retired := retiredModels[e.ID]; retired {
			// The upstream still lists these ids but answers 410, so a
			// routable snapshot must not advertise them.
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		route, endpoint, ok, warn := m.resolve(e)
		if warn != "" {
			warns = append(warns, warn)
		}
		if !ok {
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: "no route determined"})
			continue
		}
		if !m.protocolEnabled(route) {
			// Flags apply at snapshot build; a stale snapshot kept per
			// FR-002 reflects the flags it was built with until the next
			// successful refresh.
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     fmt.Sprintf("protocol %s disabled by config", route),
			})
			continue
		}
		if endpoint == "" {
			endpoint = m.endpointPath(route)
		}
		if endpoint != "" && endpointEscapesBase(m.cfg.BaseURL, endpoint) {
			// The joined URL must stay on the configured upstream
			// authority. Override endpoints flow through here, so they
			// cannot rewrite the URL host — e.g. "/v1@evil.com/x"
			// against a bare-host base-url turns the base into userinfo
			// and would send credentials/prompts to an attacker host
			// (spec 05 §2 trust boundary).
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     "resolved endpoint escapes the configured upstream host",
			})
			continue
		}
		// Resolve every protocol this model speaks, including protocols that
		// are only reachable through pass-through. The executor reads this
		// map, so a per-model endpoint override must be published here too.
		endpointPaths := m.endpointPathsFor(e.ID)
		for protocol, path := range endpointPaths {
			if path == "" || !endpointEscapesBase(m.cfg.BaseURL, path) {
				continue
			}
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     fmt.Sprintf("resolved %s endpoint escapes the configured upstream host", protocol),
			})
			continue modelLoop
		}
		inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
		outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
		ctxLimit := e.ContextLength
		outLimit := e.MaxOutputTokens
		meta, metaKnown := metadataFor(m.metadata, e.ID)
		if ctxLimit == 0 {
			ctxLimit = meta.ContextLimit
		}
		if outLimit == 0 {
			outLimit = meta.OutputLimit
		}
		if len(inputModes) == 0 {
			inputModes = meta.InputModes
		}
		if len(outputModes) == 0 {
			outputModes = meta.OutputModes
		}
		display := e.DisplayName
		if display == "" {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:           config.PublicID(m.cfg, e.ID),
			UpstreamID:         e.ID,
			DisplayName:        display,
			Protocol:           route,
			SupportedProtocols: m.supportedProtocolsFor(e.ID),
			ReasoningShape:     m.reasoningShapeFor(e.ID),
			EndpointPaths:      endpointPaths,
			EndpointPath:       endpoint,
			ContextLimit:       ctxLimit,
			OutputLimit:        outLimit,
			InputModes:         inputModes,
			OutputModes:        outputModes,
			Thinking:           mergeThinking(normalizeThinking(e.Thinking), meta, metaKnown),
		}
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID (upstream "foo" and "opencode-go/foo" both
		// claim index key "<prefix>/foo"); last-write-wins would silently
		// misroute. First in catalog order wins (same dedup rule as
		// duplicate IDs); the later record is excluded. Same-role clashes
		// are impossible — distinct upstream IDs and an injective public
		// mapping — so the slot owner always holds the key via its other
		// role, and a record's own public==upstream rewrite is not a clash.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	// An entry whose model id is absent from the upstream catalog does
	// nothing at all — the client keeps getting the default route and
	// loses Responses-only features with no other symptom. Name it.
	for id := range m.cfg.Models {
		matched := false
		for seenID := range seen {
			if seenID == id || strings.EqualFold(seenID, id) {
				matched = true
				break
			}
		}
		if !matched {
			warns = append(warns, fmt.Sprintf(
				"models[%q]: model is not in the upstream catalog; entry has no effect", id))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = entries, models, index, unsup, warns
	m.persistLocked(entries)
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// (spec 04 §4); flags default to true.
// endpointPathsFor resolves the path of every supported protocol: the
// configured endpoint when the operator pinned one, the protocol default
// otherwise.
func (m *Manager) endpointPathsFor(upstreamID string) map[string]string {
	mc, ok := m.modelConfig(upstreamID)
	if !ok || len(mc.Protocols) == 0 {
		return nil
	}
	out := make(map[string]string, len(mc.Protocols))
	for _, name := range mc.Protocols {
		route := Route(name)
		if endpoint := mc.EndpointFor(name); endpoint != "" {
			out[name] = endpoint
		} else {
			out[name] = m.endpointPath(route)
		}
	}
	return out
}

// builtinReasoningShapes covers the models whose shape is known to differ
// from the default, so a config that never mentions reasoning-shape still
// routes them correctly. Measured 2026-10-21: deepseek-v4-pro answers
// `400 reasoning_text must be passed back` to summary[] and 200 to content[];
// every other model tested (v4.1-flash, glm-5.3, glm-5.3-flash, kimi-k3)
// answers 503 to content[] and 200 to summary[].
var builtinReasoningShapes = map[string]config.ReasoningShape{
	"deepseek-v4-pro": config.ShapeContent,
}

// reasoningShapeFor resolves the reasoning wire form: explicit config first
// (so an operator can override the built-in table), then the built-in table,
// then summary.
func (m *Manager) reasoningShapeFor(upstreamID string) config.ReasoningShape {
	if mc, ok := m.modelConfig(upstreamID); ok && mc.ReasoningShape != "" {
		return mc.ReasoningShape
	}
	if shape, ok := builtinReasoningShapes[strings.ToLower(upstreamID)]; ok {
		return shape
	}
	return config.ShapeSummary
}

func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	case RouteGemini:
		return m.cfg.Protocols.Gemini
	}
	return true
}

// modelConfig resolves a model's policy, tolerating case differences against
// the configured id: the catalog and the operator's config are maintained in
// different places, and a silently inert entry looks exactly like a broken
// feature.
func (m *Manager) modelConfig(upstreamID string) (config.ModelConfig, bool) {
	if mc, ok := m.cfg.Models[upstreamID]; ok {
		return mc, true
	}
	for name, mc := range m.cfg.Models {
		if strings.EqualFold(name, upstreamID) {
			return mc, true
		}
	}
	return config.ModelConfig{}, false
}

// supportedProtocolsFor returns the protocols this model speaks natively, in
// the configured priority order (first = default route).
func (m *Manager) supportedProtocolsFor(upstreamID string) []Route {
	mc, ok := m.modelConfig(upstreamID)
	if !ok || len(mc.Protocols) == 0 {
		return nil
	}
	out := make([]Route, 0, len(mc.Protocols))
	seen := make(map[Route]struct{}, len(mc.Protocols))
	for _, name := range mc.Protocols {
		route := Route(name)
		if _, dup := seen[route]; dup {
			continue
		}
		seen[route] = struct{}{}
		out = append(out, route)
	}
	return out
}

// resolve applies route priority strictly: user override, compatibility
// table (family prefix), none (arch §5).
func (m *Manager) resolve(e rawModel) (Route, string, bool, string) {
	// Configured models own their route outright: protocols[0] is the default
	// and endpoints may pin a per-protocol path.
	if mc, ok := m.modelConfig(e.ID); ok && len(mc.Protocols) > 0 {
		def := Route(mc.DefaultProtocol())
		endpoint := mc.EndpointFor(string(def))
		// Only a non-default path is worth a diagnostic: pinning the protocol
		// default is the normal case and would flood the warning list.
		reason := ""
		if endpoint != "" && endpoint != def.EndpointPath() {
			reason = fmt.Sprintf("model %q routed by config (%s %s)", e.ID, def, endpoint)
		}
		return def, endpoint, true, reason
	}
	if route, ok := m.fallbackRoute(e.ID); ok {
		return route, "", true, ""
	}
	if isGeminiFamily(e.ID) {
		return RouteGemini, "", true, ""
	}
	return "", "", false, ""
}

// isGeminiFamily reports whether an upstream id belongs to the Gemini
// endpoint family. Gemini is not a heuristic: generateContent carries the
// model and action in the path and answers a different wire shape, so these
// ids always route to RouteGemini regardless of the configured
// fallback-routes table (an explicit `models` entry still wins, and the
// protocols.gemini switch can disable the family).
func isGeminiFamily(upstreamID string) bool {
	lower := strings.ToLower(strings.TrimSpace(upstreamID))
	return strings.HasPrefix(lower, "gemini")
}

// JoinUpstreamURL joins the configured base-url onto an endpoint path and
// is the single source of truth for that join (spec 05 §2): base-url
// already carries the version segment (default .../inference/go/v1, catalog at
// {base}/models), so a leading "/v1" on the endpoint is dropped instead of
// doubled. The authority-escape check (endpointEscapesBase) must parse
// exactly the URL this produces — two independent join formulas would be
// free to drift apart and validate one URL while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	// Plain concatenation. The previous version stripped a leading "/v1" from
	// the endpoint, which silently rewrote "/v1/chat/completions" into
	// "/chat/completions" and made two different path schemes look
	// interchangeable. Endpoints are now spelled out in full.
	return strings.TrimSuffix(baseURL, "/") + "/" + strings.TrimPrefix(endpoint, "/")
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority or scheme
// than the configured base-url. Any parse failure counts as escaping (fail
// closed).
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(JoinUpstreamURL(baseURL, endpoint))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking converts the decoded raw thinking object into its
// pluginapi form; absent objects yield nil, partial ones a partial struct.
func normalizeThinking(rt *rawThinking) *pluginapi.ThinkingSupport {
	if rt == nil {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	if rt.Min != nil {
		t.Min = *rt.Min
	}
	if rt.Max != nil {
		t.Max = *rt.Max
	}
	if rt.ZeroAllowed != nil {
		t.ZeroAllowed = *rt.ZeroAllowed
	}
	if rt.DynamicAllowed != nil {
		t.DynamicAllowed = *rt.DynamicAllowed
	}
	if len(rt.Levels) > 0 {
		t.Levels = append([]string(nil), rt.Levels...)
	}
	return t
}

// SetCachePath enables persistence of the last good catalog snapshot at path.
func (m *Manager) SetCachePath(path string) {
	m.mu.Lock()
	m.cachePath = strings.TrimSpace(path)
	m.mu.Unlock()
}

// SetMetadata installs the models.dev metadata and rebuilds the snapshot from
// the current entries, so limits/thinking levels apply immediately instead of
// waiting for the next upstream refresh.
func (m *Manager) SetMetadata(meta map[string]ModelMetadata) {
	m.mu.Lock()
	raw := m.raw
	m.metadata = meta
	m.mu.Unlock()
	if len(raw) > 0 {
		m.swap(raw)
	}
}

// Restore loads the last good snapshot from disk. It is used at startup before
// the first refresh: without it a fresh process serves no models until the
// upstream answers (a catalog outage would leave the provider empty).
// Restore does not consult catalog.stale-while-unavailable — a cache on disk
// is exactly the stale snapshot that policy is about.
func (m *Manager) Restore() bool {
	m.mu.Lock()
	path := m.cachePath
	m.mu.Unlock()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cached catalogCache
	if json.Unmarshal(data, &cached) != nil || len(cached.Entries) == 0 {
		return false
	}
	m.swap(cached.Entries, "catalog restored from cache")
	return true
}

// persistLocked writes the current entries to the cache path. Failure is
// silent by design: a read-only mount must not break catalog refresh.
func (m *Manager) persistLocked(entries []rawModel) {
	if m.cachePath == "" || len(entries) == 0 {
		return
	}
	payload, err := json.Marshal(catalogCache{FetchedAt: time.Now().UTC(), Entries: entries})
	if err != nil {
		return
	}
	tmp := m.cachePath + ".tmp"
	if os.WriteFile(tmp, payload, 0o600) == nil {
		if os.Rename(tmp, m.cachePath) != nil {
			_ = os.Remove(tmp)
		}
	}
}

// Models returns the routable snapshot (FR-003 identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
