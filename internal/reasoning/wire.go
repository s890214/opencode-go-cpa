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

// ResponseItem renders the reasoning item this upstream accepts. The
// `summary` shape is mandatory: a `content[]` reasoning item makes the
// OpenCode Go Responses endpoint answer 503 "Endpoint is unavailable.",
// while `summary[]` with non-empty text is accepted. An unknown turn is
// therefore filled with PlaceholderText rather than dropped — dropping it
// is what produced the 400 in the first place.
func ResponseItem(text string) json.RawMessage {
	if strings.TrimSpace(text) == "" {
		text = PlaceholderText
	}
	payload := map[string]any{
		"type": "reasoning",
		"summary": []map[string]any{
			{"type": "summary_text", "text": text},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every field above is a plain string or slice.
		return json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"` + PlaceholderText + `"}]}`)
	}
	return b
}
