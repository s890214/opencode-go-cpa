package gemini

import (
	"bytes"
	"encoding/json"
	"strings"

	"opencode-go-cpa/internal/adapter/chatcompletions"
	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/errclass"
)

// StreamConverter turns Gemini SSE chunks into Chat Completions SSE and feeds
// them to the Chat adapter's own converter. Client-facing output therefore
// stays identical to the Chat route (tool call deltas, terminal events,
// usage), and only the Gemini wire shape is new.
type StreamConverter struct {
	inner *chatcompletions.StreamConverter
	buf   []byte
	// model is the last modelVersion seen upstream. Gemini sends it with the
	// first chunk; every client chunk carries it so clients that read the
	// model from the stream (rather than the request) still see it.
	model string
}

// NewStreamConverter builds a converter for one client format.
func NewStreamConverter(sourceFormat string, tools ...*shared.ResponseTools) *StreamConverter {
	return &StreamConverter{inner: chatcompletions.NewStreamConverter(sourceFormat, tools...)}
}

// Feed accepts raw upstream SSE bytes (arbitrary chunk boundaries) and returns
// translated client events.
func (c *StreamConverter) Feed(chunk []byte) ([][]byte, bool, *errclass.Error) {
	c.buf = append(c.buf, chunk...)
	var events [][]byte
	for {
		idx := bytes.IndexByte(c.buf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(c.buf[:idx]), "\r")
		c.buf = c.buf[idx+1:]
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if data == "" || data == "[DONE]" || !strings.HasPrefix(strings.TrimSpace(line), "data:") {
			continue
		}
		chatChunks, model, eErr := chunkToChat([]byte(data), c.model)
		if eErr != nil {
			return events, false, eErr
		}
		if model != "" {
			c.model = model
		}
		for _, cc := range chatChunks {
			frame := make([]byte, 0, len(cc)+8)
			frame = append(frame, "data: "...)
			frame = append(frame, cc...)
			frame = append(frame, '\n', '\n')
			out, done, eErr := c.inner.Feed(frame)
			events = append(events, out...)
			if eErr != nil {
				return events, done, eErr
			}
			if done {
				return events, true, nil
			}
		}
	}
	return events, false, nil
}

// Flush closes a stream whose upstream ended without an explicit terminator.
func (c *StreamConverter) Flush() [][]byte {
	if len(c.buf) > 0 {
		// A final line without a newline is still a valid SSE event here.
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(c.buf)), "data:"))
		c.buf = nil
		if line != "" && line != "[DONE]" && json.Valid([]byte(line)) {
			if chunks, _, eErr := chunkToChat([]byte(line), c.model); eErr == nil {
				for _, cc := range chunks {
					_, _, _ = c.inner.Feed(append(append([]byte("data: "), cc...), '\n', '\n'))
				}
			}
		}
	}
	return c.inner.Flush()
}

// chunkToChat renders one Gemini chunk as zero or more Chat Completion
// chunks: text first, then one chunk per function call, then the finish
// reason, then a usage-only trailer (OpenAI's include_usage shape). It also
// returns the modelVersion found in the chunk, if any.
func chunkToChat(data []byte, fallbackModel string) ([][]byte, string, *errclass.Error) {
	var resp gResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, "", errclass.Translation("invalid gemini stream chunk: " + err.Error())
	}
	model := fallbackModel
	if resp.ModelVersion != "" {
		model = resp.ModelVersion
	}
	var out [][]byte
	var text string
	var calls []ccToolCall
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = mapFinish(cand.FinishReason)
		for _, part := range cand.Content.Parts {
			if part.Thought {
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
	}
	if text != "" {
		chunk := ccCompletion{
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []ccChoice{{
				Index: 0,
				Delta: &ccDelta{Role: "assistant", Content: text},
			}},
		}
		out = append(out, marshalChunk(chunk))
	}
	for _, call := range calls {
		chunk := ccCompletion{
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []ccChoice{{
				Index: 0,
				Delta: &ccDelta{ToolCalls: []ccToolCall{call}},
			}},
		}
		out = append(out, marshalChunk(chunk))
	}
	if finish != "" {
		chunk := ccCompletion{
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []ccChoice{{
				Index:        0,
				Delta:        &ccDelta{},
				FinishReason: strPtr(finish),
			}},
		}
		out = append(out, marshalChunk(chunk))
	}
	if usage := usageOf(resp.UsageMetadata); usage != nil {
		chunk := ccCompletion{Object: "chat.completion.chunk", Model: model, Choices: []ccChoice{}, Usage: usage}
		out = append(out, marshalChunk(chunk))
	}
	return out, resp.ModelVersion, nil
}

func marshalChunk(chunk ccCompletion) []byte {
	chunk.Created = nowUnix()
	if chunk.ID == "" {
		chunk.ID = "chatcmpl-gemini"
	}
	out, _ := json.Marshal(chunk)
	return out
}
