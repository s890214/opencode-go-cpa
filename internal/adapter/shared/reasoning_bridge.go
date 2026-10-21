package shared

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// OpenAIReasoningBridgePrefix marks a Responses reasoning item carried inside
// an Anthropic thinking signature (or redacted_thinking data). The direction
// is the mirror of ThinkingBridgePrefix: this one lets a Claude client replay
// reasoning that came from a Responses upstream.
const OpenAIReasoningBridgePrefix = "cpa-openai-reasoning-v1:"

type reasoningSummaryPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text"`
}

type reasoningEnvelope struct {
	Type             string                 `json:"type"`
	ID               string                 `json:"id,omitempty"`
	Summary          []reasoningSummaryPart `json:"summary,omitempty"`
	EncryptedContent string                 `json:"encrypted_content,omitempty"`
}

// ReasoningSummaryText concatenates the visible summary text of a Responses
// reasoning item.
func ReasoningSummaryText(item RespItem) string {
	parts := make([]string, 0, len(item.Summary))
	for _, part := range item.Summary {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// EncodeOpenAIReasoningItem serializes the replayable subset of a Responses
// reasoning item. Output-only fields the backend may add later are dropped so
// the envelope stays valid as an input item.
func EncodeOpenAIReasoningItem(item RespItem) (string, bool) {
	if item.Type != "reasoning" {
		return "", false
	}
	env := reasoningEnvelope{
		Type:             "reasoning",
		ID:               item.ID,
		EncryptedContent: item.EncryptedContent,
	}
	for _, part := range item.Summary {
		env.Summary = append(env.Summary, reasoningSummaryPart{Type: "summary_text", Text: part.Text})
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", false
	}
	return OpenAIReasoningBridgePrefix + base64.RawURLEncoding.EncodeToString(raw), true
}

// DecodeOpenAIReasoningItem reverses EncodeOpenAIReasoningItem.
func DecodeOpenAIReasoningItem(encoded string) (RespItem, bool) {
	payload, ok := strings.CutPrefix(encoded, OpenAIReasoningBridgePrefix)
	if !ok {
		return RespItem{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return RespItem{}, false
	}
	var env reasoningEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != "reasoning" {
		return RespItem{}, false
	}
	item := RespItem{Type: "reasoning", ID: env.ID, EncryptedContent: env.EncryptedContent}
	for _, part := range env.Summary {
		item.Summary = append(item.Summary, struct {
			Type string `json:"type,omitempty"`
			Text string `json:"text"`
		}{Type: "summary_text", Text: part.Text})
	}
	return item, true
}

// ThinkingBlockFromOpenAIReasoning renders a Responses reasoning item as an
// Anthropic thinking block. When encrypted_content is present the whole item
// is carried in the signature so the next Claude turn can restore it; a
// summary without ciphertext is visible thinking without a signature.
func ThinkingBlockFromOpenAIReasoning(item RespItem) (map[string]any, bool) {
	if item.Type != "reasoning" {
		return nil, false
	}
	text := ReasoningSummaryText(item)
	if item.EncryptedContent != "" {
		envelope, ok := EncodeOpenAIReasoningItem(item)
		if !ok {
			return nil, false
		}
		if text == "" {
			return map[string]any{"type": "redacted_thinking", "data": envelope}, true
		}
		return map[string]any{"type": "thinking", "thinking": text, "signature": envelope}, true
	}
	if text == "" {
		return nil, false
	}
	return map[string]any{"type": "thinking", "thinking": text}, true
}

// OpenAIReasoningFromThinkingBlock restores a reasoning item only from a
// bridge-owned signature/data. Native Anthropic thinking has no Responses
// ciphertext and is therefore not replayable.
func OpenAIReasoningFromThinkingBlock(block map[string]any) (RespItem, bool) {
	switch block["type"] {
	case "thinking":
		signature, _ := block["signature"].(string)
		return DecodeOpenAIReasoningItem(signature)
	case "redacted_thinking":
		data, _ := block["data"].(string)
		return DecodeOpenAIReasoningItem(data)
	default:
		return RespItem{}, false
	}
}

// SyntheticThinkingSignature carries plain Chat Completions reasoning through
// an Anthropic thinking block. It never reaches a native Anthropic upstream:
// the same route converts the block back to reasoning_content.
func SyntheticThinkingSignature(text string) string {
	return "cpa-chat-reasoning:" + base64.RawURLEncoding.EncodeToString([]byte(text))
}

// SyntheticThinkingText decodes SyntheticThinkingSignature; ok is false for
// any other signature.
func SyntheticThinkingText(signature string) (string, bool) {
	payload, ok := strings.CutPrefix(signature, "cpa-chat-reasoning:")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	return string(raw), true
}
