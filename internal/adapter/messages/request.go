package messages

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/errclass"
	"opencode-go-cpa/internal/thinking"
)

type anthropicBlock = map[string]any

func textBlock(text string) anthropicBlock {
	return anthropicBlock{"type": "text", "text": text}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicBlock
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// messagesRequest is the outbound Anthropic encode shape — intentionally
// divergent from shared.ClaudeTool decode type: typed stop sequences +
// named items vs raw-ignore + tool_choice.
type messagesRequest struct {
	Model         string                 `json:"model"`
	MaxTokens     int64                  `json:"max_tokens"`
	System        any                    `json:"system,omitempty"` // string or []anthropicBlock
	Messages      []anthropicMessage     `json:"messages"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	ToolChoice    any                    `json:"tool_choice,omitempty"` // {type,name?,disable_parallel_tool_use?}
	Thinking      *shared.ClaudeThinking `json:"thinking,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
}

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "openai-response" Responses, "claude"
// passthrough) into an Anthropic Messages body for upstreamModel
// (FR-005). ts carries the target model's thinking capability so
// reasoning controls resolve against what the model actually supports.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation. Errors are descriptive and redacted — no silent loss.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "claude":
		return passthroughClaude(upstreamModel, sourceBody)
	case "openai":
		return fromChatCompletions(upstreamModel, sourceBody, ts)
	case "openai-response":
		return fromResponses(upstreamModel, sourceBody, ts, shared.ResponseToolContext(tools))
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// passthroughClaude rewrites the model ID on a native Claude request,
// folding any in-history role:"system" turns into the top-level system
// field (Anthropic rejects system roles inside messages). Text joins with
// blank lines; a string system stays a string, a block-array system gains
// a text block, an absent system becomes a string. Non-text system content
// is rejected descriptively, never dropped. Without system turns it is a
// pure model rewrite with no other mutation.
func passthroughClaude(upstreamModel string, sourceBody []byte) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(sourceBody, &req); err != nil {
		return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
	}
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	rawMsgs, ok := req["messages"]
	if !ok || !shared.HasContent(rawMsgs) {
		return shared.RewriteModelID(upstreamModel, sourceBody, "claude")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &arr); err != nil {
		return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
	}
	var wire struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	hasSystem := false
	for _, raw := range arr {
		// Malformed turns fail explicitly rather than passing through.
		var w struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
		}
		if w.Role == "system" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		return shared.RewriteModelID(upstreamModel, sourceBody, "claude")
	}
	var folded []string
	kept := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
		}
		if wire.Role != "system" {
			kept = append(kept, raw)
			continue
		}
		text, eErr := systemTurnText(wire.Content)
		if eErr != nil {
			return nil, eErr
		}
		if text != "" {
			folded = append(folded, text)
		}
	}
	if len(folded) > 0 {
		if eErr := foldSystem(req, folded); eErr != nil {
			return nil, eErr
		}
	}
	msgs, err := json.Marshal(kept)
	if err != nil {
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	req["messages"] = msgs
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)
	out, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	return out, nil
}

// systemTurnText flattens one in-history system turn's content to text.
// String content passes through; text blocks join with blank lines;
// thinking blocks are omitted; images and unknown types are rejected
// descriptively rather than dropped.
func systemTurnText(raw json.RawMessage) (string, *errclass.Error) {
	if !shared.HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return "", errclass.Translation("message content must be a string or an array of blocks")
	}
	var b strings.Builder
	for _, elem := range elems {
		var blk struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(elem, &blk); err != nil {
			return "", errclass.Translation("malformed content block")
		}
		switch blk.Type {
		case "text":
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		case "image":
			return "", shared.SystemImageRejected()
		case "thinking", "redacted_thinking":
		default:
			return "", shared.UnsupportedPartType(blk.Type, EndpointPath)
		}
	}
	return b.String(), nil
}

// foldSystem merges folded in-history system texts into the envelope's
// system field: strings append with blank-line separators, block arrays
// gain one text block, absent systems become a string.
func foldSystem(req map[string]json.RawMessage, folded []string) *errclass.Error {
	joined := strings.Join(folded, "\n\n")
	rawSys, ok := req["system"]
	if !ok || !shared.HasContent(rawSys) {
		b, _ := json.Marshal(joined) // string always marshals
		req["system"] = b
		return nil
	}
	var s string
	if err := json.Unmarshal(rawSys, &s); err == nil {
		if s != "" {
			joined = s + "\n\n" + joined
		}
		b, _ := json.Marshal(joined) // string always marshals
		req["system"] = b
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(rawSys, &blocks); err != nil {
		return errclass.Translation("malformed system field")
	}
	b, _ := json.Marshal(textBlock(joined)) // marshallable composed type; cannot fail
	blocks = append(blocks, b)
	merged, err := json.Marshal(blocks)
	if err != nil {
		return errclass.Translation("model id cannot be represented as JSON")
	}
	req["system"] = merged
	return nil
}

// contentParts normalizes an OpenAI-style content field into Anthropic
// blocks via the shared kernel, which owns per-part ordering, validation,
// and the empty-input policy (FR-005 multimodal preservation, F5 parity).
func contentParts(raw json.RawMessage) ([]anthropicBlock, *errclass.Error) {
	return shared.ClaudeBlocksFromOpenAI(raw, EndpointPath)
}

// hasImage reports whether any normalized content block carries image
// content (FR-005).
func hasImage(blocks []anthropicBlock) bool {
	for _, blk := range blocks {
		if blk["type"] == "image" {
			return true
		}
	}
	return false
}

// decodeStop accepts the OpenAI stop field as a string or array of strings.
func decodeStop(raw json.RawMessage) ([]string, *errclass.Error) {
	if !shared.HasContent(raw) {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errclass.Translation("stop must be a string or an array of strings")
	}
	return many, nil
}

// systemField renders collected system text as a single string when one
// piece, or text blocks when several (AC §C: system represented correctly).
func systemField(parts []string) any {
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		blocks := make([]anthropicBlock, len(parts))
		for i, p := range parts {
			blocks[i] = textBlock(p)
		}
		return blocks
	}
}

// msgBuilder groups consecutive blocks into alternating-role Anthropic
// messages. Buckets with distinct keys merge only when both canonical
// roles are "user" (tool_result + later user text in one turn) — the
// Messages contract rejects consecutive same-role messages.
type msgBuilder struct {
	curKey string
	cur    *anthropicMessage
	msgs   []anthropicMessage
}

func (b *msgBuilder) add(key, role string, block anthropicBlock) {
	if b.cur == nil || (b.curKey != key && !(role == "user" && b.cur.Role == "user")) {
		b.flush()
		b.cur = &anthropicMessage{Role: role, Content: []anthropicBlock{}}
		b.curKey = key
	}
	b.cur.Content = append(b.cur.Content.([]anthropicBlock), block)
}

func (b *msgBuilder) flush() {
	if b.cur == nil {
		return
	}
	blocks := b.cur.Content.([]anthropicBlock)
	if len(blocks) == 1 && blocks[0]["type"] == "text" {
		b.cur.Content = blocks[0]["text"]
	}
	b.msgs = append(b.msgs, *b.cur)
	b.cur = nil
	b.curKey = ""
}

// finalize assembles the shared envelope fields, resolves the client
// reasoning effort to a thinking budget via the model's declared
// capability (FR-005: unsupported levels are rejected explicitly, never
// dropped), and encodes the request. thinkingEligible is false when the
// replayed history cannot legally continue a thinking turn (tool results
// without a signed thinking block); thinking is then disabled instead of
// sending a request Anthropic answers with 400.
func finalize(req *messagesRequest, system []string, effort string, ts *pluginapi.ThinkingSupport, thinkingEligible bool) ([]byte, *errclass.Error) {
	req.System = systemField(system)
	if req.Messages == nil {
		req.Messages = []anthropicMessage{}
	}
	if effort != "" && thinkingEligible {
		budget, _ := thinking.BudgetFromEffort(effort, ts)
		applyThinking(req, budget)
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// applyThinking maps a resolved budget onto the Messages thinking field.
// budget == 0 means reasoning off (ZeroAllowed): no thinking block, and
// sampling controls stay intact. budget < 0 is the dynamic "auto"
// sentinel; Anthropic has no dynamic budget field — an adaptive mode
// would need a catalog-declared capability signal, which does not exist
// today — so it is omitted rather than fabricated (FR-005 explicit
// omission policy). budget > 0 enables thinking and drops sampling
// controls, which Anthropic rejects alongside thinking.
//
// The budget is raised above max_tokens when needed so the visible answer
// still has room; Anthropic requires budget_tokens < max_tokens. A budget
// below Anthropic's 1024 floor is dropped.
func applyThinking(req *messagesRequest, budget int64) {
	if budget <= 0 {
		return
	}
	if budget < 1024 {
		return
	}
	if req.MaxTokens > 0 && req.MaxTokens <= budget {
		req.MaxTokens = budget + 1024
	}
	req.Thinking = &shared.ClaudeThinking{Type: "enabled", BudgetTokens: budget}
	req.Temperature, req.TopP = nil, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Messages request (FR-005, AC §C): leading system/developer messages
// become the top-level system field, assistant tool_calls become tool_use
// blocks, tool results become user tool_result blocks, stop becomes
// stop_sequences, tools become input_schema definitions, reasoning_effort
// becomes a best-effort thinking budget.
//
// Explicit omission policy (FR-005): fields with no Messages equivalent
// (logprobs, frequency_penalty, n, ...) are omitted by struct selection;
// everything representable is mapped or rejected descriptively.
func fromChatCompletions(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	var src shared.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	// The shared kernel owns the FR-005 defaulting policy: a positive
	// max_tokens (or max_completion_tokens fallback) passes through;
	// absent, zero, or negative defaults to 4096.
	maxTokens := int64(0)
	if src.MaxTokens != nil {
		maxTokens = *src.MaxTokens
	} else if src.MaxCompletionTokens != nil {
		maxTokens = *src.MaxCompletionTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	stop, eErr := decodeStop(src.Stop)
	if eErr != nil {
		return nil, eErr
	}
	req.StopSequences = stop

	var system []string
	hasToolHistory := false
	var b msgBuilder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasImage(blocks) {
				return nil, shared.SystemImageRejected()
			}
			system = append(system, shared.JoinTexts(blocks))
		case "assistant":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasImage(blocks) {
				return nil, errclass.Translation("assistant messages cannot carry image content")
			}
			if text := shared.JoinTexts(blocks); text != "" {
				b.add("assistant", "assistant", textBlock(text))
			}
			for _, tc := range m.ToolCalls {
				hasToolHistory = true
				input, eErr := shared.DecodeArgs(tc.Function.Arguments)
				if eErr != nil {
					// A malformed historical call must not fail the whole
					// turn; the tool result still carries the real payload.
					input = map[string]any{}
				}
				b.add("assistant", "assistant", anthropicBlock{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
		case "tool":
			hasToolHistory = true
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			content := any(shared.JoinTexts(blocks))
			if hasImage(blocks) {
				content = blocks // mixed parts keep their per-part order
			}
			b.add("user:tool", "user", anthropicBlock{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": content,
			})
		case "user":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			for _, blk := range blocks { // part order is preserved (FR-005)
				b.add("user", "user", blk)
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	b.flush()
	req.Messages = b.msgs

	for _, t := range src.Tools {
		if t.Type == "namespace" {
			continue
		}
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Function.Name, Description: t.Function.Description,
			InputSchema: shared.ObjectSchema(t.Function.Parameters),
		})
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	return finalize(req, system, src.ReasoningEffort, ts, !hasToolHistory)
}

// fromResponses translates an OpenAI Responses request into a Messages
// request (FR-005): instructions become the system field, message items
// map like Chat Completions messages, function_call/function_call_output
// items map to tool_use/tool_result blocks, and bridge-owned reasoning
// items restore their signed thinking block. Foreign reasoning summaries
// are not replayed: an unsigned thinking block is rejected by Anthropic.
func fromResponses(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport, tools *shared.ResponseTools) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai-response request JSON: " + err.Error())
	}
	// Same shared kernel policy as the Chat Completions leg (FR-005).
	maxTokens := int64(0)
	if src.MaxOutputTokens != nil {
		maxTokens = *src.MaxOutputTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}

	var system []string
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	if instr != "" {
		system = append(system, instr)
	}

	var b msgBuilder
	items, eErr := tools.Normalize(&src, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	hasToolHistory := false
	hasSignedThinking := false
	for _, item := range items {
		switch item.Type {
		case "message":
			blocks, eErr := contentParts(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "user":
				for _, blk := range blocks { // part order is preserved (FR-005)
					b.add("user", "user", blk)
				}
			case "assistant":
				if hasImage(blocks) {
					return nil, errclass.Translation("assistant messages cannot carry image content")
				}
				if text := shared.JoinTexts(blocks); text != "" {
					b.add("assistant", "assistant", textBlock(text))
				}
			case "system", "developer":
				if hasImage(blocks) {
					return nil, shared.SystemImageRejected()
				}
				system = append(system, shared.JoinTexts(blocks))
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call", "tool_search_call":
			hasToolHistory = true
			input, eErr := shared.DecodeArgs(item.Arguments)
			if eErr != nil {
				// Historical malformed arguments must not fail the turn.
				input = map[string]any{}
			}
			name := item.Name
			if item.Type == "tool_search_call" && name == "" {
				name = "tool_search"
			}
			b.add("assistant", "assistant", anthropicBlock{
				"type": "tool_use", "id": item.CallID, "name": name, "input": input,
			})
		case "custom_tool_call":
			hasToolHistory = true
			args := item.Arguments
			if args == "" && item.Input != "" {
				args = item.Input
			}
			input, eErr := shared.DecodeArgs(args)
			if eErr != nil {
				// Freeform custom tool input is not JSON: wrap verbatim.
				input = map[string]any{"input": args}
			}
			b.add("assistant", "assistant", anthropicBlock{
				"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input,
			})
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			hasToolHistory = true
			content, eErr := toolResultContent(item.Output)
			if eErr != nil {
				return nil, eErr
			}
			b.add("user:tool", "user", anthropicBlock{
				"type": "tool_result", "tool_use_id": item.CallID, "content": content,
			})
		case "reasoning":
			// Bridge-owned signed blocks are replayable. A Responses item
			// carrying OpenAI ciphertext is wrapped into a signature that
			// this plugin restores on the next Claude turn; a bare summary
			// is still visible thinking without a signature.
			if block, ok := shared.ThinkingBlockFromReasoningItem(item); ok {
				b.add("assistant", "assistant", anthropicBlock(block))
				hasSignedThinking = true
			} else if block, ok := shared.ThinkingBlockFromOpenAIReasoning(item); ok {
				b.add("assistant", "assistant", anthropicBlock(block))
			}
		case "input_text":
			if strings.TrimSpace(item.Text) != "" {
				b.add("user", "user", textBlock(item.Text))
			}
		case "input_image":
			url, eErr := shared.OpenAIImageURL(item.ImageURL)
			if eErr != nil {
				return nil, eErr
			}
			src, eErr := shared.ImageSource(url)
			if eErr != nil {
				return nil, eErr
			}
			b.add("user", "user", src)
		case "compaction", "compaction_summary", "context_compaction":
			text := strings.TrimSpace(item.Text)
			if text == "" {
				var sb strings.Builder
				for i, s := range item.Summary {
					if i > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(s.Text)
				}
				text = strings.TrimSpace(sb.String())
			}
			if text != "" {
				b.add("user", "user", textBlock(text))
			}
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
	}
	b.flush()
	req.Messages = sanitizeAnthropicMessages(b.msgs)

	for _, t := range src.Tools {
		// Normalize has already flattened function/custom/namespace tools;
		// hosted tools have no Anthropic function equivalent.
		if t.Type != "function" {
			continue
		}
		schema := shared.ObjectSchema(t.Parameters)
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}
	effort := ""
	if src.Reasoning != nil {
		effort = src.Reasoning.Effort
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	return finalize(req, system, effort, ts, !hasToolHistory || hasSignedThinking)
}

// toolResultContent renders a Responses function_call_output as Anthropic
// tool_result content: a plain string when it is text-only, or a block array
// when it carries images. Images are preserved instead of failing the turn.
func toolResultContent(raw json.RawMessage) (any, *errclass.Error) {
	if !shared.HasContent(raw) {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errclass.Translation("tool output must be a string or an array of content parts")
	}
	var text strings.Builder
	blocks := make([]anthropicBlock, 0, len(parts))
	hasImg := false
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			text.WriteString(p.Text)
			blocks = append(blocks, textBlock(p.Text))
		case "input_image":
			url, eErr := shared.OpenAIImageURL(p.ImageURL)
			if eErr != nil {
				return nil, eErr
			}
			src, eErr := shared.ImageSource(url)
			if eErr != nil {
				return nil, eErr
			}
			blocks = append(blocks, src)
			hasImg = true
		default:
			return nil, errclass.Translation("unsupported tool output part type " + p.Type)
		}
	}
	if !hasImg {
		return text.String(), nil
	}
	return blocks, nil
}

// sanitizeAnthropicMessages enforces the history invariants Anthropic
// rejects at request time: non-empty messages, a leading user turn, complete
// assistant tool_use → user tool_result pairs, and no trailing assistant
// whitespace prefill. It is deliberately a single pass over the converted
// history; the client's original order is preserved otherwise.
func sanitizeAnthropicMessages(msgs []anthropicMessage) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, msg := range msgs {
		blocks := anthropicBlocksOf(msg.Content)
		if len(blocks) == 0 {
			continue
		}
		if msg.Role == "assistant" && hasToolUseBlock(blocks) {
			// A tool_use turn must not carry unsigned thinking: Anthropic
			// rejects it, and a foreign provider's unsigned reasoning has
			// no replayable signature.
			kept := blocks[:0]
			for _, blk := range blocks {
				switch blk["type"] {
				case "thinking":
					if sig, _ := blk["signature"].(string); sig == "" {
						continue
					}
				case "redacted_thinking":
					if data, _ := blk["data"].(string); data == "" {
						continue
					}
				}
				kept = append(kept, blk)
			}
			blocks = kept
			if len(blocks) == 0 {
				continue
			}
		}
		msg.Content = blocks
		out = append(out, msg)
	}
	// Drop an assistant tool_use turn unless the immediately next user turn
	// answers every call. A partial turn would otherwise 400.
	cleaned := make([]anthropicMessage, 0, len(out))
	for i := 0; i < len(out); i++ {
		msg := out[i]
		ids := toolUseIDs(msg)
		if msg.Role != "assistant" || len(ids) == 0 {
			cleaned = append(cleaned, msg)
			continue
		}
		var next *anthropicMessage
		if i+1 < len(out) && out[i+1].Role == "user" {
			next = &out[i+1]
		}
		resultIDs := toolResultIDs(next)
		if completeToolPair(ids, resultIDs) {
			cleaned = append(cleaned, msg)
			continue
		}
		// Keep the user turn's non-tool content, drop the unmatched results.
		if next != nil {
			if stripped := stripToolResults(*next); len(anthropicBlocksOf(stripped.Content)) > 0 {
				cleaned = append(cleaned, stripped)
			}
			i++
		}
	}
	out = cleaned
	if len(out) > 0 && out[0].Role != "user" {
		out = append([]anthropicMessage{{Role: "user", Content: "(continuing the conversation)"}}, out...)
	}
	if len(out) > 0 && out[len(out)-1].Role == "assistant" {
		last := &out[len(out)-1]
		blocks := anthropicBlocksOf(last.Content)
		if len(blocks) > 0 {
			if text, ok := blocks[len(blocks)-1]["text"].(string); ok {
				blocks[len(blocks)-1]["text"] = strings.TrimRight(text, " 	\r\n")
			}
			last.Content = blocks
		}
		if len(anthropicBlocksOf(last.Content)) == 0 {
			out = out[:len(out)-1]
		}
	}
	// Preserve the compact one-text-block form (a JSON string) so the
	// wire shape stays byte-stable across sanitization.
	for i := range out {
		blocks := anthropicBlocksOf(out[i].Content)
		if len(blocks) == 1 && blocks[0]["type"] == "text" {
			if text, ok := blocks[0]["text"].(string); ok {
				out[i].Content = text
			}
		}
	}
	return out
}

func anthropicBlocksOf(content any) []anthropicBlock {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []anthropicBlock{textBlock(v)}
	case []anthropicBlock:
		return v
	case anthropicBlock:
		return []anthropicBlock{v}
	default:
		return nil
	}
}

func toolUseIDs(msg anthropicMessage) []string {
	var ids []string
	for _, blk := range anthropicBlocksOf(msg.Content) {
		if blk["type"] == "tool_use" {
			if id, ok := blk["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func hasToolUseBlock(blocks []anthropicBlock) bool {
	for _, blk := range blocks {
		if blk["type"] == "tool_use" {
			return true
		}
	}
	return false
}

func toolResultIDs(msg *anthropicMessage) []string {
	if msg == nil {
		return nil
	}
	var ids []string
	for _, blk := range anthropicBlocksOf(msg.Content) {
		if blk["type"] == "tool_result" {
			if id, ok := blk["tool_use_id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func completeToolPair(uses, results []string) bool {
	if len(uses) == 0 || len(results) == 0 || len(uses) != len(results) {
		return false
	}
	seen := make(map[string]struct{}, len(uses))
	for _, id := range uses {
		seen[id] = struct{}{}
	}
	for _, id := range results {
		if _, ok := seen[id]; !ok {
			return false
		}
		delete(seen, id)
	}
	return len(seen) == 0
}

func stripToolResults(msg anthropicMessage) anthropicMessage {
	blocks := anthropicBlocksOf(msg.Content)
	kept := make([]anthropicBlock, 0, len(blocks))
	for _, blk := range blocks {
		if blk["type"] != "tool_result" {
			kept = append(kept, blk)
		}
	}
	msg.Content = kept
	return msg
}

// applyToolChoiceMessages maps a normalized tool choice onto the outbound
// Messages request. "none" has no Anthropic equivalent and fails
// descriptively rather than silently unforcing the client constraint
// (FR-005 explicit policy). An explicit parallel_tool_calls=false is a
// client constraint too (FR-005): it becomes disable_parallel_tool_use on
// the synthesized or decoded tool_choice — synthesized as {"type":"auto"}
// when no tool_choice was sent, so the flag never rides alone.
func applyToolChoiceMessages(req *messagesRequest, kind, name string, parallel *bool) *errclass.Error {
	// "none" has no Anthropic representation regardless of whether tools
	// were supplied; fail descriptively before the tool-less drop.
	if kind == shared.ToolChoiceNone {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("tool_choice %q cannot be represented for %s", kind, EndpointPath),
		}
	}
	if len(req.Tools) == 0 {
		// Anthropic rejects tool_choice without tools; dropping it is the
		// only valid representation of a request whose tools all vanished.
		return nil
	}
	noParallel := parallel != nil && !*parallel
	if kind == shared.ToolChoiceAbsent && noParallel {
		kind = shared.ToolChoiceAuto
	}
	switch kind {
	case shared.ToolChoiceAbsent:
		return nil
	case shared.ToolChoiceAuto:
		req.ToolChoice = toolChoiceBlock("auto", "", noParallel)
	case shared.ToolChoiceAny:
		req.ToolChoice = toolChoiceBlock("any", "", noParallel)
	case shared.ToolChoiceNamed:
		req.ToolChoice = toolChoiceBlock("tool", name, noParallel)
	default:
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("tool_choice %q cannot be represented for %s", kind, EndpointPath),
		}
	}
	return nil
}

// toolChoiceBlock renders one outbound tool_choice object; noParallel is
// Anthropic's disable_parallel_tool_use flag.
func toolChoiceBlock(typ, name string, noParallel bool) map[string]any {
	tc := map[string]any{"type": typ}
	if name != "" {
		tc["name"] = name
	}
	if noParallel {
		tc["disable_parallel_tool_use"] = true
	}
	return tc
}
