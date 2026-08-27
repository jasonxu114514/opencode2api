# opencode2api

`opencode2api` 是一个使用 Go 编写的 OpenCode Zen / Zen Go 协议代理。它对外提供标准 OpenAI 与 Anthropic API，并自动添加 OpenCode 客户端请求头。

主要功能：

- 支持 OpenAI Chat Completions、Responses 和 Models API
- 支持 Anthropic Messages API
- 支持普通响应和 SSE 流式响应
- 支持文本、图片、thinking/reasoning、工具定义、工具调用和工具结果转换
- 分离配置 Zen key 池与 Zen Go key 池
- 支持无需上游 key 的 Zen 匿名模式，免费模型先走匿名通道，失败后按 `prefer` 顺序回退 Zen/Go key
- 按配置周期同步 Zen/Go `/v1/models`，并从 OpenCode `models.opencode.ai/api.json` 自动同步每个 Tier 的原生协议与不支持模型；不把模型 ID 硬编码在程序中
- 每 24 小时从 models.dev 更新 OpenCode 成本与弃用信息；models.dev 零成本或名称含 `free` 任一条件即可判定免费
- 模型同时存在于两个上游时按 `prefer` 配置排列 Go/Zen key 的首选与回退顺序（默认 Go）
- 支持直连、HTTP、HTTPS、SOCKS5 和 SOCKS5H 代理
- 支持从文本文件读取代理池，并与配置内的代理合并、去重
- `config.json` 支持 `//` 和 `/* ... */` 注释
- 将 key 自动均衡绑定到代理，保持连接亲和性
- 使用稳定会话哈希保持同一会话的 key/proxy 亲和性，并在节点故障时自动回退
- 代理失败后自动迁移绑定，key 失败后进行短时冷却
- 根据真实上游流量识别代理故障，并每 15 分钟通过 Cloudflare trace 并行复查异常代理
- 为不同会话生成不同的 OpenCode 会话 ID，并支持 `x-opencode-session`、`x-session-id` 和 `conversation-id` 显式指定会话
- 内置独立端口 Field Manual WebUI，可管理配置、查看 Token/上游指标、诊断路由、运行三协议 Playground 与订阅实时日志
- WebUI 使用账号密码、服务端 session、HttpOnly Cookie、CSRF 与登录限速保护
- WebUI 保存后原子写入配置并热切换 Gateway；无效配置不会影响当前流量
- stdout 输出结构化 JSON 日志；请求、Token、上游尝试与最近一小时滚动指标仅保存在进程内存中

## API 路径

| 方法 | 路径 | 协议 |
| --- | --- | --- |
| `GET` | `/v1/models` | OpenAI 模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions |
| `POST` | `/v1/responses` | OpenAI Responses |
| `POST` | `/v1/messages` | Anthropic Messages |
| `GET` | `/healthz` | 健康检查 |

`/healthz` 无需 API key，返回服务版本以及模型目录、Zen/Go key、匿名开关和代理池的汇总状态，不会暴露 key 或代理地址。模型目录尚未完成首次刷新、已经过期、没有可暴露模型或没有健康代理时返回 HTTP `503`；其余情况返回 `200`。

模型目录的过期阈值为 `models.refresh_seconds` 的两倍，且不低于 60 秒。刚启动时短暂返回 `503 starting` 属于正常现象，模型列表首次刷新成功后会变为 `200 ok`。

健康检查不会计入请求指标。它也不会触发 models.dev 刷新或写入任何监控记录。

## WebUI

示例配置会在独立的 `8081` 端口启动管理界面：

```text
http://服务器地址:8081
```

首次账号为 `admin`，密码来自 `webui.password`。服务第一次成功启动时会使用 Argon2id 将密码转换为带盐哈希，写入 `webui.password_hash`，并从配置中删除明文密码。请在首次登录后立即修改示例密码。

Field Manual WebUI 包含运行桌面、六步首次运行检查、接入手册、Token 用量、三协议 Playground、路由诊断、配置、事件日志和账号安全页面。接入手册会生成 Chat、Responses、Anthropic、Python 和 JavaScript 示例，但始终使用 `YOUR_API_KEY` 占位符，不把真实 Server Key 写入页面。

Token 页面展示用量覆盖率、每分钟趋势、模型排行与 Zen/Go Tier 分布。诊断页展示 models.dev 状态、模型原生协议与匿名判断来源、Key/代理状态、逐次上游尝试以及最近一次 Playground 追踪；实时日志通过 SSE 推送。所有动态管理数据都以 DOM 文本节点渲染。

监控、上游尝试、最近 Playground 结果和日志仅保存在内存。**进程重启会清空全部监控与诊断历史，包括 lifetime 累计。** stdout 日志仍可由 Docker 或日志平台收集；models.dev 价格快取是独立的磁盘兼容资料，不属于监控历史。

### 监控字段

登录 WebUI 后，`GET /api/monitor` 返回以下顶层字段；该管理 API 只在 `webui.listen` 上提供，并受 Session 保护：

| 字段 | 内容 |
| --- | --- |
| `version` | 当前程序版本。 |
| `metrics` | 原有请求统计：进程启动时间、活跃请求/流、lifetime 与最近一小时成功率/延迟、端点/模型/Tier/状态码聚合，以及 60 个每分钟序列点。 |
| `usage` | Token 的 `lifetime` 与 `last_hour` 统计。包含 `requests`、`reported`、`coverage`、总 Token 和按模型/Tier 聚合。 |
| `upstream` | 上游请求级路由 `requests`、尝试级 `lifetime`、`last_hour` 与 `recent`。按 Tier、匿名/Key 通道、Key 尾码聚合。 |
| `resources` | 模型目录、Key 冷却、脱敏代理节点、匿名开关和 models.dev metadata 状态。 |

`usage.*.tokens` 与模型/Tier 项均包含 `input_tokens`、`output_tokens`、`cached_tokens`、`reasoning_tokens`、`total_tokens`。其中 `input_tokens` 统一表示包含缓存读写的总输入，`cached_tokens` 单独表示缓存读取量。`metrics.series` 的每分钟点也包含这些 Token 字段和 `usage_reported`。普通 JSON、同协议 SSE 与跨协议 SSE 响应都会解析上游 usage；`coverage` 是收到 usage 的推理请求数除以已建立上游路由的推理请求数。上游未提供 usage 时不会估算 Token。

每个 `upstream.requests` 项对应一个已完成的推理请求，包含最终实际使用的 Tier、通道、Key 尾码（或 `anonymous`）、尝试次数、HTTP 状态、耗时、成功标记和结果分类。每个 `upstream.recent` 项则对应一次上游尝试，包含时间、Request ID、模型、Tier、尝试序号、匿名标记、通道、Key 尾码、`proxy_node`、HTTP 状态、耗时、成功标记和结果分类。真实 Key 在日志和 WebUI 中只显示最后 5 个字符；配置接口的内部 secret ID 仍使用 SHA-256 稳定指纹。代理 URL 的认证信息会被移除，字段名称明确为代理节点而非出口 IP。

lifetime 从当前进程启动开始；last hour 使用 60 个一分钟 Bucket。管理响应最多返回最近 500 个请求路由和 500 次上游尝试，WebUI 默认各显示最后 100 条。数据不会写入配置、metadata 快取或其他数据库。

### Playground 与诊断 API

以下接口受管理 Session 保护；POST 还要求当前 CSRF Token，并按客户端每分钟最多 12 次限制：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/api/debug/models` | 模型路由、原生协议、协议来源、Zen/Go 可用性、匿名资格/来源、成本与 metadata 状态。 |
| `POST` | `/api/debug/inference` | 通过真实 Gateway 发起 Chat、Responses 或 Anthropic 非流式诊断请求。 |

请求格式：

```json
{
  "protocol": "chat",
  "request": {
    "model": "example-model",
    "messages": [{"role": "user", "content": "Hello"}]
  }
}
```

`protocol` 可为 `chat`、`responses` 或 `anthropic`。服务端无条件把内层 `stream` 改为 `false`。诊断结果统一包含 `ok`、真实 `http_status`、`duration_ms`、`request_id`、`route` 和原始协议 `response`。一旦诊断调用已执行，管理接口本身返回 HTTP `200`，即使上游结果是 4xx/5xx；因此错误正文、路由和 Request ID 仍可一起查看。外层请求格式、Session、CSRF 或限速错误仍使用相应管理 HTTP 状态。

诊断响应不会包含本地 Server Key、上游 Key、Cookie、密码、Authorization 或代理凭据；响应中同名敏感字段及已配置敏感值会在返回浏览器前清除。最近一次结果只保存在当前管理进程内。

## 编译

需要 Go 1.24 或更高版本。

```bash
go build -o opencode2api ./
```

## 下载

预编译的 Windows、Linux 和 macOS 可执行文件可从 [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases) 下载。

## GHCR / Docker Compose 部署

正式镜像发布在 `ghcr.io/jasonxu114514/opencode2api`


```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
# 编辑 server_keys、zen_keys/go_keys，并修改 webui.password
docker compose pull
docker compose up -d
```

首次启动会把 `config.json` 导入 `opencode2api-state` 命名卷。之后应通过 WebUI 修改配置；如需再次从宿主机导入配置，可执行：

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

健康检查、WebUI 和日志：

```bash
curl http://127.0.0.1:8080/healthz
# 浏览器打开 http://127.0.0.1:8081
docker compose logs -f
```

可通过环境变量固定镜像版本和修改宿主机端口：

```bash
OPENCODE2API_VERSION=v1.2.3 OPENCODE2API_PORT=18080 OPENCODE2API_WEBUI_PORT=18081 docker compose up -d
```

不使用 Compose 时也可直接运行 GHCR 镜像：

```bash
docker volume create opencode2api-state
docker run -d --name opencode2api --restart unless-stopped \
  -p 8080:8080 -p 8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  ghcr.io/jasonxu114514/opencode2api:latest
```

## 配置

复制示例配置：

```bash
cp config.example.json config.json
```

然后编辑 `config.json`：

```json
{
  "listen": "127.0.0.1:8080",
  "server_keys": ["change-this-local-key"],
  "zen_keys": ["sk-your-zen-key"],
  "go_keys": [],
  "anonymous": false,
  "prefer": "go",
  "proxyfile": "",
  "proxies": ["direct"],
  "upstream": {
    "zen": "https://opencode.ai/zen",
    "go": "https://opencode.ai/zen/go"
  },
  "retry": {
    "max_attempts": 3,
    "timeout_seconds": 300
  },
  "models": {
    "refresh_seconds": 300,
    "protocols": {}
  },
  "performance": {
    "max_idle_conns": 2048,
    "max_idle_conns_per_host": 256,
    "max_conns_per_host": 0,
    "idle_conn_timeout_seconds": 120,
    "connect_timeout_seconds": 5,
    "failure_cooldown_seconds": 15
  },
  "logging": {
    "level": "info",
    "ring_size": 2000
  },
  "webui": {
    "enabled": true,
    "listen": "0.0.0.0:8081",
    "username": "admin",
    "password": "change-this-admin-password",
    "session_ttl_minutes": 720
  }
}
```

### 基础字段

| 字段 | 含义 |
| --- | --- |
| `listen` | 本地监听地址。默认建议使用 `127.0.0.1:8080`，避免服务直接暴露到公网。 |
| `server_keys` | 调用本代理时使用的本地 API key 列表。它们只用于本地鉴权，不会发送给 OpenCode。 |
| `zen_keys` | OpenCode Zen API key 池。允许配置多个 key。 |
| `go_keys` | OpenCode Zen Go API key 池。没有 Go key 时可以使用空数组。 |
| `anonymous` | 是否启用 Zen 匿名模式，默认 `false`。models.dev 判定为零成本，或模型名称包含 `free`，任一条件成立即可进入匿名通道。 |
| `prefer` | 模型同时存在于 Zen 与 Go 时认证 Key 的尝试顺序，值为 `go` 或 `zen`，默认 `go`。首选 Tier 失败后回退另一 Tier；仅存在于某一池时只尝试该池。 |
| `proxyfile` | 可选代理池文件路径。相对路径以 `config.json` 所在目录为基准；内容会追加到 `proxies` 并去重。 |
| `proxies` | 上游代理列表。支持 `direct`、`http://`、`https://`、`socks5://` 和 `socks5h://`。URL 可以包含代理用户名和密码。 |

`server_keys` 至少需要一个值。`anonymous` 为 `false` 时，`zen_keys` 和 `go_keys` 至少有一个池不能为空；启用匿名模式后两个上游 key 池可以同时为空。

### Zen 匿名模式

OpenCode 客户端在没有配置 Zen key 时使用固定的 `public` 凭证；Zen 服务端将它转换为匿名请求，并按出口 IP 对允许匿名访问的模型限流。本项目使用相同协议：OpenAI/Responses 上游请求发送 `Authorization: Bearer public`，Anthropic 上游请求发送 `x-api-key: public`。

启用 `anonymous` 后，以下任一条件成立即视为免费模型：

1. models.dev 已知输入、输出成本都为 `0`，且模型未弃用；不要求名称包含 `free`。
2. 模型 ID 大小写不敏感地包含 `free`；即使 metadata 尚未就绪、缺少该模型或显示为付费，也仍按名称条件视为免费。

免费模型先走匿名 Zen，非免费模型完全跳过匿名通道。匿名请求遇到任何错误——包括传输错误、4xx、5xx 或其他非 2xx 响应——都会继续切换下一个当前可用的 proxy；每个可用 proxy 最多尝试一次。anonymous 阶段不受 `retry.max_attempts` 提前截断，只有可用 proxy 全部耗尽后才进入认证 Key 阶段。认证 Tier 按 `prefer` 排序：`prefer: "go"` 为 Go key → Zen key，`prefer: "zen"` 为 Zen key → Go key；首选 Tier 仍不成功时才尝试另一个实际提供该模型且配置了 Key 的 Tier。Zen/Go Key 阶段各自拥有 `retry.max_attempts` 预算。监控中的 `proxy_node` 表示所选代理节点，不代表或推断实际出口 IP。

只有匿名通道、且 `zen_keys` 与 `go_keys` 都为空时，`/v1/models` 只展示按上述规则可匿名使用的模型。只要配置了任一真实上游 Key，模型列表仍展示该 Key 路由可用的完整模型集合。

models.dev 使用固定 30 秒超时，每 24 小时刷新一次。标准地址为 `https://models.dev/api.json`；规范化后的 OpenCode 模型成本缓存在 `config.json.models.dev.json`，以 `0600` 权限和同目录临时文件原子替换。启动会先读取可用快取，再尝试联网更新；更新失败不会丢弃旧资料，错误、更新时间与过期状态可在监控资源和诊断页查看。

### key 与代理分配规则

只需要直连时使用：

```json
"proxies": ["direct"]
```

SOCKS5 代理示例：

```json
"proxies": ["socks5://127.0.0.1:1080"]
```

多个代理示例：

```json
"proxies": [
  "http://user:password@127.0.0.1:7890",
  "socks5://127.0.0.1:1080"
]
```

也可以从文本文件加载代理池：

```json
{
  "proxyfile": "proxies.txt",
  "proxies": ["direct"]
}
```

`proxies.txt` 每行填写一个代理。支持空行、以 `#`、`;` 或 `//` 开头的整行注释，也支持在代理后使用空格加这些标记写行尾注释：

```text
# HTTP 代理
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # 备用代理
```

配置中的 `proxies` 会先加载，随后加载 `proxyfile`，重复项只保留第一次出现的位置。如果两个来源都为空，则仍使用 `direct`。`config.json` 本身支持 `//` 单行注释和 `/* ... */` 块注释；引号内的 `https://` 等内容不会被当作注释。

### `upstream`

| 字段 | 含义 |
| --- | --- |
| `upstream.zen` | Zen 上游根地址，通常保持为 `https://opencode.ai/zen`。 |
| `upstream.go` | Zen Go 上游根地址，通常保持为 `https://opencode.ai/zen/go`。 |

### `retry`

| 字段 | 含义 |
| --- | --- |
| `retry.max_attempts` | 每个认证 Key Tier 的最大尝试次数，包含第一次请求。Tier 内部遇到网络错误、认证失败、限流或 5xx 会切换节点；其他 4xx 会结束当前 Tier。只要还有另一个可用 Tier，当前 Tier 的最终失败会继续按 `prefer` 顺序回退。anonymous 不使用此上限，而是将每个当前可用 proxy 各尝试一次。 |
| `retry.timeout_seconds` | 单个客户端请求的总超时时间，同时用于限制上游响应头等待时间。 |

流式响应一旦已经向客户端输出数据，就不会切换节点重新生成，避免拼接两个不同的响应。

### `models`

| 字段 | 含义 |
| --- | --- |
| `models.refresh_seconds` | 重新读取 Zen 和 Go 模型列表及 OpenCode 能力目录的间隔秒数。两个模型列表与能力目录会并发刷新。 |
| `models.protocols` | 手动指定模型的原生协议。值只能是 `chat`、`responses` 或 `anthropic`，并覆盖自动同步结果。通常保持为空。 |


模型协议覆盖示例：

```json
"protocols": {
  "custom-model": "chat"
}
```

自动协议来源是 `https://models.opencode.ai/api.json`，并用 OpenCode 官方 Zen/Go endpoint 文档补充具体路径：模型的 `provider.npm`（或 Tier 默认 `npm`）及文档 endpoint 会映射为 OpenAI Responses、Anthropic Messages 或 OpenAI-compatible Chat。若能力目录暂时无法更新，服务会保留进程内上一份能力快照；首次启动且没有能力快照时，不会暴露能力未知的模型，避免把不支持的模型误路由到 Chat。手动 `models.protocols` 可用于上游实验模型。

模型同时存在于 Zen 与 Go 时按 `prefer` 配置排列认证 Key 顺序：值为 `go` 时先 Go 后 Zen，值为 `zen` 时先 Zen 后 Go（默认 `go`）。首选 Tier 失败后才回退另一 Tier；仅存在于某一池时只使用该池的 key。免费模型在这条认证顺序之前额外尝试匿名 Zen。

### Thinking 工具历史兼容

所有请求都会经过同一个上游请求准备流程，同协议转发和跨协议转换不再使用两套分支。通过 Chat Completions 或 Anthropic Messages API 调用 DeepSeek、Kimi/Moonshot 或 MiMo 模型时，代理会按上游的目标协议规范化 assistant 工具历史：Chat 补全缺失或空的 `reasoning_content`；Anthropic 保留有效 thinking 文本、为缺失或空的 thinking 补充兼容占位内容、将 `redacted_thinking` 转为普通 thinking，并移除这些兼容端点不接受的 `signature`。显式启用 reasoning/thinking 的别名模型也会启用该处理，普通非 reasoning 请求不会被修改。

跨协议桥接会区分 Chat/Responses 的 system 与 developer 指令，在 Anthropic 目标中按顺序合并为 system 内容；reasoning effort 会转换为兼容 thinking 预算。工具选择、空参数 `{}`、停止原因，以及 SSE 中延迟到达的工具名称、参数分片和完成事件也会转换到目标协议的对应形态。

流式响应会兼容 Chat 上游的 `delta.reasoning_content` 与 `delta.reasoning`。Anthropic 上游的 `event: error`、Responses 上游的 `response.failed` 以及 Chat 的错误类 `finish_reason` 会转换为目标协议的结构化错误事件，不会伪装成正常结束或静默断流。`redacted_thinking` 在支持加密 reasoning 的 Responses 目标中保留 `encrypted_content`，在无法表达加密块的 Chat/Anthropic 目标中使用 `[redacted thinking]` 明确占位。

协议文档未定义或当前 bridge 无法无损表达的输入 content block（例如未实现的音频/文件类型）会返回明确的转换错误，不再静默丢弃内容。

### `performance`

| 字段 | 含义 |
| --- | --- |
| `performance.max_idle_conns` | 所有上游连接池允许保留的最大空闲连接数。 |
| `performance.max_idle_conns_per_host` | 每个上游主机允许保留的最大空闲连接数。 |
| `performance.max_conns_per_host` | 每个主机的最大并发连接数。`0` 表示不设置上限。 |
| `performance.idle_conn_timeout_seconds` | 空闲连接在连接池中保留的时间。 |
| `performance.connect_timeout_seconds` | 与上游或代理建立 TCP 连接的超时时间。 |
| `performance.failure_cooldown_seconds` | 连接失败、认证失败、限流或 5xx 后节点的基础冷却时间。连续失败会指数增加冷却时间。 |

### `logging`

| 字段 | 含义 |
| --- | --- |
| `logging.level` | 日志级别，支持 `debug`、`info`、`warn` 和 `error`，可通过 WebUI 热切换。 |
| `logging.ring_size` | WebUI 最近日志环容量，范围 100–50000，默认 2000。stdout 不受此容量限制。 |

每条 stdout 日志都是单行 JSON，包含时间、级别、组件、事件以及适用的 request ID、模型、tier、状态码、耗时、重试次数和实际使用的 Key 尾码。已建立上游路由的请求会以 `info` 级别记录 `request_routed` 事件；真实 Key 只显示最后 5 个字符，anonymous 请求显示为 `anonymous`。普通“请求完成”事件仍使用 `debug` 级别；警告和错误按原级别输出。日志不会输出完整上游 key、本地 key、Authorization、Cookie、代理认证信息或请求消息正文。

### `webui`

| 字段 | 含义 |
| --- | --- |
| `webui.enabled` | 是否在独立端口启动管理服务。旧配置未包含该段时默认关闭。 |
| `webui.listen` | 管理服务监听地址，示例为 `0.0.0.0:8081`。 |
| `webui.username` | 单一管理员账号。 |
| `webui.password` | 仅用于首次初始化的明文密码，至少 10 个字符；启动后自动删除。 |
| `webui.password_hash` | 自动生成的 Argon2id 哈希，不应手动编辑，也不会由 WebUI API 返回。 |
| `webui.session_ttl_minutes` | 登录 session 有效时间，范围 5–10080 分钟。 |

WebUI 中普通配置响应只包含 key 尾码/指纹及脱敏 proxy；运行桌面和路由诊断会显示每个请求最终使用的 Key 最后 5 个字符或 `anonymous`。需要查看完整值时必须再次输入管理密码，敏感响应禁止浏览器缓存。

### 配置保存与热重载

WebUI 保存时先解析并验证完整候选配置、创建新的连接池和 Gateway，然后写入临时文件、保留 `config.json.bak` 并替换 `config.json`，最后原子切换新请求使用的运行实例。写入或初始化失败时旧实例继续工作；切换前已开始的请求不会中断。

keys、proxy、上游、重试、模型、性能、优先 tier 和日志级别会立即生效。`listen`、`webui.listen` 与 `webui.enabled` 会保存但需要重启进程。WebUI 也提供“从磁盘重载”，外部编辑后的配置仍会经过相同的验证与回滚流程。保存后的 JSON 会被规范化，原有注释不会保留。


## 会话 ID

代理会为上游添加 OpenCode 使用的 `User-Agent`、`x-opencode-client`、`x-opencode-session`、`x-opencode-request` 和 `x-opencode-project` 请求头。

- 每个请求使用不同的 `x-opencode-request`，同一次请求的重试保持不变。
- 优先使用客户端提供的 `x-opencode-session`、`x-session-affinity`、`X-Session-Id`、`x-session-id`、`conversation-id`、`conversation_id` 或 `metadata.session_id` 生成会话 ID。
- 没有显式会话标识时，使用第一条用户消息生成稳定会话 ID，使同一段多轮对话保持一致。
- 如果两个独立会话的第一条消息完全相同，建议由客户端发送不同的 `x-session-id`，以确保两个会话严格分离。
- 上游请求同时发送 `x-session-affinity`、`X-Session-Id` 和可选的 `x-parent-session-id`，以兼容 OpenCode 近期的会话关联要求。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区一直以来的支持。
