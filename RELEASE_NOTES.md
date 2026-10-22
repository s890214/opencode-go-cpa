## v0.5.3 — malformed tool arguments degrade instead of failing the turn

### Protocol

- A historical tool call whose `arguments` are not valid JSON no longer fails
  the whole request. The raw payload is preserved as `{"input": <raw>}` —
  Anthropic requires `tool_use.input` to be an object, and blanking it to `{}`
  loses the model's only output. The Chat→Messages and Responses→Messages
  conversion legs used to return a translation error instead.
- Verified existing guards are now pinned by tests: `CleanSchema` (root object
  fill, `format:uri` drop, array-item recursion, empty schema), the Anthropic
  history sanitizer (leading user, empty messages, incomplete/complete tool
  turns, unsigned thinking on tool turns), and `strict:false` on Responses
  function tools built from Chat or Claude sources.

### Tests

- Degradation assertions on all four surfaces.
- Schema and sanitizer coverage enumerated above.

## v0.5.2 — tool-result media in the Chat bridge

### Protocol

- `function_call_output` may carry `input_image` / `input_file` / `input_audio`
  blocks (Codex screenshots, PDF reads). Chat tool messages are text-only, so
  those blocks are now extracted, replaced by a marker inside the tool text, and
  re-emitted on a synthetic user message before the next non-tool item. This
  removes the `unsupported tool content part type "input_image"` rejection that
  killed any converted Codex turn carrying a screenshot.
- Parallel tool results stay adjacent and text-only results keep the exact
  legacy wire shape, so prompt caching is unaffected.

### Tests

- Plan-level coverage for image / file / unknown-part / scalar-text inputs.
- End-to-end Responses→Chat coverage asserting
  assistant → tool → user(media) → user(next) ordering.

## v0.5.1 — protocol hardening across all nine routes

### Routing

- `protocol-endpoints` now applies to a model's default route, not only its
  pass-through route.
- Per-model `endpoints` are published as `EndpointPaths`, so a pinned path
  survives every pass-through decision.
- `fallback-routes` distinguishes an absent key (use the built-in family
  table) from an explicit `[]` (do not guess).
- Every per-protocol endpoint is validated against the base URL, closing the
  SSRF gap that only covered the default endpoint.

### 400 prevention

- `tool_choice` is never sent without `tools`; `none` fails descriptively on
  Anthropic targets.
- Responses `max_output_tokens` is floored at 16, so Claude Desktop's
  `max_tokens: 1` availability probe no longer 400s.
- `additional_tools` survives when the client sends no top-level `tools`.
- `web_search` keeps `filters`, `user_location` and `search_context_size`;
  only the OpenAI-only `search_content_types` field is removed.
- Custom tools are wrapped/unwrapped on the Chat leg and restored to
  `custom_tool_call` on the Responses leg.
- Orphan tool outputs degrade to user text; malformed historical arguments
  degrade to `{}`; empty-argument tool calls emit `arguments: "{}"`.
- System/developer messages are collapsed to the head, `include_usage` is
  injected, o-series models use `max_completion_tokens`, and Responses→Chat
  keeps `metadata`, `seed`, `stop`, `response_format` and related fields.
- Function tools carry `strict: false`, avoiding schema-strictness 400s.

### Reasoning

- Signed Anthropic thinking is bridged into Responses `reasoning` items and
  restored on the next Claude turn.
- Responses reasoning items are carried in Anthropic thinking signatures and
  restored on the next Responses turn.
- Chat `reasoning_content` becomes a thinking block for Claude clients and a
  reasoning summary for Responses clients.
- Unsigned thinking is removed from tool-call assistant turns instead of being
  replayed into an Anthropic 400.
- Thinking budgets raise `max_tokens` to `budget + 1024` rather than being
  clipped to half the output window.

### Streaming and media

- Reasoning deltas and reasoning output items are emitted on all conversion
  legs; empty deltas are skipped.
- Empty-argument tool calls are announced at `content_block_stop`.
- Document blocks become `input_file`; images inside `tool_result` become
  `input_image` instead of failing as text-only.
- Bare `sk-...` values are covered by error redaction.

### Verification

- `go test ./...` and `go vet ./...` pass.
- Full live suite against the real OpenCode Go upstream: 18/18 3×3
  non-stream/stream smoke cases, plus tool calling, reasoning, Codex tools,
  error propagation, multipart tool output and session affinity.

### Upgrade

- Replace the plugin binary and restart CLIProxyAPI.
- No configuration change is required.

## v0.5.0 — one config table, pass-through that really passes through

### Per-model policy replaces two overlapping tables

`protocol-preference` and `route-overrides` described the same model twice, and
`route-overrides` could only hold one endpoint while the model spoke three
protocols. Both are now folded into one block:

```yaml
models:
  deepseek-v4.1-flash:
    protocols: [chat-completions, responses, messages]   # first = default route
  deepseek-v4-pro:
    protocols: [chat-completions, responses, messages]
    reasoning-shape: content
  custom-proxy-model:
    protocols: [chat-completions, responses]
    endpoints:
      responses: /v1/responses
```

- `protocols` is ordered: the first entry is the default route, and any client
  whose protocol appears in the list is passed through untouched.
- `endpoints` is keyed by protocol, so one model can pin a different custom path
  for each protocol it serves.
- `reasoning-shape` is `summary` (default) or `content`.
- The legacy tables still load and are merged into `models`, so existing
  configurations keep working.

### Reasoning shape is per model, not guessed

Measured on the OpenCode Go gateway:

| model | `summary[]` | `content[]` |
|---|---|---|
| `deepseek-v4-pro` | **400** `reasoning_text must be passed back` | **200** |
| `deepseek-v4.1-flash` | 200 | **503** `Endpoint is unavailable.` |
| `glm-5.3-flash` / `kimi-k3` | 200 | **503** / **400** |

There is no shape that works for all of them, so it is configuration:
`reasoning-shape: content` for v4-pro, `summary` for everything else.

### Pass-through no longer rewrites tools

The translator's tool normalization used to run on the native Responses route
too, which flattened Codex's four namespace trees (45 MCP tools, 34 of them
under `mcp__codex_app`) into anonymous `ns__name` functions and degraded custom
tools to plain functions, dropping their grammar. The upstream accepts both
shapes verbatim — measured 200 with a namespace tree and `custom_tool_call` for
a custom tool — so pass-through now forwards the tools array as-is, sanitising
only `web_search` (its `search_content_types` field is OpenAI-only).

### Reasoning binding is stateless

`pendingReasoning` / `hasPendingReasoning` are gone. In a multi-turn history the
second tool call could inherit the first turn's reasoning text, because only a
few branches reset the cursor. Two precomputed arrays replace it:

- `reasoningText[i]` — the inbound reasoning that applies at position i, cleared
  at every user turn so it cannot cross a conversation boundary.
- `reasoningBeforeCall[i]` — whether a tool call follows, i.e. whether that
  reasoning is worth keeping.

`reasoning_content: null` is now treated as missing (the upstream answers 400
for `null` and 200 for `""`).

### Streamed capture survives any chunk boundary

`reasoningTee` parsed each network chunk on its own, so every SSE frame the
network happened to split was dropped; at 64-byte chunks it captured 0 of 103
characters. It now uses the shared `SSEFramer`, with a test that feeds the same
stream in 1/3/7/64/4096-byte slices.

### Dropped tools are logged

A translated request that loses `web_search` or `apply_patch` now records a
`warn` line naming what was lost and why. Pass-through selection is logged at
`debug` level.

### Release

- CI artifact name fixed: `PLUGIN_ID` was still `opencode-go-cliproxyapi`, so the
  zip never matched the deployment directory.
- `gofmt` clean.

---

## v0.4.0

### Thinking-mode pass-back (fixes the first tool-result turn)

OpenCode Go runs tool-calling turns in thinking mode. When the assistant turn
that produced a tool call is replayed without its reasoning, the upstream
answers:

```
400 invalid_request_error: The `reasoning_content` in the thinking mode
must be passed back to the API.
```

What actually broke it: the translator dropped the `reasoning` input items a
Responses client sends back. Codex does send them — `summary[]` plus an opaque
`encrypted_content` blob — and the upstream accepts that shape as-is, so
discarding the item made every tool turn look reasoning-free.

- Forward those items verbatim when the summary carries text; the upstream
  reads summary text only and ignores `encrypted_content`.
- Capture the upstream thinking keyed by tool call id, on both streaming and
  non-streaming responses, for Chat Completions and Responses routes.
- Replay it on the next turn: `reasoning_content` for Chat Completions
  targets, a `reasoning` item for Responses targets.
- Emit a neutral placeholder when nothing was captured. The upstream treats
  an empty summary exactly like a missing item (`400`).
- Orphaned reasoning items (no trailing tool call) are still dropped.
- Anthropic `thinking` blocks ahead of `tool_use` are decoded and replayed.

### Other fixes

- `protocol-preference` is published in the registration metadata.
- A `protocol-preference` entry whose model is not in the upstream catalog logs
  a warning.
- Model ids and protocol names are normalised at load time and matched
  case-insensitively against the catalog.
- The `protocols.*` switches gate pass-through as well.
- Default `base-url` is `https://opencode.ai/inference/go/v1`.

### Upgrade notes

- Replace the plugin binary and restart CLIProxyAPI.
- No config change is required; the legacy tables keep working.
- The reasoning cache is in-process, bounded (512 entries, 2 h TTL) and cleared
  on restart.

**Full Changelog**: https://github.com/s890214/opencode-go-cpa/commits/main
