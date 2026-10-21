package shared

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// ThinkingBridgePrefix marks an Anthropic thinking/redacted_thinking block
// carried inside a Responses reasoning item's encrypted_content field. The
// prefix keeps unrelated providers' ciphertext isolated: only blocks this
// plugin encoded are ever decoded back.
const ThinkingBridgePrefix = "cpa-anthropic-thinking:"

// EncodeThinkingBlock serializes one signed Anthropic thinking block into an
// opaque string. A thinking block without a signature (or a redacted block
// without data) is not replayable and reports false.
func EncodeThinkingBlock(block map[string]any) (string, bool) {
	kind, _ := block["type"].(string)
	switch kind {
	case "thinking":
		signature, _ := block["signature"].(string)
		if strings.TrimSpace(signature) == "" {
			return "", false
		}
	case "redacted_thinking":
		data, _ := block["data"].(string)
		if strings.TrimSpace(data) == "" {
			return "", false
		}
	default:
		return "", false
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return "", false
	}
	return ThinkingBridgePrefix + base64.RawURLEncoding.EncodeToString(raw), true
}

// DecodeThinkingBlock reverses EncodeThinkingBlock and re-validates the
// payload, so a malformed bridge envelope can never inject an unsigned block
// into an Anthropic tool turn.
func DecodeThinkingBlock(encrypted string) (map[string]any, bool) {
	encoded, ok := strings.CutPrefix(encrypted, ThinkingBridgePrefix)
	if !ok {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, false
	}
	if _, ok := EncodeThinkingBlock(block); !ok {
		return nil, false
	}
	return block, true
}

// ReasoningItemFromThinkingBlock renders the Responses reasoning item that
// carries a signed Anthropic block through a Codex client: the summary is
// the visible thinking text, encrypted_content is the verbatim block.
func ReasoningItemFromThinkingBlock(id string, block map[string]any) (RespItem, bool) {
	encrypted, ok := EncodeThinkingBlock(block)
	if !ok {
		return RespItem{}, false
	}
	text, _ := block["thinking"].(string)
	item := RespItem{Type: "reasoning", ID: id, EncryptedContent: encrypted}
	if text != "" {
		item.Summary = []struct {
			Type string `json:"type,omitempty"`
			Text string `json:"text"`
		}{{Type: "summary_text", Text: text}}
	}
	return item, true
}

// ThinkingBlockFromReasoningItem decodes a Responses reasoning item that was
// produced by this bridge. Foreign reasoning items (OpenAI ciphertext, plain
// summaries) are not replayable to Anthropic and report false; callers must
// not fabricate an unsigned thinking block in that case.
func ThinkingBlockFromReasoningItem(item RespItem) (map[string]any, bool) {
	if item.EncryptedContent == "" {
		return nil, false
	}
	return DecodeThinkingBlock(item.EncryptedContent)
}
