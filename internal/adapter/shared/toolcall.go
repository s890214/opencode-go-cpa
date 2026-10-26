package shared

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// markedToolCallID matches the upstream's own external-tool call-id shape,
// e.g. "call_00_ET_bVdDa3Ywtq34CfeXHzX21052".
var markedToolCallID = regexp.MustCompile(`^call_[0-9]+_ET_[A-Za-z0-9]+$`)

// callIndexPrefix captures the group number of an upstream-style call id.
var callIndexPrefix = regexp.MustCompile(`^call_([0-9]+)_`)

// MarkToolCallID returns id unchanged when it already carries the upstream's
// external-tool marker, and otherwise derives a stable marked id from it.
//
// Why: the Responses endpoint runs DeepSeek in thinking mode and answers 400
// "The `reasoning_text` in the thinking mode must be passed back to the API."
// when a replayed function_call carries an id it does not recognize and the
// turn has no replayable reasoning item. A call whose id carries the marker is
// accepted without reasoning, so normalizing every replayed id into that shape
// makes reasoning-free replays valid: histories whose thinking was never
// stored, and histories whose calls came from another route (ids this endpoint
// cannot know). It complements the reasoning store — when captured thinking is
// available it is still replayed; this only fixes the id shape.
//
// The mapping is a pure function of id (index only disambiguates id-less
// calls), so a function_call and its function_call_output always normalize to
// the same value and repeated replays stay stable (idempotent: already-marked
// ids pass through untouched). Semantics follow xspeed1989's
// opencode-go-cliproxyapi `MarkToolCallID` (MIT).
func MarkToolCallID(id string, index int) string {
	call := strings.TrimSpace(id)
	if markedToolCallID.MatchString(call) {
		return call
	}
	seed := call
	if seed == "" {
		// id-less calls: keep siblings distinct by folding their position in.
		seed = fmt.Sprintf("#%d", index)
	}
	sum := sha256.Sum256([]byte(seed))
	suffix := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	group := ""
	if m := callIndexPrefix.FindStringSubmatch(call); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			group = fmt.Sprintf("%02d", n%100)
		}
	}
	if group == "" {
		group = fmt.Sprintf("%02d", int(sum[0])%100)
	}
	return "call_" + group + "_ET_" + suffix[:20]
}

// NormalizeResponsesCallIDs rewrites every call id in a Responses request body
// to the marked shape MarkToolCallID produces. It is a no-op for bodies whose
// `input` is the string convenience form, and for ids already marked, so
// applying it twice is harmless.
//
// It runs on the *upstream-bound* body: client ids are only a label on this
// hop, and the rewrite is deterministic, so the ids the client later replays
// normalize to the same values.
func NormalizeResponsesCallIDs(body []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	rawInput, ok := root["input"]
	if !ok {
		return body, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		// `input: "..."` (string form) carries no call ids.
		return body, nil
	}
	changed := false
	index := 0
	for i, item := range items {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(item, &m); err != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(m["type"], &typ)
		switch typ {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
		default:
			continue
		}
		var id string
		if raw, ok := m["call_id"]; ok {
			_ = json.Unmarshal(raw, &id)
		}
		marked := MarkToolCallID(id, index)
		index++
		if marked == strings.TrimSpace(id) {
			continue
		}
		encoded, err := json.Marshal(marked)
		if err != nil {
			continue
		}
		m["call_id"] = encoded
		// `id` on a function_call item is the same value the item was created
		// with; keep the pair consistent when it looks like a call id.
		if raw, ok := m["id"]; ok {
			var itemID string
			if json.Unmarshal(raw, &itemID) == nil && strings.HasPrefix(itemID, "call_") {
				m["id"] = encoded
			}
		}
		rebuilt, err := json.Marshal(m)
		if err != nil {
			continue
		}
		items[i] = rebuilt
		changed = true
	}
	if !changed {
		return body, nil
	}
	newInput, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	root["input"] = newInput
	return json.Marshal(root)
}
