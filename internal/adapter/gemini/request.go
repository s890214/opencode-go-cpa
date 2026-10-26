package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cpa/internal/adapter/chatcompletions"
	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/errclass"
)

// BuildRequest converts any inbound client format into a Google
// generateContent body. It reuses the Chat Completions adapter for the first
// hop (so Claude/Responses histories get the same tool pairing, system
// hoisting and reasoning handling as the Chat route) and only owns the
// Chat→Gemini half.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	chatBody, eErr := chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	if eErr != nil {
		return nil, eErr
	}
	return FromChatRequest(chatBody, ts)
}

// chatRequest is the subset of a Chat Completions request Gemini needs.
type chatRequest struct {
	Messages []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID       string `json:"tool_call_id"`
		ReasoningContent string `json:"reasoning_content"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	ReasoningEffort     string          `json:"reasoning_effort"`
}

type gPart struct {
	Text             string         `json:"text,omitempty"`
	Thought          bool           `json:"thought,omitempty"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
	InlineData       *gInlineData   `json:"inlineData,omitempty"`
	FileData         *gFileData     `json:"fileData,omitempty"`
	FunctionCall     *gFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *gFunctionResp `json:"functionResponse,omitempty"`
}

type gInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type gFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type gFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gFunctionResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gTool struct {
	FunctionDeclarations []gFunctionDecl `json:"functionDeclarations"`
}

type gFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gGenerationConfig struct {
	MaxOutputTokens int64            `json:"maxOutputTokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	TopP            *float64         `json:"topP,omitempty"`
	StopSequences   []string         `json:"stopSequences,omitempty"`
	ThinkingConfig  *gThinkingConfig `json:"thinkingConfig,omitempty"`
	CandidateCount  int              `json:"candidateCount,omitempty"`
}

type gThinkingConfig struct {
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
}

type gToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode,omitempty"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type gRequest struct {
	SystemInstruction *gContent          `json:"systemInstruction,omitempty"`
	Contents          []gContent         `json:"contents"`
	Tools             []gTool            `json:"tools,omitempty"`
	ToolConfig        *gToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *gGenerationConfig `json:"generationConfig,omitempty"`
}

// FromChatRequest renders a Chat Completions body as a Gemini request.
func FromChatRequest(chatBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	var src chatRequest
	if err := json.Unmarshal(chatBody, &src); err != nil {
		return nil, errclass.Translation("malformed chat request for gemini: " + err.Error())
	}
	req := gRequest{}
	var systemParts []string
	// Tool results carry only tool_call_id; Gemini needs the function name,
	// so resolve it from the assistant turn that produced the call.
	toolNames := map[string]string{}
	for _, msg := range src.Messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}
	for _, msg := range src.Messages {
		switch msg.Role {
		case "system", "developer":
			parts, eErr := decodeChatParts(msg.Content)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if p.Text != "" {
					systemParts = append(systemParts, p.Text)
				}
			}
		case "user":
			parts, eErr := decodeChatParts(msg.Content)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Contents = append(req.Contents, gContent{Role: "user", Parts: parts})
			}
		case "assistant":
			parts, eErr := decodeChatParts(msg.Content)
			if eErr != nil {
				return nil, eErr
			}
			for _, tc := range msg.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if !json.Valid([]byte(args)) || args == "" {
					raw := args
					if raw == "" {
						raw = "null"
					}
					wrapped, _ := json.Marshal(map[string]string{"input": raw})
					args = string(wrapped)
				}
				parts = append(parts, gPart{FunctionCall: &gFunctionCall{Name: tc.Function.Name, Args: json.RawMessage(args)}})
			}
			if len(parts) > 0 {
				req.Contents = append(req.Contents, gContent{Role: "model", Parts: parts})
			}
		case "tool":
			name := toolNames[msg.ToolCallID]
			if name == "" {
				name = "tool"
			}
			output := ""
			if parts, eErr := decodeChatParts(msg.Content); eErr == nil {
				var b strings.Builder
				for _, part := range parts {
					if part.Text != "" {
						b.WriteString(part.Text)
					}
				}
				output = b.String()
			}
			response, _ := json.Marshal(map[string]any{"output": output, "tool_call_id": msg.ToolCallID})
			req.Contents = append(req.Contents, gContent{Role: "user", Parts: []gPart{
				{FunctionResponse: &gFunctionResp{Name: name, Response: response}},
			}})
		default:
			return nil, shared.ValidateRole(msg.Role, "Gemini")
		}
	}
	if len(systemParts) > 0 {
		req.SystemInstruction = &gContent{Parts: []gPart{{Text: strings.Join(systemParts, "\n\n")}}}
	}
	if len(req.Contents) == 0 {
		// Gemini requires at least one turn; an empty conversation is a
		// client bug, and a descriptive 400 beats an opaque upstream one.
		return nil, errclass.Translation("gemini request has no user content")
	}
	for _, t := range src.Tools {
		if t.Type != "" && t.Type != "function" {
			// Hosted tools have no Gemini equivalent on this endpoint.
			continue
		}
		decl := gFunctionDecl{Name: t.Function.Name, Description: t.Function.Description}
		if schema := sanitizeSchema(t.Function.Parameters); len(schema) > 0 {
			decl.Parameters = schema
		}
		req.Tools = append(req.Tools, gTool{FunctionDeclarations: []gFunctionDecl{decl}})
	}
	if len(req.Tools) > 0 {
		if cfg := toolConfig(src.ToolChoice); cfg != nil {
			req.ToolConfig = cfg
		}
	}
	gen := &gGenerationConfig{Temperature: src.Temperature, TopP: src.TopP, StopSequences: stopSequences(src.Stop)}
	if src.MaxCompletionTokens != nil {
		gen.MaxOutputTokens = *src.MaxCompletionTokens
	} else if src.MaxTokens != nil {
		gen.MaxOutputTokens = *src.MaxTokens
	}
	if gen.MaxOutputTokens > 0 && gen.MaxOutputTokens < 16 {
		// Gemini rejects tiny caps the same way Responses does; a 1-token
		// availability probe should still succeed.
		gen.MaxOutputTokens = 16
	}
	if tc := thinkingConfig(src.ReasoningEffort, ts); tc != nil {
		gen.ThinkingConfig = tc
	}
	if gen.MaxOutputTokens != 0 || gen.Temperature != nil || gen.TopP != nil || len(gen.StopSequences) > 0 || gen.ThinkingConfig != nil {
		req.GenerationConfig = gen
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("failed to encode gemini request: " + err.Error())
	}
	return out, nil
}

// decodeChatParts converts Chat Completions content (string or typed parts)
// into Gemini parts. Images become inlineData for data URLs and fileData for
// hosted URLs; unknown part types are dropped rather than failing the turn.
func decodeChatParts(raw json.RawMessage) ([]gPart, *errclass.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return []gPart{{Text: text}}, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errclass.Translation("invalid chat content for gemini")
	}
	out := make([]gPart, 0, len(parts))
	for _, p := range parts {
		typ, _ := p["type"].(string)
		switch typ {
		case "", "text", "input_text", "output_text":
			if t, ok := p["text"].(string); ok && t != "" {
				out = append(out, gPart{Text: t})
			}
		case "image_url", "input_image":
			url, mime := imageURLOf(p)
			if url == "" {
				continue
			}
			if data, media, ok := decodeDataURL(url); ok {
				out = append(out, gPart{InlineData: &gInlineData{MimeType: firstNonEmpty(mime, media), Data: data}})
				continue
			}
			out = append(out, gPart{FileData: &gFileData{MimeType: mime, FileURI: url}})
		}
	}
	return out, nil
}

func imageURLOf(p map[string]any) (url, mime string) {
	switch v := p["image_url"].(type) {
	case string:
		return v, ""
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			url = s
		}
		if s, ok := v["mime_type"].(string); ok {
			mime = s
		}
	}
	if s, ok := p["image_url"].(map[string]any); ok && url == "" {
		if u, ok := s["url"].(string); ok {
			url = u
		}
	}
	if s, ok := p["url"].(string); ok && url == "" {
		url = s
	}
	return url, mime
}

func decodeDataURL(raw string) (data, mime string, ok bool) {
	if !strings.HasPrefix(raw, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	mime = strings.TrimSuffix(meta, ";base64")
	if !strings.Contains(meta, ";base64") {
		return "", "", false
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return "", "", false
	}
	return payload, mime, true
}

func mustParts(raw json.RawMessage) []map[string]any {
	var parts []map[string]any
	_ = json.Unmarshal(raw, &parts)
	return parts
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sanitizeSchema trims JSON-Schema keywords Gemini rejects. The OpenAPI
// subset accepts type/properties/required/enum/items/description; anything
// else (format, additionalProperties, $schema, strict…) is dropped.
func sanitizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	clean := cleanSchemaMap(schema)
	out, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return out
}

func cleanSchemaMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "additionalProperties", "$schema", "strict", "format", "pattern", "$ref", "definitions", "$defs", "oneOf", "anyOf", "not":
			continue
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				continue
			}
			cleaned := make(map[string]any, len(props))
			for name, sub := range props {
				if m, ok := sub.(map[string]any); ok {
					cleaned[name] = cleanSchemaMap(m)
				}
			}
			out[k] = cleaned
		case "items":
			if m, ok := v.(map[string]any); ok {
				out[k] = cleanSchemaMap(m)
			}
		default:
			out[k] = v
		}
	}
	if typ, _ := out["type"].(string); typ == "object" {
		if _, ok := out["properties"]; !ok {
			out["properties"] = map[string]any{}
		}
	}
	return out
}

func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// toolConfig maps Chat tool_choice onto Gemini's function-calling modes.
func toolConfig(raw json.RawMessage) *gToolConfig {
	if len(raw) == 0 {
		return nil
	}
	cfg := &gToolConfig{}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			cfg.FunctionCallingConfig.Mode = "AUTO"
		case "none":
			cfg.FunctionCallingConfig.Mode = "NONE"
		case "required":
			cfg.FunctionCallingConfig.Mode = "ANY"
		default:
			return nil
		}
		return cfg
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) == nil && named.Type == "function" && named.Function.Name != "" {
		cfg.FunctionCallingConfig.Mode = "ANY"
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{named.Function.Name}
		return cfg
	}
	return nil
}

// thinkingConfig maps a client effort onto Gemini's thinking controls: a
// model that advertises named levels gets thinkingLevel, a budget-only model
// gets a proportional thinkingBudget.
func thinkingConfig(effort string, ts *pluginapi.ThinkingSupport) *gThinkingConfig {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" || effort == "none" {
		if effort == "none" && ts != nil && ts.ZeroAllowed {
			return &gThinkingConfig{ThinkingBudget: intPtr(0)}
		}
		return nil
	}
	if ts != nil && len(ts.Levels) > 0 {
		for _, level := range ts.Levels {
			if strings.EqualFold(level, effort) {
				return &gThinkingConfig{ThinkingLevel: strings.ToLower(level)}
			}
		}
		return nil
	}
	if ts != nil && ts.Max > 0 {
		ratio := 0.5
		switch effort {
		case "minimal":
			ratio = 0.1
		case "low":
			ratio = 0.25
		case "high":
			ratio = 0.75
		case "max", "xhigh":
			ratio = 1.0
		}
		budget := int(float64(ts.Max) * ratio)
		if budget > 0 && budget < ts.Min {
			budget = ts.Min
		}
		if budget > ts.Max {
			budget = ts.Max
		}
		return &gThinkingConfig{ThinkingBudget: intPtr(budget)}
	}
	return nil
}

func intPtr(v int) *int { return &v }

// String renders a request for debug logs without echoing message text.
func String(body []byte) string {
	return fmt.Sprintf("gemini request %d bytes", len(body))
}
