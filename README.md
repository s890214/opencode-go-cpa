# OpenCode Go CLIProxyAPI Plugin (opencode-go-cpa)

> Forked from [massiveits/opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) (MIT).
> This fork adds per-request protocol preference (native pass-through) and keeps `apply_patch` on native Responses routes.

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes OpenCode Go as a single provider (`opencode-go`).

The plugin unifies model discovery, protocol translation, and execution across OpenCode Go's upstream endpoints while leveraging CLIProxyAPI's built-in authentication, scheduling, keys rotation, and cooldown management.

## The Problem

OpenCode Go exposes models across multiple API protocols (OpenAI Chat Completions `/v1/chat/completions`, Anthropic Messages `/v1/messages`, OpenAI Responses `/v1/responses`, and Gemini `generateContent` under `/google/v1beta/models`).

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

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-cpa`.
There is exactly **one** place a channel can be declared: the `upstreams` list.
The first entry is the default channel CPA picks when a model name carries no
prefix; nothing is inherited between entries.

> Upgrading from 0.7 and earlier: the top-level `base-url`, `api-keys`,
> `models`, `protocol-endpoints`, `catalog-path`, `quota-path`, `model-prefix`,
> `messages-auth` and `fallback-routes` keys are gone — move each channel's
> values into its `upstreams` entry. `protocols.*`, `route-overrides`,
> `protocol-preference` (`models` covers them) and
> `catalog.stale-while-unavailable` (the last good snapshot is always kept) were
> removed. A config that still uses the old shape fails to load with
> `upstreams: at least one channel is required` instead of silently serving
> nothing.

```yaml
plugins:
  configs:
    opencode-go-cpa:
      # ---- global settings (one per plugin instance) ----
      request-timeout: "5m"            # total upstream request budget
      stream-idle-timeout: "5m"        # max silence between stream chunks
      max-response-bytes: 67108864     # max non-streaming body (64 MiB)
      catalog-refresh-interval: "15m"  # model catalog poll cadence, min "1m"
      # catalog-cache-path: "/var/lib/cliproxyapi/opencode-go/catalog.json"
      # metadata-url: "https://models.opencode.ai/api.json"
      allow-http: false                # http:// upstreams for local testing only

      # ---- channels: one entry per upstream, first = default ----
      upstreams:
        - name: "opencode-go"          # required, unique, model-ID prefix
          base-url: "https://opencode.ai/inference/go"
                                       # omitted -> the default vendor above
          catalog-path: "/v1/models"   # default
          quota-path: "/v1/usage"      # default
          model-prefix:
            enabled: true              # false -> bare model IDs (one channel only)
            value: "opencode-go"       # defaults to `name`
          messages-auth: "x-api-key"   # or "bearer" for a bearer-only gateway
          protocol-endpoints:          # appended to base-url verbatim
            chat-completions: "/openai/v1/chat/completions"
            responses: "/openai/v1/responses"
            messages: "/anthropic/v1/messages"
          api-keys:                    # at least one; ${ENV_VAR} expands
            - value: "${OPENCODE_GO_API_KEY}"
              name: "Personal"         # optional account label
          # Per-model policy: `protocols` is ordered, the first entry is the
          # default route, and a client whose protocol is listed here is passed
          # through with no translation. `reasoning-shape` is "summary"
          # (default) or "content"; `endpoints` pins a path per protocol.
          models:
            deepseek-v4.1-flash:
              protocols: ["chat-completions", "responses", "messages"]
            deepseek-v4-pro:
              protocols: ["chat-completions", "responses", "messages"]
              reasoning-shape: "content"
          # Family fallback for models `models` does not list. Omit the key to
          # use the built-in table; `fallback-routes: []` disables guessing.
          fallback-routes:
            - { prefix: "gpt", protocol: "responses" }
            - { prefix: "glm", protocol: "chat-completions" }
```

### Configuration Options

Global (one per plugin instance):

| Option | Type | Default | Description |
|---|---|---|---|
| `upstreams` | `[]object` | *(Required)* | Every channel, first entry = default. See the per-channel table below. |
| `request-timeout` | `duration` | `5m` | Total upstream HTTP request budget. Must be positive. |
| `stream-idle-timeout` | `duration` | `5m` | Maximum silence between stream chunks before the stream is closed. A long answer that keeps streaming is never cut; only a stalled upstream trips it. |
| `max-response-bytes` | `int64` | `67108864` (64 MiB) | Maximum non-streaming response body size in bytes. |
| `catalog-refresh-interval` | `duration` | `15m` | Model catalog poll cadence per channel. Minimum `1m`. |
| `catalog-cache-path` | `string` | `<user-cache-dir>/opencode-go/catalog.json` | Absolute path of the last-good catalog snapshot (one file per channel). Served immediately at startup, so a catalog outage at boot does not expose an empty model list. |
| `metadata-url` | `string` | `https://models.opencode.ai/api.json` | models.dev-style metadata feed filling limits, modalities and thinking levels the upstream omits. Upstream values win; failures are ignored. |
| `allow-http` | `bool` | `false` | Permits `http://` in `base-url` for local testing. |

Per channel (`upstreams[]`):

| Option | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | *(Required)* | Unique channel name: credential label, model-ID prefix and log tag. |
| `base-url` | `string` | `https://opencode.ai/inference/go` | Service root. Every path below appends verbatim, so a versioned base plus versioned paths yields `/v1/v1/...`. Must be HTTPS (or HTTP with `allow-http`) without query, fragment or userinfo. |
| `api-keys` | `[]object` | *(Required)* | Keys for this channel (`- value: "..."`, optional `name:`). `$ENV_VAR` expands; duplicates and empty values are rejected. |
| `catalog-path` | `string` | `/v1/models` | Model list endpoint, relative to `base-url`. |
| `quota-path` | `string` | `/v1/usage` | Usage/quota endpoint, relative to `base-url`. |
| `protocol-endpoints` | `map` | `/v1/{chat/completions,responses,messages}`, `/google/v1beta/models` | Path of each protocol, appended to `base-url`. |
| `messages-auth` | `string` | `x-api-key` | Header the Messages route authenticates with: `x-api-key` (OpenCode Go) or `bearer` (an Anthropic-wire gateway that rejects `x-api-key`). |
| `model-prefix.enabled` | `bool` | `true` | When `false`, publishes bare model IDs. Only one channel may do this. |
| `model-prefix.value` | `string` | channel `name` | Prefix string when enabled. Must be unique across channels. |
| `models` | `map` | `{}` | Per-model policy: `protocols` (ordered, first = default route), `reasoning-shape` (`summary` \| `content`), `endpoints` (per-protocol path override). |
| `fallback-routes` | `[]object` | *(built-in table)* | Family prefixes for models `models` does not list (`{prefix, protocol}`, first match wins). `[]` disables guessing. |

## Several upstream channels

Each entry is a full channel description — nothing is shared except the global
settings above:

```yaml
upstreams:
  - name: "opencode-go"
    api-keys:
      - value: "${OPENCODE_GO_API_KEY}"
  - name: "od"
    base-url: "https://second-vendor.example"
    catalog-path: "/v1/models"
    protocol-endpoints:
      chat-completions: "/v1/chat/completions"
      responses: "/v1/responses"
      messages: "/v1/messages"
    messages-auth: "bearer"
    api-keys:
      - value: "${SECOND_VENDOR_KEY}"
```

Rules that matter:

- **A channel owns everything it uses.** No key, base URL, path, model policy or
  auth scheme is inherited, so a mistyped entry can never aim one vendor's key
  at another vendor's host.
- **Model IDs are namespaced.** Every channel publishes `<prefix>/<model>`, with
  the prefix defaulting to its `name`. Exactly one channel may set
  `model-prefix.enabled: false` (bare IDs) — two would collide, because the host
  keeps one executor per model ID, and that is rejected at load.
- **Routing follows the configured key→channel mapping**, not a field stored in
  the credential file: moving a key between channels re-routes on the next
  reload without touching auth files.
- **Each credential reports its own quota**, resolved through its channel's
  `base-url` + `quota-path`. A channel whose upstream has no usage endpoint
  fails only its own row.
- **Each channel keeps its own catalog snapshot** (`catalog.json` for the first,
  `catalog-<name>.json` for the rest) and refresh state. A model that neither
  `models` nor `fallback-routes` can route stays excluded — list it under that
  channel's `models` when its ID matches no family prefix.

## Testing

```powershell
# Run all tests
go test -tags debug ./...

# Run tests with coverage
go test -tags debug ./... -cover

# Optional: hit the real upstream through the Gemini adapter (needs a key)
OPENCODE_LIVE=1 OPENCODE_GO_API_KEY=... go test ./internal/adapter/gemini -run TestLive -v

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

## Credential lifecycle

Credentials are API keys only: `api-keys` is the single source of truth and
there is no browser login. The Console device-code flow that 0.6.0 shipped was
removed in 0.6.1 — `auth.login.start` / `auth.login.poll` answer with a
descriptive `auth_failure`, and `auth.refresh` returns API-key records
untouched because a key never expires. A record without an `api_key` is
rejected at parse time instead of being accepted as an OAuth credential.

Credential files mirror `api-keys`:
a configured key without a record is created, and a record whose key was
removed from the config is deleted. Deletion is limited to files this plugin
created — the file name must be `opencode-go-key-<sha256(key)>.json` **and** the
`api_key` inside must hash to that name. Hand-written
files are never deleted automatically, and a record disabled while its key is
still configured is re-enabled. Removal stages the file under a name the auth
watcher ignores and then unlinks it, so a partially failed removal cannot leave
a loadable credential behind.
