package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ModelMetadata is the subset of models.dev data this plugin can use. It is
// merged UNDER the upstream catalog: the upstream response always wins, and
// metadata only fills gaps (limits the upstream omitted, thinking levels it
// never advertises).
type ModelMetadata struct {
	ContextLimit   int64
	OutputLimit    int64
	InputModes     []string
	OutputModes    []string
	Reasoning      bool
	ThinkingLevels []string
}

// metadataBudget caps the metadata document (the real feed is ~6 MiB).
const metadataBudget = 32 << 20

// devEntry mirrors one models.dev model entry. Unknown fields are ignored.
type devEntry struct {
	ID        string `json:"id"`
	Reasoning bool   `json:"reasoning"`
	Limits    struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
}

// FetchMetadata downloads and decodes the models.dev feed into per-model
// metadata. The feed groups models by provider; this provider appears as
// "opencode-go", with "opencode" as the wider catalog it is drawn from.
// A nil client is a programming error and returns an error rather than
// panicking inside the host callback.
func FetchMetadata(ctx context.Context, client HostClient, url string) (map[string]ModelMetadata, error) {
	if client == nil {
		return nil, fmt.Errorf("metadata: host client unavailable")
	}
	if strings.TrimSpace(url) == "" {
		return nil, nil
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     url,
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: network error")
	}
	if int64(len(resp.Body)) > metadataBudget {
		return nil, fmt.Errorf("metadata: response exceeds budget")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata: http %d", resp.StatusCode)
	}
	var root map[string]struct {
		Models map[string]devEntry `json:"models"`
	}
	if err := json.Unmarshal(resp.Body, &root); err != nil {
		return nil, fmt.Errorf("metadata: invalid json")
	}
	out := make(map[string]ModelMetadata)
	// Narrow section first, then the wider one: entries present in both keep
	// the opencode-go values (same model, plan-specific pricing/limits).
	for _, section := range []string{"opencode", "opencode-go"} {
		provider, ok := root[section]
		if !ok {
			continue
		}
		for id, entry := range provider.Models {
			key := strings.TrimSpace(id)
			if key == "" {
				key = strings.TrimSpace(entry.ID)
			}
			if key == "" {
				continue
			}
			meta := ModelMetadata{
				ContextLimit: entry.Limits.Context,
				OutputLimit:  entry.Limits.Output,
				InputModes:   entry.Modalities.Input,
				OutputModes:  entry.Modalities.Output,
				Reasoning:    entry.Reasoning,
			}
			for _, option := range entry.ReasoningOptions {
				if option.Type == "effort" && len(option.Values) > 0 {
					meta.ThinkingLevels = append([]string(nil), option.Values...)
				}
			}
			out[key] = meta
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("metadata: no known provider section")
	}
	return out, nil
}

// mergeThinking fills thinking capability gaps from metadata: a model that
// advertises reasoning but no levels still lets a client request thinking, and
// a model whose upstream object is absent gets the effort ladder models.dev
// publishes. Upstream values are never overwritten.
func mergeThinking(upstream *pluginapi.ThinkingSupport, meta ModelMetadata, known bool) *pluginapi.ThinkingSupport {
	ts := upstream
	if ts == nil {
		if !known || (!meta.Reasoning && len(meta.ThinkingLevels) == 0) {
			return nil
		}
		ts = &pluginapi.ThinkingSupport{}
	}
	if len(ts.Levels) == 0 && len(meta.ThinkingLevels) > 0 {
		ts.Levels = append([]string(nil), meta.ThinkingLevels...)
	}
	if len(meta.ThinkingLevels) == 0 && meta.Reasoning && !ts.ZeroAllowed && ts.Min == 0 && ts.Max == 0 {
		// Reasoning with no ladder (toggle-only model): thinking can be
		// switched off and left to the upstream to size.
		ts.ZeroAllowed = true
		ts.DynamicAllowed = true
	}
	return ts
}

// metadataFor resolves metadata whose key matches case-insensitively; the
// feed and the upstream catalog are maintained separately and have differed
// in case before.
func metadataFor(meta map[string]ModelMetadata, id string) (ModelMetadata, bool) {
	if len(meta) == 0 {
		return ModelMetadata{}, false
	}
	if m, ok := meta[id]; ok {
		return m, true
	}
	for key, m := range meta {
		if strings.EqualFold(key, id) {
			return m, true
		}
	}
	return ModelMetadata{}, false
}

// catalogCache is the on-disk last-good snapshot. It stores the decoded
// upstream entries (not enriched records), so a restart rebuilds against the
// CURRENT config exactly like a refresh would.
type catalogCache struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Entries   []rawModel `json:"entries"`
}
