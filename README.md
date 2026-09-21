# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md)

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

Cost and deprecation metadata come from [models.dev](https://models.dev/api.json), refreshed every 24 hours. Metadata requests use a 30-second timeout per HTTP client. Refresh failures retain existing data.

### Anonymous access and fallback

With `anonymous: true`, either condition makes a model eligible for the anonymous Zen lane:

- Its ID contains `free`, case-insensitively.
- models.dev reports zero input and output cost and the model is not deprecated.

Eligibility is a routing decision; the upstream can still reject or rate-limit the request. Anonymous requests use `public` in the upstream authentication header.

The routing sequence is:

1. For an eligible model, try each available anonymous proxy once.
2. Try authenticated tiers in `prefer` order, using only tiers with a configured key and a route for that model.
3. Apply `retry.max_attempts` separately to each authenticated tier.

Anonymous attempts are not cut short by `retry.max_attempts`, but all attempts share the request timeout. Network errors, authentication failures, rate limits, and server errors can rotate keys. Other 4xx responses end the current tier; another available tier may still be tried.

Requests are encoded for each tier's own protocol. Once a stream has started, the gateway does not retry generation on another node. A recognized stale Responses reasoning reference can trigger one repair pass; selected-key diagnostics never use that replay.

When only anonymous access is configured, `/v1/models` exposes only models eligible for that lane.

### Sessions and proxies

Keys are initially spread across proxies. Real traffic can trigger proxy checks, key rebinding, and cooldowns. Unhealthy proxies are rechecked every 15 minutes against Cloudflare trace.

A stable session hash selects the preferred key or anonymous proxy. Send an explicit `x-session-id` to separate independent conversations. The gateway also accepts `x-opencode-session`, `x-session-affinity`, `conversation-id`, `conversation_id`, and `metadata.session_id`.

Without an explicit ID, the first user message is used as a seed. Conversations beginning with the same content can therefore share affinity. Changing pool membership can change the selected node.

Cooldowns grow exponentially up to eight times `performance.failure_cooldown_seconds`, and a longer `Retry-After` is honored. If every authenticated key is cooling down, routing may try the key whose cooldown ends first. Anonymous nodes still in cooldown are skipped.

## Configuration reference

See [config.example.json](config.example.json) for a complete starting configuration. JSON supports `//` and `/* ... */` comments; unknown fields and invalid values are rejected.

### Keys, listeners, and routing

| Field                    | Default / requirement                                                     |
| ------------------------ | ------------------------------------------------------------------------- |
| `listen`                 | `127.0.0.1:8080`.                                                         |
| `server_keys`            | At least one local key is required.                                       |
| `zen_keys`, `go_keys`    | At least one upstream key is required unless anonymous access is enabled. |
| `anonymous`              | `false`.                                                                  |
| `prefer`                 | `go`; accepts `go` or `zen`.                                              |
| `upstream.zen`           | `https://opencode.ai/zen`.                                                |
| `upstream.go`            | `https://opencode.ai/zen/go`.                                             |
| `proxies`                | Falls back to `["direct"]` when both proxy sources are empty.             |
| `proxyfile`              | Optional; relative paths resolve beside the configuration file.           |
| `models.refresh_seconds` | `300`; minimum 1.                                                         |
| `models.protocols`       | `{}`; per-model native protocol overrides.                                |

Proxy entries accept `direct`, `http://`, `https://`, `socks5://`, and `socks5h://`, including URL credentials. The inline list is merged with `proxyfile` and deduplicated in order.

A proxy file contains one entry per line and supports blank lines and comments:

```text
# Primary proxy
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # Backup proxy
direct
```

Comment markers are `#`, `;`, and `//` at the start of a line or after whitespace.

### Timeouts and connection pools

| Field                                   | Default | Meaning                                                |
| --------------------------------------- | ------- | ------------------------------------------------------ |
| `retry.max_attempts`                    | `3`     | Attempts per authenticated tier, including the first.  |
| `retry.timeout_seconds`                 | `300`   | Total inference timeout, including stream consumption. |
| `performance.attempt_timeout_seconds`   | `0`     | Header wait per attempt; 0 uses the request timeout.   |
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

### Saving and reloading

The server validates a candidate configuration and builds its replacement gateway before saving and switching. Failed validation or persistence leaves the active gateway in place. Requests already running continue on their original gateway.

Keys, proxies, upstream URLs, retry settings, model settings, logging, and routing preferences apply to new requests immediately. Changes to `listen`, `webui.listen`, or `webui.enabled` require a process restart. Saved JSON is normalized; comments are not retained.

## Monitoring and persistence

Request outcomes describe the completed inference. A failed SSE response or truncated stream counts as an error even when HTTP 200 was already sent; its outcome is `stream_error`. Client cancellations are recorded as `client_canceled`. The WebUI displays those outcomes alongside the HTTP status.

Upstream **attempt** records measure the HTTP exchange through receipt of response headers. Their success and latency are separate from completion of the full JSON body or stream.

Token statistics use reported usage only. Input tokens include cache reads and writes; cached tokens separately count cache reads. Missing usage is not estimated, and coverage shows how many routed inference requests reported usage.

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
  embed.go               Embeds the three static assets into the executable
  index.html             Page structure
  app.js                 UI behavior
  styles.css             UI styles
```

Suggested reading order:

1. [Command entry](cmd/opencode2api/main.go) → [runtime management](internal/gateway/runtime.go) → [HTTP handlers](internal/gateway/gateway.go).
2. [Upstream attempts](internal/gateway/upstream.go) and [model routing](internal/models/catalog.go) explain how a request reaches a provider.
3. [Request conversion](internal/protocol/request.go), [response conversion](internal/protocol/response.go), and [stream transport](internal/protocol/stream.go) cover protocol behavior.
4. [Management routes](internal/admin/server.go) and [WebUI logic](webui/app.js) cover configuration and diagnostics.

Run Go formatting, analysis, and the command build:

```bash
gofmt -w cmd internal webui
go vet ./...
go build -o opencode2api ./cmd/opencode2api
```

For local development, start the service with `go run ./cmd/opencode2api -config config.json`. Release builds still inject the version with `-ldflags "-X main.version=vX.Y.Z"`.

Node.js is only needed for development formatting and JavaScript syntax checks:

```bash
npm ci --ignore-scripts
npm run format
npm run format:check
npm run check:web
```

`.editorconfig`, `.gitattributes`, Go formatting, and pinned Prettier settings standardize the source. CI runs `go vet` and builds with Go 1.24 and stable Go on Linux/Windows, plus formatting, WebUI syntax, and entrypoint shell syntax checks. Release archives include both READMEs.

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
