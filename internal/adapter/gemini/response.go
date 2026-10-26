package gemini

import (
	"encoding/json"
	"time"

	"opencode-go-cpa/internal/adapter/chatcompletions"
	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/errclass"
)

// gResponse is one Gemini generateContent response or stream chunk.
type gResponse struct {
	ResponseID   string `json:"responseId"`
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		Content struct {
			Role  string  `json:"role"`
			Parts []gPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type ccUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

type ccToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type ccMessage struct {
	Role      string       `json:"role"`
	Content   any          `json:"content"`
	ToolCalls []ccToolCall `json:"tool_calls,omitempty"`
}

type ccChoice struct {
	Index        int        `json:"index"`
	Message      *ccMessage `json:"message,omitempty"`
	Delta        *ccMessage `json:"delta,omitempty"`
	FinishReason *string    `json:"finish_reason"`
}

type ccCompletion struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []ccChoice `json:"choices"`
	Usage   *ccUsage   `json:"usage,omitempty"`
}

// ConvertNonStreamResponse turns a Gemini response into the Chat Completions
// shape and lets the Chat adapter render the client's protocol, so every
// client format keeps one converter.
func ConvertNonStreamResponse(sourceFormat string, status int, body []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, body)
	}
	chatBody, eErr := toChatCompletion(body)
	if eErr != nil {
		return nil, eErr
	}
	return chatcompletions.ConvertNonStreamResponse(sourceFormat, 200, chatBody, tools...)
}

func toChatCompletion(body []byte) ([]byte, *errclass.Error) {
	var resp gResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errclass.Translation("invalid gemini response JSON: " + err.Error())
	}
	var text string
	var calls []ccToolCall
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = mapFinish(cand.FinishReason)
		for _, part := range cand.Content.Parts {
			if part.Thought {
				// Thought summaries are not assistant output; the token
				// count still carries them via thoughtsTokenCount.
				continue
			}
			if part.Text != "" {
				text += part.Text
			}
			if part.FunctionCall != nil {
				call := ccToolCall{Index: len(calls), ID: shared.MarkToolCallID("gemini:"+part.FunctionCall.Name+":"+string(part.FunctionCall.Args), len(calls)), Type: "function"}
				call.Function.Name = part.FunctionCall.Name
				args := ""
				if len(part.FunctionCall.Args) > 0 {
					args = string(part.FunctionCall.Args)
				}
				call.Function.Arguments = shared.DefaultArgs(args)
				calls = append(calls, call)
			}
		}
		if finish == "" && len(calls) > 0 {
			finish = "tool_calls"
		}
	}
	model := resp.ModelVersion
	out := ccCompletion{
		ID:      firstNonEmpty(resp.ResponseID, "chatcmpl-gemini"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ccChoice{{
			Index:        0,
			Message:      &ccMessage{Role: "assistant", Content: text, ToolCalls: calls},
			FinishReason: strPtr(firstNonEmpty(finish, "stop")),
		}},
		Usage: usageOf(resp.UsageMetadata),
	}
	if text == "" && len(calls) == 0 {
		out.Choices[0].Message.Content = ""
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, errclass.Translation("failed to encode chat completion from gemini: " + err.Error())
	}
	return encoded, nil
}

func usageOf(u *struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}) *ccUsage {
	if u == nil {
		return nil
	}
	completion := u.CandidatesTokenCount + u.ThoughtsTokenCount
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + completion
	}
	usage := &ccUsage{PromptTokens: u.PromptTokenCount, CompletionTokens: completion, TotalTokens: total}
	if u.CachedContentTokenCount > 0 {
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: u.CachedContentTokenCount}
	}
	return usage
}

func mapFinish(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL", "TOOL_CALL":
		return "tool_calls"
	case "":
		return ""
	default:
		return "stop"
	}
}

func strPtr(v string) *string { return &v }

func nowUnix() int64 { return time.Now().Unix() }
