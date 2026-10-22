// Package chatcompletions implements the OpenCode Go Chat Completions
// protocol adapter (arch §8) for routes served via /v1/chat/completions:
// request conversion (FR-005), non-stream response conversion (FR-006),
// and SSE stream conversion (AC §B).
package chatcompletions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/catalog"
	"opencode-go-cpa/internal/errclass"
	"opencode-go-cpa/internal/thinking"
)

// EndpointPath is the upstream OpenCode Go Chat Completions endpoint.
var EndpointPath = catalog.RouteChatCompletions.EndpointPath()

// AuthHeaders returns the FR-007 authorization headers for an OpenCode Go key.
func AuthHeaders(key string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	return h
}

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "openai-response" Responses, "claude"
// Messages) into a Chat Completions body for upstreamModel (FR-005).
// ts carries the target model's thinking capability so reasoning
// budgets map to a supported effort level instead of being silently
// degraded (FR-005). Unknown formats are ClassUnsupported; malformed
// input is ClassTranslation. Errors are descriptive and redacted — no
// silent loss of tools or reasoning controls.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "openai":
		return buildOpenAIRequest(upstreamModel, sourceBody, shared.ResponseToolContext(tools))
	case "claude":
		return claudeToChat(upstreamModel, sourceBody, ts, shared.ResponseToolContext(tools))
	case "openai-response":
		return responsesToChat(upstreamModel, sourceBody, ts, shared.ResponseToolContext(tools))
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// toolCallPlaceholder is the non-empty placeholder CC Switch injects into a
// tool-call assistant message whose real thinking is unknown. Thinking-mode
// upstreams (DeepSeek, Kimi) require a non-empty reasoning_content on every
// assistant message that carries tool_calls; an empty string is not enough.
const toolCallPlaceholder = "tool call"

// toolCallReasoning returns the reasoning that belongs to one tool call:
// the text carried by the inbound request when it is still attached,
// otherwise whatever the cache captured from the upstream answer that
// produced the call, otherwise the non-empty placeholder.
func toolCallReasoning(store *shared.ResponseTools, callID, pending string, hasPending bool) *string {
	text := toolCallPlaceholder
	switch {
	case hasPending:
		text = pending
	case len(callID) > 0:
		if v, ok := store.Reasoning().Get(callID); ok {
			text = v
		}
	}
	return &text
}

// isDeepSeekChatModel mirrors CC Switch's exact-ID check. History repairs that
// rewrite assistant turns are limited to DeepSeek: other providers read the
// same history differently and their prompt caches would churn.
func isDeepSeekChatModel(model string) bool {
	name := model
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "deepseek" || strings.HasPrefix(name, "deepseek-")
}

// ensureToolCallReasoning backfills a non-empty reasoning_content on every
// assistant message that carries tool_calls (CC Switch parity). An empty or
// null value is rejected by thinking-mode upstreams just like a missing one.
func ensureToolCallReasoning(msgs []ccMessage) {
	for i := range msgs {
		m := &msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		if m.ReasoningContent == nil || strings.TrimSpace(*m.ReasoningContent) == "" {
			text := toolCallPlaceholder
			m.ReasoningContent = &text
		}
	}
}

// coalesceReplayedToolTurn merges assistant messages that Responses replays
// between a tool-call turn and its results back into the tool-call message
// (CC Switch parity). Chat requires the whole turn to be one assistant
// message; a text-only assistant in the middle of unanswered calls is
// rejected by strict thinking-mode upstreams.
func coalesceReplayedToolTurn(msgs []ccMessage) []ccMessage {
	out := make([]ccMessage, 0, len(msgs))
	open := -1
	pending := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					pending[tc.ID] = struct{}{}
				}
			}
			if open >= 0 {
				target := &out[open]
				target.Content = mergeAssistantContent(target.Content, m.Content)
				if m.ReasoningContent != nil {
					text := strings.TrimSpace(*m.ReasoningContent)
					switch {
					case text == "":
					case target.ReasoningContent == nil || strings.TrimSpace(*target.ReasoningContent) == "":
						merged := text
						target.ReasoningContent = &merged
					case !strings.Contains(*target.ReasoningContent, text):
						merged := *target.ReasoningContent + "\n\n" + text
						target.ReasoningContent = &merged
					}
				}
				target.ToolCalls = append(target.ToolCalls, m.ToolCalls...)
				continue
			}
			if len(pending) > 0 {
				open = len(out)
			}
		case "tool":
			if m.ToolCallID != "" {
				delete(pending, m.ToolCallID)
			}
			if len(pending) == 0 {
				open = -1
			}
		default:
			open = -1
			pending = make(map[string]struct{})
		}
		out = append(out, m)
	}
	return out
}

// mergeAssistantContent folds one assistant turn's text into another without
// dropping either side (string concatenation or content-part append).
func mergeAssistantContent(target, addition any) any {
	if contentEmpty(addition) {
		return target
	}
	if contentEmpty(target) {
		return addition
	}
	if ts, ok := target.(string); ok {
		if as, ok := addition.(string); ok {
			return ts + "\n" + as
		}
	}
	parts, _ := target.([]ccContentPart)
	extra, _ := addition.([]ccContentPart)
	merged := make([]ccContentPart, 0, len(parts)+len(extra))
	merged = append(merged, parts...)
	merged = append(merged, extra...)
	return merged
}

func contentEmpty(content any) bool {
	switch v := content.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []ccContentPart:
		return len(v) == 0
	}
	return false
}

// firstToolCallID extracts the id of the first tool call in a raw
// Chat Completions tool_calls array ("" when absent or malformed).
func firstToolCallID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &calls) != nil || len(calls) == 0 {
		return ""
	}
	return calls[0].ID
}

// buildOpenAIRequest rewrites the top-level model field of an OpenAI request
// body to upstreamModel, normalizes role:"developer" messages to role:"system",
// strips any malformed top-level thinking object, and replays the reasoning of
// every assistant tool call. DeepSeek models fail if thinking lacks a valid
// string type field, if messages contain role:"developer", or if an assistant
// turn carrying tool_calls has no reasoning_content while thinking mode is on.
func buildOpenAIRequest(upstreamModel string, body []byte, tools *shared.ResponseTools) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	rawThinking, hasThinking := req["thinking"]
	validThinking := hasThinking && isValidThinking(rawThinking)
	if hasThinking && !validThinking {
		delete(req, "thinking")
	}

	var msgsModified bool
	if rawMsgs, ok := req["messages"]; ok {
		rawStr := string(rawMsgs)
		needsDeveloper := strings.Contains(rawStr, `"developer"`)
		needsReasoning := strings.Contains(rawStr, `"tool_calls"`)
		if needsDeveloper || needsReasoning {
			var msgs []map[string]json.RawMessage
			if err := json.Unmarshal(rawMsgs, &msgs); err == nil {
				for _, m := range msgs {
					var role string
					if err := json.Unmarshal(m["role"], &role); err != nil {
						continue
					}
					if role == "developer" {
						m["role"] = json.RawMessage(`"system"`)
						msgsModified = true
						role = "system"
					}
					if !needsReasoning || role != "assistant" {
						continue
					}
					rawCalls, hasCalls := m["tool_calls"]
					if !hasCalls || len(rawCalls) == 0 || string(rawCalls) == "null" {
						continue
					}
					if rawReasoning, hasReasoning := m["reasoning_content"]; hasReasoning {
						// null is not the same as "": the upstream accepts
						// an empty string but answers 400 for null, so a
						// present-but-null field still needs the backfill.
						switch trimmed := strings.TrimSpace(string(rawReasoning)); trimmed {
						case "", "null":
						default:
							continue
						}
					}
					encoded, err := json.Marshal(*toolCallReasoning(tools, firstToolCallID(rawCalls), "", false))
					if err != nil {
						continue
					}
					m["reasoning_content"] = encoded
					msgsModified = true
				}
				if msgsModified {
					if b, err := json.Marshal(msgs); err == nil {
						req["messages"] = b
					}
				}
			}
		}
	}
	if raw, ok := req["model"]; ok && string(raw) == `"`+upstreamModel+`"` && (validThinking || !hasThinking) && !msgsModified {
		return body, nil
	}
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)

	b, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	return b, nil
}

// isValidThinking reports whether raw is a JSON object with a non-empty string "type" field.
func isValidThinking(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	rawType, ok := obj["type"]
	if !ok {
		return false
	}
	var typeStr string
	if err := json.Unmarshal(rawType, &typeStr); err != nil {
		return false
	}
	return strings.TrimSpace(typeStr) != ""
}

type imageURLField struct {
	URL string `json:"url"`
}

type ccContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *imageURLField `json:"image_url,omitempty"`
}

type ccMessage struct {
	Role      string              `json:"role"`
	Content   any                 `json:"content"` // string, []ccContentPart, or nil
	ToolCalls []shared.CCToolCall `json:"tool_calls,omitempty"`
	// ReasoningContent replays the thinking that produced ToolCalls.
	// Upstreams in thinking mode reject a tool turn without it, so the
	// translators fill it from the reasoning cache; the pointer keeps an
	// intentional empty string on the wire (omitempty drops nil only).
	ReasoningContent *string `json:"reasoning_content,omitempty"`
	ToolCallID       string  `json:"tool_call_id,omitempty"`
}

type ccStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ccRequest struct {
	Model               string           `json:"model"`
	Messages            []ccMessage      `json:"messages"`
	MaxTokens           *int64           `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64           `json:"max_completion_tokens,omitempty"`
	Stop                any              `json:"stop,omitempty"`
	Stream              bool             `json:"stream,omitempty"`
	StreamOptions       *ccStreamOptions `json:"stream_options,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	Tools               []shared.CCTool  `json:"tools,omitempty"`
	ToolChoice          json.RawMessage  `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
}

// isOpenAIOSeries mirrors the o-series check used by CC Switch: a model name
// starting with "o" followed by a digit uses max_completion_tokens instead
// of max_tokens.
func isOpenAIOSeries(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return len(m) > 1 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'
}

// setMaxTokens assigns the token cap to the field the target model expects.
func setMaxTokens(req *ccRequest, model string, n int64) {
	if n <= 0 {
		return
	}
	if isOpenAIOSeries(model) {
		req.MaxCompletionTokens = &n
		return
	}
	req.MaxTokens = &n
}

// enableStreamUsage asks OpenAI-compatible upstreams to emit the terminal
// usage chunk; many only do so when include_usage is explicit.
func enableStreamUsage(req *ccRequest) {
	if req.Stream {
		req.StreamOptions = &ccStreamOptions{IncludeUsage: true}
	}
}

// encode finalizes and encodes a translated request; the composed types
// are always marshallable.
func encode(req *ccRequest) []byte {
	if req.Messages == nil {
		req.Messages = []ccMessage{}
	}
	b, _ := json.Marshal(req)
	return b
}

// ---- claude (Anthropic Messages) -> Chat Completions (FR-005, AC §B) ----

type claudeBlock = map[string]any

// claudeToChat translates an Anthropic Messages request into a Chat
// Completions request (FR-005): top-level system becomes a system
// message, text blocks become string content, tool_use blocks become
// assistant tool_calls, tool_result blocks become role:"tool" messages,
// max_tokens/stop_sequences/tools map to their CC equivalents
// (max_tokens defaulted by the shared kernel, FR-005), and the thinking
// budget maps to a capability-aware reasoning_effort via
// thinking.EffortFromBudget. Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
func claudeToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport, tools *shared.ResponseTools) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	out := &ccRequest{
		Model:       upstreamModel,
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	setMaxTokens(out, upstreamModel, src.MaxTokens)
	if len(src.StopSequences) > 0 {
		out.Stop = src.StopSequences
	}
	if shared.ThinkingEnabled(src.Thinking) {
		out.ReasoningEffort = thinking.EffortFromBudget(src.Thinking.BudgetTokens, ts)
	}
	var systemTexts []string
	if src.System != "" {
		systemTexts = append(systemTexts, src.System)
	}
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "user":
			msgs, eErr := claudeUserMessages(m)
			if eErr != nil {
				return nil, eErr
			}
			out.Messages = append(out.Messages, msgs...)
		case "assistant":
			msg, eErr := claudeAssistantMessage(m, tools)
			if eErr != nil {
				return nil, eErr
			}
			if msg != nil {
				out.Messages = append(out.Messages, *msg)
			}
		case "system":
			var sb strings.Builder
			sb.WriteString(m.Content)
			for i := range m.Blocks {
				blk := &m.Blocks[i]
				switch blk.Kind {
				case "text":
					sb.WriteString(blk.Text)
				case "image":
					return nil, shared.SystemImageRejected()
				default:
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
			}
			if sb.Len() > 0 {
				systemTexts = append(systemTexts, sb.String())
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	if len(systemTexts) > 0 {
		out.Messages = append([]ccMessage{{Role: "system", Content: strings.Join(systemTexts, "\n\n")}}, out.Messages...)
	}
	for _, t := range src.Tools {
		// Claude tools carry no type field; validation cannot reject here.
		schema := shared.ObjectSchema(t.InputSchema)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	applyToolChoiceCC(out, src.ToolChoiceKind, src.ToolChoiceName)
	enableStreamUsage(out)
	return encode(out), nil
}

// claudeUserMessages keeps all tool replies adjacent, then emits tool images
// in a user message associated with their tool_call_id. This preserves parallel
// tool-call protocol ordering and retains the original image bytes/URLs.
func claudeUserMessages(m *shared.ClaudeMessageRecord) ([]ccMessage, *errclass.Error) {
	var msgs []ccMessage
	var parts []ccContentPart
	if m.Content != "" {
		parts = append(parts, ccContentPart{Type: "text", Text: m.Content})
	}
	for _, blk := range m.Blocks {
		switch blk.Kind {
		case "text":
			parts = append(parts, ccContentPart{Type: "text", Text: blk.Text})
		case "image":
			parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: blk.URL}})
		case "tool_result":
			result, eErr := shared.ToolResultParts(blk.Result)
			if eErr != nil {
				return nil, eErr
			}
			var text strings.Builder
			hasImage := false
			for _, part := range result {
				if part.ImageURL != "" {
					hasImage = true
				} else {
					text.WriteString(part.Text)
				}
			}
			toolText := text.String()
			if blk.IsError {
				toolText = shared.ToolResultErrorPrefix + toolText
			}
			if hasImage {
				toolText += "\n[Images from this tool result are in the following user message, labeled with tool_call_id " + blk.CallID + ".]"
				parts = append(parts, ccContentPart{Type: "text", Text: "Tool result images for tool_call_id " + blk.CallID + ":"})
				for _, part := range result {
					if part.ImageURL != "" {
						parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: part.ImageURL}})
					}
				}
			}
			msgs = append(msgs, ccMessage{Role: "tool", Content: toolText, ToolCallID: blk.CallID})
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	if len(parts) > 0 {
		var content any = parts
		if len(parts) == 1 && parts[0].Type == "text" {
			content = parts[0].Text
		}
		msgs = append(msgs, ccMessage{Role: "user", Content: content})
	}
	return msgs, nil
}

// claudeAssistantMessage converts an assistant turn: text blocks join
// the message content, tool_use blocks become tool_calls. Historical
// thinking blocks have no Chat Completions field of their own, but when
// the turn carries tool calls the upstream insists on getting that
// thinking back: it is replayed as reasoning_content, falling back to the
// captured copy for the first call and then to an empty placeholder.
// Returns nil for empty turns.
func claudeAssistantMessage(m *shared.ClaudeMessageRecord, tools *shared.ResponseTools) (*ccMessage, *errclass.Error) {
	msg := &ccMessage{Role: "assistant"}
	var sb strings.Builder
	sb.WriteString(m.Content)
	var think strings.Builder
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			sb.WriteString(blk.Text)
		case "thinking":
			think.WriteString(blk.Text)
		case "redacted_thinking":
			// omitted: encrypted metadata with no readable equivalent
		case "tool_use":
			tc := shared.CCToolCall{ID: blk.CallID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = shared.DefaultArgs(string(blk.Input))
			msg.ToolCalls = append(msg.ToolCalls, tc)
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	if sb.Len() > 0 {
		msg.Content = sb.String()
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return nil, nil
	}
	if len(msg.ToolCalls) > 0 {
		msg.ReasoningContent = toolCallReasoning(tools, msg.ToolCalls[0].ID, think.String(), think.Len() > 0)
	}
	return msg, nil
}

// ---- openai-response (Responses) -> Chat Completions (FR-005) ----

// responsesToChat translates an OpenAI Responses request into a Chat
// Completions request (FR-005): instructions and system items become
// system messages, message items map by role, function_call items merge
// into assistant tool_calls, function_call_output items become role:"tool"
// messages, reasoning.effort maps to reasoning_effort,
// parallel_tool_calls passes through as-is (the reverse leg forwards the
// same field), and max_output_tokens maps to max_tokens. Historical
// reasoning items are omitted (no CC equivalent; FR-005 explicit omission
// policy).
func responsesToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport, tools *shared.ResponseTools) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai-response request JSON: " + err.Error())
	}
	out := &ccRequest{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	if src.MaxOutputTokens != nil && *src.MaxOutputTokens > 0 {
		setMaxTokens(out, upstreamModel, *src.MaxOutputTokens)
	}
	if src.Reasoning != nil && src.Reasoning.Effort != "" {
		out.ReasoningEffort = strings.ToLower(strings.TrimSpace(src.Reasoning.Effort))
	}
	var systemTexts []string
	addSystem := func(text string) {
		if text != "" {
			systemTexts = append(systemTexts, text)
		}
	}
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	addSystem(instr)

	items, eErr := tools.Normalize(&src, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	knownCalls := make(map[string]struct{})
	var pendingReasoning string
	var hasPendingReasoning bool
	// Chat tool messages cannot carry media, but Responses tool results may
	// hold screenshots or files. Extract those blocks and re-emit them on a
	// synthetic user message before the next non-tool item, keeping parallel
	// tool results adjacent so tool_calls/tool ordering stays valid.
	var pendingToolMedia []map[string]any
	flushToolMedia := func() {
		if len(pendingToolMedia) == 0 {
			return
		}
		out.Messages = append(out.Messages, ccMessage{Role: "user", Content: pendingToolMedia})
		pendingToolMedia = nil
	}
	for _, item := range items {
		if item.Type != "function_call_output" && item.Type != "custom_tool_call_output" {
			flushToolMedia()
		}
		switch item.Type {
		case "message":
			content, eErr := respContent(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "system", "developer":
				// Flatten to text; images have no system-message
				// equivalent and are rejected descriptively (FR-005).
				text := ""
				if s, ok := content.(string); ok {
					text = s
				} else if parts, ok := content.([]ccContentPart); ok {
					var sb strings.Builder
					for _, p := range parts {
						if p.Type != "text" {
							return nil, shared.SystemImageRejected()
						}
						sb.WriteString(p.Text)
					}
					text = sb.String()
				}
				addSystem(text)
			case "user", "assistant":
				if content != nil {
					out.Messages = append(out.Messages, ccMessage{Role: item.Role, Content: content})
				}
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call":
			knownCalls[item.CallID] = struct{}{}
			args := shared.DefaultArgs(item.Arguments)
			tc := shared.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = args
			reasoning := toolCallReasoning(tools, item.CallID, pendingReasoning, hasPendingReasoning)
			// Merge into the last assistant message: both consecutive
			// function_call items and the commentary text Responses
			// replays as a separate message belong to the same turn.
			// Chat models treat a standalone text-only assistant turn as
			// complete and stop before the calls.
			if n := len(out.Messages); n > 0 {
				last := &out.Messages[n-1]
				if last.Role == "assistant" {
					last.ToolCalls = append(last.ToolCalls, tc)
					if last.ReasoningContent == nil {
						last.ReasoningContent = reasoning
					}
					continue
				}
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "assistant", ToolCalls: []shared.CCToolCall{tc},
				ReasoningContent: reasoning,
			})
		case "custom_tool_call":
			knownCalls[item.CallID] = struct{}{}
			input := item.Input
			if input == "" {
				input = item.Arguments
			}
			tc := shared.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = shared.WrapCustomToolInput(input)
			reasoning := toolCallReasoning(tools, item.CallID, pendingReasoning, hasPendingReasoning)
			if n := len(out.Messages); n > 0 {
				last := &out.Messages[n-1]
				if last.Role == "assistant" {
					last.ToolCalls = append(last.ToolCalls, tc)
					if last.ReasoningContent == nil {
						last.ReasoningContent = reasoning
					}
					continue
				}
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "assistant", ToolCalls: []shared.CCToolCall{tc},
				ReasoningContent: reasoning,
			})
		case "function_call_output", "custom_tool_call_output":
			text := ""
			plan, eErr := shared.PlanChatToolOutput(item.Output, "tool messages carry text only")
			if eErr != nil {
				return nil, eErr
			}
			if plan != nil {
				text = plan.Text
				if len(plan.Media) > 0 {
					pendingToolMedia = append(pendingToolMedia,
						map[string]any{"type": "text", "text": fmt.Sprintf("[media output of tool call %s]", item.CallID)})
					pendingToolMedia = append(pendingToolMedia, plan.Media...)
				}
			} else {
				text, eErr = shared.RespOutputText(item.Output, "tool messages carry text only")
				if eErr != nil {
					return nil, eErr
				}
			}
			if _, ok := knownCalls[item.CallID]; !ok {
				// Orphan result: a tool message without a preceding
				// tool_calls entry is rejected by strict backends. Degrade
				// it to user text so the conversation still works.
				out.Messages = append(out.Messages, ccMessage{
					Role: "user", Content: "[tool result without a matching call] " + text,
				})
			} else {
				out.Messages = append(out.Messages, ccMessage{
					Role: "tool", Content: text, ToolCallID: item.CallID,
				})
			}
			// Tool output closes the turn: a reasoning item that follows
			// belongs to the next one.
			pendingReasoning, hasPendingReasoning = "", false
		case "reasoning":
			// No Chat Completions field of its own, but the following
			// function_call must replay it as reasoning_content or the
			// upstream rejects the whole conversation with 400 while
			// thinking mode is on.
			pendingReasoning = respItemReasoning(item)
			hasPendingReasoning = true
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
	}

	flushToolMedia()

	// CC Switch parity: DeepSeek validates tool-result adjacency strictly,
	// so collapse assistant messages replayed inside an unanswered tool
	// turn; every thinking-mode upstream also needs a non-empty
	// reasoning_content on the messages that carry tool_calls.
	if isDeepSeekChatModel(upstreamModel) {
		out.Messages = coalesceReplayedToolTurn(out.Messages)
	}
	ensureToolCallReasoning(out.Messages)

	if len(systemTexts) > 0 {
		out.Messages = append([]ccMessage{{Role: "system", Content: strings.Join(systemTexts, "\n\n")}}, out.Messages...)
	}

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := shared.ObjectSchema(t.Parameters)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	applyToolChoiceCC(out, kind, tcName)
	enableStreamUsage(out)
	encoded := mergeChatExtras(encode(out), body)
	return injectIncludeUsage(encoded, out.Stream), nil
}

// chatPassthroughFields are Responses-side knobs that have a direct Chat
// Completions equivalent. They are copied after the typed conversion so a
// strict upstream keeps behavior the client asked for.
var chatPassthroughFields = []string{
	"frequency_penalty", "logit_bias", "logprobs", "metadata", "n",
	"presence_penalty", "response_format", "seed", "service_tier", "stop",
	"top_logprobs", "user",
}

func mergeChatExtras(encoded, source []byte) []byte {
	var dst map[string]json.RawMessage
	if json.Unmarshal(encoded, &dst) != nil {
		return encoded
	}
	var src map[string]json.RawMessage
	if json.Unmarshal(source, &src) != nil {
		return encoded
	}
	changed := false
	for _, key := range chatPassthroughFields {
		if raw, ok := src[key]; ok {
			dst[key] = raw
			changed = true
		}
	}
	if !changed {
		return encoded
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return encoded
	}
	return out
}

// injectIncludeUsage sets stream_options.include_usage on an already
// encoded request, preserving any other stream_options the client sent.
func injectIncludeUsage(encoded []byte, stream bool) []byte {
	if !stream {
		return encoded
	}
	var dst map[string]json.RawMessage
	if json.Unmarshal(encoded, &dst) != nil {
		return encoded
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := dst["stream_options"]; ok {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	b, err := json.Marshal(opts)
	if err != nil {
		return encoded
	}
	dst["stream_options"] = b
	out, err := json.Marshal(dst)
	if err != nil {
		return encoded
	}
	return out
}

// respItemReasoning extracts the thinking text carried by a Responses
// reasoning item, preferring summary[] and falling back to content[].
func respItemReasoning(item shared.RespItem) string {
	var sb strings.Builder
	for _, part := range item.Summary {
		sb.WriteString(part.Text)
	}
	if sb.Len() > 0 {
		return sb.String()
	}
	if len(item.Content) > 0 {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Content, &parts) == nil {
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
		}
	}
	return sb.String()
}

// respContent normalizes a Responses content field (JSON string or typed
// parts) into Chat Completions content: a plain string, content parts,
// or nil when empty (FR-005 multimodal preservation). The shared kernel
// owns validation and the empty-input policy — empty-text messages drop
// identically on every route (F5 parity).
func respContent(raw json.RawMessage) (any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	switch {
	case len(parts) == 0:
		return nil, nil
	case len(parts) == 1 && parts[0].ImageURL == "":
		return parts[0].Text, nil
	}
	out := make([]ccContentPart, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: p.ImageURL}})
			continue
		}
		out = append(out, ccContentPart{Type: "text", Text: p.Text})
	}
	return out, nil
}

// applyToolChoiceCC maps a normalized tool choice onto the outbound Chat
// Completions request, which natively represents every normalized kind
// (FR-005). It is infallible: shared.DecodeToolChoice emits exactly the
// closed set below, so the switch covers every possible kind (the
// exhaustiveness test in request_test.go enforces this against
// shared.AllToolChoiceKinds); absent omits the field.
func applyToolChoiceCC(req *ccRequest, kind, name string) {
	if len(req.Tools) == 0 {
		// OpenAI-compatible backends reject tool_choice without tools.
		return
	}
	switch kind {
	case shared.ToolChoiceAuto:
		req.ToolChoice = json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		req.ToolChoice = json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		req.ToolChoice = json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": name},
		})
		req.ToolChoice = b
	}
}
