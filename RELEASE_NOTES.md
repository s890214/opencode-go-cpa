## v0.4.0

### Thinking-mode pass-back (fixes the first tool-result turn)

OpenCode Go runs tool-calling turns in thinking mode. When the assistant turn
that produced a tool call is replayed without its reasoning, the upstream
answers:

```
400 invalid_request_error: The `reasoning_content` in the thinking mode
must be passed back to the API.
```

Codex speaks the Responses protocol and only replays the call item plus its
output; the upstream Responses endpoint never hands it the reasoning
(`reasoning_tokens` is 0), so every conversation broke on the first tool
result.

- Capture the upstream thinking keyed by tool call id, on both streaming and
  non-streaming responses, for Chat Completions and Responses routes.
- Replay it on the next turn: `reasoning_content` for Chat Completions
  targets, a `{"type":"reasoning","summary":[...]}` item for Responses
  targets.
- Emit a neutral placeholder when nothing was captured. The upstream treats
  an empty summary exactly like a missing item (`400`), so the field cannot be
  left blank; the placeholder is a short factual sentence, not invented
  analysis.
- Never use the `content[]` shape for a reasoning item: the upstream answers
  `503 Endpoint is unavailable.` for it. The `summary[]` shape is accepted.
- Orphaned reasoning items (no trailing tool call) are still dropped, as
  before; only the ones a tool call depends on are kept and replayed.
- Anthropic `thinking` blocks ahead of `tool_use` are now decoded and replayed
  instead of being discarded.

### Other fixes

- `protocol-preference` is now published in the registration metadata, so the
  management UI can list it. It was previously config-file only.
- A `protocol-preference` entry whose model is not in the upstream catalog now
  logs a warning. Before, a typo or an upstream rename silently disabled
  pass-through with no diagnostic at all.
- Model ids and protocol names in `protocol-preference` are trimmed, lowercased
  and de-duplicated at load time, and matched case-insensitively against the
  catalog.
- Pass-through no longer rewrites `EndpointPath` for a model that has an
  explicit `route-overrides` endpoint.
- The `protocols.*` switches now gate pass-through as well, matching the
  catalog build.
- Default `base-url` is `https://opencode.ai/inference/go/v1`.

### Upgrade notes

- Replace the plugin binary and restart CLIProxyAPI.
- No config change is required: existing `protocol-preference` and
  `route-overrides` blocks keep working.
- The reasoning cache is in-process, bounded (512 entries, 2 h TTL) and cleared
  on restart. A conversation that spans a restart replays an empty placeholder
  instead of a real summary — the request still succeeds.

**Full Changelog**: https://github.com/s890214/opencode-go-cpa/commits/main
