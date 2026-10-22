package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"opencode-go-cpa/internal/errclass"
)

// Chat tool messages are text-only on every OpenAI-compatible upstream, but a
// Responses function_call_output may carry image / file / audio blocks (Codex
// screenshots, PDF reads). Chat bridges therefore move those blocks into a
// synthetic user message and leave a marker in the tool text, which is exactly
// what the reference implementation does. Keeping the text path byte-identical
// when no media is present preserves prompt caching.
const (
	// ToolMediaMovedMarker replaces a media block inside the tool text.
	ToolMediaMovedMarker = "[tool result media moved to the following user message]"
)

// ChatToolMediaPlan is the Chat-side view of a tool result that carries media.
// Text is what the role:"tool" message keeps; Media holds chat content parts
// re-emitted on a following user message.
type ChatToolMediaPlan struct {
	Text  string
	Media []map[string]any
}

// PlanChatToolOutput inspects a Responses function_call_output payload. It
// returns nil when the payload has no media, so callers keep the existing
// text-only path untouched. targetNoun names the target wire vocabulary in
// rejection messages.
func PlanChatToolOutput(raw json.RawMessage, targetNoun string) (*ChatToolMediaPlan, *errclass.Error) {
	if !HasContent(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		// Whole-string image data URLs are a legitimate scalar form; small
		// inline strings stay text so workflows that inspect them keep working.
		if part, ok := wholeStringImagePart(s); ok {
			return &ChatToolMediaPlan{Text: ToolMediaMovedMarker, Media: []map[string]any{part}}, nil
		}
		return nil, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, errclass.Translation("tool_result content must be a string or an array of blocks")
	}

	var b strings.Builder
	media := make([]map[string]any, 0, 2)
	for _, block := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &kind) != nil {
			return nil, errclass.Translation("malformed content block")
		}
		switch kind.Type {
		case "text", "output_text", "input_text":
			var t struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(block, &t); err != nil {
				return nil, errclass.Translation("malformed text content block")
			}
			b.WriteString(t.Text)
		case "input_image", "image_url", "image":
			part, ok := chatImagePart(block)
			if !ok {
				return nil, errclass.Translation(fmt.Sprintf(
					"unsupported image content block; %s", targetNoun))
			}
			media = append(media, part)
			b.WriteString(ToolMediaMovedMarker)
		case "input_file":
			part, ok := chatFilePart(block)
			if !ok {
				return nil, errclass.Translation(fmt.Sprintf(
					"input_file block needs file_id or file_data; %s", targetNoun))
			}
			media = append(media, part)
			b.WriteString(ToolMediaMovedMarker)
		case "input_audio":
			part, ok := chatAudioPart(block)
			if !ok {
				return nil, errclass.Translation(fmt.Sprintf(
					"input_audio block needs an input_audio object; %s", targetNoun))
			}
			media = append(media, part)
			b.WriteString(ToolMediaMovedMarker)
		default:
			return nil, errclass.Translation(fmt.Sprintf(
				"unsupported tool content part type %q; %s", kind.Type, targetNoun))
		}
	}
	if len(media) == 0 {
		return nil, nil
	}
	return &ChatToolMediaPlan{Text: b.String(), Media: media}, nil
}

// wholeStringImagePart recognizes a scalar that is itself a complete base64
// image data URL. Mirrors the reference threshold: tiny inline icons stay text.
func wholeStringImagePart(s string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(s)
	const minBytes = 8 * 1024
	if len(trimmed) < minBytes {
		return nil, false
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "data:image/") || !strings.Contains(lower, ";base64,") {
		return nil, false
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": trimmed},
	}, true
}

// chatImagePart maps input_image / image_url / Anthropic-style image blocks to
// the Chat image_url part.
func chatImagePart(block json.RawMessage) (map[string]any, bool) {
	var item struct {
		Type     string          `json:"type"`
		ImageURL json.RawMessage `json:"image_url"`
		Source   *struct {
			Type      string `json:"type"`
			URL       string `json:"url"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(block, &item); err != nil {
		return nil, false
	}
	// input_image / image_url: image_url is either a bare string or {url:...}
	if len(item.ImageURL) > 0 {
		var url string
		if json.Unmarshal(item.ImageURL, &url) == nil && strings.TrimSpace(url) != "" {
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			}, true
		}
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(item.ImageURL, &obj) == nil && strings.TrimSpace(obj.URL) != "" {
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": obj.URL},
			}, true
		}
	}
	// Anthropic-style image: {"type":"image","source":{"type":"base64","media_type":...,"data":...}}
	if item.Source != nil {
		switch item.Source.Type {
		case "base64":
			if item.Source.Data == "" {
				return nil, false
			}
			mediaType := item.Source.MediaType
			if mediaType == "" {
				mediaType = "image/png"
			}
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + mediaType + ";base64," + item.Source.Data},
			}, true
		case "url":
			if strings.TrimSpace(item.Source.URL) == "" {
				return nil, false
			}
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": item.Source.URL},
			}, true
		}
	}
	return nil, false
}

// chatFilePart maps a Responses input_file block to the Chat file part.
func chatFilePart(block json.RawMessage) (map[string]any, bool) {
	var item struct {
		FileID   string `json:"file_id"`
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(block, &item); err != nil {
		return nil, false
	}
	if item.FileID == "" && item.FileData == "" {
		return nil, false
	}
	file := map[string]any{}
	if item.FileID != "" {
		file["file_id"] = item.FileID
	}
	if item.FileData != "" {
		file["file_data"] = item.FileData
	}
	if item.Filename != "" {
		file["filename"] = item.Filename
	}
	return map[string]any{"type": "file", "file": file}, true
}

// chatAudioPart maps a Responses input_audio block to the Chat input_audio part.
func chatAudioPart(block json.RawMessage) (map[string]any, bool) {
	var item struct {
		InputAudio json.RawMessage `json:"input_audio"`
	}
	if err := json.Unmarshal(block, &item); err != nil || len(item.InputAudio) == 0 {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(item.InputAudio, &obj) != nil || len(obj) == 0 {
		return nil, false
	}
	return map[string]any{"type": "input_audio", "input_audio": obj}, true
}
