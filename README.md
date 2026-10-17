# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md)

A Go gateway for **OpenCode Zen and Zen Go**. It exposes Chat Completions, Responses, and Anthropic Messages endpoints, translates between their native protocols, and manages upstream keys and proxies.

The executable includes the WebUI. Running the service requires no Node.js runtime or database.

## Capabilities

- JSON and SSE responses across all three inference protocols.
- Text, images, reasoning, function tools, tool calls, and tool results; file content where the target protocol can represent it.
- Separate Zen and Go key pools, configurable tier preference, retries, and session affinity.
- Optional anonymous Zen access with the OpenCode `public` credential.
- Direct, HTTP, HTTPS, SOCKS5, and SOCKS5H connections, including a proxy file.
- Dynamic model discovery, native protocol metadata, and disk caches.
- A separate management port with configuration editing, a Playground, diagnostics, token statistics, and live logs.
- Configuration hot reload with validation before switching new requests to a replacement gateway.
- Go subscription quota dashboard with remaining percentages, reset times, and available models.
- Local API key creation, naming, enabling, disabling, and deletion with immediate updates.
- Sticky `sota` / `sweet` model groups with ordering, failure fallback, streaming protection, and persisted state.

See the [management console guide (Chinese)](docs/management.zh-CN.md) for the new pages and configuration.

## Quick start

Download a binary from [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases), or build with **Go 1.24 or newer**:

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

Before starting, edit `config.json`:

1. Replace `server_keys` with your own local API key.
2. Supply Zen or Go keys. Alternatively, set `anonymous: true` and empty both upstream key arrays.
3. Replace `webui.password`. The example enables the WebUI with username `admin`.

```bash
./opencode2api -config config.json
```

On Windows, use `Copy-Item config.example.json config.json`, build with `go build -o opencode2api.exe ./cmd/opencode2api`, then run `.\opencode2api.exe -config config.json`.

The example listens on `127.0.0.1:8080` for the API and `0.0.0.0:8081` for the WebUI. Open `http://localhost:8081` locally. Restrict management access and use an HTTPS reverse proxy when accessing it over a network.

Command-line options:

| Option        | Default       | Purpose                            |
| ------------- | ------------- | ---------------------------------- |
| `-config`     | `config.json` | Configuration file path.           |
| `-listen`     | Unset         | Override the API listen address.   |
| `-web-listen` | Unset         | Override the WebUI listen address. |

The configuration directory must be writable for password migration, configuration saves, and model caches.

## Docker

The published image is `ghcr.io/jasonxu114514/opencode2api`.

```bash
cp config.example.json config.json
# Configure keys and replace the WebUI password before starting.
docker compose up -d
docker compose logs -f
```

Compose imports the host configuration into the `opencode2api-state` volume **only on first startup**. Subsequent changes should be made through the WebUI. To import an edited host configuration again:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

The container runs the service as an unprivileged user. Compose uses a read-only root filesystem, a writable state volume, and a temporary `/tmp` filesystem.

| Compose variable            | Default  | Effect                                            |
| --------------------------- | -------- | ------------------------------------------------- |
| `OPENCODE2API_VERSION`      | `latest` | Image tag. Pin a release tag for a fixed version. |
| `OPENCODE2API_PORT`         | `8080`   | Host port mapped to container port 8080.          |
| `OPENCODE2API_WEBUI_PORT`   | `8081`   | Host port mapped to container port 8081.          |
| `OPENCODE2API_LISTEN`       | Unset    | Explicit container API listen override.           |
| `OPENCODE2API_WEBUI_LISTEN` | Unset    | Explicit container WebUI listen override.         |

The last two variables become `LISTEN_ADDRESS` and `WEBUI_LISTEN_ADDRESS` inside the container. Empty values defer to the configuration file. While initializing a configuration, the entrypoint changes the example API address `127.0.0.1:8080` to `0.0.0.0:8080` so published ports can reach it.

Changing host ports does not change container listeners. If you change the internal API port, also update the port mapping and the image health check, which uses port 8080.

For a local image:

```bash
docker build -t opencode2api:local .
docker volume create opencode2api-state
docker run -d --name opencode2api \
  -p 8080:8080 -p 8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

A `proxyfile` used in Docker must also be available inside the container at the configured path.

## API usage

`server_keys` authenticate clients to this gateway. They are separate from `zen_keys` and `go_keys` and are never used as upstream credentials.

Send `Authorization: Bearer YOUR_LOCAL_API_KEY` or `x-api-key: YOUR_LOCAL_API_KEY`. Health checks require no authentication.

| Method | Path                   | Description                                               |
| ------ | ---------------------- | --------------------------------------------------------- |
| GET    | `/v1/models`           | Models that can be routed with the current configuration. |
| POST   | `/v1/chat/completions` | Chat Completions.                                         |
| POST   | `/v1/responses`        | Responses.                                                |
| POST   | `/v1/messages`         | Anthropic Messages.                                       |
| POST   | `/v1/systemone`        | System One structured decisions.                          |
| GET    | `/healthz`             | Readiness and resource summary.                           |

Discover an available model first:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"
```

Replace `MODEL_ID` below with an ID from that response.

**Chat Completions**

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}'
```

**Responses**

```bash
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","input":"Hello"}'
```

**Anthropic Messages**

```bash
curl http://localhost:8080/v1/messages \
  -H "x-api-key: YOUR_LOCAL_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}'
```

For streaming, add `"stream": true` to the body and use `curl -N`. Responses include an `x-request-id` for correlation. API request bodies are limited to 32 MiB.

**System One**

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

System One pairs a free-form state with typed questions and returns typed answers, so the payload shares no shape with the chat/responses bridge and is relayed verbatim, never translated. Only models whose native protocol is System One are served here; a decision payload submitted on a message endpoint (`/v1/chat/completions`, `/v1/responses`) is likewise relayed verbatim instead of converted. Streaming upstream replies are relayed as-is without reset recovery.

### Compatibility boundaries

Requests using the upstream's native protocol retain provider-specific fields. Requests crossing protocols pass through a common representation; not every option has an equivalent. For example, Chat JSON output constraints and `seed` are not forwarded to Anthropic Messages. Unsupported content or tool types may be rejected.

The gateway exposes the routes listed above. It does not implement embeddings, file uploads, image generation, or Responses retrieval and cancellation. It does not store conversation history; clients must supply the history or use features supported by the actual upstream.

## Routing and models

### Model discovery

The gateway refreshes Zen/Go `/v1/models` and OpenCode's [capability catalog](https://models.opencode.ai/api.json) on the configured interval. Native protocols and model limits are tracked separately for each tier. OpenCode's Zen/Go documentation is a fallback for protocol discovery.

`models.protocols` overrides discovery:

```json
{
  "models": {
    "refresh_seconds": 300,
    "protocols": {
      "custom-model": "chat"
    }
  }
}
```

Allowed protocol values are `chat`, `responses`, and `anthropic`. Models using unsupported native protocols are filtered from discovery unless overridden.

Cost, deprecation, and model limits come from the official [OpenCode metadata endpoint](https://models.opencode.ai/api.json), refreshed every 24 hours. OpenCode's capability catalog remains authoritative for context and token limits; metadata `limit.context`, `limit.input`, and `limit.output` fill fields missing from that catalog. These limits are exposed by `/v1/models` as `context_window`/`context_length`, `max_input`, and `max_output` for discovery clients such as CLIProxyAPI and Pi. Metadata requests use a 30-second timeout per HTTP client. Refresh failures retain existing data.

Each `/v1/models` entry also carries `cost` (USD per million tokens: `input`, `output`, `cache_read`, `cache_write`, plus OpenClaw's `cacheRead`/`cacheWrite`) when the metadata states a price. Reasoning models list their levels in `reasoning_efforts` and in OpenClaw's `compat.supportedReasoningEfforts`: `minimal` through `max`, capped at the probed ceiling when the model's effort is `auto`. A forced or probed level is reported as `default_reasoning_effort`. Cache prices appear after the next metadata refresh.

### Anonymous access and fallback

With `anonymous: true`, either condition makes a model eligible for the anonymous Zen lane:

- Its ID contains `free`, case-insensitively.
- OpenCode metadata reports zero input and output cost and the model is not deprecated.

Eligibility is a routing decision; the upstream can still reject or rate-limit the request. Anonymous requests use `public` in the upstream authentication header.

The routing sequence is:

1. For an eligible model, try each available anonymous proxy once.
2. Try authenticated tiers in `prefer` order, using only tiers with a configured key and a route for that model.
3. Apply `retry.max_attempts` separately to each authenticated tier.

Anonymous attempts are not cut short by `retry.max_attempts`, but all attempts share the request timeout. Network errors, authentication failures, rate limits, and server errors can rotate keys. Other 4xx responses end the current tier; another available tier may still be tried.

Requests are encoded for each tier's own protocol. With `performance.first_event_timeout_seconds` enabled, a successful SSE response is held until the first complete data event. A timeout or disconnect before that event uses the existing key/proxy retry budget; comments do not satisfy the timeout. Once released downstream, the stream is never replayed. The default is `0` (disabled); try `15` seconds for upstreams that accept requests but send no events. This bounds the first event, not the first text token, and does not apply to System One. Rotation aliases use their separate `rotation.first_output_seconds` timeout. A recognized stale Responses reasoning reference can trigger one repair pass; selected-key diagnostics never use that replay.

When only anonymous access is configured, `/v1/models` exposes only models eligible for that lane.

Free models never need a Zen key: with `anonymous: true` they are served by the public lane, and a configured key is only their fallback. To keep keys but expose free models only, set `models.free_only: true`. Paid models then disappear from `/v1/models`, direct requests for them are rejected, and rotation aliases keep only free members; free models may still fall back to the configured keys.

### Free model availability

The gateway probes models whose IDs contain `free` (case-insensitive), or whose metadata reports zero input/output cost without deprecation, with a short request using its native protocol. It tries the enabled anonymous lane first and then one configured Zen Key. A valid completed response on either lane keeps the model enabled; the model is disabled in discovery and routing only when every attempted lane explicitly reports that the model does not exist or has been retired. Authentication errors, rate limits, timeouts, transport errors, 5xx responses, and malformed replies are inconclusive: they do not create a new disable, and are checked again in one hour. Probe traffic does not alter production key/proxy cooldowns or request statistics. It still reaches the upstream and may consume its free allowance.

System One models use a native `noul` question asking whether 1 + 1 equals 2; they are never sent chat/tool payloads. Model discovery includes `native_protocol` and `supported_endpoints`; rotation aliases are listed only when Go keys and eligible models exist. Legacy v1.3.6 disables are cleared on load and scheduled for immediate recheck because those probes used invalid session IDs.

The intervals are fixed: enabled models are checked every **1 hour**, models confirmed unavailable every **24 hours**. An inconclusive due recheck of a disabled model preserves its original disable reason and schedules another retry in 24 hours. A successful recheck restores the model automatically. The WebUI **Free model availability** page can restore it immediately, which puts the model under manual control: it stays enabled and is excluded from automatic probing, so later probe results cannot overwrite it. Use the per-model **manual/automatic toggle** on the same page to return it to automatic probing; the next scheduler pass then schedules a probe. Records left as `manually_enabled` by older versions migrate to manual control on load. An in-flight probe cannot undo a manual restore. Models without a usable Zen/anonymous lane, or without a supported inference protocol, are skipped.

Manual control also pauses effort discovery for `reasoning.effort=auto`; learned levels stop being forced after two hours without verification. Returning to automatic control allows effort discovery to resume.

State survives restarts in `config.json.<upstream-fingerprint>.availability.json`, separated by the Zen upstream URL. The background scheduler checks for due models once per minute, skips manually controlled models, and probes the rest sequentially, with a 60-second timeout per lane. No channels available means no automatic disable.

Administration endpoints (login required; writes also require CSRF): `GET /api/models/availability`, `POST /api/models/restore` with `{"model":"MODEL_ID"}` (restores and takes manual control), and `POST /api/models/manual` with `{"model":"MODEL_ID","manual":true|false}` to switch between manual and automatic control.

### Sessions and proxies

Keys are initially spread across proxies. Real traffic can trigger proxy checks, key rebinding, and cooldowns. Unhealthy proxies are rechecked every 15 minutes against Cloudflare trace.

A stable session hash selects the preferred key or anonymous proxy. Send an explicit `x-session-id` to separate independent conversations. The gateway also accepts `x-opencode-session`, `x-session-affinity`, `conversation-id`, `conversation_id`, and `metadata.session_id`.

Without an explicit ID, the first user message is used as a seed. Conversations beginning with the same content can therefore share affinity. Changing pool membership can change the selected node.

Cooldowns grow exponentially up to eight times `performance.failure_cooldown_seconds`, and a longer `Retry-After` is honored. If every authenticated key is cooling down, routing may try the key whose cooldown ends first. Anonymous nodes still in cooldown are skipped.

## Configuration reference

See [config.example.json](config.example.json) for a complete starting configuration. JSON supports `//` and `/* ... */` comments; unknown fields and invalid values are rejected.

### Keys, listeners, and routing

| Field                       | Default / requirement                                                       |
| --------------------------- | --------------------------------------------------------------------------- |
| `listen`                    | `127.0.0.1:8080`.                                                           |
| `server_keys`               | At least one local key is required.                                         |
| `zen_keys`, `go_keys`       | At least one upstream key is required unless anonymous access is enabled.   |
| `anonymous`                 | `false`.                                                                    |
| `prefer`                    | `go`; accepts `go` or `zen`.                                                |
| `upstream.zen`              | `https://opencode.ai/inference/openai`.                                                  |
| `upstream.go`               | `https://opencode.ai/zen/go`.                                               |
| `proxies`                   | Falls back to `["direct"]` when both proxy sources are empty.               |
| `proxyfile`                 | Optional; relative paths resolve beside the configuration file.             |
| `models.refresh_seconds`    | `300`; minimum 1.                                                           |
| `models.protocols`          | `{}`; per-model native protocol overrides.                                  |
| `models.free_only`          | `false`; expose and route only free models, even with Zen or Go keys.       |
| `reasoning.effort`          | Empty (off); `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, or `none`. |
| `reasoning.effort_by_model` | `{}`; per-model overrides of `reasoning.effort`.                            |

### Forced thinking level

`reasoning.effort` sets the thinking level used upstream when a request does not
state one of its own, and `reasoning.effort_by_model` overrides it for the model
IDs it names. It applies to all three protocols, in either direction of
conversion, and matters most for cross-protocol traffic: an Anthropic
`thinking.budget_tokens` can only be approximated by the Chat and Responses
surfaces, so a forced level reaches a rung that the budget alone cannot express.

```json
{
  "reasoning": {
    "effort": "high",
    "effort_by_model": { "claude-opus-5": "max" }
  }
}
```

A level a client states explicitly always wins, so this only replaces a level
nobody asked for: an absent one, or one derived from `thinking.budget_tokens`.
Use `"none"` to remove reasoning from requests that do not ask for it.

Levels are reported faithfully in both directions. An explicit
`output_config.effort` survives conversion instead of being shadowed by a
`thinking` block, `budget_tokens` keeps its rung (`8192` is `high`, `32000` is
`xhigh`, so the two stay distinguishable), and the Responses target carries the
level as `reasoning.effort` rather than dropping it.

The WebUI Configuration Center exposes the same default and per-model fields.
The management API returns them as `reasoning` from `GET /api/config` and accepts
the object in `PUT /api/config`; clients that omit the field keep the existing
value, while sending an empty object clears it.

Proxy entries accept `direct`, `http://`, `https://`, `socks5://`, and `socks5h://`, including URL credentials. The inline list is merged with `proxyfile` and deduplicated in order.

A proxy file contains one entry per line and supports blank lines and comments:

```text
# Primary proxy
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # Backup proxy
direct
```

Comment markers are `#`, `;`, and `//` at the start of a line or after whitespace.

#### Rotating proxy pools

Suffix an entry with `#pool` (or prefix it with `pool+`) to declare that the endpoint is a rotating proxy pool such as [Resin](https://github.com/Resinat/Resin), where every new TCP connection exits from a different IP:

```text
http://127.0.0.1:7890#pool
socks5://127.0.0.1:1080#pool
pool+http://127.0.0.1:7891
```

A pool entry is handled differently from a static proxy:

| Behavior                  | Static proxy                                                        | `#pool` entry                                                                |
| ------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| Connection reuse          | Keep-alive plus HTTP/2 multiplexing (`performance.*` pool settings) | A fresh TCP + CONNECT tunnel per request (`DisableKeepAlives`, no HTTP/2)    |
| Failure cooldown          | 15s–120s exponential backoff, inflated by `Retry-After`             | None: the next request simply draws a new exit IP                             |
| Health eviction           | Marked unhealthy, keys rebound, rechecked every 15 minutes          | Never evicted; the health check is skipped entirely                           |
| Anonymous lane retries    | At most one attempt per configured proxy                            | Up to `retry.max_attempts`, cycling the pool entry                            |

Use it when the endpoint behind the URL already performs the rotation and the pool's own exit IPs are individually rate-limited or flaky; the gateway then treats each attempt as a fresh endpoint instead of pinning the conversation to one exit. A single `#pool` entry is enough — you no longer need to list dozens of exits. Static entries keep the pooling behavior described below.

### Timeouts and connection pools

| Field                                   | Default | Meaning                                                |
| --------------------------------------- | ------- | ------------------------------------------------------ |
| `retry.max_attempts`                    | `3`     | Attempts per authenticated tier, including the first.  |
| `retry.timeout_seconds`                 | `300`   | Total inference timeout, including stream consumption. |
| `performance.attempt_timeout_seconds`   | `0`     | Whole-attempt bound; 0 uses the request timeout.       |
| `performance.connect_timeout_seconds`   | `5`     | Connection establishment timeout.                      |
| `performance.failure_cooldown_seconds`  | `15`    | Base failure cooldown.                                 |
| `performance.max_idle_conns`            | `2048`  | Idle connection limit per proxy transport.             |
| `performance.max_idle_conns_per_host`   | `256`   | Idle connection limit per host and transport.          |
| `performance.max_conns_per_host`        | `0`     | Connection limit per host; 0 is unlimited.             |
| `performance.idle_conn_timeout_seconds` | `120`   | Idle connection lifetime.                              |

The per-attempt header timeout is capped by the request timeout. Set it below the total timeout if a slow upstream should leave time for fallback. Expired request contexts stop further attempts without penalizing unused keys or proxies.

### Logging and management

| Field                         | Default / requirement                                         |
| ----------------------------- | ------------------------------------------------------------- |
| `logging.level`               | `info`; accepts `debug`, `info`, `warn`, `error`.             |
| `logging.ring_size`           | `2000`; range 100–50,000.                                     |
| `logging.dump_request_bodies` | `false`. Requires `debug` logging to emit outbound bodies.    |
| `webui.enabled`               | `false` when omitted; the example sets it to `true`.          |
| `webui.listen`                | `0.0.0.0:8081`.                                               |
| `webui.username`              | Required when the WebUI is enabled; the example uses `admin`. |
| `webui.password`              | Bootstrap password with a minimum length of 10.               |
| `webui.password_hash`         | Generated Argon2id hash; replaces the plaintext password.     |
| `webui.session_ttl_minutes`   | `720`; range 5–10,080.                                        |

On startup, a bootstrap password is hashed and removed from the configuration. The backup containing that bootstrap plaintext is deleted. Keep configuration files and backups private: upstream keys and proxy credentials remain necessary configuration secrets.

Body dumps are opt-in and capped at 64 KiB per prepared body. Configured secrets are redacted, but conversation content may still be sensitive.

## WebUI and diagnostics

The management listener serves the UI and `/api/*` routes. Authentication uses one administrator account, server-side sessions, HttpOnly/SameSite cookies, CSRF checks for writes, and login throttling.

| Management route                                 | Purpose                                                               |
| ------------------------------------------------ | --------------------------------------------------------------------- |
| `POST /api/auth/login`                           | Log in and obtain a session cookie and CSRF token.                    |
| `GET /api/auth/session`, `POST /api/auth/logout` | Inspect or end the session.                                           |
| `GET /api/config`, `PUT /api/config`             | Read masked configuration or apply changes.                           |
| `POST /api/config/reload`                        | Reload the file from disk.                                            |
| `POST /api/config/reveal`                        | Reveal configured secrets after password verification.                |
| `PUT /api/account`                               | Update credentials after password verification; invalidates sessions. |
| `GET /api/monitor`                               | Request, token, upstream, and resource statistics.                    |
| `GET /api/debug/models`                          | Model routes, key fingerprints, and metadata diagnostics.             |
| `POST /api/debug/inference`                      | Run a Playground request.                                             |
| `GET /api/logs`, `GET /api/logs/stream`          | Recent logs or a live SSE subscription.                               |

Playground requests require a session and `X-CSRF-Token` and are limited to 12 per minute per client IP:

```json
{
  "protocol": "chat",
  "key": { "mode": "auto" },
  "request": {
    "model": "MODEL_ID",
    "messages": [{ "role": "user", "content": "Hello" }]
  }
}
```

The server forces `stream: false`. Use `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}` to test a specific key from `/api/debug/models`. A selected request makes at most one upstream attempt, without anonymous access, key rotation, tier fallback, or reasoning replay.

**Both automatic and selected diagnostics preserve production key cooldowns, failure counts, proxy health, and bindings.** They still make real upstream requests, can consume provider quota, and appear in monitoring.

After execution, the management endpoint returns HTTP 200 with the actual result in `ok`, `http_status`, `request_id`, `route`, and `response`. Selected-key results also include `selected_key` and `key_test`: `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error`, or `unavailable`. Invalid management input, authentication, CSRF, and throttling retain their own HTTP error statuses.

### Interface language

The WebUI loads its translations from `webui/js/i18n.js`. It ships with **简体中文** (default), **English**, and **Tiếng Việt**. The original Simplified Chinese copy lives in `index.html` and `webui/js/` and stays the fallback for every string without a translation, so adding a language never rewrites the original.

- The selector sits in the topbar, and on the login card.
- The choice is stored in `localStorage` under `opencode2api.ui.lang`.
- Switching re-renders both static text (`data-i18n`, `data-i18n-attr`) and JavaScript-generated text without reloading the page.
- Dates and times follow the selected locale as well.

To add a language, add one block to `DICTIONARY` in `webui/js/i18n.js`, then register its code in `SUPPORTED`, `LABELS`, and `DATE_LOCALES`.

### Saving and reloading

The server validates a candidate configuration and builds its replacement gateway before saving and switching. Failed validation or persistence leaves the active gateway in place. Requests already running continue on their original gateway.

Keys, proxies, upstream URLs, retry settings, model settings, logging, and routing preferences apply to new requests immediately. Changes to `listen`, `webui.listen`, or `webui.enabled` require a process restart. Saved JSON is normalized; comments are not retained.

## Monitoring and persistence

Request outcomes describe the completed inference. A failed SSE response or truncated stream counts as an error even when HTTP 200 was already sent; its outcome is `stream_error`. Client cancellations are recorded as `client_canceled`. The WebUI displays those outcomes alongside the HTTP status.

Upstream **attempt** records measure the HTTP exchange through receipt of response headers. Their success and latency are separate from completion of the full JSON body or stream.

Token statistics use reported usage only. Input tokens include cache reads and writes; cached tokens separately count cache reads. Missing usage is not estimated, and coverage shows how many routed inference requests reported usage. `usage.server_keys` splits requests and tokens by the local server key the caller authenticated with (its fingerprint, as shown in `/api/api-keys`); Playground traffic is counted under `playground`.

| Data                                                      | Storage / retention                                                                          |
| --------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| Sessions, metrics, token totals, latest Playground result | Process memory; cleared on restart.                                                          |
| Request and attempt details                               | Recent hour, capped at 10,000 requests and 20,000 attempts; API returns at most 500 of each. |
| Live log ring                                             | Process memory; size set by `logging.ring_size`.                                             |
| Structured logs                                           | JSON on stdout; collect externally for durable history.                                      |
| Configuration and previous version                        | `config.json` and `config.json.bak`.                                                         |
| Model directory cache                                     | `config.json.models.catalog.json`.                                                           |
| Price/deprecation cache                                   | `config.json.models.dev.json`.                                                               |

Cache filenames are based on the actual configuration path. Lifetime counters mean the current process lifetime. Instances do not share sessions or monitoring state.

### Health checks

`/healthz` does not increment monitoring counters or trigger network requests. It reports resource counts without keys or proxy addresses.

- HTTP 503: initial catalog pending, no models routable with the current configuration, or no healthy proxies.
- HTTP 200: ready, including when a usable catalog cache is stale. In that case the model status is `stale` and overall status is `degraded`.

The staleness threshold is twice `models.refresh_seconds`, with a minimum of 60 seconds. Readiness is a discovery and resource check; it does not verify that an upstream key will accept the next inference.

## Development

Go source is organized into packages by responsibility. The command wires the service together; implementation packages live under `internal/`.

```text
cmd/
  opencode2api/main.go    CLI flags, startup, and graceful shutdown
internal/
  admin/                 Management API, login sessions, and Playground
  buildinfo/             Version shared by health and management endpoints
  config/                Configuration, persistence, passwords, and redaction
  gateway/               HTTP routing, retries, pools, refresh, and runtime
  httpx/                 Shared HTTP responses, body handling, and headers
  identity/              Request IDs and session affinity
  jsonutil/              JSON access and decoding helpers
  models/                Model catalog, capabilities, pricing, and caches
  protocol/              Request/response conversion and SSE parsing/output
  telemetry/             Request tracking, metrics, logs, and recovery
webui/
  embed.go               Embeds the static assets into the executable
  index.html             Page structure
  css/                   Design tokens, base styles, components and page layouts
  js/                    ES modules: shell, i18n, shared helpers and one module per page
```

Suggested reading order:

1. [Command entry](cmd/opencode2api/main.go) → [runtime management](internal/gateway/runtime.go) → [HTTP handlers](internal/gateway/gateway.go).
2. [Upstream attempts](internal/gateway/upstream.go) and [model routing](internal/models/catalog.go) explain how a request reaches a provider.
3. [Request conversion](internal/protocol/request.go), [response conversion](internal/protocol/response.go), and [stream transport](internal/protocol/stream.go) cover protocol behavior.
4. [Management routes](internal/admin/server.go) and [WebUI logic](webui/js/main.js) cover configuration and diagnostics.

Run Go formatting, analysis, and the command build:

```bash
gofmt -w cmd internal webui
go vet ./...
go build -o opencode2api ./cmd/opencode2api
```

For local development, start the service with `go run ./cmd/opencode2api -config config.json`. Release builds still inject the version with `-ldflags "-X main.version=vX.Y.Z"`.

Node.js is only needed for the WebUI JavaScript syntax check:

```bash
for f in webui/js/*.js webui/js/pages/*.js; do node --input-type=module --check < "$f"; done
```

`.editorconfig`, `.gitattributes`, and Go formatting standardize the source. CI runs `go vet` and builds with Go 1.24 and stable Go on Linux/Windows, plus Go formatting, WebUI syntax, and entrypoint shell syntax checks. Release archives include all three READMEs.

## Troubleshooting

| Symptom                              | Check                                                                                     |
| ------------------------------------ | ----------------------------------------------------------------------------------------- |
| API returns 401                      | Use a configured local `server_keys` value.                                               |
| Health remains `starting`            | Inspect catalog refresh logs and outbound connectivity; add protocol overrides if needed. |
| No models are exposed                | Check configured tiers, anonymous eligibility, and native protocol support.               |
| Requests return 502/504              | Inspect upstream attempts, credentials, proxies, and total/per-attempt timeouts.          |
| HTTP 200 but generation failed       | Inspect the SSE error event and request outcome, not only HTTP status.                    |
| Host edits have no effect in Docker  | The active file is in the state volume; import it again or use the WebUI.                 |
| Container ports are unreachable      | Check listener addresses, published ports, and the active volume configuration.           |
| Monitoring disappeared after restart | Monitoring is stored only in memory; collect stdout logs externally.                      |

## Acknowledgements

Thanks to the [LINUX DO](https://linux.do) community for its support.

### Adaptive reasoning effort

Set `reasoning.effort` (or a per-model override) to `auto` to use the highest effort successfully validated by the free-model probe. Existing concrete levels retain their meaning and client-specified effort takes precedence. No model-name allowlist is maintained.

After a baseline succeeds, reasoning-capable models are probed in `max`, `xhigh`, `high`, `medium`, `low`, `minimal` order. Only an explicit reasoning-parameter rejection advances to a lower level. Transient failures stop the search and cannot be interpreted as a lower capability ceiling. If every level is rejected, no reasoning override is injected.

Observations are persisted by upstream URL, model and native protocol, displayed in the availability page, and used for at most two hours. Active probes cover only free Zen/anonymous models; Go/paid models, unknown capabilities and unverified levels receive no automatic override. Setting `auto` does not expand the probe scope or guarantee that upstreams enforce every accepted field. Probing may require several small requests per model and uses upstream allowance.

## License

SPDX-License-Identifier: GPL-3.0-or-later

This project is free software: you may redistribute and modify it under the GNU General Public License as published by the Free Software Foundation, either version 3 or (at your option) any later version. It is distributed without any warranty. See [LICENSE](LICENSE) for the full terms.
