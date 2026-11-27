# 管理控制台与运行指南

[English](management.md) | 简体中文 | [Tiếng Việt](management.vi.md) | [Italiano](management.it.md)

本文说明当前实现的管理功能、OAuth、额度、模型轮转、诊断与持久化。服务启动及推理 API 示例见[简体中文 README](../README.zh-CN.md)，完整配置起点为 [config.example.json](../config.example.json)。管理端与推理 API 使用不同监听地址；下文 `/api/*` 都是管理端路由。

## 启动与访问

Linux 下使用 Go 1.24 或以上版本，在项目目录以 Bash 编译运行：

```bash
if [ ! -f config.json ]; then
  cp config.example.json config.json
fi
export GOCACHE="$PWD/.local/go-cache"
export GOMODCACHE="$PWD/.local/go-modules"
go build -trimpath -o opencode2api ./cmd/opencode2api
```

启动前在本机编辑配置：替换 `server_keys` 和 `webui.password`，填入 `zen_keys` 或 `go_keys`。本地管理建议 `webui.listen: "127.0.0.1:8081"`；示例的 `0.0.0.0:8081` 会监听所有接口。密码至少 10 字符，示例管理员名为 `admin`。

```bash
./opencode2api -config config.json
```

Windows 原生方式为 `go build -trimpath -o opencode2api.exe ./cmd/opencode2api`，然后在 PowerShell 执行 `.\opencode2api.exe -config .\config.json`。Docker/Compose 的卷初始化、文件导入、端口与镜像指引见 README；管理操作需使用实际运行配置，而非仅编辑未重新导入的宿主副本。

匿名通道 `anonymous` 默认关闭。至少一个上游 Key、已保存 Go/Zen OAuth 或明确开启匿名才满足启动条件；不能凭证全空先启动，再进入 WebUI 做首次 OAuth。匿名流量可能被上游封堵，不应为检查代理而开启匿名推理。

配置目录须可写。首次启动会把 `webui.password` 迁移为 Argon2id `password_hash` 并尝试删除包含引导明文密码的备份；这不会加密其他凭证。

## 页面与管理认证

| 页面 | 地址 | 功能 |
| --- | --- | --- |
| 概览 | `/#overview` | 请求、Token、上游尝试和运行资源。 |
| 诊断 | `/#diagnostics` | 模型路由、Playground、实时日志。 |
| 接入指南 | `/#access` | 客户端连接参数。 |
| API Key 管理 | `/#api-keys` | 本地 Key 生命周期与用量。 |
| 模型与额度 | `/#model-quotas` | Go 目录、共享额度、两类 OAuth、免费模型可用性。 |
| 模型轮转 | `/#rotation` | 分组顺序、当前位置、暂停与尝试记录。 |
| 配置中心 | `/#config` | Key、代理、超时、路由、WARP 与日志设置。 |
| 账号安全 | `/#account` | 修改管理员账号和密码。 |

界面支持简中、英语、越南语、意大利语，语言与主题保存在浏览器。旧 `#playground`、`#logs` 会进入诊断，`#availability` 进入模型与额度。

管理员通过用户名/密码登录，获得 `HttpOnly`、`SameSite=Strict` Session Cookie。所有管理 API 除登录均需有效 Session；写操作另需 `X-CSRF-Token`，带 `Origin` 时须与服务来源一致。登录/Session 响应提供 `csrf_token`。本地 `server_keys` 不能代替管理登录。

Session 默认 720 分钟，范围 5–10,080 分钟。修改管理员凭证或重启进程会使现有 Session 失效；改账号接口要求再次验证当前密码。登录有限流；Playground 每直连客户端每分钟最多 12 次。服务不信任 `X-Forwarded-For`，反向代理后的客户端会共享对端限制；启用 `-trust-cloudflare-client-ip` 时，loopback 对端（本机 `cloudflared`）改按 `CF-Connecting-IP` 限流，IPv6 按 /64。HTTPS 反向代理须正确设置 `X-Forwarded-Proto`，它参与 Cookie Secure 标记与来源校验。

## 配置保存与重载

`GET /api/config` 返回运行配置、安全 ID/掩码、实际监听地址及需重启字段。Key/代理更新使用以下一种表示：

```json
{"id":"EXISTING_FINGERPRINT"}
```

```json
{"value":"NEW_VALUE"}
```

`PUT /api/config` 接收完整配置表单，不是局部 PATCH；遗漏的 Key/代理列表可能被视为空列表。保留已有值时提交其 ID。使用页面保存可以保留未在表单中编辑的高级配置。OAuth 凭证、轮转和管理员密码由各自专用接口管理；普通保存不会接收或覆盖浏览器提交的 OAuth Token。

保存顺序为校验、构建候选运行时、持久化、切换。校验或配置持久化失败时旧运行时继续服务。配置文件允许 `//`、`/* ... */` 注释，API 请求必须是标准 JSON；未知字段、重复顶层字段、非法值及同一方案新旧 OAuth 字段并存会被拒绝。保存后文件规范化，注释消失。

Key、代理、上游、超时、模型、偏好、reasoning 和日志设置热应用到新请求。`listen`、`webui.listen`、`webui.enabled` 必须重启；页面同时显示已配置值和实际监听。文件手动修改后须执行「从磁盘重载」或 `POST /api/config/reload`，程序不自动监视文件。

`POST /api/config/reveal` 要求管理 Session、CSRF 及再次验证密码，可返回手动 Key 与代理值；不会返回密码、密码哈希或 OAuth Token。只在确实需要复制时使用，避免把结果写进日志或分享。

常用管理默认值：

| 配置 | 默认 / 范围 |
| --- | --- |
| `webui.enabled` | 内置默认关闭，示例开启。 |
| `webui.listen` | `0.0.0.0:8081`。 |
| `logging.level` | `info`；支持 `debug`、`info`、`warn`、`error`。 |
| `logging.ring_size` | `2000`；范围 100–50,000。 |
| `logging.dump_request_bodies` | `false`；还需 debug 才输出，每个准备好的请求体最多 64 KiB。 |
| `models.active_probes` | `false`；真实流量提供默认可用性观察。 |

## Go OAuth 与 Zen PAYG OAuth

「模型与额度」有两个独立面板。Go 使用订阅路由；Zen 使用 Console 工作区按量计费（PAYG），**推理会消耗该工作区 credits**。Go 登录不代表获得 Zen 权限或余额，不会自动连接 PAYG。

操作流程：

1. 在对应面板开始登录；Zen 必须先明确确认 PAYG。
2. 自行打开官方 OpenCode Device OAuth 验证链接，输入显示的 `user_code`，确认这是自己发起的授权。页面不自动跳转。
3. 后端按返回的间隔轮询。存在多个组织时暂停自动完成，选择订阅/credits 所属组织，再连接所选组织；只能选择本次授权允许的组织。
4. 成功后刷新配置与模型视图。失败或过期可取消并重新开始。

不需要粘贴 Token、读取本机凭证文件或提交回调 URL。离开页面或隐藏浏览器停止自动轮询；返回后重新读取状态，未过期登录可继续。两类待完成登录按管理 Session 和方案隔离，不能跨方案使用登录 ID。取消只清除对应待完成登录；退出管理 Session 取消该 Session 的待完成登录，保留已保存连接。

| 操作 | Go 接口 | Zen 接口 | 请求体 |
| --- | --- | --- | --- |
| 安全状态 | `GET /api/go/auth` | `GET /api/zen/auth` | 无。 |
| 开始登录 | `POST /api/go/auth/start` | `POST /api/zen/auth/start` | Go 为 `{}`；Zen 为 `{"acknowledge_payg":true}`。 |
| 轮询/选组织 | `POST /api/go/auth/poll` | `POST /api/zen/auth/poll` | `{"id":"LOGIN_ID"}`；选组织另加 `organization_id`。 |
| 取消待完成登录 | `DELETE /api/go/auth` | `DELETE /api/zen/auth` | 无。 |
| 断开已存连接 | `DELETE /api/go/connection` | `DELETE /api/zen/connection` | 无。 |

全部接口要求 Session，写操作要求 CSRF。登录、状态、配置与 reveal 响应都不返回 OAuth 访问/刷新凭证。

连接分别存为配置的 `go_oauth`、`zen_oauth`，后端按需刷新并持久化。配置及备份因此包含敏感访问/刷新凭证，应限制读取权限。旧 `go_subscription`/`zen_subscription` 仅作为加载迁移别名，保存使用新字段；同一方案的新旧字段同时出现会被拒绝。

路由边界：

- 只接纳已认证官方远程配置实际提供、通过对应方案主机/路径及支持 SDK 校验的模型。不从 Go URL 删除 `/go` 来猜测 PAYG，也不接纳任意远程认证头。旧 `/zen` 路由仅在远程配置明确提供时接受。
- Go 与 Zen 的凭证、模型来源及刷新独立。`prefer` 决定同一模型的可用首选；选择 `zen` 允许已连接 Zen OAuth 消耗 credits。
- 选定 OAuth 后只使用该 Tier。Go 授权失效、额度耗尽、429 或其他错误不会自动转入 Zen PAYG、手动 Key 或匿名通道。
- 手动 `go_keys`/`zen_keys` 保留，但对应 OAuth 存在期间不作备用，即使需要重新登录或没有可用路由也如此。
- 断开仅移除对应本地连接，不撤销官方 Session、不影响另一连接；随后重新发现该 Tier 的手动模型，发现未完成时拒绝猜测路由。匿名关闭时，不能断开唯一剩余上游凭证，需先配置其他 Key/连接。
- OAuth 订阅余量和 credits 当前未知，不复用手动 Key 额度，不根据模型数量或成功请求推算。

实现参考固定源码快照：Go 的 [v2 `4617210822bdbb31e5749751afbfb4584ccedac8`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8)、[dev `7745079647b65036fe142e09ccd8623b25be1878`](https://github.com/anomalyco/opencode/tree/7745079647b65036fe142e09ccd8623b25be1878)，Console/PAYG 的 [dev `03d848f7b8d71d31d3e56f2abd8b337efbc1bc32`](https://github.com/anomalyco/opencode/tree/03d848f7b8d71d31d3e56f2abd8b337efbc1bc32)。源码兼容不等于真实授权和推理已验证；上游权限、工作区或配置格式改变时，可能需要重登或更新实现。

## 模型、额度与可用性

`GET /api/model-quotas` 返回 Go 模型与额度摘要，模型以网关目录为准。手动 Go Key 从已配置 `upstream.go` 加 `/v1/usage` 查询，默认是 `https://opencode.ai/zen/go/v1/usage`，浏览器不接收上游 Key。

- 提供 5 小时滚动、每周、每月的共享百分比窗口和重置时间，不提供逐模型余额、金额或可用次数。
- 多个 Key 可能共用套餐，不能相加。百分比精度由上游决定。
- 成功缓存 60 秒，失败 15 秒；失败保留上次成功值并标记历史，没有历史则未知。
- 页面显示时每 60 秒查询；隐藏或离开时停止周期查询。手动刷新也受缓存限制。
- Go OAuth 存在时不借用手动 Go Key 数据；OAuth 额度和 Zen credits 保持未知。

额度格式依据[上游 usage 源码](https://github.com/anomalyco/opencode/blob/dev/packages/console/app/src/routes/zen/go/v1/usage.ts)。这不是稳定公开接口的保证，格式改变会报告未知/异常，不虚构余额。

免费可用性默认来自真实请求。明确的模型不存在/下架错误才可禁用；401、403、429、普通 4xx/5xx、超时、连接错误和畸形响应不能单独证明模型下架。手动恢复保持模型可路由并退出自动探测，恢复自动控制后才重新排程。

`models.active_probes: true` 才启用后台合成推理，调度器每分钟检查到期项，逐模型、逐通道执行。正常/无法判断的模型通常一小时复查，确认不可用的模型 24 小时复查；单通道超时 60 秒。请求先走已开启匿名通道，再尝试可用 Zen Key，WARP 开启时使用其出口；没有合适通道就跳过。主动探测会消耗实际额度，不改变生产 Key/代理冷却及请求统计。

| 管理接口 | 用途 |
| --- | --- |
| `GET /api/models/availability` | 读取免费模型状态和 reasoning 探测结果。 |
| `POST /api/models/probe` | 将自动控制模型标为到期并唤醒调度；不绕过 `active_probes` 开关。 |
| `POST /api/models/restore` | `{"model":"MODEL_ID"}`，恢复并交给手动控制。 |
| `POST /api/models/manual` | `{"model":"MODEL_ID","manual":true}`；`false` 恢复自动控制。 |

`reasoning.effort: "auto"` 使用近期成功验证的免费 Zen/匿名强度上限。主动探测关闭时不会主动学习新上限；手动控制也暂停自动探测。观测结果两小时后失效。Go/付费或能力未知模型不推算；客户端明确 effort 始终优先。

## 本地 API Key

管理对象是网关 `server_keys`，与 OpenCode 上游 Key 分开。创建接口在响应中返回完整新 Key，列表只返回指纹、名称、掩码、状态、时间和用量。页面支持创建后复制 Key 与客户端环境变量。

| 接口 | 请求与结果 |
| --- | --- |
| `GET /api/api-keys` | 安全列表。 |
| `POST /api/api-keys` | `{"name":"client-a"}` 自动生成；可另带 `value` 自定义，成功 201。 |
| `PATCH /api/api-keys/{id}` | `{"name":"client-b","enabled":false}`，可只提交需要修改的字段。 |
| `DELETE /api/api-keys/{id}` | 删除本地 Key。 |

自动 Key 使用 192 位随机数。自定义 Key 长度 16–256，字符为字母、数字、`.`、`_`、`-`；名称 1–80 字符，创建接口最多允许 200 个 Key。至少保留一个启用 Key。停用/删除对新请求立即返回 401，在途请求可能已完成认证。

`server_key_metadata` 按指纹保存名称、启用状态和创建时间，旧 `server_keys` 自动兼容。用量显示近一小时与当前进程累计，请求与 Token 按认证命中的 Key 归类。Playground 单独记为 `playground`，不计入借用的本地 Key。统计跨热重载保留、重启清零，不提供独立配额、模型权限或到期日。

## 代理与 WARP

配置中心支持批量导入内联代理、逐项检查及暂存移除。代理文件 `proxyfile` 的内容应在来源文件管理；相对路径以配置目录为准。移除仅为草稿，保存完整配置后才生效。

`POST /api/proxies/check` 要求 Session、CSRF，以下两种请求只能选一种：

```json
{"id":"SAVED_PROXY_ID"}
```

```json
{"value":"https://proxy.example:8443"}
```

已保存代理的 ID 来自 `GET /api/config`，`value` 用于草稿。单次超时 20 秒；页面最多四并发，服务端最多八并发，超出返回 429、不排队。正常完成返回 HTTP 200，包含 `ok`、`state`、`reason`、`latency_ms` 及可用时的 `status`；认证、CSRF 和格式错误使用各自 HTTP 状态。

检查只验证所选代理能否连接已配置 Zen 上游：隔离客户端通过该代理向 `upstream.zen` 加 `/v1/models` 发无认证 GET，不发送 Zen/Go Key、`public` 或 OAuth，不推理、不跟随重定向、不用 WARP，也不更改生产健康、Key 冷却或绑定。匿名关闭时仍可使用。

| `state` | 解读 |
| --- | --- |
| `available` | 上游 200–399 或 401；401 的 `authentication_required` 只证明到站，未验证 Key。 |
| `unavailable` | 本次确认代理侧拒绝连接/DNS 不存在（`proxy_unreachable`），或代理隧道 407（`proxy_authentication_required`）。 |
| `limited` | 429，不证明代理损坏。 |
| `rejected` | 上游其他拒绝或代理隧道拒绝；上游 407 不等于代理认证失败。 |
| `inconclusive` | 超时、无法定位的传输错误或前提缺失，不能认定损坏。 |

批量清理只应把本次 `unavailable` 作为确认不可用筛选，不能把限流、拒绝、超时都当成永久损坏；正式推理仍可能受模型、认证、额度和策略限制。

WARP 默认关闭。开启后免费匿名/手动 Zen 请求及对应目录/探测使用进程内隧道；Go、付费与 OAuth 保持代理路由。需要 Cloudflare UDP `2408`，不需要 TUN 或改路由表。`warp.pool_size` 为 1–32 的并发出口上限（默认 2），`warp.stack` 默认为 `ipv6`，也可 `ipv4`/`dual`。初始化失败拒绝启动或候选配置，不静默改走代理。

出口 403/429 可触发退役与补充，重试仍受预算限制；更换出口不保证额外额度。IPv6 `/64`、IPv4 `/32` 出口按网段去重/冷却。`warp.source_ipv6` 要求 IPv6 堆栈、`/64` 或更短前缀；Linux 使用 `IPV6_FREEBIND`，Windows 必须已分配所选源地址。该设置不会配置主机网络。`/healthz` 只提供 WARP 状态/数量，`/api/monitor` 的 `resources.warp` 另含出口信息。

## SOTA / Sweet 轮转

客户端继续使用原 API Base URL、Key 和三种消息接口，只把 `model` 改为别名 `sota` 或 `sweet`。别名可修改，满足条件时列入 `/v1/models`；实际模型调用保持兼容。轮转只走 Go（手动 Key 或 Go OAuth），不跨 Tier 回退 PAYG，System One 不加入轮转。

SOTA 固定成员为 `deepseek-v4.1-flash`、`glm-5.3`、`grok-4.6`、`kimi-k3`、`qwen3.8-max`。Sweet 为其余可用 Go 消息模型，初始优先 `glm-5.3-flash`，之后按名称排序，新模型追加；下架成员保留列表但不可选用。实际可用性来自目录和路由，不因固定名称而保证可调用。

每组共享一个当前位置，成功后保持，失败后推进。版本号阻止较早的并发失败把新位置改回去；手动选择不打断正在进行的回答。每请求不重复访问同一模型。

- 普通错误每模型最多三次，最多消耗三个普通错误模型预算；耗尽的模型暂停 15 秒。
- 429/明确额度错误直接跳到下一个模型，暂停取 `Retry-After`，缺省 60 秒，不新增普通错误预算；已发生普通错误仍计数。
- 不使用普通 Key 跨 Tier 重试或 stale-reasoning 补发；单模型故障不暂停整个 Go Key。共享额度可能让整组都不可用。
- 总预算为 `retry.timeout_seconds`，`rotation.first_output_seconds` 限制每次等待有效输出，默认 30 秒、范围 1–300 秒。非流式需在此期限内读完有效响应。
- 流式前导事件暂存到有实际输出/正常结束后才提交；提交前可重试，提交后错误保留内容、发送协议 SSE 错误并终止。不会在一条流拼接替代答案。客户端取消不触发切换或暂停。
- 响应 `model` 保持请求别名，`X-Resolved-Model` 指实际模型；Token 归实际模型。记录保存分类原因，不保存请求体、Key 或上游错误正文。

| 管理接口 | 内容 |
| --- | --- |
| `GET /api/rotation` | 两组顺序/别名、当前模型、暂停、持久化状态、最近 100 次尝试及每组 30 次切换。 |
| `PUT /api/rotation` | 提交 `sota`/`sweet` 的完整 `alias`、`order` 和 `first_output_seconds`；只能组内排序，不可丢成员。 |
| `POST /api/rotation/current` | `{"group":"sota","model":"glm-5.3"}`，切换并清除该模型暂停。 |

别名 1–64 位字母、数字、`.`、`_`、`-`，首位为字母/数字，不得重复或与真实模型冲突。目录变化导致提交成员不匹配时，先刷新再排序。

配置存于 `rotation`，运行状态为 `config.json.rotation.json`。热重载共享状态；首次无法读取/写入状态会拒绝启动，后续写入失败在管理页面提示。

## Playground、日志与监控

Playground 通过 `POST /api/debug/inference` 在进程内调用同一网关，强制非流式。以下为自动路由示例：

```json
{"protocol":"chat","key":{"mode":"auto"},"request":{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}}
```

指定 Key 时用 `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}` 替换 `key`，ID 来自 `GET /api/debug/models`。选定 Key 只做一次上游尝试，不匿名、不换 Key、不跨 Tier、不 reasoning 重放。可选 `relay` 用已知索引指定代理，例如 `{"index":0}`。索引必须来自当前诊断目录。

诊断不修改生产 Key 冷却、失败计数、代理健康或绑定，但会发真实请求、消耗额度/credits 并进入监控。管理响应成功封装为 HTTP 200，以 `ok`、`http_status`、`request_id`、`route`、`response` 表示实际结果。所选 Key 另有 `selected_key`、`key_test`，如 `usable`、`rejected`、`rate_limited`、`transport_error`、`upstream_error`、`request_error`、`unavailable`；格式/认证/限流错误仍使用对应 HTTP 状态。

日志位于 `GET /api/logs?after=SEQUENCE&limit=COUNT` 与 `GET /api/logs/stream`。SSE 支持 `Last-Event-ID` 或 `after` 续接；过期/丢失事件会有 `gap`，无法补回已淘汰数据。Session 失效后服务端结束日志流。

`GET /api/monitor` 提供聚合请求、Token、上游尝试与资源。`GET /api/debug/models` 提供路由、Key 指纹、模型元数据、代理和最近诊断结果。Token 只使用上游报告，不估算缺失值；输入包括缓存读写，cached 单列缓存读。`usage.server_keys` 以本地 Key 分组，`playground` 单列。

请求结果表示完整推理：流失败即使 HTTP 200 也记为 `stream_error`，调用方取消为 `client_canceled`。上游 attempt 只测量收到响应头为止的交换，成功率和延迟不可当成整条回答成功率。

## 持久化、健康与安全

| 数据 | 位置与生命周期 |
| --- | --- |
| 配置/上一版本 | `config.json`、`config.json.bak`，包含敏感信息。 |
| 模型/价格缓存 | `config.json.models.catalog.json`、`config.json.models.dev.json`。 |
| 免费可用性 | `config.json.<Zen上游指纹>.availability.json`，按上游隔离。 |
| 轮转状态 | `config.json.rotation.json`。 |
| Session、Token 累计、日志环、最近 Playground | 进程内存，重启清空。 |
| 请求/尝试详情 | 最近一小时，最多 10,000/20,000 条，接口各最多返回 500 条。 |
| 长期日志 | stdout JSON，须自行安全收集与保留。 |

文件前缀以实际 `-config` 路径为准。多个进程不共享 Session、监控或内存计数，避免并发修改同一状态文件。

`GET /healthz` 位于推理端口，无需认证，不触发上游检查或监控计数。目录未就绪/为空、WARP 无活跃出口、必要代理无健康节点时返回 503；可用但过期的目录可返回 200、`degraded`，过期阈值为两倍刷新间隔且至少 60 秒。OAuth 重登需求会体现在 issues，但就绪不等于下次推理一定成功。公开信息为状态和计数，不暴露 Key/代理地址。

本地使用应限制为 loopback；网络管理须用 HTTPS 和访问限制。程序自身不终止 TLS。配置/备份含手动 Key、代理密码与 OAuth Token，须用文件权限或 Windows ACL 限制，分享源码时排除。调试请求体输出虽脱敏且截断，仍可能含私人对话，应按需短时开启。不要把完整配置、reveal、Cookie、授权头或真实推理内容作为排错附件。

## 验证与维护

本地开发检查：

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

正常部署验收需核实管理登录与 CSRF、Key 停用/恢复、配置保存与监听重启、模型发现、三种消息协议和 SSE 错误、轮转状态，以及所需的真实 OAuth/上游功能。真实推理、OAuth、WARP 和额度查询可能依赖账号权限并消耗额度；静态核对不能替代这些验证。

升级前安全备份程序、实际配置和状态，停止进程后替换程序；回退需恢复兼容的配置与状态。Compose 用户注意保存卷中的实际文件。不要把凭证、日志、运行状态或本地验证产物加入源码。
