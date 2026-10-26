// Package gemini implements the Google generateContent upstream protocol
// (open code's /google/v1beta endpoint). Requests are normalized through the
// Chat Completions adapter first and responses are turned back into Chat
// Completions payloads, so the three client-facing translations
// (chat/messages/responses) keep using their existing, tested converters and
// only the Gemini wire shape lives here.
package gemini

import (
	"net/url"
	"strings"
)

// UpstreamURL appends the model and action to the configured Gemini endpoint
// prefix. The Google API carries both in the path:
//
//	<base><prefix>/<model>:generateContent
//	<base><prefix>/<model>:streamGenerateContent?alt=sse
//
// prefix is the resolved endpoint path (default "/google/v1beta/models").
func UpstreamURL(joinedPrefix, model string, stream bool) string {
	name := strings.TrimSpace(model)
	name = strings.TrimPrefix(name, "models/")
	action := ":generateContent"
	suffix := ""
	if stream {
		action = ":streamGenerateContent"
		suffix = "?alt=sse"
	}
	return strings.TrimRight(joinedPrefix, "/") + "/" + url.PathEscape(name) + action + suffix
}
