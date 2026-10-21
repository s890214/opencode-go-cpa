package reasoning

import (
	"encoding/json"
	"strings"
)

// TextFromResponseItem extracts the human-readable thinking from a
// Responses `reasoning` item. Upstreams emit the text in `summary[]`
// ("summary_text") or, more rarely, in `content[]` ("reasoning_text");
// both are accepted here so a captured item can be replayed in the shape
// the upstream actually wants.
func TextFromResponseItem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var item struct {
		Type    string `json:"type"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range item.Summary {
		sb.WriteString(part.Text)
	}
	if sb.Len() == 0 {
		for _, part := range item.Content {
			sb.WriteString(part.Text)
		}
	}
	return sb.String()
}

// PlaceholderText stands in when no real reasoning was captured. The
// upstream rejects an empty summary exactly like a missing item
// ("The `reasoning_content` in the thinking mode must be passed back"),
// so the field cannot simply be blank; a short neutral sentence satisfies
// the check without inventing analysis the model did not do.
const PlaceholderText = "Continuing after the tool result."

// Shape values accepted by ResponseItem. They mirror
// config.ReasoningShape without importing it.
const (
	ShapeSummary = "summary"
	ShapeContent = "content"
)

// ResponseItem renders the reasoning item in the shape this upstream expects.
//
// Measured 2026-10-21: deepseek-v4-pro answers `400 reasoning_text must be
// passed back` to the summary form and accepts content[]; deepseek-v4.1-flash
// and glm-5.3-flash answer `503 Endpoint is unavailable.` to content[] and
// accept summary[]. There is no single shape that works for both, so the shape
// is per-model configuration rather than a guess.
//
// An empty text always becomes PlaceholderText: an empty summary is rejected
// exactly like a missing item.
func ResponseItem(text, shape string) json.RawMessage {
	if strings.TrimSpace(text) == "" {
		text = PlaceholderText
	}
	var payload map[string]any
	if shape == ShapeContent {
		payload = map[string]any{
			"type":    "reasoning",
			"content": []map[string]any{{"type": "reasoning_text", "text": text}},
		}
	} else {
		payload = map[string]any{
			"type":    "reasoning",
			"summary": []map[string]any{{"type": "summary_text", "text": text}},
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every field above is a plain string or slice.
		return json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"` + PlaceholderText + `"}]}`)
	}
	return b
}
