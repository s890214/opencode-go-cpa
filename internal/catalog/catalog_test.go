package catalog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/config"
)

const testKey = "sk-test-key-123"

// fakeClient is a canned, thread-safe HostClient capturing the last request.
type fakeClient struct {
	mu     sync.Mutex
	resp   pluginapi.HTTPResponse
	err    error
	gotReq *pluginapi.HTTPRequest
}

func (f *fakeClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotReq = &req
	return f.resp, f.err
}

func (f *fakeClient) set(resp pluginapi.HTTPResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp, f.err = resp, err
}

func testCfg() config.Config {
	return config.Config{
		BaseURL:          "https://opencode.ai/zen/go",
		CatalogPath:      "/v1/models",
		ModelPrefix:      config.ModelPrefix{Enabled: true, Value: "opencode-go"},
		MaxResponseBytes: 1 << 20,
		Catalog:          config.Catalog{StaleWhileUnavailable: true},
		Protocols:        config.Protocols{ChatCompletions: true, Messages: true, Responses: true},
	}
}

func newManager(cfg config.Config, fc *fakeClient) *Manager {
	return New(cfg, fc)
}

func mustRefresh(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Refresh(context.Background(), testKey); err != nil {
		t.Fatalf("Refresh: unexpected error %v", err)
	}
}

func findModel(t *testing.T, models []ModelRecord, upstreamID string) ModelRecord {
	t.Helper()
	for _, mo := range models {
		if mo.UpstreamID == upstreamID {
			return mo
		}
	}
	t.Fatalf("model %q not found in %+v", upstreamID, models)
	return ModelRecord{}
}

func TestRouteEndpointPath(t *testing.T) {
	cases := []struct {
		route Route
		want  string
	}{
		{RouteChatCompletions, "/v1/chat/completions"},
		{RouteMessages, "/v1/messages"},
		{RouteResponses, "/v1/responses"},
		{Route("bogus"), ""},
	}
	for _, c := range cases {
		if got := c.route.EndpointPath(); got != c.want {
			t.Errorf("%q.EndpointPath() = %q, want %q", c.route, got, c.want)
		}
	}
}

func TestRefreshRequestShape(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	req := fc.gotReq
	if req.Method != "GET" {
		t.Errorf("Method = %q, want GET", req.Method)
	}
	if req.URL != "https://opencode.ai/zen/go/v1/models" {
		t.Errorf("URL = %q", req.URL)
	}
	if got := req.Headers.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+testKey)
	}
	if got := req.Headers.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestRefreshSuccess(t *testing.T) {
	body := `{"data":[
		{"id":"gpt-5.6-luna"},
		{"id":"glm-5.2"},
		{"id":"minimax-m3"}
	]}`
	cfg := testCfg()
	cfg.Models = map[string]config.ModelConfig{
		"gpt-5.6-luna": {Protocols: []string{"responses"}, Endpoints: map[string]string{"responses": "/v1/responses"}},
	}
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	models := m.Models()
	luna := findModel(t, models, "gpt-5.6-luna")
	if luna.PublicID != "opencode-go/gpt-5.6-luna" {
		t.Errorf("PublicID = %q", luna.PublicID)
	}
	if luna.DisplayName != "gpt-5.6-luna" || luna.Protocol != RouteResponses ||
		luna.EndpointPath != "/v1/responses" {
		t.Errorf("luna mapping wrong: %+v", luna)
	}

	glm := findModel(t, models, "glm-5.2")
	if glm.DisplayName != "glm-5.2" {
		t.Errorf("DisplayName fallback = %q, want upstream id", glm.DisplayName)
	}
	if glm.Protocol != RouteChatCompletions || glm.EndpointPath != "/v1/chat/completions" {
		t.Errorf("glm route wrong: %+v", glm)
	}
	mm := findModel(t, models, "minimax-m3")
	if mm.DisplayName != "minimax-m3" || mm.Protocol != RouteMessages {
		t.Errorf("minimax wrong: %+v", mm)
	}

	// Pinning a protocol's default path is the normal case and stays silent;
	// only a non-default endpoint earns a diagnostic.
	if len(m.Unsupported()) != 0 || len(m.Warnings()) != 0 {
		t.Errorf("unexpected diagnostics: %v %v", m.Unsupported(), m.Warnings())
	}
}

func TestRoutePriorityMatrix(t *testing.T) {
	cfg := testCfg()
	cfg.Models = map[string]config.ModelConfig{
		"totally-new-x": {Protocols: []string{"messages"}, Endpoints: map[string]string{"messages": "/v1/messages-override"}},
		"grok-4.5":      {Protocols: []string{"chat-completions"}, Endpoints: map[string]string{"chat-completions": "/v1/chat/completions"}},
	}
	body := `{"data":[
		{"id":"glm-5.2"},
		{"id":"totally-new-x"},
		{"id":"mystery-model"},
		{"id":"glm-5.1"},
		{"id":"qwen3.8-max"},
		{"id":"grok-4.5"}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	models := m.Models()
	if got := findModel(t, models, "glm-5.2").Protocol; got != RouteChatCompletions {
		t.Errorf("compat route wrong: got %q", got)
	}
	ovr := findModel(t, models, "totally-new-x")
	if ovr.Protocol != RouteMessages || ovr.EndpointPath != "/v1/messages-override" {
		t.Errorf("override not applied: %+v", ovr)
	}
	if got := findModel(t, models, "glm-5.1").Protocol; got != RouteChatCompletions {
		t.Errorf("compat route wrong: got %q", got)
	}
	if got := findModel(t, models, "qwen3.8-max").Protocol; got != RouteMessages {
		t.Errorf("compat route wrong: got %q", got)
	}
	if got := findModel(t, models, "grok-4.5").Protocol; got != RouteChatCompletions {
		t.Errorf("override should beat compat: got %q", got)
	}
	unsup := m.Unsupported()
	if len(unsup) != 1 || unsup[0].UpstreamID != "mystery-model" || unsup[0].Reason != "no route determined" {
		t.Errorf("unsupported wrong: %+v", unsup)
	}
	for _, mo := range models {
		if mo.UpstreamID == "mystery-model" {
			t.Error("unsupported model leaked into routable set")
		}
	}
}

// TestCompatExactIDs pins every documented exact ID from the Aug 2026
// OpenCode Go docs to its required route.
func TestCompatExactIDs(t *testing.T) {
	cases := map[string]Route{
		// Responses.
		"grok-4.5":                   RouteResponses,
		"gpt-5.6-luna":               RouteResponses,
		"muse-spark-1.2-contributor": RouteResponses,
		// Chat Completions.
		"glm-5.3":           RouteChatCompletions,
		"glm-5.2":           RouteChatCompletions,
		"glm-5.1":           RouteChatCompletions,
		"kimi-k3":           RouteChatCompletions,
		"kimi-k2.7-code":    RouteChatCompletions,
		"kimi-k2.6":         RouteChatCompletions,
		"deepseek-v4-pro":   RouteChatCompletions,
		"deepseek-v4-flash": RouteChatCompletions,
		"mimo-v2.5":         RouteChatCompletions,
		"mimo-v2.5-pro":     RouteChatCompletions,
		"hy3":               RouteChatCompletions,
		"hy4-preview":       RouteChatCompletions,
		"longcat-2.0":       RouteChatCompletions,
		// Messages.
		"minimax-m3":   RouteMessages,
		"minimax-m2.7": RouteMessages,
		"minimax-m2.5": RouteMessages,
		"qwen3.8-max":  RouteMessages,
		"qwen3.7-max":  RouteMessages,
		"qwen3.7-plus": RouteMessages,
		"qwen3.6-plus": RouteMessages,
	}
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	i := 0
	for id := range cases {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q}`, id)
		i++
	}
	sb.WriteString(`]}`)
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(sb.String())}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	for _, mo := range m.Models() {
		if want := cases[mo.UpstreamID]; mo.Protocol != want {
			t.Errorf("%s: Protocol = %q, want %q", mo.UpstreamID, mo.Protocol, want)
		}
		if mo.EndpointPath != mo.Protocol.EndpointPath() {
			t.Errorf("%s: EndpointPath = %q", mo.UpstreamID, mo.EndpointPath)
		}
	}
	if len(m.Models()) != len(cases) {
		t.Errorf("got %d models, want %d", len(m.Models()), len(cases))
	}
}

func TestCompatPrefixFallback(t *testing.T) {
	cases := map[string]Route{
		"glm-9.9":        RouteChatCompletions,
		"qwen99-max":     RouteMessages,
		"gpt-9":          RouteResponses,
		"muse-spark-9":   RouteResponses,
		"kimi-k99":       RouteChatCompletions,
		"deepseek-v99":   RouteChatCompletions,
		"minimax-m99":    RouteMessages,
		"mimo-x":         RouteChatCompletions,
		"hy3-extreme":    RouteChatCompletions,
		"hy4-turbo":      RouteChatCompletions,
		"longcat-max":    RouteChatCompletions,
		"GLM-5.2":        RouteChatCompletions,
		"DeepSeek-V4-X":  RouteChatCompletions,
		"HY3":            RouteChatCompletions,
		"HY4":            RouteChatCompletions,
		"GPT-5.6-LUNA-X": RouteResponses,
		"QWEN4-MAX":      RouteMessages,
	}
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	i := 0
	for id := range cases {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q}`, id)
		i++
	}
	sb.WriteString(`]}`)
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(sb.String())}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	if got := len(m.Models()); got != len(cases) {
		t.Fatalf("got %d routable, want %d", got, len(cases))
	}
	for _, mo := range m.Models() {
		if want := cases[mo.UpstreamID]; mo.Protocol != want {
			t.Errorf("%s: Protocol = %q, want %q", mo.UpstreamID, mo.Protocol, want)
		}
	}
}

func TestDedupFirstWins(t *testing.T) {
	body := `{"data":[
		{"id":"glm-5.2"},
		{"id":"glm-5.2"}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	models := m.Models()
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	warns := m.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], `"glm-5.2"`) {
		t.Errorf("warnings = %v, want duplicate diagnostic", warns)
	}
}

func TestMissingIDSkippedAndDiagnosed(t *testing.T) {
	body := `{"data":[{}, {"id":"glm-5.2"}]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	if len(m.Models()) != 1 {
		t.Fatalf("got %d models, want 1", len(m.Models()))
	}
	unsup := m.Unsupported()
	if len(unsup) != 1 || unsup[0].UpstreamID != "" || unsup[0].Reason != "missing id" {
		t.Errorf("unsupported = %+v, want missing-id diagnostic", unsup)
	}
}

func seedGoodSnapshot(t *testing.T, m *Manager) {
	t.Helper()
	mustRefresh(t, m)
}

func TestRefreshErrorsKeepStaleSnapshot(t *testing.T) {
	cases := []struct {
		name     string
		resp     pluginapi.HTTPResponse
		err      error
		maxBytes int64
		wantCat  string
	}{
		{"http 429", pluginapi.HTTPResponse{StatusCode: 429, Body: []byte("{}")}, nil, 1 << 20, "http 429"},
		{"http 500", pluginapi.HTTPResponse{StatusCode: 500}, nil, 1 << 20, "http 500"},
		{"malformed json", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("{not json")}, nil, 1 << 20, "invalid json"},
		{"transport error", pluginapi.HTTPResponse{}, errors.New("dial tcp: refused"), 1 << 20, "network error"},
		{"oversized body", pluginapi.HTTPResponse{StatusCode: 200, Body: make([]byte, catalogBudgetFloor+1)}, nil, 64, "response exceeds max-response-bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.MaxResponseBytes = c.maxBytes
			fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"gpt-5.6-luna"}]}`)}}
			m := newManager(cfg, fc)
			seedGoodSnapshot(t, m)

			fc.set(c.resp, c.err)
			err := m.Refresh(context.Background(), testKey)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantCat) {
				t.Errorf("error %q missing category %q", err, c.wantCat)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("error leaks api key: %q", err)
			}
			if len(m.Models()) != 1 {
				t.Errorf("stale snapshot lost: %d models", len(m.Models()))
			}
			if got := m.Models()[0].UpstreamID; got != "gpt-5.6-luna" {
				t.Errorf("stale snapshot wrong model %q", got)
			}
		})
	}
}

// TestFetchBudgetFloor pins the catalogBudgetFloor fix: an undersized
// max-response-bytes must not make every refresh fail permanently when the
// live catalog is larger than the knob (with stale-while-unavailable:false
// that would wipe the routable set each tick). The fetch check uses
// max(MaxResponseBytes, floor); genuinely oversized bodies (>floor) are
// still rejected.
func TestFetchBudgetFloor(t *testing.T) {
	// Valid ~2KB catalog body, larger than the undersized 1024-byte knob.
	body := `{"data":[` + strings.Repeat(`{"id":"glm-5.2"},`, 2048) + `{"id":"glm-5.1"}]}`
	if int64(len(body)) <= 1024 {
		t.Fatalf("precondition: catalog body %d bytes should exceed the knob", len(body))
	}
	cfg := testCfg()
	cfg.MaxResponseBytes = 1024
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	models := m.Models()
	if len(models) != 2 || models[0].UpstreamID != "glm-5.2" || models[1].UpstreamID != "glm-5.1" {
		t.Fatalf("undersized max-response-bytes broke refresh: %+v", models)
	}

	// A body exceeding the floor stays rejected even with a small knob.
	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: make([]byte, catalogBudgetFloor+1)}, nil)
	err := m.Refresh(context.Background(), testKey)
	if err == nil || !strings.Contains(err.Error(), "response exceeds max-response-bytes") {
		t.Fatalf("oversized body (>floor) accepted: err=%v", err)
	}
}

// TestRefreshNilClientClassifiedError pins that a Manager built without a
// host client (direct construction only; production always wires the bridge)
// fails Refresh through the classified path instead of panicking on Do.
func TestRefreshNilClientClassifiedError(t *testing.T) {
	m := New(testCfg(), nil)
	err := m.Refresh(context.Background(), testKey)
	if err == nil || !strings.Contains(err.Error(), "host client unavailable") {
		t.Fatalf("nil client must yield classified error, got %v", err)
	}
	if len(m.Models()) != 0 {
		t.Fatalf("nil-client refresh must not fabricate models: %+v", m.Models())
	}
}

func TestRefreshErrorClearsWhenNoStale(t *testing.T) {
	cfg := testCfg()
	cfg.Catalog.StaleWhileUnavailable = false
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"gpt-5.6-luna"}]}`)}}
	m := newManager(cfg, fc)
	seedGoodSnapshot(t, m)

	fc.resp = pluginapi.HTTPResponse{StatusCode: 503}
	clearErr := m.Refresh(context.Background(), testKey)
	if clearErr == nil || !strings.Contains(clearErr.Error(), "http 503") {
		t.Fatalf("clear-path error = %v, want http 503 category", clearErr)
	}
	if got := m.Models(); len(got) != 0 {
		t.Errorf("routable set not cleared: %+v", got)
	}
	if got := m.Unsupported(); len(got) != 0 {
		t.Errorf("diagnostics not cleared: %+v", got)
	}
	if got := m.Warnings(); len(got) != 0 {
		t.Errorf("warnings not cleared: %+v", got)
	}

	// Next success restores the routable set.
	fc.err = nil
	fc.resp = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"glm-5.2"}]}`)}
	mustRefresh(t, m)
	if got := m.Models(); len(got) != 1 || got[0].UpstreamID != "glm-5.2" {
		t.Errorf("recovery failed: %+v", got)
	}
}

func TestPublicIDWithoutPrefix(t *testing.T) {
	cfg := testCfg()
	cfg.ModelPrefix.Enabled = false
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"glm-5.2"}]}`)}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	if got := m.Models()[0].PublicID; got != "glm-5.2" {
		t.Errorf("PublicID = %q, want bare id", got)
	}
}

// TestLookupByPrefixedBareAndMiss pins O(1) ID resolution over the swap
// index: public (prefixed) and bare upstream IDs resolve; misses do not.
func TestLookupByPrefixedBareAndMiss(t *testing.T) {
	body := `{"data":[{"id":"glm-5.2"},{"id":"gpt-5.6-luna"}]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	rec, ok := m.Lookup("opencode-go/glm-5.2")
	if !ok || rec.UpstreamID != "glm-5.2" || rec.Protocol != RouteChatCompletions {
		t.Errorf("prefixed lookup = %+v %v", rec, ok)
	}
	rec, ok = m.Lookup("glm-5.2")
	if !ok || rec.PublicID != "opencode-go/glm-5.2" {
		t.Errorf("bare lookup = %+v %v", rec, ok)
	}
	rec, ok = m.Lookup("gpt-5.6-luna")
	if !ok || rec.PublicID != "opencode-go/gpt-5.6-luna" {
		t.Errorf("bare lookup second model = %+v %v", rec, ok)
	}
	if _, ok := m.Lookup("no-such-model"); ok {
		t.Error("miss must not resolve")
	}
	if _, ok := m.Lookup(""); ok {
		t.Error("empty id must not resolve")
	}

	// Lookup must track the refreshed snapshot.
	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}, nil)
	mustRefresh(t, m)
	if _, ok := m.Lookup("glm-5.2"); ok {
		t.Error("cleared snapshot must not serve stale lookups")
	}
}

// TestSeedFromServesOldRecordsWithNewCfg pins the F5 fix: SeedFrom gives m
// prev's last-good snapshot rebuilt under m's OWN cfg — prefix-off here, so
// seeded PublicIDs are bare where prev's were prefixed — and subsequent
// refreshes use m's catalog-path while prev stays untouched.
func TestSeedFromServesOldRecordsWithNewCfg(t *testing.T) {
	body := `{"data":[{"id":"glm-5.2"},{"id":"glm-5.2"},{"id":"mystery-model"}]}`
	prev := newManager(testCfg(), &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(body),
	}})
	mustRefresh(t, prev)

	cfg := testCfg()
	cfg.BaseURL = "https://mirror.test/api"
	cfg.CatalogPath = "/models"
	cfg.ModelPrefix.Enabled = false
	fc := &fakeClient{}
	m := newManager(cfg, fc)
	m.SeedFrom(prev)

	got := m.Models()
	if len(got) != 1 || got[0].UpstreamID != "glm-5.2" || got[0].PublicID != "glm-5.2" {
		t.Fatalf("seeded models = %+v, want glm-5.2 with recomputed bare PublicID", got)
	}
	if rec, ok := m.Lookup("glm-5.2"); !ok || rec.PublicID != "glm-5.2" {
		t.Errorf("seeded bare lookup = %+v %v", rec, ok)
	}
	if _, ok := m.Lookup("opencode-go/glm-5.2"); ok {
		t.Error("prev's prefixed public id must not resolve under m's prefix-off cfg")
	}
	if gotU, wantU := m.Unsupported(), prev.Unsupported(); !reflect.DeepEqual(gotU, wantU) {
		t.Errorf("seeded unsupported = %+v, want %+v", gotU, wantU)
	}
	if gotW, wantW := m.Warnings(), prev.Warnings(); !reflect.DeepEqual(gotW, wantW) {
		t.Errorf("seeded warnings = %v, want %v", gotW, wantW)
	}

	// The next refresh uses m's own cfg: its catalog-path and prefix-off
	// PublicIDs; prev keeps serving its own snapshot untouched.
	fc.resp = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"kimi-k3"}]}`)}
	mustRefresh(t, m)
	if fc.gotReq.URL != "https://mirror.test/api/models" {
		t.Errorf("refresh URL = %q, want the seeded manager's own catalog-path", fc.gotReq.URL)
	}
	if got := m.Models(); len(got) != 1 || got[0].PublicID != "kimi-k3" || got[0].UpstreamID != "kimi-k3" {
		t.Errorf("post-seed refresh models = %+v, want prefix-off kimi-k3", got)
	}
	if got := prev.Models(); len(got) != 1 || got[0].UpstreamID != "glm-5.2" {
		t.Errorf("prev mutated by seeding: %+v", got)
	}
}

func TestRouteCountsViaModels(t *testing.T) {
	body := `{"data":[
		{"id":"glm-5.2"},{"id":"glm-5.1"},
		{"id":"minimax-m3"},{"id":"qwen3.8-max"},{"id":"qwen3.7-plus"},
		{"id":"gpt-5.6-luna"}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	counts := map[Route]int{}
	for _, mo := range m.Models() {
		counts[mo.Protocol]++
	}
	if counts[RouteChatCompletions] != 2 || counts[RouteMessages] != 3 || counts[RouteResponses] != 1 {
		t.Errorf("counts = %v", counts)
	}
}

func TestModelsReturnCopies(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"gpt-5.6-luna"}]}`),
	}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	// The returned slice is a copy; records are rebuilt fresh on every swap
	// and consumers treat them as read-only, so only list isolation is
	// guaranteed (nested slices are shared by design).
	got := m.Models()
	got[0].UpstreamID = "mutated"
	got[0] = ModelRecord{}
	fresh := m.Models()
	if fresh[0].UpstreamID != "gpt-5.6-luna" {
		t.Errorf("snapshot mutated through returned slice: %+v", fresh[0])
	}

	unsup := m.Unsupported()
	_ = append(unsup, UnsupportedModel{UpstreamID: "x"})
	if len(m.Unsupported()) != 0 {
		t.Error("Unsupported not returning a copy")
	}
	warns := m.Warnings()
	_ = append(warns, "x")
	if len(m.Warnings()) != 0 {
		t.Error("Warnings not returning a copy")
	}
}

func TestConcurrentRefreshAndReads(t *testing.T) {
	fc := &fakeClient{}
	m := newManager(testCfg(), fc)

	var writerWG, readerWG sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 3; r++ {
		writerWG.Add(1)
		go func(r int) {
			defer writerWG.Done()
			for i := 0; i < 50; i++ {
				if i%10 == 0 {
					fc.set(pluginapi.HTTPResponse{StatusCode: 500}, nil)
				} else {
					fc.set(pluginapi.HTTPResponse{StatusCode: 200,
						Body: []byte(fmt.Sprintf(`{"data":[{"id":"glm-%d"}]}`, r))}, nil)
				}
				_ = m.Refresh(context.Background(), testKey)
			}
		}(r)
	}
	for r := 0; r < 3; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.Models()
					_ = m.Unsupported()
					_ = m.Warnings()
				}
			}
		}()
	}
	writerWG.Wait() // writers are bounded; then release readers
	close(stop)
	readerWG.Wait()
}

func TestProtocolFlagsExcludeRoutes(t *testing.T) {
	catalogBody := `{"data":[
		{"id":"glm-5.2"},
		{"id":"qwen3.7-max"},
		{"id":"gpt-5.6-luna"}
	]}`
	routes := []struct {
		protocol  string
		field     func(p config.Protocols) bool
		excluded  string
		routableA string
		routableB string
	}{
		{"chat-completions", func(p config.Protocols) bool { return p.ChatCompletions }, "glm-5.2", "qwen3.7-max", "gpt-5.6-luna"},
		{"messages", func(p config.Protocols) bool { return p.Messages }, "qwen3.7-max", "glm-5.2", "gpt-5.6-luna"},
		{"responses", func(p config.Protocols) bool { return p.Responses }, "gpt-5.6-luna", "glm-5.2", "qwen3.7-max"},
	}
	for _, tc := range routes {
		t.Run(tc.protocol, func(t *testing.T) {
			cfg := testCfg()
			switch tc.protocol {
			case "chat-completions":
				cfg.Protocols.ChatCompletions = false
			case "messages":
				cfg.Protocols.Messages = false
			case "responses":
				cfg.Protocols.Responses = false
			}
			fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(catalogBody)}}
			m := newManager(cfg, fc)
			mustRefresh(t, m)

			models := m.Models()
			for _, mo := range models {
				if mo.UpstreamID == tc.excluded {
					t.Errorf("disabled-protocol model %q still routable: %+v", tc.excluded, mo)
				}
			}
			findModel(t, models, tc.routableA)
			findModel(t, models, tc.routableB)

			unsup := m.Unsupported()
			if len(unsup) != 1 || unsup[0].UpstreamID != tc.excluded ||
				unsup[0].Reason != fmt.Sprintf("protocol %s disabled by config", map[string]Route{
					"chat-completions": RouteChatCompletions,
					"messages":         RouteMessages,
					"responses":        RouteResponses,
				}[tc.protocol]) {
				t.Fatalf("unsupported = %+v, want %q excluded with disabled-reason", unsup, tc.excluded)
			}
		})
	}
}

func TestProtocolFlagsAllOnKeepsEverything(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[
		{"id":"glm-5.2"},{"id":"qwen3.7-max"},{"id":"gpt-5.6-luna"}
	]}`)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	if got := len(m.Models()); got != 3 {
		t.Fatalf("models = %d, want all three routable with default flags", got)
	}
	if len(m.Unsupported()) != 0 {
		t.Fatalf("unexpected unsupported entries: %+v", m.Unsupported())
	}
}

func TestProtocolEnabledUnknownRouteDefaultsTrue(t *testing.T) {
	m := newManager(testCfg(), &fakeClient{})
	if !m.protocolEnabled(Route("bogus")) {
		t.Fatal("unknown routes must default to enabled")
	}
}

// TestOverrideApplicationRecordedInWarnings pins spec 04 §5: an applied
// user route override MUST be visible in diagnostics.
func TestOverrideApplicationRecordedInWarnings(t *testing.T) {
	cfg := testCfg()
	cfg.Models = map[string]config.ModelConfig{
		"weird-model": {Protocols: []string{"chat-completions"}, Endpoints: map[string]string{"chat-completions": "/v1/custom"}},
	}
	fc := &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"weird-model"}]}`),
	}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	warns := m.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], `model "weird-model" routed by config`) ||
		!strings.Contains(warns[0], "chat-completions /v1/custom") {
		t.Fatalf("override warning wrong: %+v", warns)
	}
}

// JoinUpstreamURL is a plain concatenation: base is the service root and the
// endpoint is appended verbatim. There is no version-segment stripping any
// more — that used to turn "/v1/chat/completions" into "/chat/completions" and
// made two different path schemes look interchangeable.
func TestJoinUpstreamURL(t *testing.T) {
	cases := []struct{ base, endpoint, want string }{
		{"https://opencode.ai/inference/go", "/openai/v1/chat/completions", "https://opencode.ai/inference/go/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/go/", "/v1/responses", "https://opencode.ai/inference/go/v1/responses"},
		{"https://opencode.ai/inference/go", "", "https://opencode.ai/inference/go/"},
	}
	for _, c := range cases {
		if got := JoinUpstreamURL(c.base, c.endpoint); got != c.want {
			t.Errorf("JoinUpstreamURL(%q, %q) = %q, want %q", c.base, c.endpoint, got, c.want)
		}
	}
}

// TestEndpointEscapesBase pins the authority-escape check used at snapshot
// build. With plain concatenation an endpoint cannot reach the authority slot:
// "@" and "//" stay inside the path, so only parse failures fail closed.
func TestEndpointEscapesBase(t *testing.T) {
	cases := []struct {
		base     string
		endpoint string
		want     bool
	}{
		{"https://opencode.ai", "/v1@evil.com/x", false}, // @ stays in the path
		{"https://opencode.ai", "@evil.com/", false},
		{"https://opencode.ai", "v1@evil.com", false},
		{"https://opencode.ai", "//evil.com/x", false},
		{"https://opencode.ai", "/v1/chat/completions", false},
		{"https://OPENCODE.ai", "/openai/v1/chat/completions", false}, // host compare is case-insensitive
	}
	for _, c := range cases {
		if got := endpointEscapesBase(c.base, c.endpoint); got != c.want {
			t.Errorf("endpointEscapesBase(%q, %q) = %v, want %v", c.base, c.endpoint, got, c.want)
		}
	}
}

func TestFailClearsLookupIndex(t *testing.T) {
	cfg := testCfg()
	cfg.Catalog.StaleWhileUnavailable = false
	fc := &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"glm-5.2"}]}`),
	}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	if _, ok := m.Lookup("glm-5.2"); !ok {
		t.Fatal("precondition: glm-5.2 must resolve after good refresh")
	}

	fc.resp = pluginapi.HTTPResponse{StatusCode: 503}
	if err := m.Refresh(context.Background(), testKey); err == nil {
		t.Fatal("expected refresh failure")
	}
	if got := m.Models(); len(got) != 0 {
		t.Fatalf("models not cleared: %+v", got)
	}
	// FR-002: a cleared snapshot must stop resolving previously-valid IDs.
	if _, ok := m.Lookup("glm-5.2"); ok {
		t.Fatal("Lookup still resolves IDs from the cleared snapshot")
	}
}

// TestSeedFromAppliesProtocolKillSwitch pins the seed-revalidation fix: a
// protocol disabled between snapshots must demote its routed records at
// seed time. Otherwise an operator flipping protocols.messages=false during
// a catalog outage keeps serving messages-routed models off the stale
// snapshot until a refresh succeeds — the kill-switch is inert exactly when
// it needs to bite.
func TestSeedFromAppliesProtocolKillSwitch(t *testing.T) {
	body := `{"data":[{"id":"minimax-m3"},{"id":"glm-5.2"}]}`
	prev := newManager(testCfg(), &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(body),
	}})
	mustRefresh(t, prev)
	findModel(t, prev.Models(), "minimax-m3") // precondition: routable pre-seed

	cfg := testCfg()
	cfg.Protocols.Messages = false
	m := newManager(cfg, &fakeClient{})
	m.SeedFrom(prev)

	for _, mo := range m.Models() {
		if mo.Protocol == RouteMessages {
			t.Fatalf("disabled-protocol record still routable after seed: %+v", mo)
		}
	}
	findModel(t, m.Models(), "glm-5.2") // unaffected route keeps serving
	if _, ok := m.Lookup("minimax-m3"); ok {
		t.Fatal("disabled-protocol record must not resolve via Lookup")
	}
	var found bool
	for _, u := range m.Unsupported() {
		if u.UpstreamID == "minimax-m3" && u.Reason == "protocol messages disabled by config" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unsupported = %+v, want minimax-m3 demotion diagnostic", m.Unsupported())
	}
	findModel(t, prev.Models(), "minimax-m3") // seeding must not mutate prev
}

// TestSeedFromRecomputesPublicIDsUnderNewPrefix pins prefix recomputation at
// seed time: records rebuilt from prev's raw entries publish m's configured
// prefix value, and prev's prefixed public id stops resolving.
func TestSeedFromRecomputesPublicIDsUnderNewPrefix(t *testing.T) {
	prev := newManager(testCfg(), &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"glm-5.2"}]}`),
	}})
	mustRefresh(t, prev)
	if got := findModel(t, prev.Models(), "glm-5.2").PublicID; got != "opencode-go/glm-5.2" {
		t.Fatalf("precondition: prev PublicID = %q", got)
	}

	cfg := testCfg()
	cfg.ModelPrefix.Value = "og"
	m := newManager(cfg, &fakeClient{})
	m.SeedFrom(prev)

	got := m.Models()
	if len(got) != 1 || got[0].UpstreamID != "glm-5.2" || got[0].PublicID != "og/glm-5.2" {
		t.Fatalf("seeded models = %+v, want glm-5.2 republished as og/glm-5.2", got)
	}
	if rec, ok := m.Lookup("og/glm-5.2"); !ok || rec.UpstreamID != "glm-5.2" {
		t.Errorf("new-prefix lookup = %+v %v", rec, ok)
	}
	if _, ok := m.Lookup("opencode-go/glm-5.2"); ok {
		t.Error("old-prefix public id must stop resolving after seed")
	}
}

// TestSwapPublicUpstreamCrossKeyCollision pins the index-collision fix: with
// the prefix enabled, upstream "foo" publishes public "opencode-go/foo" while
// an upstream literally named "opencode-go/foo" claims that same key as its
// UpstreamID; last-write-wins would silently misroute one of them. First in
// catalog order wins, warning is emitted, loser lands in Unsupported.
func TestSwapPublicUpstreamCrossKeyCollision(t *testing.T) {
	const wantMsg = `public id "opencode-go/foo" collides with upstream id "opencode-go/foo"`
	for _, order := range []string{"foo first", "opencode-go/foo first"} {
		t.Run(order, func(t *testing.T) {
			ids := []string{"foo", "opencode-go/foo"}
			if order == "opencode-go/foo first" {
				ids[0], ids[1] = ids[1], ids[0]
			}
			body := fmt.Sprintf(`{"data":[{"id":%q},{"id":%q}]}`, ids[0], ids[1])
			fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
			cfg := testCfg()
			cfg.Models = map[string]config.ModelConfig{
				ids[0]: {Protocols: []string{"chat-completions"}, Endpoints: map[string]string{"chat-completions": "/v1/chat/completions"}},
				ids[1]: {Protocols: []string{"messages"}, Endpoints: map[string]string{"messages": "/v1/messages"}},
			}
			m := newManager(cfg, fc)
			mustRefresh(t, m)

			models := m.Models()
			if len(models) != 1 || models[0].UpstreamID != ids[0] || models[0].PublicID != "opencode-go/"+ids[0] {
				t.Fatalf("models = %+v, want single winner %q in catalog order", models, ids[0])
			}
			if rec, ok := m.Lookup("opencode-go/foo"); !ok || rec.UpstreamID != ids[0] {
				t.Fatalf("contested key resolves to %+v (%v), want the first-in-order record", rec, ok)
			}
			foundWarn := false
			for _, w := range m.Warnings() {
				if w == wantMsg {
					foundWarn = true
				}
			}
			if !foundWarn {
				t.Fatalf("warnings = %v, want %q", m.Warnings(), wantMsg)
			}
			unsup := m.Unsupported()
			if len(unsup) != 1 || unsup[0].UpstreamID != ids[1] || unsup[0].Reason != wantMsg {
				t.Fatalf("unsupported = %+v, want loser %q with %q", unsup, ids[1], wantMsg)
			}
		})
	}
}

// TestRefreshDataKeyAbsentVsEmpty pins the absence-vs-empty split: a body
// without a "data" key is upstream shape drift — the snapshot clears like an
// empty plan AND a diagnostic is recorded — while a present-but-empty array
// is an intended clear that stays silent.
func TestRefreshDataKeyAbsentVsEmpty(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"glm-5.2"}]}`),
	}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	const driftWarn = `upstream catalog response missing "data" field`

	// Key absent: cleared snapshot plus shape-drift warning.
	fc.set(pluginapi.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"object":"list","url":"/v1/models"}`),
	}, nil)
	mustRefresh(t, m)
	if got := m.Models(); len(got) != 0 {
		t.Fatalf("absent data key kept stale models: %+v", got)
	}
	found := false
	for _, w := range m.Warnings() {
		if w == driftWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %q", m.Warnings(), driftWarn)
	}

	// Key present but empty array: intended clear, no warning.
	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}, nil)
	mustRefresh(t, m)
	if got := m.Models(); len(got) != 0 {
		t.Fatalf("empty plan did not clear: %+v", got)
	}
	if got := m.Warnings(); len(got) != 0 {
		t.Fatalf("intended clear must stay silent, got %v", got)
	}
}

// JoinUpstreamURL concatenates base and endpoint verbatim, so no endpoint can
// move a request to another authority. This used to be a real attack surface:
// the old join stripped a leading "/v1", which left "@/evil.com" sitting at the
// authority position and could redirect a seeded catalog entry to an attacker.
func TestEndpointCannotEscapeConfiguredHost(t *testing.T) {
	base := "https://opencode.ai"
	for _, endpoint := range []string{
		"/v1@evil.com/x",
		"@evil.com/",
		"//evil.com/x",
		"v1@evil.com",
		"/openai/v1/chat/completions",
	} {
		got := JoinUpstreamURL(base, endpoint)
		if !strings.HasPrefix(got, "https://opencode.ai/") {
			t.Fatalf("%q escaped the configured host: %s", endpoint, got)
		}
	}
}

func TestSwapUsesConfiguredProtocolEndpointForDefaultRoute(t *testing.T) {
	cfg := testCfg()
	cfg.BaseURL = "https://example.com"
	cfg.ProtocolEndpoints = map[string]string{
		"chat-completions": "/openai/v1/chat/completions",
		"messages":         "/anthropic/v1/messages",
		"responses":        "/openai/v1/responses",
	}
	cfg.Models = map[string]config.ModelConfig{
		"claude-haiku-5-5": {Protocols: []string{"messages"}},
	}
	m := newManager(cfg, nil)
	m.swap([]rawModel{{ID: "claude-haiku-5-5"}})
	rec := findModel(t, m.Models(), "claude-haiku-5-5")
	if rec.EndpointPath != "/anthropic/v1/messages" {
		t.Errorf("default-route endpoint = %q, want configured /anthropic/v1/messages", rec.EndpointPath)
	}
}

func TestSwapPublishesPerProtocolEndpointOverrides(t *testing.T) {
	cfg := testCfg()
	cfg.BaseURL = "https://example.com"
	cfg.ProtocolEndpoints = map[string]string{
		"chat-completions": "/openai/v1/chat/completions",
		"messages":         "/anthropic/v1/messages",
		"responses":        "/openai/v1/responses",
	}
	cfg.Models = map[string]config.ModelConfig{
		"m1": {
			Protocols: []string{"chat-completions", "responses"},
			Endpoints: map[string]string{"responses": "/openai/v1/custom-responses"},
		},
	}
	m := newManager(cfg, nil)
	m.swap([]rawModel{{ID: "m1"}})
	rec := findModel(t, m.Models(), "m1")
	if got := rec.EndpointPaths["responses"]; got != "/openai/v1/custom-responses" {
		t.Errorf("EndpointPaths[responses] = %q", got)
	}
	if got := rec.EndpointPaths["chat-completions"]; got != "/openai/v1/chat/completions" {
		t.Errorf("EndpointPaths[chat-completions] = %q", got)
	}
}

func TestFallbackRoutesEmptyDisablesGuessing(t *testing.T) {
	cfg := testCfg()
	cfg.BaseURL = "https://example.com"
	cfg.FallbackRoutes = []config.FallbackRoute{} // explicit empty
	m := newManager(cfg, nil)
	m.swap([]rawModel{{ID: "glm-99"}})
	if got := len(m.Models()); got != 0 {
		t.Fatalf("explicit empty fallback-routes still routed %d model(s)", got)
	}
}

func TestFallbackRoutesNilUsesBuiltinTable(t *testing.T) {
	cfg := testCfg()
	cfg.BaseURL = "https://example.com"
	m := newManager(cfg, nil)
	m.swap([]rawModel{{ID: "glm-99"}})
	rec := findModel(t, m.Models(), "glm-99")
	if rec.Protocol != RouteChatCompletions {
		t.Errorf("nil fallback-routes route = %q, want chat-completions", rec.Protocol)
	}
}
