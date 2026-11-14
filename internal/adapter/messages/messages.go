// Package messages implements the Anthropic Messages protocol adapter
// (arch §8): upstream endpoint /v1/messages serving the MiniMax and Qwen
// routes (FR-004). Request translation is FR-005, response and stream
// translation FR-006, acceptance coverage AC §C.
//
// Per arch §8 this package is isolated from the scheduler and catalog
// manager and uses only stdlib JSON primitives.
package messages

import (
	"net/http"

	"opencode-go-cpa/internal/catalog"
)

// Messages-route auth schemes. Values mirror config.MessagesAuthScheme*; the
// plugin package asserts both sets agree, and the adapter stays free of the
// config package so it can be used on its own.
const (
	AuthSchemeXAPIKey = "x-api-key"
	AuthSchemeBearer  = "bearer"
)

// EndpointPath is the upstream endpoint path for the Messages route (FR-004).
var EndpointPath = catalog.RouteMessages.EndpointPath()

// anthropicVersion is the API version header value the upstream expects.
const anthropicVersion = "2023-06-01"

// AuthHeaders returns the upstream authentication headers for a Messages
// request: the key in the scheme the channel asked for (x-api-key by default,
// bearer for a gateway that only honours bearer tokens) plus the required
// anthropic-version header. The key is never logged (FR-007, FR-011).
func AuthHeaders(key, scheme string) http.Header {
	h := make(http.Header)
	if scheme == AuthSchemeBearer {
		h.Set("Authorization", "Bearer "+key)
	} else {
		h.Set("x-api-key", key)
	}
	h.Set("anthropic-version", anthropicVersion)
	return h
}
