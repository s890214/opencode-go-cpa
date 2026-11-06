package shared

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ToolSearchShimName is the Chat function name a client-executed Responses
// tool_search is disguised as. A client that already declared a function of
// the same name gets a numeric suffix instead (tool_search_1, ...), so the
// restored item type never depends on a name the client already owns.
const ToolSearchShimName = "tool_search"

// tool_search execution values: client search runs in the client and is
// bridged; server search runs upstream and has no Chat equivalent.
const (
	ToolSearchExecutionClient = "client"
	ToolSearchExecutionServer = "server"
)

// toolSearchShimDescription is the fallback description when the client's
// declaration carries none.
const toolSearchShimDescription = "Search for deferred tools and load the matching tools."

// toolSearchShimSchema is the fallback parameter schema: one free-text query.
var toolSearchShimSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)

// ToolSearchCall is the Responses tool_search_call item restored from a Chat
// function call: the shim name never reaches the client, and arguments travel
// as a JSON object like the native shape rather than a JSON string.
type ToolSearchCall struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Execution string          `json:"execution"`
	Status    string          `json:"status"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolSearchItemID derives the item identity from the call id, so the stream
// announcement and the terminal payload always agree.
func ToolSearchItemID(callID string) string { return "tsc_" + callID }

// ToolSearchCallFromArgs builds the restored item from the accumulated Chat
// arguments. Arguments that do not decode to a JSON object cannot have come
// from a valid search request, so the call is marked incomplete; a missing
// query still yields a well-formed completed call.
func ToolSearchCallFromArgs(callID, args string) ToolSearchCall {
	status, obj := toolSearchArguments(args)
	return ToolSearchCall{
		Type: "tool_search_call", ID: ToolSearchItemID(callID), CallID: callID,
		Execution: ToolSearchExecutionClient, Status: status, Arguments: obj,
	}
}

// ToolSearchQueryText renders the arguments a replayed tool_search_call
// carried as the arguments string the Chat shim call takes.
func ToolSearchQueryText(arguments string) string {
	return DefaultArgs(arguments)
}

// ToolSearchResultText renders the tools a search discovered as the tool
// result body the Chat model reads: the native summary shape, with the
// callable (flattened) names of everything the search returned.
func ToolSearchResultText(tools []RespTool, status string) string {
	names := make([]string, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		add(tool.Name)
	}
	if strings.TrimSpace(status) == "" {
		status = "completed"
	}
	b, err := json.Marshal(map[string]any{"status": status, "tools": names})
	if err != nil {
		return `{"status":"completed","tools":[]}`
	}
	return string(b)
}

// toolSearchArguments reports the call status and the object form of the
// arguments. Only a body that is not a JSON object is a defect worth marking
// incomplete; absent arguments decode to an empty object.
func toolSearchArguments(args string) (string, json.RawMessage) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "completed", json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj == nil {
		return "incomplete", json.RawMessage(`{}`)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return "incomplete", json.RawMessage(`{}`)
	}
	return "completed", json.RawMessage(buf.Bytes())
}
