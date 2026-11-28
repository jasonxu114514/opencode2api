# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md)

A Go gateway for OpenCode Zen and Zen Go. Clients use OpenAI Chat Completions, OpenAI Responses, or Anthropic Messages; the gateway selects an available upstream and converts the request and response when the native protocol differs. System One decisions have a separate pass-through endpoint.

The executable embeds the management WebUI. Running it requires neither Node.js nor a database. Model discovery, inference, OAuth, and optional WARP still require access to their upstream services.

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
2. Supply a Zen API Key in `zen_keys` (recommended), or a Go key in `go_keys`. The example leaves both arrays empty and disables anonymous access, so configure an upstream credential before starting. An already saved Go OAuth (`go_oauth`) or Zen OAuth (`zen_oauth`) connection also satisfies this requirement; starting from empty credentials solely to open the OAuth WebUI is not supported.
3. Replace `webui.password`. The example enables the WebUI with username `admin`.

**Anonymous traffic may be heavily blocked upstream. Prefer a Zen API Key.** Anonymous access is off by default in the configuration and WebUI; existing configurations that explicitly set `anonymous: true` keep that choice. Enable it deliberately only if you accept that free requests may still fail.

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

## Docker and Compose

The published image is `ghcr.io/jasonxu114514/opencode2api`. Prepare `config.json` as above before starting. For published container ports, keep the internal API/WebUI listeners on `0.0.0.0:8080` and `0.0.0.0:8081`; restrict access using host-side port bindings and HTTPS where needed.

```bash
docker compose up -d
docker compose logs -f
```

Compose seeds `opencode2api-state` from the host configuration **only on first startup**. Later, the active configuration lives in that volume; use the WebUI for changes. To import an edited host file again:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Back up the active configuration and state before replacing them. The entrypoint runs the service as an unprivileged user and changes the exact example API listener `127.0.0.1:8080` to `0.0.0.0:8080` only while seeding. Compose uses a read-only root filesystem, a writable state volume, and a `/tmp` tmpfs. Default published ports bind on all host interfaces; edit `ports` to use `127.0.0.1:8080:8080` and `127.0.0.1:8081:8081` for local access only.

| Compose variable | Default | Effect |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | Image tag; pin a release tag for a fixed version. |
| `OPENCODE2API_PORT` | `8080` | Published host API port. |
| `OPENCODE2API_WEBUI_PORT` | `8081` | Published host management port. |
| `OPENCODE2API_LISTEN` | Unset | Explicit container API listener override. |
| `OPENCODE2API_WEBUI_LISTEN` | Unset | Explicit container management listener override. |

The last two become `LISTEN_ADDRESS` and `WEBUI_LISTEN_ADDRESS` in the container. Empty values defer to the active config. Changing host ports does not change internal listeners. If the internal API port changes, update both the port mapping and image health check, which uses 8080. A `proxyfile` must be mounted at its configured container path.

Build and run a local image if needed:

```bash
docker build -t opencode2api:local .
docker volume create opencode2api-state
docker run -d --name opencode2api \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$PWD/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

WARP needs outbound UDP to Cloudflare port `2408`; the image does not need host networking or additional network capabilities. A network that blocks this traffic prevents WARP initialization.

## Call the API

The following are the existing compatibility routes. Set an OpenAI-compatible client's base URL to `http://127.0.0.1:8080/v1`; management uses its separate listener.

| Method | Path | Behavior |
| --- | --- | --- |
| `GET` | `/v1/models` | Models routable with the current configuration, including eligible rotation aliases. |
| `POST` | `/v1/chat/completions` | Chat Completions, JSON or SSE. |
| `POST` | `/v1/responses` | Responses, JSON or SSE. |
| `POST` | `/v1/messages` | Anthropic Messages, JSON or SSE. |
| `POST` | `/v1/systemone` | Native System One decisions, without protocol translation. |
| `GET` | `/healthz` | Unauthenticated readiness and resource summary. |

All `/v1/*` routes require an enabled `server_keys` value in `Authorization: Bearer ...` or `x-api-key`. Requests are limited to 32 MiB. Inference responses carry `x-request-id` for log correlation.

Discover the actual model IDs before making a request:

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}'
```

Use these standard JSON bodies with the corresponding endpoint:

```json
{"model":"MODEL_ID","input":"Hello"}
```

```json
{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}
```

The first body is for Responses, the second for Anthropic Messages. Anthropic clients may also send `anthropic-version: 2023-06-01`. For SSE, add `"stream": true` and use `curl -N` or another client that consumes events as they arrive. A stream can fail after HTTP 200; inspect its error events and final outcome.

System One requires a model whose `native_protocol` is `systemone`, with a decision body such as:

```json
{"model":"SYSTEM_ONE_MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}
```

It forwards the decision body and answer without conversion. The message endpoints also recognize this decision shape for a System One model; ordinary chat messages cannot be converted into decisions. An upstream System One stream is relayed without stream-reset recovery.

### Compatibility limits

The three message protocols support text, representable image/file content, reasoning, function definitions, tool calls, tool results, and streaming. Native requests preserve provider-specific fields where the gateway does not deliberately shape them. Cross-protocol conversion only carries fields with a supported equivalent; for example, Chat `seed` and JSON output constraints are not translated into Anthropic options. Unsupported content or tools can produce a validation error.

Responses assistant reasoning, text, and function-call items are grouped into assistant turns when converted. User messages and tool results separate turns; incomplete or unmatched tool results remain errors when converting to Chat Completions.

The gateway does not implement embeddings, file uploads, image generation, or Responses retrieval/cancellation. It does not store conversation history for clients. Supply the history in requests; upstream stateful features are not a promise of local session storage.

## Configuration and routing

[config.example.json](config.example.json) is the configuration reference to start from. File input permits `//` and `/* ... */` comments; API request bodies require standard JSON. Unknown fields, invalid values, repeated top-level fields, and conflicting old/new OAuth fields are rejected. Saving normalizes the configuration and removes comments.

| Setting | Default / meaning |
| --- | --- |
| `server_keys` | At least one enabled local key is required. |
| `zen_keys`, `go_keys` | Independent upstream key pools; empty in the example. |
| `prefer` | `go`; selects the preferred available authenticated tier, or `zen`. |
| `anonymous` | `false`; enables the upstream `public` credential for eligible free models. |
| `upstream.zen` | `https://opencode.ai/zen` for manual Zen keys. |
| `upstream.go` | `https://opencode.ai/zen/go` for manual Go keys and their usage endpoint. |
| `upstream.anonymous` | `https://opencode.ai/inference` for anonymous traffic. |
| `models.refresh_seconds` | `300`; model discovery interval. |
| `models.protocols` | Per-model native-protocol overrides: `chat`, `responses`, `anthropic`, or `systemone`. |
| `models.free_only` | `false`; filters both model discovery and routing to free models. |
| `models.active_probes` | `false`; optional synthetic free-model inference probes. |
| `reasoning.effort` | Unset in built-in defaults; the example sets `high`. |
| `reasoning.effort_by_model` | Per-model override of the default effort. |

### Models and upstream selection

Discovery combines upstream model lists with the OpenCode capability and price catalog at `https://models.opencode.ai/api.json`. It tracks native protocols per tier and exposes available context/token limits, prices, `native_protocol`, `supported_endpoints`, and reasoning metadata through `/v1/models`. Price/deprecation metadata refreshes every 24 hours; failed refreshes retain existing data. Prices are metadata, not a billing estimate or account balance.

With manual keys, eligible free models can first use the anonymous lane when explicitly enabled, then the available keyed tiers in `prefer` order. Without anonymous access they use configured keys. Free eligibility comes from a case-insensitive `free` in the model ID or non-deprecated metadata with zero input and output cost; eligibility does not guarantee acceptance upstream. Paid models require an appropriate authenticated route. `models.free_only` also filters members of rotation groups.

OAuth routes follow separate rules: the selected OAuth tier owns the request, uses only its confirmed routes, and does not fall back to anonymous traffic, manual keys, or another tier after failure. Selecting Go OAuth never triggers a PAYG request on authorization failure, quota exhaustion, or 429. When both tiers serve a model, explicitly choosing `prefer: "zen"` permits the connected Zen PAYG route.

### Timeouts, proxies, and affinity

| Setting | Default | Meaning |
| --- | --- | --- |
| `retry.max_attempts` | `3` | Retry budget for keyed loops; anonymous and rotation paths have their own selection rules. |
| `retry.timeout_seconds` | `300` | Whole inference budget, including stream consumption. |
| `performance.attempt_timeout_seconds` | `0` | Optional shorter attempt bound; zero leaves the request timeout. |
| `performance.first_event_timeout_seconds` | `0` | Optional first-event wait; zero disables it. |
| `performance.connect_timeout_seconds` | `5` | Connection establishment timeout. |
| `performance.failure_cooldown_seconds` | `15` | Base cooldown after failures. |
| `performance.max_anonymous_exits` | `8` effective | Maximum distinct static anonymous exits; zero selects this built-in limit. |
| `performance.free_tier_block_seconds` | `900` effective | Free-tier exit block cooldown; zero selects the built-in value. |
| `performance.max_idle_conns` | `2048` | Idle connections per transport. |
| `performance.max_idle_conns_per_host` | `256` | Idle connections per host/transport. |
| `performance.max_conns_per_host` | `0` | Connection cap; zero means unlimited. |
| `performance.idle_conn_timeout_seconds` | `120` | Idle connection lifetime. |

`proxies` accepts `direct`, HTTP, HTTPS, SOCKS5, and SOCKS5H URLs. `proxyfile` adds one proxy per nonempty line, relative to the configuration directory; entries are deduplicated and an empty resolved list means direct access. Line comments start with `#`, `;`, or `//` at the beginning or after whitespace. Credentials embedded in proxy URLs are secrets.

Mark a provider-managed rotating endpoint with `pool+https://proxy.example:8443` or `https://proxy.example:8443#pool`. Each attempt then opens a fresh connection without keep-alive/HTTP/2 reuse; these entries do not use static-node cooldown or health eviction. The provider must actually rotate its exits. Static proxies retain connection reuse, health tracking, and key bindings. `proxy_bindings` can pin a key fingerprint to a configured proxy.

An explicit `x-session-id` provides conversation affinity. OpenCode session headers and supported conversation fields also work; otherwise the initial system/user content supplies a seed. Generated identities are scoped to the local API key and process lifetime. Valid official OpenCode session IDs are preserved. Separate independent conversations with explicit IDs and use separate keys where user isolation is required; identical starts under one key can share affinity.

An optional `models.anonymous_backoff` object pauses the anonymous lane after repeated failures. Its defaults are threshold `6`, base `1` hour, maximum `24` hours. Omitting the object disables this lane-level breaker.

### Free-model request shaping and reasoning

Free-model compatibility can inject the official OpenCode system prompt and tool definitions, rename client tools on the wire, and restore their names in responses. This also affects free models served by keys. The gateway relays tool calls; it does not execute tools on the host. System One is excluded from message/tool shaping.

During shaping, ordinary ASCII spaces in client tools' top-level descriptions are replaced with non-breaking spaces (`U+00A0`, NBSP) by default. Schemas, parameter descriptions, and the appended official tool definitions are unchanged by this conversion. Spaces inside code examples in those descriptions also change; upstream matching rules may change over time.

The injected prompt includes the model and UTC date, and reports unknown client directory, workspace, platform, or Git status as `unknown`. It never substitutes the gateway host's environment. To supply these facts explicitly, add the gateway-only `client_environment` field at the top level of each inference request:

```json
{"client_environment":{"working_directory":"C:\\work\\project","workspace_root":"C:\\work\\project","platform":"windows","is_git_repo":true}}
```

The gateway consumes and removes this extension before forwarding. It also recognizes formatted `<env>` blocks in system/developer instructions and standalone user `<environment_context>` updates; the latest user update wins. When injecting a prompt, source priority is explicit `client_environment`, recognized client declarations, configured `upstream.prompt_environment`, then unknown. Each source supplies a complete snapshot: missing fields are not filled from lower-priority sources, and a working directory does not imply a workspace root. Each request owns its snapshot; there is no environment cache across requests or sessions. Send the environment with every request, directly or in its conversation history.

Paths are validated as text without filesystem access. `windows` normalizes to `win32`; `linux` and `darwin` are also accepted. To set a deployment fallback, place the four inner fields above in `upstream.prompt_environment`, edit the configuration file, and reload it; these advanced values have no dedicated UI fields. A client-supplied official prompt is still preserved. Unknown environment values have been accepted in live `deepseek-v4.1-flash-free` requests; other models and lanes remain unverified.

Reasoning accepts `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `none`, `auto`, or an empty value. Explicit client effort takes precedence. `none` removes reasoning settings. `auto` uses a recently validated effort ceiling only for eligible free Zen/anonymous models; it does not expand probe scope. Automatic discovery needs active probes, may make several billable/quota-consuming requests, and expires after two hours. Go/paid models and unverified levels get no inferred override.

Availability is learned from real traffic by default. Synthetic checks run only with `models.active_probes: true`; the management probe action schedules checks but does not override this switch. Explicit model-retired/not-found evidence can disable a model. Rate limits, authentication failures, generic errors, and timeouts do not prove retirement. Manual restore keeps a model enabled and excludes it from automatic probing until automatic control is restored.

### WARP

`warp.enabled` defaults to `false`. When enabled, free anonymous/manual-Zen traffic and their discovery/probe paths use an in-process Cloudflare WARP tunnel. Go traffic, paid traffic, and OAuth routes retain their configured proxy routing. WARP needs outbound UDP to Cloudflare port `2408`; no TUN device or routing-table change is required. Initialization failure rejects startup or the candidate configuration.

`warp.pool_size` is a concurrency-driven exit ceiling from 1 to 32, default 2. `warp.stack` accepts `ipv6` (default), `ipv4`, or `dual`. Replacing an exit does not guarantee a fresh upstream allowance. `warp.source_ipv6` is optional, requires the IPv6 stack and a prefix of `/64` or shorter, and on Windows the selected source addresses must already be assigned to an interface. It does not configure host networking.

## Management and OAuth

The WebUI has overview, diagnostics/Playground/logs, client access, API keys, models/quota, rotation, configuration, and account pages. It supports Simplified Chinese, English, Vietnamese, and Italian; language/theme choices are stored in the browser. See the [management guide](docs/management.md) for endpoint details.

Management authentication uses an administrator session cookie, CSRF tokens for writes, and login throttling. Changing the administrator credentials invalidates sessions. Local API keys do not grant management access. `GET /api/config` masks secrets; password-confirmed reveal can expose manual keys/proxy values, but never OAuth tokens.

Use **Models & Quota** to start a Device OAuth login. Open the displayed official verification URL yourself, enter `user_code`, authorize the request you initiated, and select an organization/workspace if asked. The backend polls and refreshes credentials; there is no token-paste, local credential-file import, or callback-URL workflow.

| Connection | Scope and constraints |
| --- | --- |
| Go OAuth | Official Go subscription routes only; allowance is shown as unknown. |
| Zen OAuth | Console PAYG using workspace credits; explicit acknowledgement is required before starting. Credit balance is shown as unknown. |

Go and Zen pending logins, saved credentials, refresh state, and disconnect actions are separate. Hiding/leaving the page stops automatic polling; returning can resume an unexpired login. Logout cancels that session's pending logins and preserves completed connections. Canceling a pending login does not disconnect an existing account.

Saved connections are `go_oauth` and `zen_oauth` in the configuration and contain access/refresh credentials. Protect the configuration and its backups. Manual keys remain saved but are not fallback routes while the corresponding OAuth connection exists, including when it needs a new login. Disconnect removes only the local connection, restores manual routing after discovery, and does not revoke the official session. Keep another credential before disconnecting the last usable connection with anonymous access disabled.

Only routes from authenticated official remote configuration that pass plan-specific URL and SDK validation are accepted. The gateway does not derive PAYG URLs from Go routes or import arbitrary remote headers. Ordinary config saves preserve backend-managed OAuth credentials. Legacy `go_subscription`/`zen_subscription` fields migrate on load; combining old and new fields for one plan is rejected.

OAuth integration follows the upstream source snapshots listed in the management guide. This documents implementation behavior, not successful live authorization or inference against every account; upstream permissions and route formats can change.

### Local keys, quota, and rotation

The API-key page creates, names, enables, disables, and deletes local keys. Generated keys contain 192 random bits. Custom keys accept 16–256 characters from letters, digits, `.`, `_`, and `-`; names accept 1–80 characters, with at most 200 keys. At least one must remain enabled. New requests observe changes immediately. There are no per-key model permissions, spending limits, or expiry settings.

Manual Go-key quota comes from the configured Go upstream's `/v1/usage`: shared rolling/weekly/monthly percentage windows, not per-model balances. Keys can share the same allowance, so their percentages are not added. Successes cache for 60 seconds, failures for 15; old data is marked stale. OAuth quota/credits are unknown and are never inferred from manual keys or successful requests.

Rotation aliases `sota` and `sweet` use Go models and work with the three message endpoints, including streaming. Each group has a service-wide current model that stays after success and advances on failure. SOTA contains `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, and `qwen3.8-max`; Sweet collects other eligible Go models. Availability follows actual discovery, not the fixed names. Aliases/order and the first-output timeout are editable in `/#rotation`.

The ordinary-error budget is three models with up to three attempts each. Quota/rate-limit failures skip directly to the next model; rotating cannot create extra shared quota. Once stream output is committed, an error ends that answer and advances the group for the next request. Responses keep the requested alias in `model`; `X-Resolved-Model` identifies the actual model. State persists beside the configuration.

## Operations and security

Configuration saves validate and construct a replacement runtime before applying it. Failed validation or persistence keeps the active runtime. Keys, proxies, routing, model settings, and logging apply to new requests; listeners and WebUI enablement require restart. File edits require `POST /api/config/reload` or the UI reload action; there is no automatic file watcher.

| Data | Storage |
| --- | --- |
| Configuration and previous version | `config.json`, `config.json.bak`. |
| Model and metadata caches | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Availability and rotation | `config.json.<upstream-fingerprint>.availability.json`, `config.json.rotation.json`. |
| Sessions, per-key last-hour usage, recent requests/attempts, log ring, last Playground result | Memory; reset on process restart. |
| Per-server-key lifetime usage | `usage.json` beside the config: loaded on start, rewritten every 5 minutes and on shutdown; the last-hour window stays in memory. |
| Structured logs | JSON on stdout; retention requires an external collector. |

Names use the actual configuration path. Monitoring survives hot reloads but is not shared across processes. Token counts use upstream-reported usage; missing usage is not estimated. Request completion and upstream header-attempt metrics measure different things. Playground has a separate usage bucket. The recent-hour buffers retain at most 10,000 requests/20,000 attempts and return at most 500 of each.

`GET /healthz` makes no upstream request and requires no key. Pending/empty discovery, unavailable WARP, or missing required healthy proxies return 503. A usable stale catalog can return 200 with `status: "degraded"`; stale means older than twice the refresh interval, at least 60 seconds. Readiness is not proof of a valid key, remaining allowance, or successful inference. The public response contains counts/status, not secret values or proxy addresses.

Keep API and management listeners on loopback for local use. For network access, use HTTPS and restrict access to management. A trusted reverse proxy must set `X-Forwarded-Proto` consistently; rate limiting uses the direct peer, so clients behind one proxy share its limits. The executable itself serves HTTP.

Configuration files and backups hold upstream keys, proxy passwords, and OAuth tokens; password hashing does not encrypt them. Restrict filesystem permissions (Windows ACLs when applicable) and exclude runtime files from source sharing. `logging.dump_request_bodies` is off by default and requires debug logging; even redacted, capped body dumps can reveal conversation content. Playground and availability probes send real upstream requests and can consume quota or credits.

## Local development and verification

`cmd/opencode2api` starts the service. `internal/admin` owns management, `gateway` owns routing/runtime, `protocol` handles JSON/SSE conversion, `models` owns discovery, `oauth` owns official authorization, `config` owns validation/persistence, and `telemetry` owns metrics/logging. Static assets live in `webui`; no frontend package installation is required.

The native Go commands for a normal development environment are:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
```

For workspace-local caches, set `GOCACHE` and `GOMODCACHE` to persistent project directories before invoking Go. Node.js is optional for checking JavaScript syntax, without installing packages:

```bash
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Run these commands in Bash (Git Bash on Windows). See [verification and maintenance](docs/management.md#verification-and-maintenance) for deployment checks. A release version can be injected with `-ldflags "-X main.version=vX.Y.Z"`.

| Symptom | What to inspect |
| --- | --- |
| API 401 | The local key, its enabled state, and the client header. |
| Startup rejects credentials | Configure an upstream key/saved OAuth connection, or deliberately enable anonymous access. |
| No models / health 503 | Discovery status, supported native protocols, configured credentials, and resource availability. |
| OAuth requests fail | Safe connection state and whether reauthorization is required; no automatic PAYG fallback occurs. |
| HTTP 200 but generation fails | SSE error events, request outcome, and `x-request-id` logs. |
| Changes do not apply | Save/reload the active config; restart for listener changes. |
| Host config edits do not affect Compose | The active config is in the state volume; import the file again or use the WebUI. |
| Usage disappears after restart | Counters are in memory; retain structured logs separately. |

Before upgrading, back up the executable, configuration, and state securely. Stop the process before replacing the executable; rollback should restore a matching program/config/state set.

## Acknowledgements

Thanks to the [LINUX DO](https://linux.do) community for its support.

## License

SPDX-License-Identifier: GPL-3.0-or-later. See [LICENSE](LICENSE). The software is provided without warranty.
