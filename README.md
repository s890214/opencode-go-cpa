# OpenCode Go CLIProxyAPI Plugin (opencode-go-cpa)

> Forked from [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) (MIT).
> This fork adds per-request protocol preference (native pass-through) and keeps `apply_patch` on native Responses routes.

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes OpenCode Go as a single provider (`opencode-go`).

The plugin unifies model discovery, protocol translation, and execution across OpenCode Go's upstream endpoints while leveraging CLIProxyAPI's built-in authentication, scheduling, keys rotation, and cooldown management.

## The Problem

OpenCode Go exposes models across multiple API protocols (OpenAI Chat Completions `/v1/chat/completions`, Anthropic Messages `/v1/messages`, and OpenAI Responses `/v1/responses`).

Without this plugin, using OpenCode Go in CLIProxyAPI requires configuring separate provider blocks for each protocol family. This leads to:
- **Duplicated configuration & keys**: The same API keys must be configured across multiple provider blocks.
- **Fragmented scheduling & rotation**: Keys rotation, rate limits, and cooldowns cannot be shared across protocols—exhausting quota on one protocol does not coordinate with another.
- **Client protocol burden**: Clients must know beforehand which upstream protocol and endpoint each model requires.
- **Fragmented catalog**: Models are split across disjoint provider namespaces instead of a unified model list.

## The Solution

This plugin exposes OpenCode Go as a single provider (`opencode-go`) backed by a shared keys pool:
- **Unified auth pool**: Configure keys once; CLIProxyAPI schedules, rotates, and cools down keys across all protocols.
- **Transparent protocol translation & routing**: Clients request models (e.g. `opencode-go/glm-5.2`, `opencode-go/gpt-5.6-luna`) without needing to know the upstream protocol format.
- **Single model catalog**: All models are discovered and published under the `opencode-go` provider namespace in `/v1/models`.

## Features

- **Single Provider Namespace**: Exposes models under the `opencode-go` provider prefix (e.g. `opencode-go/glm-5.2`, `opencode-go/qwen3.7-max`, `opencode-go/gpt-5.6-luna`).
- **Multi-Protocol Translation**: Translates requests and streaming responses between client formats and upstream endpoints:
  - OpenAI Chat Completions (`/v1/chat/completions`)
  - Anthropic Messages (`/v1/messages`)
  - OpenAI Responses (`/v1/responses`)
- **Thinking & Reasoning Support**: Maps reasoning effort across supported client and upstream formats.
- **Dynamic Catalog Discovery**: Fetches remote model catalogs with local fallback and custom route overrides.
- **Multi-Key Auth Scheduling**: Pools multiple API keys with CLIProxyAPI's native scheduler for rotation, retries, and error cooldowns across all protocols.
- **Thinking-mode pass-back**: OpenCode Go runs tool-calling turns in thinking mode and answers
  `400 invalid_request_error` when the assistant turn that produced a call replays without its
  reasoning. Earlier versions discarded the `reasoning` input items a Responses client sends
  back — and Codex does send them (`summary[]` plus an opaque `encrypted_content`) — so every
  tool-result turn looked reasoning-free to the upstream. Those items are now forwarded verbatim
  when they carry summary text; the upstream reads the summary text only and ignores
  `encrypted_content`. When the client sends none, the plugin fills in the captured reasoning or
  a neutral placeholder instead of failing the turn.
- **Native quotas**: Rolling, weekly, and monthly quotas use CLIProxyAPI's generic quota endpoints and the selected credential. The separate plugin page is removed.

## Requirements

- **CLIProxyAPI**: `v8.0.0+`
- **Go Toolchain**: Go 1.26.7+ (with CGO enabled for C-shared build mode)

## Build

Build the shared library with the `debug` tag for local development.
CI handles release builds.

### Windows (AMD64)
```powershell
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/opencode-go-cpa.dll .
```

### Linux (AMD64)
```bash
go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/opencode-go-cpa.so .
```

### macOS (ARM64)
```bash
go build -tags debug -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-cpa.dylib .
```

Place the compiled binary into your CLIProxyAPI plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Configuration

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-cpa`:

> Upgrading from 0.5: `base-url` is now the service root, not the versioned
> root. If your config has `base-url: .../v1`, drop the trailing `/v1` and
> spell the full protocol paths in `protocol-endpoints`; the join is verbatim,
> so leaving both produces `/v1/v1/...`.

```yaml
plugins:
  configs:
    opencode-go-cpa:
      # Upstream service root (default: "https://opencode.ai/inference/go").
      # Every other path below is appended to it verbatim.
      base-url: "https://opencode.ai/inference/go"

      # Where each protocol lives, appended to base-url as-is.
      # Defaults are the upstream's generic paths; the documented layout is
      # chat/responses under /openai/v1/ and messages under /anthropic/v1/.
      protocol-endpoints:
        chat-completions: "/openai/v1/chat/completions"
        responses: "/openai/v1/responses"
        messages: "/anthropic/v1/messages"

      # Usage/quota endpoint (default: "/v1/usage"). Not a protocol endpoint,
      # so it has its own setting.
      # quota-path: "/v1/usage"

      # Catalog discovery path (default: "/v1/models"), relative to base-url.
      # Also separate, because the upstream serves the model list outside the
      # protocol paths.
      # catalog-path: "/v1/models"

      # Client-facing model ID prefix configuration
      model-prefix:
        enabled: true           # true -> "opencode-go/<model>", false -> bare "<model>" (default: true)
        value: "opencode-go"    # prefix name (default: "opencode-go")

      # OpenCode Go API keys (at least one required). Supports ${ENV_VAR} expansion.
      api-keys:
        - value: "sk-opencode-key-1"
          name: "Personal"      # optional account label
        - value: "sk-opencode-key-2"
        - value: "${OPENCODE_GO_API_KEY}"

      # Catalog discovery settings
      catalog:
        refresh-interval: "15m"          # discovery refresh cadence, min "1m" (default: "15m")
        stale-while-unavailable: true    # retain last good catalog snapshot on refresh failure (default: true)

      # Per-model policy. One entry per model:
      #   protocols        protocols the upstream speaks natively, in priority
      #                    order — the first one is the default route. A client
      #                    whose protocol appears here is passed through with
      #                    no translation at all.
      #   reasoning-shape  "summary" (default) or "content". Upstreams disagree
      #                    and answer 400/503 for the wrong one; measured:
      #                    deepseek-v4-pro wants content, deepseek-v4.1-flash
      #                    and glm want summary.
      #   endpoints        optional per-protocol path override. Only needed
      #                    when a protocol lives somewhere other than its
      #                    default path.
      models:
        deepseek-v4.1-flash:
          protocols: ["chat-completions", "responses", "messages"]
        deepseek-v4-pro:
          protocols: ["chat-completions", "responses", "messages"]
          reasoning-shape: "content"
        claude-haiku-5-5:
          protocols: ["messages"]
        "custom-proxy-model":
          protocols: ["chat-completions", "responses"]
          endpoints:
            responses: "/openai/v1/responses"   # must start with /

      # Optional family fallback for models not listed above. Omit this key to
      # use the built-in family table; set `fallback-routes: []` to disable
      # guessing entirely.
      fallback-routes:
        - { prefix: "gpt", protocol: "responses" }
        - { prefix: "glm", protocol: "chat-completions" }

      # Legacy shortcut: a single-protocol model can be written as
      #   some-model: { protocols: [messages], endpoint: "/v1/messages" }
      # The old `protocol-preference` and `route-overrides` tables still load
      # and are merged into `models`, so existing configs keep working.

      # Protocol enable/disable switches (all default to true)
      protocols:
        chat-completions: true   # enables models routed to /v1/chat/completions
        messages: true           # enables models routed to /v1/messages
        responses: true          # enables models routed to /v1/responses

      # Execution settings
      request-timeout: "5m"              # upstream request timeout (default: "5m")
      max-response-bytes: 67108864       # max non-streaming response body size in bytes (default: 64 MiB)
      allow-http: false                  # allow http:// scheme for local mock/testing (default: false)
```

### Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(Required)* | List of API keys (`- value: "..."`, optional `name: "Personal"`). Supports `${ENV_VAR}` expansion. Duplicates and empty values are rejected. |
| `base-url` | `string` | `https://opencode.ai/inference/go` | Upstream service root. Every path below appends to it verbatim. Must be valid HTTPS (or HTTP if `allow-http: true`) without query parameters, fragments, or userinfo. |
| `protocol-endpoints` | `map` | `/v1/{chat/completions,responses,messages}` | Path of each protocol, appended to `base-url`. Change these when the upstream moves an endpoint. |
| `quota-path` | `string` | `/v1/usage` | Usage/quota endpoint, relative to `base-url`. Separate from the protocol endpoints. |
| `catalog-path` | `string` | `/v1/models` | Model catalog endpoint, relative to `base-url`. Separate from the protocol endpoints because the upstream serves the model list on its own path. |
| `model-prefix.enabled` | `bool` | `true` | When `true`, client-facing model names use `<prefix>/<model>`. When `false`, uses bare model IDs. |
| `model-prefix.value` | `string` | `opencode-go` | Provider prefix string when prefixing is enabled. |
| `catalog.refresh-interval` | `duration` | `15m` | Interval between catalog polling refreshes (e.g. `15m`, `1h`). Minimum is `1m`. |
| `catalog.stale-while-unavailable` | `bool` | `true` | When `true`, serves the last valid catalog snapshot if an update fails. |
| `protocols.chat-completions` | `bool` | `true` | Protocol switch for Chat Completions endpoints. |
| `protocols.messages` | `bool` | `true` | Protocol switch for Messages endpoints. |
| `protocols.responses` | `bool` | `true` | Protocol switch for Responses endpoints. |
| `models` | `map` | `{}` | Per-model policy: `protocols` (ordered, first = default route), `reasoning-shape` (`summary` \| `content`), `endpoints` (per-protocol path override). A model listed here owns its route outright. |
| `fallback-routes` | `[]object` | *(built-in table)* | Family prefixes for models `models` does not list (`{prefix, protocol}`, first match wins). Omit the key to use the built-in table; `[]` disables guessing. |
| `models.<id>.reasoning-shape` | `string` | `summary` | Wire form of the reasoning item on the Responses route. `deepseek-v4-pro` needs `content`; every other measured model needs `summary` (or answers 503 for `content`). |
| `models.<id>.endpoints` | `map` | `{}` | Optional path override per protocol; only needed when a protocol is not served at its default path. |
| `protocol-preference` | `map` | `{}` | Legacy: protocols a model speaks natively. Merged into `models`. |
| `route-overrides` | `map` | `{}` | Legacy: `{ protocol, endpoint }` per model, where `protocol` also becomes the default route. Merged into `models`. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP request timeout. Must be positive. |
| `max-response-bytes` | `int64` | `67108864` (64 MiB) | Maximum non-streaming response body size in bytes. |
| `allow-http` | `bool` | `false` | When `true`, permits `http://` scheme in `base-url` for local testing. |

## Testing

```powershell
# Run all tests
go test -tags debug ./...

# Run tests with coverage
go test -tags debug ./... -cover

# Run linter / vetting
go vet ./...
```

## Account names and quotas

Set an optional `name` on each `api-keys` entry, alongside `value`.
For example, use `name: Personal` or `name: Work`. Without a name, one key
uses **OpenCode Go**; multiple keys use **OpenCode Go 1**, **OpenCode Go 2**, and subsequent numbers.
Default numbers follow configuration order. Explicit names remain stable when keys are reordered.
The usage response contains no email or account identity. The plugin does not infer an email from a key.

New credential files have readable names. Existing auth IDs remain unchanged, and
legacy generated labels are replaced when parsed. Existing custom labels are preserved
unless configuration specifies a name. Existing filenames are retained automatically;
stop CLIProxyAPI before renaming an old file to a readable `.json` filename, and keep
its JSON `id` and all other fields unchanged. Back up the file first.

Discover support with `GET /v0/management/quota/providers`, then call
`POST /v0/management/quota/fetch` with `{"auth_index":"<selected index>"}`.
These endpoints require the CLIProxyAPI management key. The normalized response contains
`subscription.plan`, `groups[].buckets[].window`, `remainingFraction`, and `resetTime`.
Missing windows are omitted. Invalid readings return an error, not an invented zero.
Quota reset is unsupported. The old plugin quota route and resource page are removed.
