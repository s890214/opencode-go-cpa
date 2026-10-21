// Package responses implements the OpenAI Responses protocol adapter
// (arch §8): upstream endpoint /v1/responses serving GPT 5.6 Luna and
// any catalog model declared compatible (FR-004). Request translation is
// FR-005, response and stream translation FR-006, acceptance coverage
// AC §D. Responses support is REQUIRED first-release scope, not
// experimental (07-open-questions.md §5).
//
// Per arch §8 this package is isolated from the scheduler and catalog
// manager and uses only stdlib JSON primitives.
package responses

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/catalog"
	"opencode-go-cpa/internal/errclass"
	"opencode-go-cpa/internal/reasoning"
	"opencode-go-cpa/internal/thinking"
)

// EndpointPath is the upstream endpoint path for the Responses route (FR-004).
var EndpointPath = catalog.RouteResponses.EndpointPath()

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "claude" Anthropic Messages,
// "openai-response" native Responses passthrough) into a Responses body
// for upstreamModel (FR-005, AC §D). upstreamModel replaces whatever
// client-facing model ID the request carried (FR-003). ts is the target
// model's declared reasoning capability and drives budget→effort mapping
// via the shared thinking package; nil falls back to the default ladder.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation; messages are descriptive and redacted.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "openai-response":
		if !strings.HasPrefix(strings.ToLower(upstreamModel), "gpt") {
			return fromResponsesNormalized(upstreamModel, sourceBody, tools...)
		}
		return shared.RewriteModelID(upstreamModel, sourceBody, "openai-response")
	case "openai":
		return fromChatCompletions(upstreamModel, sourceBody, ts, tools...)
	case "claude":
		return fromClaudeMessages(upstreamModel, sourceBody, ts, tools...)
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// reasoningForCall renders the reasoning item that must precede a
// function_call. Precedence: the text carried by the inbound request (which
// the caller passes as hint), then the captured upstream copy for that call
// id, then a neutral placeholder. The shape comes from the target model: the
// upstream answers 400 for a summary on a model that wants content, and 503
// for the reverse.
func reasoningForCall(tools []*shared.ResponseTools, callID, hint string) json.RawMessage {
	store := shared.ResponseToolContext(tools)
	text := strings.TrimSpace(hint)
	if text == "" && len(callID) > 0 {
		if v, ok := store.Reasoning().Get(callID); ok {
			text = v
		}
	}
	return reasoning.ResponseItem(text, store.ReasoningShape())
}

// rawCallID reads the call_id out of a decoded Responses item map.
func rawCallID(m map[string]json.RawMessage) string {
	raw, ok := m["call_id"]
	if !ok || len(raw) == 0 {
		return ""
	}
	var id string
	_ = json.Unmarshal(raw, &id)
	return id
}

// fromResponsesNormalized normalizes a native Responses passthrough body
// for non-GPT upstream models: merges additional_tools input items into
// tools, unrolls namespace tools, rewrites custom tools to functions, and
// strips search_content_types from web_search tools (the OpenCode Go
// native Responses server only allows it on web_search_preview). Unknown
// top-level fields pass through via the raw map.
func fromResponsesNormalized(upstreamModel string, sourceBody []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(sourceBody, &src); err != nil {
		return nil, errclass.Translation("invalid Responses request body: malformed JSON")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(sourceBody, &raw); err != nil {
		return nil, errclass.Translation("invalid Responses request body: malformed JSON")
	}
	if raw == nil {
		return nil, errclass.Translation("invalid Responses request body: malformed JSON")
	}

	// Verbatim raw input preservation: decode input as raw messages so
	// unmodeled fields (web_search_call action/status, reasoning
	// summary/encrypted_content) survive byte-for-byte.
	preserved := make([]json.RawMessage, 0)
	// additional_tools items carry tool definitions that belong in the tools
	// array; they are collected raw so nothing is lost on the way.
	var extraTools []json.RawMessage
	inputTouched := false
	// Thinking-mode bookkeeping is precomputed rather than carried in a
	// mutable cursor. The previous design kept the last reasoning text in a
	// variable that only a few branches reset, so in a multi-turn history the
	// second tool call could inherit the first turn's text. Two arrays make
	// that impossible:
	//   reasoningText[i]        inbound reasoning that applies at position i,
	//                           cleared at every user turn so it cannot cross
	//                           a conversation boundary
	//   reasoningBeforeCall[i]  whether a tool call follows position i, i.e.
	//                           whether its reasoning is worth keeping
	lastWasReasoning := false
	if inputRaw, ok := raw["input"]; ok && len(inputRaw) > 0 && string(inputRaw) != "null" {
		var items []json.RawMessage
		if err := json.Unmarshal(inputRaw, &items); err == nil {
			inputTouched = true
			reasoningText := make([]string, len(items))
			reasoningBeforeCall := make([]bool, len(items))

			current := ""
			for i, item := range items {
				var meta struct {
					Type string `json:"type"`
					Role string `json:"role"`
				}
				_ = json.Unmarshal(item, &meta)
				switch meta.Type {
				case "reasoning":
					current = reasoning.TextFromResponseItem(item)
				case "message":
					if meta.Role == "user" {
						current = ""
					}
				}
				reasoningText[i] = current
			}

			// A reasoning item is only worth keeping when a tool call follows
			// it: that is the one case the upstream insists on. An orphan
			// reasoning item is still dropped, as before.
			carry := false
			for i := len(items) - 1; i >= 0; i-- {
				var meta struct {
					Type string `json:"type"`
					Role string `json:"role"`
				}
				_ = json.Unmarshal(items[i], &meta)
				switch meta.Type {
				case "function_call", "custom_tool_call":
					carry = true
					reasoningBeforeCall[i] = true
				case "message":
					if meta.Role == "user" {
						carry = false
					}
				case "reasoning":
					reasoningBeforeCall[i] = carry
				}
			}
			for idx, item := range items {
				var meta struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(item, &meta)
				switch meta.Type {
				case "additional_tools":
					var at struct {
						Tools []json.RawMessage `json:"tools"`
					}
					if err := json.Unmarshal(item, &at); err != nil {
						return nil, errclass.Translation("additional_tools requires a tools array")
					}
					if at.Tools == nil {
						return nil, errclass.Translation("additional_tools requires a tools array")
					}
					// Kept raw so a custom tool's grammar survives the merge.
					extraTools = append(extraTools, at.Tools...)
				case "custom_tool_call":
					var m map[string]json.RawMessage
					if err := json.Unmarshal(item, &m); err != nil {
						return nil, errclass.Translation("invalid Responses request body: malformed JSON")
					}
					if !lastWasReasoning {
						preserved = append(preserved, reasoningForCall(tools, rawCallID(m), reasoningText[idx]))
						lastWasReasoning = true
					}
					typeBytes, _ := json.Marshal("function_call")
					m["type"] = typeBytes
					if inRaw, ok := m["input"]; ok {
						var s string
						var argsStr string
						if json.Unmarshal(inRaw, &s) == nil {
							b, _ := json.Marshal(map[string]string{"input": s})
							argsStr = string(b)
						} else {
							b, _ := json.Marshal(map[string]json.RawMessage{"input": inRaw})
							argsStr = string(b)
						}
						ab, _ := json.Marshal(argsStr)
						m["arguments"] = ab
						delete(m, "input")
					}
					b, err := json.Marshal(m)
					if err != nil {
						return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
					}
					preserved = append(preserved, b)
				case "custom_tool_call_output":
					var m map[string]json.RawMessage
					if err := json.Unmarshal(item, &m); err != nil {
						return nil, errclass.Translation("invalid Responses request body: malformed JSON")
					}
					typeBytes, _ := json.Marshal("function_call_output")
					m["type"] = typeBytes
					b, err := json.Marshal(m)
					if err != nil {
						return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
					}
					preserved = append(preserved, b)
					lastWasReasoning = false
				case "compaction":
					// Dropped: non-GPT upstreams reject compaction blobs.
				case "reasoning":
					if !reasoningBeforeCall[idx] {
						// Orphan reasoning (no trailing tool call): the
						// upstream neither needs nor can decrypt it.
						continue
					}
					// Replay the client's own item verbatim when it carries
					// text, because Codex sends summary[] together with an
					// opaque encrypted_content blob and the upstream accepts
					// that exact shape. Rebuilding it would discard fields.
					// Rebuilding is still needed in three cases: the item's
					// text is empty (the upstream reads text only and ignores
					// encrypted_content), the item uses the shape this model
					// rejects, or the captured upstream text is available.
					shape := shared.ResponseToolContext(tools).ReasoningShape()
					text := reasoningText[idx]
					keepVerbatim := strings.TrimSpace(text) != "" && reasoningShapeOf(item) == shape
					if keepVerbatim {
						preserved = append(preserved, item)
					} else {
						preserved = append(preserved, reasoning.ResponseItem(text, shape))
					}
					lastWasReasoning = true
				case "function_call":
					var m map[string]json.RawMessage
					if err := json.Unmarshal(item, &m); err != nil {
						return nil, errclass.Translation("invalid Responses request body: malformed JSON")
					}
					if !lastWasReasoning {
						preserved = append(preserved, reasoningForCall(tools, rawCallID(m), reasoningText[idx]))
						lastWasReasoning = true
					}
					var args string
					if rawArgs, ok := m["arguments"]; ok {
						_ = json.Unmarshal(rawArgs, &args)
					}
					ab, _ := json.Marshal(shared.DefaultArgs(args))
					m["arguments"] = ab
					b, err := json.Marshal(m)
					if err != nil {
						return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
					}
					preserved = append(preserved, b)
					// Parallel calls share the single reasoning item that
					// precedes them; a following output item ends the turn.
					lastWasReasoning = true
				default:
					preserved = append(preserved, item)
					lastWasReasoning = false
				}
			}
		} else {
			var s string
			if json.Unmarshal(inputRaw, &s) != nil {
				return nil, errclass.Translation("input must be a string or an array of items")
			}
		}
	}

	modelBytes, _ := json.Marshal(upstreamModel)
	raw["model"] = modelBytes

	// Native pass-through, so the tools array is forwarded as-is. Normalize is
	// a translation-layer step (Chat upstreams have no namespace concept) and
	// running it here flattened Codex's four namespace trees — 45 MCP tools —
	// into anonymous `ns__name` functions and degraded custom tools to plain
	// functions, dropping their grammar. The upstream accepts both shapes
	// verbatim (measured 200 / custom_tool_call), so nothing needs rewriting.
	//
	// The one exception is web_search: its search_content_types field is an
	// OpenAI-only knob the upstream rejects, so only that field is removed;
	// filters/user_location/search_context_size stay intact.
	entries := make([]json.RawMessage, 0)
	if rawTools, ok := raw["tools"]; ok && len(rawTools) > 0 {
		if err := json.Unmarshal(rawTools, &entries); err != nil {
			return nil, errclass.Translation("tools must be an array")
		}
	}
	changed := false
	for i, entry := range entries {
		var meta struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(entry, &meta)
		if meta.Type == "custom" && meta.Name != "" {
			shared.ResponseToolContext(tools).RegisterCustomTool(meta.Name)
		}
		if meta.Type != "web_search" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(entry, &obj) != nil {
			continue
		}
		if _, ok := obj["search_content_types"]; !ok {
			continue
		}
		delete(obj, "search_content_types")
		if b, err := json.Marshal(obj); err == nil {
			entries[i] = b
			changed = true
		}
	}
	if len(extraTools) > 0 {
		entries = append(entries, extraTools...)
		changed = true
	}
	if changed {
		if b, err := json.Marshal(entries); err == nil {
			raw["tools"] = b
		}
	}

	if inputTouched {
		inputBytes, _ := json.Marshal(preserved)
		raw["input"] = inputBytes
	}

	if len(src.ToolChoice) > 0 {
		raw["tool_choice"] = src.ToolChoice
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
	}
	return out, nil
}

// ---- target wire shape -------------------------------------------------

type responsesEnvelope struct {
	Model             string            `json:"model"`
	Input             []any             `json:"input,omitempty"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []shared.RespTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	MaxToolCalls      *int              `json:"max_tool_calls,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
}

func msgItem(role string, parts []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// textPart renders message text; assistant history replays with
// output_text, every other role uses input_text.
func textPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// respToolChoice renders a shared.DecodeToolChoice kind into the Responses
// wire value; absent yields nil so omitempty drops the field. It is
// infallible: shared.DecodeToolChoice emits exactly the closed set below,
// so the switch covers every possible kind (the exhaustiveness test in
// request_test.go enforces this against shared.AllToolChoiceKinds).
func respToolChoice(kind, name string) json.RawMessage {
	switch kind {
	case shared.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		return json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{"type": "function", "name": name})
		return b
	}
	return nil
}

// ---- Chat Completions source -------------------------------------------

// contentParts normalizes an OpenAI-style content field (JSON string or
// typed parts) into Responses input parts for the given role (FR-005
// multimodal preservation). The shared kernel owns validation and the
// empty-input policy: empty-text parts decode to zero parts so empty
// messages drop identically on every route (F5 parity).
func contentParts(raw json.RawMessage, role string) ([]map[string]any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, imagePart(p.ImageURL))
			continue
		}
		out = append(out, textPart(role, p.Text))
	}
	return out, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Responses request (FR-005, AC §D): system/developer messages become
// instructions, user/assistant turns become message items (assistant
// replaying output_text), tool_calls history becomes function_call items,
// tool results become function_call_output items, max_tokens/
// max_completion_tokens become max_output_tokens, and reasoning_effort is
// capability-checked against ts and rejected descriptively when the model
// cannot represent it (matching the reverse Responses→CC leg).
// tool_choice is decoded by the shared classifier and rendered into the
// Responses wire values ("auto"/"none"/"required", flat
// {type:function,name} for a forced tool); parallel_tool_calls passes
// through as-is.
//
// Explicit omission policy (FR-005 "where compatible"): `stop` has no
// Responses equivalent and is dropped, as are sampling/extras without an
// equivalent (logprobs, frequency_penalty, presence_penalty, n, seed,
// response_format, logit_bias) via struct selection. Tool calls and
// reasoning controls are never dropped silently.
func fromChatCompletions(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	var src shared.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	req := &responsesEnvelope{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if src.MaxTokens != nil && *src.MaxTokens > 0 {
		v := shared.ResponsesMaxOutputTokens(*src.MaxTokens)
		req.MaxOutputTokens = &v
	} else if src.MaxCompletionTokens != nil && *src.MaxCompletionTokens > 0 {
		v := shared.ResponsesMaxOutputTokens(*src.MaxCompletionTokens)
		req.MaxOutputTokens = &v
	}
	if src.ReasoningEffort != "" {
		norm := strings.ToLower(strings.TrimSpace(src.ReasoningEffort))
		if norm != "auto" { // "auto" sentinel omits reasoning block (FR-005)
			req.Reasoning = map[string]any{"effort": norm}
		}
	}

	var instr strings.Builder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			// Instructions have no image representation: reject like the
			// sibling routes instead of dropping silently (FR-005).
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, shared.SystemImageRejected()
				}
			}
			if text := shared.JoinTexts(parts); text != "" {
				if instr.Len() > 0 {
					instr.WriteString("\n\n")
				}
				instr.WriteString(text)
			}
		case "user":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
		case "assistant":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
			if len(m.ToolCalls) > 0 {
				// Thinking mode demands the reasoning that produced the
				// calls; one item covers the whole parallel batch.
				req.Input = append(req.Input, reasoningForCall(tools, m.ToolCalls[0].ID, ""))
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": shared.DefaultArgs(tc.Function.Arguments),
				})
			}
		case "tool":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, errclass.Translation(fmt.Sprintf(
						"unsupported tool content part type %q; function_call_output carries text only", typ))
				}
			}
			req.Input = append(req.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  shared.JoinTexts(parts),
			})
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	req.Instructions = instr.String()

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		strict := false
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  shared.ObjectSchema(t.Function.Parameters),
			Strict:      &strict,
		})
	}
	if len(req.Tools) > 0 {
		req.ToolChoice = respToolChoice(kind, tcName)
	} else {
		req.ParallelToolCalls = nil
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// ---- Claude Messages source --------------------------------------------

// reasoningEffortFor is the single off-state policy for the Responses
// target (FR-005), applied to an already-resolved effort value from both
// source legs. The dynamic "auto" sentinel has no wire representation here,
// so the reasoning field is omitted entirely (no dynamic sentinel on the
// Responses wire). "none" forwards verbatim when the model declares it via
// SupportedLevels — the same admission thinking.ValidateEffort applies and
// matching the CC-upstream leg which forwards the validated value as-is;
// when "none" is not declared there is nothing to express, so the field is
// omitted rather than clamped to the weakest representable level. Every
// other validated level forwards as-is.
func reasoningEffortFor(effort string, ts *pluginapi.ThinkingSupport) (string, bool) {
	switch {
	case effort == "auto":
		return "", false
	case effort == "none":
		if slices.Contains(thinking.SupportedLevels(ts), "none") {
			return effort, true
		}
		return "", false
	default:
		return effort, true
	}
}

// fromClaudeMessages translates an Anthropic Messages request into a
// Responses request (FR-005, AC §D): system becomes instructions, text/
// image blocks become input parts, tool_use/tool_result become
// function_call/function_call_output items, an enabled thinking budget
// maps to reasoning.effort through reasoningEffortFor (the "auto" sentinel
// and an undeclared "none" omit the field entirely; a declared "none"
// forwards), max_tokens becomes max_output_tokens (defaulted by the
// shared kernel, FR-005). Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
//
// Explicit omission policy (FR-005 "where compatible"): stop_sequences is
// dropped (no Responses equivalent); thinking/redacted_thinking blocks are
// dropped because signed Anthropic chain-of-thought has no Responses input
// representation (the thinking *control* maps to reasoning.effort); images
// nested inside tool_result content are rejected descriptively, never
// silently lost (FR-005). The tool_result is_error flag has no Responses
// field and is preserved as an "[error] " marker in the output text rather
// than lost silently.
func fromClaudeMessages(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:           upstreamModel,
		MaxOutputTokens: func() *int64 { v := shared.ResponsesMaxOutputTokens(src.MaxTokens); return &v }(),
		Stream:          src.Stream,
		Temperature:     src.Temperature,
		TopP:            src.TopP,
	}
	if shared.ThinkingEnabled(src.Thinking) {
		if effort, ok := reasoningEffortFor(thinking.EffortFromBudget(src.Thinking.BudgetTokens, ts), ts); ok {
			req.Reasoning = map[string]any{"effort": effort}
		}
	}
	req.Instructions = src.System

	for _, m := range src.Messages {
		if m.Role == "system" {
			text := m.Content
			for _, blk := range m.Blocks {
				switch blk.Kind {
				case "text":
					if text != "" {
						text += "\n\n"
					}
					text += blk.Text
				case "image":
					return nil, shared.SystemImageRejected()
				case "thinking", "redacted_thinking":
					// omitted per the FR-005 policy in the doc comment
				default:
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
			}
			if text != "" {
				if req.Instructions != "" {
					req.Instructions += "\n\n"
				}
				req.Instructions += text
			}
			continue
		}
		switch m.Role {
		case "user", "assistant":
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
		var parts []map[string]any
		flush := func() {
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
				parts = nil
			}
		}
		if m.Content != "" {
			parts = append(parts, textPart(m.Role, m.Content))
		}
		var thinkText string
		reasoningEmitted := false
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case "text":
				parts = append(parts, textPart(m.Role, blk.Text))
			case "image":
				parts = append(parts, imagePart(blk.URL))
			case "document":
				if doc, ok := documentPart(blk); ok {
					parts = append(parts, doc)
				}
			case "thinking", "redacted_thinking":
				// A Claude client replaying bridge-owned reasoning restores
				// the original Responses reasoning item; native Anthropic
				// thinking has no OpenAI ciphertext and is dropped.
				block := map[string]any{"type": blk.Kind, "thinking": blk.Thinking, "signature": blk.Signature, "data": blk.Data}
				if item, ok := shared.OpenAIReasoningFromThinkingBlock(block); ok {
					flush()
					req.Input = append(req.Input, reasoningItem(item))
					thinkText = shared.ReasoningSummaryText(item)
					reasoningEmitted = true
				}
			case "tool_use":
				flush()
				args := shared.DefaultArgs(string(blk.Input))
				if !reasoningEmitted {
					// Thinking mode demands the reasoning that produced
					// the call; the block above (if present) is the
					// original text, otherwise the cache fills it in.
					req.Input = append(req.Input, reasoningForCall(tools, blk.CallID, thinkText))
					reasoningEmitted = true
				}
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   blk.CallID,
					"name":      blk.Name,
					"arguments": args,
				})
			case "tool_result":
				flush()
				output, eErr := claudeToolResultOutput(blk.Result, blk.IsError)
				if eErr != nil {
					return nil, eErr
				}
				req.Input = append(req.Input, map[string]any{
					"type":    "function_call_output",
					"call_id": blk.CallID,
					"output":  output,
				})
				reasoningEmitted = false
			default:
				flush()
				return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
			}
		}
		flush()
	}

	maxUses := 0
	for _, t := range src.Tools {
		// Claude server-side search tools map to the Responses hosted tool,
		// not to a function with an empty schema.
		if strings.HasPrefix(t.Type, "web_search") {
			tool := shared.RespTool{Type: "web_search"}
			if len(t.AllowedDomains) > 0 {
				b, _ := json.Marshal(map[string]any{"allowed_domains": t.AllowedDomains})
				tool.Filters = b
			}
			if t.MaxUses > 0 && (maxUses == 0 || t.MaxUses < maxUses) {
				maxUses = t.MaxUses
			}
			if len(t.UserLocation) > 0 {
				tool.UserLocation = t.UserLocation
			}
			req.Tools = append(req.Tools, tool)
			continue
		}
		// Claude tools carry no type field (validation cannot reject here).
		strict := false
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  shared.ObjectSchema(t.InputSchema),
			Strict:      &strict,
		})
	}
	if len(req.Tools) > 0 {
		req.ToolChoice = respToolChoice(src.ToolChoiceKind, src.ToolChoiceName)
	}
	if maxUses > 0 {
		req.MaxToolCalls = &maxUses
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// claudeToolResultOutput renders an Anthropic tool_result as Responses
// function_call_output: a string when text-only, a content-part array when
// the result carries images. The old text-only path rejected screenshots.
func claudeToolResultOutput(raw json.RawMessage, isError bool) (any, *errclass.Error) {
	parts, eErr := shared.ToolResultParts(raw)
	if eErr != nil {
		return nil, eErr
	}
	var text strings.Builder
	blocks := make([]map[string]any, 0, len(parts))
	hasImage := false
	for _, part := range parts {
		if part.ImageURL != "" {
			blocks = append(blocks, map[string]any{"type": "input_image", "image_url": part.ImageURL})
			hasImage = true
			continue
		}
		text.WriteString(part.Text)
		blocks = append(blocks, map[string]any{"type": "input_text", "text": part.Text})
	}
	if !hasImage {
		out := text.String()
		if isError {
			out = shared.ToolResultErrorPrefix + out
		}
		return out, nil
	}
	if isError {
		blocks = append([]map[string]any{{"type": "input_text", "text": shared.ToolResultErrorPrefix}}, blocks...)
	}
	return blocks, nil
}

// documentPart renders one Anthropic document block as a Responses
// input_file part, preserving base64 and URL sources.
func documentPart(blk shared.ClaudeBlock) (map[string]any, bool) {
	name := blk.Title
	if name == "" {
		name = "document"
	}
	if blk.SourceURL != "" {
		return map[string]any{"type": "input_file", "filename": name, "file_url": blk.SourceURL}, true
	}
	if blk.Data != "" {
		media := blk.MediaType
		if media == "" {
			media = "application/pdf"
		}
		return map[string]any{
			"type": "input_file", "filename": name,
			"file_data": "data:" + media + ";base64," + blk.Data,
		}, true
	}
	return nil, false
}

// reasoningItem renders a Responses reasoning input item with only the
// replayable fields: id, summary, and encrypted_content.
func reasoningItem(item shared.RespItem) map[string]any {
	out := map[string]any{"type": "reasoning"}
	if item.ID != "" {
		out["id"] = item.ID
	}
	if len(item.Summary) > 0 {
		summary := make([]map[string]any, 0, len(item.Summary))
		for _, part := range item.Summary {
			summary = append(summary, map[string]any{"type": "summary_text", "text": part.Text})
		}
		out["summary"] = summary
	}
	if item.EncryptedContent != "" {
		out["encrypted_content"] = item.EncryptedContent
	}
	return out
}

// reasoningShapeOf reports which wire shape an inbound reasoning item uses.
func reasoningShapeOf(item json.RawMessage) string {
	var probe struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(item, &probe) == nil && len(probe.Content) > 0 {
		return reasoning.ShapeContent
	}
	return reasoning.ShapeSummary
}
