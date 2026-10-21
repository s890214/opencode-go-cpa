package plugin

import (
	"testing"

	"opencode-go-cpa/internal/adapter/shared"
	"opencode-go-cpa/internal/catalog"
)

func reasoningOf(t *testing.T, id string) (string, bool) {
	t.Helper()
	return shared.DefaultReasoningStore().Get(id)
}

func TestCaptureChatBodyBindsEveryToolCall(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"why",
		"tool_calls":[{"id":"c1"},{"id":"c2"}]}}]}`)
	captureNonStreamReasoning(catalog.RouteChatCompletions, body)
	for _, id := range []string{"c1", "c2"} {
		if v, ok := reasoningOf(t, id); !ok || v != "why" {
			t.Fatalf("%s = (%q, %v), want (why, true)", id, v, ok)
		}
	}
}

func TestCaptureChatBodyIgnoresTurnsWithoutCalls(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"plain answer"}}]}`)
	captureNonStreamReasoning(catalog.RouteChatCompletions, body)
	if n := shared.DefaultReasoningStore().Len(); n != 0 {
		t.Fatalf("plain answers must not be cached, len = %d", n)
	}
}

func TestCaptureResponsesBodyBindsReasoning(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	body := []byte(`{"output":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"function_call","call_id":"c7","name":"f","arguments":"{}"}
	]}`)
	captureNonStreamReasoning(catalog.RouteResponses, body)
	if v, ok := reasoningOf(t, "c7"); !ok || v != "think" {
		t.Fatalf("c7 = (%q, %v), want (think, true)", v, ok)
	}
}

func TestCaptureIgnoresMalformedBody(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	captureNonStreamReasoning(catalog.RouteChatCompletions, []byte(`{"choices":`))
	captureNonStreamReasoning(catalog.RouteResponses, []byte(`not json`))
	if n := shared.DefaultReasoningStore().Len(); n != 0 {
		t.Fatalf("malformed bodies must not cache, len = %d", n)
	}
}

func TestReasoningTeeBindsStreamedChatCall(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	tee := newReasoningTee(catalog.RouteChatCompletions)
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step one \"}}]}\n\n"))
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step two\"}}]}\n\n"))
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_s1\"}]}}]}\n\n"))
	if n := shared.DefaultReasoningStore().Len(); n != 0 {
		t.Fatalf("nothing may be cached before the stream ends, len = %d", n)
	}
	tee.Flush()
	if v, ok := reasoningOf(t, "call_s1"); !ok || v != "step one step two" {
		t.Fatalf("call_s1 = (%q, %v)", v, ok)
	}
}

func TestReasoningTeeHandlesResponsesItems(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	tee := newReasoningTee(catalog.RouteResponses)
	tee.Feed([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"why\"}]}}\n\n"))
	tee.Feed([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_r1\"}}\n\n"))
	tee.Flush()
	if v, ok := reasoningOf(t, "call_r1"); !ok || v != "why" {
		t.Fatalf("call_r1 = (%q, %v)", v, ok)
	}
}

func TestReasoningTeeFlushIsIdempotent(t *testing.T) {
	shared.DefaultReasoningStore().Reset()
	tee := newReasoningTee(catalog.RouteChatCompletions)
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"a\"}}]}\n\n"))
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c\"}]}}]}\n\n"))
	tee.Flush()
	tee.Feed([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"late\"}}]}\n\n"))
	tee.Flush()
	if v, _ := reasoningOf(t, "c"); v != "a" {
		t.Fatalf("late chunk leaked into the bind: %q", v)
	}
}

// The tee must survive any chunk boundary the network hands it. The old
// per-chunk line split captured nothing below ~1 KB per chunk.
func TestReasoningTeeHandlesSplitChunks(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"world\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_split\"}]}}]}\n\n"

	for _, size := range []int{1, 3, 7, 64, 4096, len(stream)} {
		shared.DefaultReasoningStore().Reset()
		tee := newReasoningTee(catalog.RouteChatCompletions)
		for i := 0; i < len(stream); i += size {
			end := i + size
			if end > len(stream) {
				end = len(stream)
			}
			tee.Feed([]byte(stream[i:end]))
		}
		tee.Flush()
		v, ok := shared.DefaultReasoningStore().Get("call_split")
		if !ok || v != "hello world" {
			t.Fatalf("chunk size %d: got (%q, %v), want (hello world, true)", size, v, ok)
		}
	}
}
