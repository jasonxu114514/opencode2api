# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md)

使用 Go 编写的 **OpenCode Zen / Zen Go API 网关**。对外提供 Chat Completions、Responses 和 Anthropic Messages 接口，按上游原生协议转换请求，并统一管理上游 Key 与代理。

WebUI 内嵌在可执行文件中，运行服务无需 Node.js 或数据库。

## 功能

- 三种推理协议之间的普通 JSON 响应与 SSE 流式转换。
- 文本、图片、reasoning、函数工具、工具调用和工具结果；目标协议允许时支持文件内容转换。
- 独立的 Zen / Go Key 池，可配置优先级、重试和会话亲和性。
- 使用 OpenCode `public` 凭证的可选 Zen 匿名通道。
- 直连、HTTP、HTTPS、SOCKS5、SOCKS5H 连接，以及代理文件。
- 动态模型发现、原生协议与能力信息、磁盘缓存。
- 独立管理端口，支持配置编辑、Playground、路由诊断、Token 统计和实时日志。
- 配置热重载：先验证新配置，再切换新请求使用的 Gateway。
- Go 共享套餐额度看板，显示剩余百分比、重置时间和可用模型。
- API Key 生成、自定义、命名、启停和删除，变更立即生效。
- `sota` / `sweet` 分组别名，支持模型排序、故障切换、流式输出保护和状态恢复。

新增页面及调用示例见[管理控制台说明](docs/management.zh-CN.md)。

## 快速开始

从 [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases) 下载可执行文件，或使用 **Go 1.24 及以上版本**编译：

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

启动前编辑 `config.json`：

1. 将 `server_keys` 替换为自己的本地 API Key。
2. 填入 Zen 或 Go Key；也可以设置 `anonymous: true`，并清空两个上游 Key 数组。
3. 替换 `webui.password`。示例配置启用了 WebUI，用户名为 `admin`。

```bash
./opencode2api -config config.json
```

Windows 下使用 `Copy-Item config.example.json config.json` 复制配置，执行 `go build -o opencode2api.exe ./cmd/opencode2api` 编译，再运行 `.\opencode2api.exe -config config.json`。

示例配置的 API 监听 `127.0.0.1:8080`，WebUI 监听 `0.0.0.0:8081`。本机可访问 `http://localhost:8081`。通过网络访问管理界面时，请限制访问范围，并使用 HTTPS 反向代理。

命令行参数：

| 参数          | 默认值        | 用途                  |
| ------------- | ------------- | --------------------- |
| `-config`     | `config.json` | 配置文件路径。        |
| `-listen`     | 未设置        | 覆盖 API 监听地址。   |
| `-web-listen` | 未设置        | 覆盖 WebUI 监听地址。 |

配置所在目录需要可写，以便迁移密码、保存配置和写入模型缓存。

## Docker 部署

已发布镜像：`ghcr.io/jasonxu114514/opencode2api`。

```bash
cp config.example.json config.json
# 启动前填写 Key，并替换 WebUI 密码。
docker compose up -d
docker compose logs -f
```

Compose **仅在首次启动时**将宿主机配置导入 `opencode2api-state` 命名卷。后续建议通过 WebUI 修改配置。需要重新导入宿主机文件时：

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

容器内的服务进程使用普通用户运行。Compose 启用只读根文件系统，并提供可写状态卷和临时 `/tmp` 文件系统。

| Compose 环境变量            | 默认值   | 作用                                 |
| --------------------------- | -------- | ------------------------------------ |
| `OPENCODE2API_VERSION`      | `latest` | 镜像标签；可指定发行标签来固定版本。 |
| `OPENCODE2API_PORT`         | `8080`   | 映射到容器 8080 的宿主机端口。       |
| `OPENCODE2API_WEBUI_PORT`   | `8081`   | 映射到容器 8081 的宿主机端口。       |
| `OPENCODE2API_LISTEN`       | 未设置   | 显式覆盖容器内 API 监听地址。        |
| `OPENCODE2API_WEBUI_LISTEN` | 未设置   | 显式覆盖容器内 WebUI 监听地址。      |

后两项在容器内对应 `LISTEN_ADDRESS` 与 `WEBUI_LISTEN_ADDRESS`。值为空时使用配置文件中的地址。首次初始化配置时，入口脚本会将示例 API 地址 `127.0.0.1:8080` 改为 `0.0.0.0:8080`，使其能够通过发布端口访问。

修改宿主机端口不会改变容器内监听地址。如果修改容器内 API 端口，还需要同步调整端口映射和镜像健康检查；默认健康检查使用 8080 端口。

构建并运行本地镜像：

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

在 Docker 中使用 `proxyfile` 时，代理文件也需要挂载到容器内配置指定的位置。

## API 调用

`server_keys` 用于客户端调用本地网关，与 `zen_keys`、`go_keys` 相互独立，不会作为上游认证凭证发送。

请求可以使用 `Authorization: Bearer YOUR_LOCAL_API_KEY` 或 `x-api-key: YOUR_LOCAL_API_KEY`。健康检查无需认证。

| 方法 | 路径                   | 用途                       |
| ---- | ---------------------- | -------------------------- |
| GET  | `/v1/models`           | 当前配置下可以路由的模型。 |
| POST | `/v1/chat/completions` | Chat Completions。         |
| POST | `/v1/responses`        | Responses。                |
| POST | `/v1/messages`         | Anthropic Messages。       |
| GET  | `/healthz`             | 就绪状态与资源汇总。       |

先获取可用模型：

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"
```

将以下示例中的 `MODEL_ID` 替换为模型列表返回的 ID。

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

需要流式响应时，在请求体中加入 `"stream": true`，并使用 `curl -N`。响应通过 `x-request-id` 提供请求关联标识。API 请求体上限为 32 MiB。

### 协议兼容边界

客户端协议与上游原生协议相同时，会保留供应商特有字段。跨协议请求经过统一中间结构转换，部分参数没有等价表达。例如，Chat 的 JSON 输出约束与 `seed` 不会被转发到 Anthropic Messages。不支持的内容块或工具类型可能被拒绝。

网关仅实现上表中的接口，不提供 embeddings、文件上传、图像生成、Responses 查询或取消接口。网关不保存会话历史；客户端需要提交历史，或使用实际上游支持的相关能力。

## 路由与模型

### 模型发现

服务按配置周期刷新 Zen / Go 的 `/v1/models` 和 OpenCode [能力目录](https://models.opencode.ai/api.json)，分别记录两个 Tier 的原生协议与模型限制。OpenCode 的 Zen / Go 文档作为协议发现的回退来源。

可通过 `models.protocols` 覆盖自动发现结果：

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

协议值仅允许 `chat`、`responses`、`anthropic`。使用不支持的原生协议的模型会从模型列表中隐藏，除非配置了覆盖值。

成本和弃用信息来自 [models.dev](https://models.dev/api.json)，每 24 小时刷新一次；每个 HTTP 客户端的拉取超时为 30 秒。刷新失败时保留已有数据。

### 匿名通道与回退

启用 `anonymous: true` 后，满足以下任一条件的模型可以进入 Zen 匿名通道：

- 模型 ID 包含 `free`，不区分大小写。
- models.dev 中的输入、输出成本均为零，且模型未弃用。

这只是网关的路由判断，上游仍可能拒绝或限制请求。匿名请求在上游认证头中使用 `public` 凭证。

路由顺序：

1. 符合条件的模型先尝试匿名通道，每个可用代理最多一次。
2. 按 `prefer` 顺序尝试认证通道，仅包含已配置 Key 且可以提供该模型的 Tier。
3. 每个认证 Tier 分别使用 `retry.max_attempts` 预算。

匿名阶段不受 `retry.max_attempts` 截断，但所有阶段共享请求总超时。网络错误、认证失败、限流和服务端错误可以触发 Key 轮换；其他 4xx 会结束当前 Tier，仍可继续尝试另一个可用 Tier。

每个 Tier 使用自己的原生协议编码请求。流开始后不再切换节点重新生成。识别到过期的 Responses reasoning 引用时，可执行一次修复重试；指定 Key 的诊断不会执行该重试。

只有匿名通道可用时，`/v1/models` 仅展示符合匿名条件的模型。

### 会话与代理

Key 初始化时均衡分配到代理。真实流量可以触发代理检查、Key 重新绑定和失败冷却。已经标记异常的代理，每 15 分钟通过 Cloudflare trace 复查一次。

服务使用稳定会话哈希选择首选 Key 或匿名代理。建议通过 `x-session-id` 区分独立会话，同时支持 `x-opencode-session`、`x-session-affinity`、`conversation-id`、`conversation_id` 与 `metadata.session_id`。

没有显式会话 ID 时，使用第一条用户消息生成会话标识，因此开场内容相同的会话可能共享亲和性。调整 Key 或代理池成员后，原有会话选择的节点可能变化。

失败冷却按指数增长，最高为 `performance.failure_cooldown_seconds` 的八倍；如果上游的 `Retry-After` 更长，则使用更长时间。所有认证 Key 都处于冷却时，路由仍可能尝试最早结束冷却的 Key；仍在冷却的匿名节点会被跳过。

## 配置参考

完整起始配置见 [config.example.json](config.example.json)。配置支持 `//` 和 `/* ... */` 注释，未知字段和无效取值会被拒绝。

### Key、监听地址与路由

| 字段                     | 默认值或要求                              |
| ------------------------ | ----------------------------------------- |
| `listen`                 | `127.0.0.1:8080`。                        |
| `server_keys`            | 至少一个本地 Key。                        |
| `zen_keys`、`go_keys`    | 未启用匿名模式时，至少需要一个上游 Key。  |
| `anonymous`              | `false`。                                 |
| `prefer`                 | `go`；可选 `go`、`zen`。                  |
| `upstream.zen`           | `https://opencode.ai/zen`。               |
| `upstream.go`            | `https://opencode.ai/zen/go`。            |
| `proxies`                | 两个代理来源都为空时，使用 `["direct"]`。 |
| `proxyfile`              | 可选；相对路径基于配置文件所在目录解析。  |
| `models.refresh_seconds` | `300`；最小为 1。                         |
| `models.protocols`       | `{}`；按模型 ID 覆盖原生协议。            |

代理支持 `direct`、`http://`、`https://`、`socks5://` 和 `socks5h://`，URL 可包含认证信息。配置内代理先加载，再追加 `proxyfile` 内容，并按首次出现的顺序去重。

代理文件每行一个地址，允许空行和注释：

```text
# 首选代理
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # 备用代理
direct
```

支持 `#`、`;`、`//` 注释标记，标记需要位于行首或空白字符之后。

### 超时与连接池

| 字段                                    | 默认值 | 含义                                             |
| --------------------------------------- | ------ | ------------------------------------------------ |
| `retry.max_attempts`                    | `3`    | 每个认证 Tier 的尝试次数，包含首次请求。         |
| `retry.timeout_seconds`                 | `300`  | 推理请求总超时，包含流式响应读取。               |
| `performance.attempt_timeout_seconds`   | `0`    | 每次尝试等待响应头的超时；0 表示使用请求总超时。 |
| `performance.connect_timeout_seconds`   | `5`    | 建立连接的超时。                                 |
| `performance.failure_cooldown_seconds`  | `15`   | 失败冷却基数。                                   |
| `performance.max_idle_conns`            | `2048` | 每个代理 Transport 的空闲连接上限。              |
| `performance.max_idle_conns_per_host`   | `256`  | 每个 Transport 对单个主机的空闲连接上限。        |
| `performance.max_conns_per_host`        | `0`    | 单个主机的连接上限；0 表示不限制。               |
| `performance.idle_conn_timeout_seconds` | `120`  | 空闲连接保留时间。                               |

单次响应头超时不会超过请求总超时。希望慢节点仍能留出回退时间时，可以将它设为小于总超时的值。请求上下文过期后会停止后续尝试，不会惩罚尚未实际使用的 Key 或代理。

### 日志与管理端

| 字段                          | 默认值或要求                                    |
| ----------------------------- | ----------------------------------------------- |
| `logging.level`               | `info`；可选 `debug`、`info`、`warn`、`error`。 |
| `logging.ring_size`           | `2000`；范围为 100–50,000。                     |
| `logging.dump_request_bodies` | `false`；配合 `debug` 日志级别输出上游请求体。  |
| `webui.enabled`               | 省略时为 `false`；示例配置设置为 `true`。       |
| `webui.listen`                | `0.0.0.0:8081`。                                |
| `webui.username`              | 启用 WebUI 时必填；示例为 `admin`。             |
| `webui.password`              | 首次初始化密码，最小长度为 10。                 |
| `webui.password_hash`         | 自动生成的 Argon2id 哈希，用于替代明文密码。    |
| `webui.session_ttl_minutes`   | `720`；范围为 5–10,080。                        |

启动时会将初始密码哈希化，并从配置中删除明文；包含初始明文密码的备份也会删除。配置和备份仍包含运行所需的上游 Key、代理凭据，应限制文件访问权限。

请求体日志默认关闭，每份准备好的上游请求体最多输出 64 KiB。已配置的敏感值会脱敏，但对话内容本身仍可能包含敏感信息。

## WebUI 与诊断

管理监听端口提供 WebUI 和 `/api/*` 接口。认证使用单一管理员账号、服务端 Session、HttpOnly / SameSite Cookie、写操作 CSRF 校验及登录限速。

| 管理接口                                         | 用途                                            |
| ------------------------------------------------ | ----------------------------------------------- |
| `POST /api/auth/login`                           | 登录，获取 Session Cookie 和 CSRF Token。       |
| `GET /api/auth/session`、`POST /api/auth/logout` | 查看或结束当前 Session。                        |
| `GET /api/config`、`PUT /api/config`             | 读取脱敏配置或应用修改。                        |
| `POST /api/config/reload`                        | 从磁盘重载配置。                                |
| `POST /api/config/reveal`                        | 验证密码后查看配置中的敏感值。                  |
| `PUT /api/account`                               | 验证密码后修改账号，修改后使已有 Session 失效。 |
| `GET /api/monitor`                               | 请求、Token、上游和资源统计。                   |
| `GET /api/debug/models`                          | 模型路由、Key 指纹和 metadata 诊断。            |
| `POST /api/debug/inference`                      | 执行 Playground 请求。                          |
| `GET /api/logs`、`GET /api/logs/stream`          | 最近日志或 SSE 实时订阅。                       |

Playground 需要已登录的 Session 和 `X-CSRF-Token`，每个客户端 IP 每分钟最多 12 次：

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

服务端强制设置 `stream: false`。需要指定 Key 时，使用 `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}`，其中指纹来自 `/api/debug/models`。指定 Key 的诊断最多执行一次上游尝试，不进入匿名通道，不轮换 Key，不切换 Tier，也不重放 reasoning 请求。

**自动模式和指定 Key 模式都不会改变生产 Key 的冷却、失败次数，以及代理的健康状态和绑定关系。** 诊断仍会真实访问上游，可能消耗供应商配额，并会计入监控。

执行后，管理接口返回 HTTP 200，实际结果位于 `ok`、`http_status`、`request_id`、`route` 和 `response`。指定 Key 时还返回 `selected_key` 与 `key_test`，其值包括 `usable`、`rejected`、`rate_limited`、`transport_error`、`upstream_error`、`request_error`、`unavailable`。管理请求格式、认证、CSRF 和限速错误仍返回各自对应的 HTTP 状态。

### 保存与重载

服务先验证候选配置并创建新的 Gateway，然后保存配置并切换实例。验证或保存失败时，当前 Gateway 继续处理请求；已经开始的请求继续使用原有实例。

Key、代理、上游地址、重试、模型、日志和路由偏好立即对新请求生效。`listen`、`webui.listen`、`webui.enabled` 的修改需要重启进程。保存后的 JSON 会规范化，不保留原有注释。

## 监控与持久化

请求结果以完整推理过程为准。即使 HTTP 200 已经发出，SSE 错误事件或异常断流仍会计为失败，并标记 `stream_error`；客户端取消标记为 `client_canceled`。WebUI 会将这些结果与 HTTP 状态一起显示。

上游**尝试记录**统计的是收到响应头为止的 HTTP 交互，其成功状态和耗时与完整 JSON 响应或流的最终完成情况分开计算。

Token 统计仅使用上游报告的 usage。输入 Token 包含缓存读取和写入，缓存 Token 单独统计缓存读取量。缺失 usage 时不估算 Token，覆盖率表示已经建立路由的推理请求中有多少报告了 usage。

| 数据                                            | 保存位置或保留规则                                                        |
| ----------------------------------------------- | ------------------------------------------------------------------------- |
| Session、指标、Token 累计、最近 Playground 结果 | 进程内存；重启清空。                                                      |
| 请求与尝试明细                                  | 最近一小时，最多 10,000 个请求和 20,000 次尝试；API 分别最多返回 500 条。 |
| 实时日志环形缓冲                                | 进程内存，大小由 `logging.ring_size` 指定。                               |
| 结构化日志                                      | stdout JSON；需要长期保留时由外部系统收集。                               |
| 当前配置与上一版本                              | `config.json`、`config.json.bak`。                                        |
| 模型目录缓存                                    | `config.json.models.catalog.json`。                                       |
| 成本与弃用信息缓存                              | `config.json.models.dev.json`。                                           |

缓存文件名基于实际配置路径生成。lifetime 仅表示当前进程运行期间的累计，各实例之间不共享 Session 或监控状态。

### 健康检查

`/healthz` 不增加监控计数，也不触发网络请求；返回资源数量，不包含 Key 或代理地址。

- HTTP 503：模型目录尚未就绪、当前配置下没有可路由模型，或没有健康代理。
- HTTP 200：服务就绪。可用目录缓存过期时仍返回 200，同时模型状态为 `stale`，整体状态为 `degraded`。

过期阈值为 `models.refresh_seconds` 的两倍，且不低于 60 秒。就绪状态检查目录和资源，不会实际验证某个上游 Key 能否完成下一次推理。

## 开发

Go 源码按职责拆分为独立包。程序入口负责组装服务，具体实现放在 `internal/` 中。

```text
cmd/
  opencode2api/main.go    命令行参数、启动与优雅退出
internal/
  admin/                 管理 API、登录会话与 Playground
  buildinfo/             健康检查和管理接口共享的版本信息
  config/                配置解析、持久化、密码与脱敏
  gateway/               HTTP 路由、重试、资源池、刷新与运行时
  httpx/                 通用 HTTP 响应、响应体处理与请求头
  identity/              请求标识与会话亲和性
  jsonutil/              JSON 取值与解码辅助函数
  models/                模型目录、能力、价格与缓存
  protocol/              请求和响应转换、SSE 解析与输出
  telemetry/             请求跟踪、指标、日志与异常恢复
webui/
  embed.go               将三个静态资源内嵌到可执行文件
  index.html             页面结构
  app.js                 界面交互
  styles.css             界面样式
```

建议阅读顺序：

1. [程序入口](cmd/opencode2api/main.go) → [运行时管理](internal/gateway/runtime.go) → [HTTP 处理](internal/gateway/gateway.go)。
2. [上游请求](internal/gateway/upstream.go)和[模型路由](internal/models/catalog.go)说明请求如何选择并到达上游。
3. [请求转换](internal/protocol/request.go)、[响应转换](internal/protocol/response.go)和[流式传输](internal/protocol/stream.go)说明协议处理过程。
4. [管理路由](internal/admin/server.go)和 [WebUI 交互](webui/app.js)说明配置编辑与诊断功能。

执行 Go 格式化、静态分析和程序构建：

```bash
gofmt -w cmd internal webui
go vet ./...
go build -o opencode2api ./cmd/opencode2api
```

开发时可以直接运行 `go run ./cmd/opencode2api -config config.json`。发布构建仍通过 `-ldflags "-X main.version=vX.Y.Z"` 注入版本号。

Node.js 仅用于开发时的格式化和 JavaScript 语法检查：

```bash
npm ci --ignore-scripts
npm run format
npm run format:check
npm run check:web
```

项目通过 `.editorconfig`、`.gitattributes`、Go 格式化和固定版本的 Prettier 统一格式。CI 覆盖 Linux / Windows 上的 Go 1.24 与稳定版 Go 的 `go vet` 和构建，以及格式、WebUI 语法和容器入口脚本语法检查。发布压缩包包含中英文两份 README。

## 常见问题

| 现象                          | 排查方向                                           |
| ----------------------------- | -------------------------------------------------- |
| API 返回 401                  | 使用配置中的本地 `server_keys`。                   |
| 健康检查一直为 `starting`     | 查看目录刷新日志和网络连通性，必要时补充协议覆盖。 |
| 模型列表为空                  | 检查配置的 Tier、匿名资格和原生协议是否受支持。    |
| 请求返回 502 / 504            | 查看上游尝试、凭据、代理以及请求总超时和单次超时。 |
| HTTP 200 但生成失败           | 查看 SSE 错误事件和请求结果，不能只看 HTTP 状态。  |
| Docker 中宿主机配置修改不生效 | 当前配置在状态卷中；重新导入，或通过 WebUI 修改。  |
| 无法通过容器端口访问          | 检查监听地址、端口映射和状态卷中的实际配置。       |
| 重启后监控消失                | 监控仅保存在内存，需要外部收集 stdout 日志。       |

## 致谢

感谢 [LINUX DO](https://linux.do) 社区的支持。
