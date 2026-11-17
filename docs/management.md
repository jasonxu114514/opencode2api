# Management console and operations guide

[English](management.md) | [简体中文](management.zh-CN.md) | [Tiếng Việt](management.vi.md) | [Italiano](management.it.md)

This guide covers the implemented management features, OAuth, quota, model rotation, diagnostics, and persistence. See the [English README](../README.md) for startup and inference API examples, and [config.example.json](../config.example.json) for a configuration starting point. Management and inference use separate listeners; every `/api/*` route below belongs to the management listener.

## Startup and access

On Linux, use Go 1.24 or later and build with Bash from the project directory:

```bash
if [ ! -f config.json ]; then
  cp config.example.json config.json
fi
export GOCACHE="$PWD/.local/go-cache"
export GOMODCACHE="$PWD/.local/go-modules"
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Before starting, edit the configuration locally: replace `server_keys` and `webui.password`, and supply `zen_keys` or `go_keys`. For local management, use `webui.listen: "127.0.0.1:8081"`; the example's `0.0.0.0:8081` listens on all interfaces. The password must contain at least 10 characters; the example administrator name is `admin`.

```bash
./opencode2api -config config.json
```

On native Windows, build with `go build -trimpath -o opencode2api.exe ./cmd/opencode2api`, then run `.\opencode2api.exe -config .\config.json` in PowerShell. The README documents Docker/Compose volume initialization, configuration imports, ports, and images. Manage the configuration actually used by the running service; editing a host copy without importing it again does not change the active volume configuration.

The anonymous lane, `anonymous`, is disabled by default. Startup requires at least one upstream key, a saved Go/Zen OAuth connection, or explicitly enabled anonymous access. You cannot start with all credentials empty just to complete the first OAuth login in the WebUI. Anonymous traffic may be blocked upstream; do not enable anonymous inference just to check proxies.

The configuration directory must be writable. On first startup, `webui.password` is migrated to an Argon2id `password_hash`, and the program attempts to remove the backup containing the bootstrap plaintext password. Other credentials are not encrypted by this migration.

## Pages and management authentication

| Page | Address | Purpose |
| --- | --- | --- |
| Overview | `/#overview` | Requests, tokens, upstream attempts, and runtime resources. |
| Diagnostics | `/#diagnostics` | Model routes, Playground, and live logs. |
| Client access | `/#access` | Client connection settings. |
| API key management | `/#api-keys` | Local key lifecycle and usage. |
| Models and quota | `/#model-quotas` | Go catalog, shared quota, both OAuth plans, and free-model availability. |
| Model rotation | `/#rotation` | Group order, current selection, pauses, and attempt history. |
| Configuration | `/#config` | Keys, proxies, timeouts, routes, WARP, and logging. |
| Account security | `/#account` | Change the administrator username and password. |

The UI supports Simplified Chinese, English, Vietnamese, and Italian. Language and theme choices are stored in the browser. The old `#playground` and `#logs` links open Diagnostics; `#availability` opens Models and quota.

Administrators log in with a username and password and receive an `HttpOnly`, `SameSite=Strict` session cookie. Every management API except login requires a valid session. Writes also require `X-CSRF-Token`; when an `Origin` header is present, it must match the service's origin. Login and session responses provide `csrf_token`. Local `server_keys` do not grant management access.

Sessions default to 720 minutes, with a range of 5–10,080 minutes. Changing administrator credentials or restarting the process invalidates existing sessions; the account update endpoint requires the current password again. Login is throttled. Playground allows at most 12 requests per direct client per minute. The service does not trust `X-Forwarded-For`, so clients behind a reverse proxy share its limits. An HTTPS reverse proxy must set `X-Forwarded-Proto` correctly because it affects the cookie's Secure flag and origin checks.

## Saving and reloading configuration

`GET /api/config` returns the runtime configuration, safe IDs/masked values, effective listeners, and fields requiring restart. A key/proxy update uses one of these representations:

```json
{"id":"EXISTING_FINGERPRINT"}
```

```json
{"value":"NEW_VALUE"}
```

`PUT /api/config` accepts a complete configuration form rather than a partial PATCH. Omitted key/proxy lists may be treated as empty. Submit the existing value's ID to retain it. Saving through the UI preserves advanced configuration that the form does not edit. OAuth credentials, rotation, and administrator passwords have dedicated endpoints; ordinary configuration saves do not accept browser-supplied OAuth tokens or overwrite the saved credentials with them.

Saving validates the candidate, builds its runtime, persists the configuration, and switches to it. If validation or configuration persistence fails, the existing runtime continues serving. Configuration files allow `//` and `/* ... */` comments; API bodies require standard JSON. Unknown fields, repeated top-level fields, invalid values, and old/new OAuth fields for the same plan are rejected. Saving normalizes the file and removes comments.

Keys, proxies, upstreams, timeouts, models, preferences, reasoning, and logging apply to new requests without a restart. Changes to `listen`, `webui.listen`, or `webui.enabled` require restart; the UI shows configured and effective listeners. After editing the file manually, select the disk reload action or call `POST /api/config/reload`. The program does not watch the file automatically.

`POST /api/config/reveal` requires a management session, CSRF, and password verification again. It can return manual keys and proxy values, but never the password, password hash, or OAuth tokens. Use it only when copying a value is necessary, and keep its output out of logs and shared material.

Common management defaults:

| Setting | Default / range |
| --- | --- |
| `webui.enabled` | Disabled in built-in defaults; enabled in the example. |
| `webui.listen` | `0.0.0.0:8081`. |
| `logging.level` | `info`; supports `debug`, `info`, `warn`, and `error`. |
| `logging.ring_size` | `2000`; range 100–50,000. |
| `logging.dump_request_bodies` | `false`; also requires debug logging, capped at 64 KiB per prepared body. |
| `models.active_probes` | `false`; real traffic supplies default availability observations. |

## Go OAuth and Zen PAYG OAuth

Models and quota contains two separate panels. Go uses subscription routes; Zen uses Console workspace pay-as-you-go billing (PAYG), and **inference consumes that workspace's credits**. Signing in to Go does not establish Zen permissions or balance, or automatically connect PAYG.

Sign-in steps:

1. Start login in the appropriate panel. Zen requires explicit PAYG acknowledgement first.
2. Open the official OpenCode Device OAuth verification link yourself, enter the displayed `user_code`, and confirm that you initiated the authorization. The page does not redirect automatically.
3. The backend polls at the returned interval. When several organizations are available, automatic completion pauses. Select the organization holding the subscription/credits and connect it; only organizations allowed by this authorization can be selected.
4. On success, configuration and model views refresh. Cancel and start again if login fails or expires.

No token pasting, local credential-file import, or callback-URL submission is required. Leaving the page or hiding the browser stops automatic polling; returning reads the status again and can resume an unexpired login. Pending logins are scoped to the management session and plan; login IDs cannot be used across plans. Canceling clears only the corresponding pending login. Logging out cancels that session's pending logins while retaining saved connections.

| Action | Go endpoint | Zen endpoint | Body |
| --- | --- | --- | --- |
| Safe status | `GET /api/go/auth` | `GET /api/zen/auth` | None. |
| Start login | `POST /api/go/auth/start` | `POST /api/zen/auth/start` | Go: `{}`; Zen: `{"acknowledge_payg":true}`. |
| Poll/select organization | `POST /api/go/auth/poll` | `POST /api/zen/auth/poll` | `{"id":"LOGIN_ID"}`; add `organization_id` when selecting an organization. |
| Cancel pending login | `DELETE /api/go/auth` | `DELETE /api/zen/auth` | None. |
| Disconnect saved account | `DELETE /api/go/connection` | `DELETE /api/zen/connection` | None. |

All endpoints require a session, and writes require CSRF. Login, status, configuration, and reveal responses never return OAuth access/refresh credentials.

Connections are saved as `go_oauth` and `zen_oauth` in the configuration. The backend refreshes credentials when needed and persists them. Configuration and backups therefore contain sensitive access/refresh credentials and need restricted read permissions. Legacy `go_subscription`/`zen_subscription` fields are accepted only as load-time migration aliases; saves use the new fields. Supplying both old and new fields for one plan is rejected.

Routing boundaries:

- Only models actually supplied by authenticated official remote configuration and accepted by the plan-specific host/path and supported-SDK checks are used. PAYG URLs are not guessed by removing `/go` from Go URLs, and arbitrary remote authentication headers are not imported. Legacy `/zen` routes are accepted only when explicitly provided by remote configuration.
- Go and Zen credentials, model sources, and refresh state are separate. `prefer` selects the preferred available route for a model; choosing `zen` permits the connected Zen OAuth account to consume credits.
- Once OAuth is selected, only that tier serves the request. Go authorization failure, quota exhaustion, 429, or other errors do not automatically switch to Zen PAYG, manual keys, or anonymous access.
- Manual `go_keys`/`zen_keys` remain saved but are not fallback routes while the corresponding OAuth connection exists, including when a new login is needed or no usable route is available.
- Disconnecting removes only the corresponding local connection, without revoking the official session or affecting the other plan. Manual models for that tier are then rediscovered; routing is not guessed while discovery is pending. With anonymous access disabled, the last remaining upstream credential cannot be disconnected until another key/connection is configured.
- OAuth subscription allowance and credits are currently unknown. Manual-key quota, model counts, and successful requests are not used to infer them.

The implementation references fixed upstream source snapshots: Go [v2 `4617210822bdbb31e5749751afbfb4584ccedac8`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8) and [dev `7745079647b65036fe142e09ccd8623b25be1878`](https://github.com/anomalyco/opencode/tree/7745079647b65036fe142e09ccd8623b25be1878), and Console/PAYG [dev `03d848f7b8d71d31d3e56f2abd8b337efbc1bc32`](https://github.com/anomalyco/opencode/tree/03d848f7b8d71d31d3e56f2abd8b337efbc1bc32). Source compatibility does not establish successful live authorization or inference. Changes to upstream permissions, workspaces, or configuration formats may require signing in again or updating the implementation.

## Models, quota, and availability

`GET /api/model-quotas` returns Go models and a quota summary, using the gateway catalog as the model source. Manual Go-key quota is queried at configured `upstream.go` plus `/v1/usage`, defaulting to `https://opencode.ai/zen/go/v1/usage`. Upstream keys are not sent to the browser.

- The response provides shared percentages and reset times for rolling five-hour, weekly, and monthly windows, not individual model balances, monetary amounts, or remaining request counts.
- Several keys may share one subscription; their allowances must not be added. Percentage precision is determined by the upstream.
- Successful results cache for 60 seconds, failures for 15. Failures retain previous successful data and label it stale; without history, quota is unknown.
- While visible, the page polls every 60 seconds. Hiding or leaving it stops periodic queries. Manual refresh also respects the server cache.
- When Go OAuth is connected, manual Go-key data is not reused. OAuth allowance and Zen credits remain unknown.

The format follows the [upstream usage implementation](https://github.com/anomalyco/opencode/blob/dev/packages/console/app/src/routes/zen/go/v1/usage.ts). This is not a promise of a stable public interface. Format changes produce unknown/error states rather than invented balances.

Free-model availability is learned from real requests by default. Only explicit model-not-found/retired evidence can disable a model. Statuses 401, 403, 429, generic 4xx/5xx, timeouts, transport errors, and malformed replies alone do not establish retirement. Manual restore keeps a model routable and excludes it from automatic probes until automatic control is restored.

Background synthetic inference requires `models.active_probes: true`. The scheduler checks due records every minute and processes models and lanes sequentially. Healthy/inconclusive models are normally checked after one hour; confirmed unavailable models after 24 hours. Each lane has a 60-second timeout. Probes try the enabled anonymous lane, then an available Zen key, using WARP exits when enabled; models without a suitable lane are skipped. Active probes use real upstream allowance without changing production key/proxy cooldowns or request statistics.

| Management endpoint | Purpose |
| --- | --- |
| `GET /api/models/availability` | Free-model state and reasoning-probe results. |
| `POST /api/models/probe` | Mark automatically controlled models due and wake the scheduler; does not override `active_probes`. |
| `POST /api/models/restore` | `{"model":"MODEL_ID"}` restores a model and puts it under manual control. |
| `POST /api/models/manual` | `{"model":"MODEL_ID","manual":true}`; `false` restores automatic control. |

`reasoning.effort: "auto"` uses a recently validated effort ceiling for free Zen/anonymous models. New ceilings are not actively learned with active probes disabled; manual control also pauses automatic probes. Observations expire after two hours. Levels are not inferred for Go/paid models or unknown capabilities. Explicit client effort always takes precedence.

## Local API keys

These endpoints manage the gateway's `server_keys`, separately from OpenCode upstream keys. Creation returns the complete new key; lists contain only fingerprint, name, masked value, state, timestamps, and usage. The UI can copy the key and client environment variables after creation.

| Endpoint | Request and result |
| --- | --- |
| `GET /api/api-keys` | Safe key list. |
| `POST /api/api-keys` | `{"name":"client-a"}` generates a key; add `value` for a custom key. Returns 201 on success. |
| `PATCH /api/api-keys/{id}` | `{"name":"client-b","enabled":false}`; only fields being changed are required. |
| `DELETE /api/api-keys/{id}` | Delete a local key. |

Generated keys contain 192 random bits. Custom keys accept 16–256 letters, digits, `.`, `_`, or `-`. Names accept 1–80 characters, and the creation endpoint allows at most 200 keys. At least one key must remain enabled. Disabled/deleted keys receive 401 on new requests; in-flight requests may already have authenticated.

`server_key_metadata` stores names, enabled state, and creation times by fingerprint. Existing `server_keys` remain compatible. Usage shows the last hour and process lifetime, attributing requests and tokens to the key that authenticated them. Playground is recorded separately under `playground`, without charging its borrowed local key. Statistics survive hot reloads and reset on restart. There are no per-key quotas, model permissions, or expiration dates.

## Proxies and WARP

Configuration supports bulk import of inline proxies, individual checks, and staged removal. Manage `proxyfile` entries in their source file; relative paths are resolved against the configuration directory. Removal remains a draft until the complete configuration is saved.

`POST /api/proxies/check` requires a session and CSRF. Submit exactly one of these requests:

```json
{"id":"SAVED_PROXY_ID"}
```

```json
{"value":"https://proxy.example:8443"}
```

Saved proxy IDs come from `GET /api/config`; `value` is for drafts. Each check times out after 20 seconds. The UI runs at most four concurrently; the server accepts at most eight and returns 429 for excess checks without queuing. A completed check returns HTTP 200 with `ok`, `state`, `reason`, `latency_ms`, and `status` when available. Authentication, CSRF, and input errors use their corresponding HTTP statuses.

A check only determines whether the selected proxy can reach the configured Zen upstream. An isolated client sends an unauthenticated GET through that proxy to `upstream.zen` plus `/v1/models`. It sends no Zen/Go key, `public` credential, or OAuth token; performs no inference; follows no redirects; does not use WARP; and does not change production health, key cooldowns, or bindings. It works with anonymous access disabled.

| `state` | Meaning |
| --- | --- |
| `available` | Upstream 200–399 or 401. For 401, `authentication_required` proves reachability without validating a key. |
| `unavailable` | Confirmed proxy-side connection refusal/DNS-not-found (`proxy_unreachable`) or proxy tunnel 407 (`proxy_authentication_required`) on this check. |
| `limited` | 429; does not establish a broken proxy. |
| `rejected` | Other upstream refusal or proxy tunnel rejection. An origin 407 is not a proxy-authentication failure. |
| `inconclusive` | Timeout, unlocalized transport failure, or missing prerequisites; not proof of a broken proxy. |

For bulk cleanup, only `unavailable` is a confirmed-unavailable filter for that check. Rate limits, rejections, and timeouts do not establish permanent failure. Production inference can still fail because of model, authentication, allowance, or policy constraints.

WARP is disabled by default. When enabled, free anonymous/manual-Zen requests and their discovery/probe paths use an in-process tunnel. Go, paid, and OAuth routes retain proxy routing. It needs outbound Cloudflare UDP on port `2408`, without TUN or routing-table changes. `warp.pool_size` is a concurrency-driven exit ceiling of 1–32 (default 2). `warp.stack` defaults to `ipv6`, with `ipv4`/`dual` also available. Initialization failure rejects startup or the candidate configuration without silently falling back to proxies.

Exit statuses 403/429 can trigger retirement and replacement, with retries still bounded by their budget. A new exit does not guarantee additional quota. IPv6 `/64` and IPv4 `/32` egress blocks are deduplicated/cooled. `warp.source_ipv6` requires the IPv6 stack and a prefix of `/64` or shorter. Linux uses `IPV6_FREEBIND`; Windows requires the selected source addresses to be assigned already. This setting does not configure host networking. `/healthz` reports WARP state/counts only; `resources.warp` in `/api/monitor` also contains exit information.

## SOTA / Sweet rotation

Clients keep the same API base URL, key, and three message endpoints, changing only `model` to the `sota` or `sweet` alias. Aliases are editable and appear in `/v1/models` when eligible. Direct model calls remain compatible. Rotation uses only Go (manual keys or Go OAuth), never cross-tier PAYG fallback; System One does not join rotation.

SOTA contains the fixed models `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, and `qwen3.8-max`. Sweet includes other available Go message models, initially prioritizing `glm-5.3-flash`, then sorting by name. New models are appended; retired members stay listed but cannot be selected. Availability comes from the catalog and routes, rather than being guaranteed by the fixed names.

Each group has a shared current position. Success keeps it; failure advances it. A generation prevents an older concurrent failure from reverting a newer position. Manual selection does not interrupt an answer in progress. A request does not revisit the same model.

- Ordinary errors allow up to three attempts per model and at most three ordinary-error models. A model that exhausts its attempts pauses for 15 seconds.
- A 429/explicit quota failure immediately advances to the next model. The pause follows `Retry-After`, defaulting to 60 seconds. It does not add to the ordinary-error model budget, although earlier ordinary errors still count.
- Rotation does not use regular-key cross-tier retries or stale-reasoning replay. A single-model failure does not pause the entire Go key. Shared quota may leave the whole group unavailable.
- The total budget is `retry.timeout_seconds`. `rotation.first_output_seconds` bounds each wait for useful output, defaults to 30 seconds, and accepts 1–300 seconds. A non-streaming response must be fully read and valid within that limit.
- Stream prelude events are held until useful output or normal completion. Errors before commitment can retry; errors after commitment preserve emitted content, send the protocol's SSE error, and end the response. Replacement answers are never spliced into one stream. Client cancellation does not trigger a switch or pause.
- Response `model` keeps the requested alias; `X-Resolved-Model` identifies the actual model. Tokens are attributed to that model. Records contain classified reasons, without request bodies, keys, or upstream error bodies.

| Management endpoint | Content |
| --- | --- |
| `GET /api/rotation` | Both groups' order/aliases, current models, pauses, persistence state, last 100 attempts, and last 30 switches per group. |
| `PUT /api/rotation` | Full `alias`/`order` for `sota` and `sweet`, plus `first_output_seconds`; reorder within groups without dropping members. |
| `POST /api/rotation/current` | `{"group":"sota","model":"glm-5.3"}` selects a model and clears its pause. |

Aliases accept 1–64 letters, digits, `.`, `_`, and `-`, beginning with a letter/digit. They must be distinct and cannot conflict with actual model IDs. If catalog changes cause a member mismatch on save, refresh before reordering.

Configuration is stored under `rotation`; runtime state is `config.json.rotation.json`. Hot reloads share that state. Initial state read/write failure prevents startup; subsequent persistence failures are shown in the management page.

## Playground, logs, and monitoring

Playground invokes the same gateway in-process through `POST /api/debug/inference`, forcing non-streaming output. Automatic routing example:

```json
{"protocol":"chat","key":{"mode":"auto"},"request":{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}}
```

For a selected key, replace `key` with `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}`; obtain its ID from `GET /api/debug/models`. A selected-key check makes one upstream attempt without anonymous access, key switching, cross-tier fallback, or reasoning replay. Optional `relay` pins a known proxy index, for example `{"index":0}`. Use an index from the current diagnostic catalog.

Diagnostics preserve production key cooldowns, failure counts, proxy health, and bindings. They still make real requests, consume allowance/credits, and appear in monitoring. A completed management call wraps the result in HTTP 200, with the actual outcome in `ok`, `http_status`, `request_id`, `route`, and `response`. Selected-key results also include `selected_key` and `key_test`, such as `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error`, or `unavailable`. Input, authentication, and throttling errors keep their corresponding HTTP statuses.

Logs are available at `GET /api/logs?after=SEQUENCE&limit=COUNT` and `GET /api/logs/stream`. SSE resumes using `Last-Event-ID` or `after`; expired/missed events produce `gap`, and evicted data cannot be replayed. The server ends a log stream when its session becomes invalid.

`GET /api/monitor` provides aggregate requests, tokens, upstream attempts, and resources. `GET /api/debug/models` provides routes, key fingerprints, model metadata, proxies, and the latest diagnostic result. Tokens use only upstream-reported usage, without estimating missing values. Input includes cache reads/writes; cached tokens separately count reads. `usage.server_keys` groups local-key usage, with `playground` separate.

Request outcomes describe the complete inference. Failed streams count as `stream_error` even after HTTP 200; client cancellation is `client_canceled`. An upstream attempt measures the exchange only until response headers arrive, so its success rate and latency are not the success rate and latency of the complete answer.

## Persistence, health, and security

| Data | Location and lifecycle |
| --- | --- |
| Configuration/previous version | `config.json`, `config.json.bak`; contain sensitive information. |
| Model/price caches | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Free-model availability | `config.json.<Zen-upstream-fingerprint>.availability.json`, isolated by upstream. |
| Rotation state | `config.json.rotation.json`. |
| Sessions, token totals, log ring, latest Playground | Process memory; cleared on restart. |
| Request/attempt details | Last hour, capped at 10,000/20,000 records; APIs return at most 500 of each. |
| Durable logs | JSON on stdout; collect and retain securely yourself. |

Prefixes use the actual `-config` path. Processes do not share sessions, monitoring, or in-memory counters. Avoid concurrent writes to the same state files.

`GET /healthz` is on the inference port, requires no authentication, and triggers no upstream check or monitoring count. It returns 503 while the catalog is pending/empty, WARP has no active exit, or required proxies have no healthy node. A usable stale catalog may return 200 with `degraded`; staleness means twice the refresh interval, at least 60 seconds. OAuth reauthorization needs appear in issues, but readiness does not guarantee the next inference. Public information includes state/counts, without keys or proxy addresses.

Restrict local use to loopback. Network management needs HTTPS and access controls; the program does not terminate TLS itself. Configuration/backups contain manual keys, proxy passwords, and OAuth tokens: limit filesystem permissions or Windows ACLs and exclude them from source sharing. Even redacted, truncated request-body dumps can contain private conversations; enable them only briefly when needed. Do not attach full configuration, reveal output, cookies, authentication headers, or real inference content to troubleshooting reports.

## Verification and maintenance

Local development checks:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Normal deployment acceptance should verify management login and CSRF, key disable/restore, configuration saves and listener restarts, model discovery, the three message protocols and SSE errors, rotation state, and required live OAuth/upstream features. Real inference, OAuth, WARP, and quota queries may depend on account permissions and consume allowance. Static checks do not replace them.

Before upgrading, securely back up the executable, active configuration, and state. Stop the process before replacing the executable; rollback needs compatible configuration and state. Compose users must preserve the actual files in the volume. Keep credentials, logs, runtime state, and local verification artifacts out of source control.
