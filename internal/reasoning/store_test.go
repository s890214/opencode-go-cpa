package reasoning

import (
	"testing"
	"time"
)

func TestStorePutGet(t *testing.T) {
	s := New(4, time.Hour)
	s.Put("call_1", "thinking")
	v, ok := s.Get("call_1")
	if !ok || v != "thinking" {
		t.Fatalf("Get = (%q, %v), want (thinking, true)", v, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("unknown id must miss")
	}
	s.Put("", "ignored")
	if s.Len() != 1 {
		t.Fatalf("empty call id must be ignored, len = %d", s.Len())
	}
}

// An empty text is a recorded fact ("this call had no reasoning"), not a
// miss: the translators send a placeholder instead of guessing.
func TestStoreEmptyTextIsKnown(t *testing.T) {
	s := New(4, time.Hour)
	s.Put("call_1", "")
	v, ok := s.Get("call_1")
	if !ok {
		t.Fatal("empty text must still be a hit")
	}
	if v != "" {
		t.Fatalf("text = %q, want empty", v)
	}
}

func TestStoreExpiresEntries(t *testing.T) {
	s := New(4, time.Minute)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Put("call_1", "thinking")
	now = now.Add(2 * time.Minute)
	if _, ok := s.Get("call_1"); ok {
		t.Fatal("expired entry must miss")
	}
}

func TestStoreEvictsOldestBeyondMax(t *testing.T) {
	s := New(2, time.Hour)
	s.Put("a", "1")
	s.Put("b", "2")
	s.Put("c", "3")
	if _, ok := s.Get("a"); ok {
		t.Fatal("oldest entry must be evicted")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("newest entry must survive")
	}
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2", s.Len())
	}
}

func TestResponseItemUsesSummaryForm(t *testing.T) {
	// The upstream answers 503 for a content[]-shaped reasoning item, so
	// the emitted shape is always summary[].
	item := string(ResponseItem("why", ShapeSummary))
	if item != `{"summary":[{"text":"why","type":"summary_text"}],"type":"reasoning"}` &&
		item != `{"type":"reasoning","summary":[{"text":"why","type":"summary_text"}]}` {
		t.Fatalf("item = %s", item)
	}
	if got := TextFromResponseItem(ResponseItem("why", ShapeSummary)); got != "why" {
		t.Fatalf("round trip = %q", got)
	}
}

// The upstream rejects an empty summary just like a missing item, so the
// placeholder must carry text.
func TestResponseItemEmptyBecomesPlaceholder(t *testing.T) {
	if got := TextFromResponseItem(ResponseItem("", ShapeSummary)); got != PlaceholderText {
		t.Fatalf("placeholder text = %q, want %q", got, PlaceholderText)
	}
	if got := TextFromResponseItem(ResponseItem("   ", ShapeSummary)); got != PlaceholderText {
		t.Fatalf("blank text must also become the placeholder, got %q", got)
	}
}
