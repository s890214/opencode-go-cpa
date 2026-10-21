// Package reasoning keeps the upstream thinking text that accompanied a
// tool call, keyed by that call's id, so a later turn can hand it back.
//
// DeepSeek-style upstreams run tool-calling turns in thinking mode and
// reject the follow-up turn with
//
//	400 invalid_request_error: The `reasoning_content` in the thinking
//	mode must be passed back to the API.
//
// whenever the assistant turn that produced the call replays without its
// reasoning. Responses clients (Codex) only replay the call item and its
// output — they never receive the reasoning, because the upstream
// Responses endpoint does not emit one (reasoning_tokens: 0). The store
// bridges that gap: the executor records what the upstream said while
// answering, and the request translators read it back.
//
// Everything here is bounded: entries expire, the map has a hard ceiling,
// and oversized texts are truncated, because the host runs the plugin
// under a small GOMEMLIMIT.
package reasoning

import (
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxEntries bounds the live map. With maxTextBytes each entry
	// costs at most 8 KiB, so the whole store stays under ~4 MiB.
	DefaultMaxEntries = 512
	// DefaultTTL drops entries that no follow-up turn claimed in time.
	DefaultTTL = 2 * time.Hour
	// maxTextBytes truncates pathological reasoning blobs.
	maxTextBytes = 8192
)

type entry struct {
	text     string
	storedAt time.Time
}

// Store is a bounded TTL map of call id → reasoning text. The zero value
// is not usable; call New.
type Store struct {
	mu      sync.Mutex
	entries map[string]entry
	order   []string // insertion order, oldest first
	max     int
	ttl     time.Duration
	now     func() time.Time
}

// New returns a store bounded by max entries and ttl. Non-positive values
// fall back to the defaults.
func New(max int, ttl time.Duration) *Store {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{
		entries: make(map[string]entry, max),
		order:   make([]string, 0, max),
		max:     max,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Put records the reasoning that produced callID. An empty callID is
// ignored (nothing can look it up later) and an empty text is stored as
// an explicit "known but empty" marker, which is different from a miss:
// callers use that distinction to send a placeholder instead of guessing.
func (s *Store) Put(callID, text string) {
	if s == nil || strings.TrimSpace(callID) == "" {
		return
	}
	if len(text) > maxTextBytes {
		text = text[:maxTextBytes]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if _, exists := s.entries[callID]; !exists {
		s.order = append(s.order, callID)
	}
	s.entries[callID] = entry{text: text, storedAt: s.now()}
	// Enforce the ceiling by dropping the oldest insertions. A re-Put
	// keeps its original queue position, so a hot id cannot pin stale
	// neighbours.
	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// Get returns the stored reasoning and whether the id is known.
func (s *Store) Get(callID string) (string, bool) {
	if s == nil || strings.TrimSpace(callID) == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[callID]
	if !ok {
		return "", false
	}
	if s.now().Sub(e.storedAt) > s.ttl {
		delete(s.entries, callID)
		return "", false
	}
	return e.text, true
}

// Len reports the live entry count (diagnostics and tests).
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.entries)
}

// Reset empties the store (tests).
func (s *Store) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]entry, s.max)
	s.order = s.order[:0]
}

func (s *Store) pruneLocked() {
	if len(s.order) == 0 {
		return
	}
	cutoff := s.now().Add(-s.ttl)
	kept := s.order[:0]
	for _, id := range s.order {
		e, ok := s.entries[id]
		if !ok {
			continue
		}
		if e.storedAt.Before(cutoff) {
			delete(s.entries, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}
