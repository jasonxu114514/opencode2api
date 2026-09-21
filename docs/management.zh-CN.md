# 管理控制台：模型轮转、Go 额度与 API Key

本文说明三个新增管理页面的使用方式、配置和错误处理。

## 模型与额度

管理页面 `/#model-quotas`，登录后读取 `GET /api/model-quotas`。

- 模型来自网关同步的 OpenCode Go 目录，支持搜索和复制模型 ID。
- 额度由网关使用已配置的 Go Key 查询 `https://opencode.ai/zen/go/v1/usage`。浏览器不会收到上游 Key。
- 显示 5 小时、每周、每月窗口的剩余百分比和重置时间。接口只提供共享套餐窗口，不提供独立的逐模型余额，不推算金额或可用次数。多个 Key 的额度不相加。
- 成功响应缓存 60 秒，失败缓存 15 秒；失败时保留上次成功结果并明确标记历史数据。没有成功数据时显示未知。
- 页面打开期间每 60 秒查询一次；离开页面或隐藏浏览器时停止周期查询。手动刷新也受服务端缓存约束。

依据：[OpenCode 官方额度接口源码](https://github.com/anomalyco/opencode/blob/dev/packages/console/app/src/routes/zen/go/v1/usage.ts)。该接口未列入正式 API 文档，若响应格式变化，会显示查询异常而非虚构额度。

## API Key 管理

管理页面 `/#api-keys`，支持列表、生成、自定义、重命名、启用、停用、删除和创建后复制环境变量。

- 管理的是网关的 `server_keys`，不修改 OpenCode Go 的上游 Key。
- 自动生成 192 位随机密钥；自定义密钥为 16–256 位英文字母、数字、`-`、`_` 或 `.`。
- 列表只显示名称、尾码、状态和创建时间。创建后可复制完整密钥及环境变量。
- 变更热应用；停用和删除后，新请求返回 401。在途请求可以完成。至少保留一个启用的 Key。
- 所有管理接口需要登录，写操作需要 CSRF Token。
- 元数据以 `server_key_metadata` 保存在原配置中，按 Key 指纹关联；已有 `server_keys` 自动兼容。
- 当前未实现按 Key 单独分配模型权限、用量配额或过期时间。

## 验证和构建

```bash
go test ./...
go test -race ./...
go vet ./...
npm run check:web
npx prettier --check webui/index.html webui/app.js webui/styles.css
go build -trimpath -o opencode2api ./cmd/opencode2api
```

升级前请备份程序、配置及轮转状态，再停止服务并替换可执行文件。旧版本不认识新增的 `server_key_metadata` 和 `rotation` 字段；回退时应同时恢复旧程序和旧配置。

## SOTA / 甜点模型轮转

管理页：`/#rotation`。调用时继续使用 网关的 Base URL（默认
`http://127.0.0.1:8080/v1`）和已有 API Key，将 `model` 设为 `sota` 或
`sweet`。两个别名列入 `/v1/models`，支持 Chat Completions、Responses 和
Messages，以及这三种协议的流式输出。真实模型 ID 的原有调用路径保持不变。

- SOTA 固定成员：`deepseek-v4.1-flash`、`glm-5.3`、`grok-4.6`、`kimi-k3`、
  `qwen3.8-max`，默认按上述顺序。
- 甜点组包含其余 Go 可用模型，初次以 `glm-5.3-flash` 为首，随后按名称排序。
  新模型追加在末尾；下架或协议不受支持的模型保留在列表，但不参与调用。
- 每组共享一个当前模型。成功后保持位置；失败切换后，下次从新位置开始。
  并发请求通过位置版本号避免旧请求把游标移回去；手动切换不打断在途回答。
- 普通错误每模型最多 3 次（包含首次），每请求最多消耗 3 个普通错误模型预算。
  模型耗尽重试后暂停 15 秒。分组请求不使用原有 Key 重试、跨 Tier 降级、
  自动重定向或 stale-reasoning 补发，也不因单个模型故障暂停整个 Go Key。
- 429 或明确额度错误直接跳到下一模型，不新增普通错误模型计数；已经发生的
  普通错误仍计数。允许遍历整组，同一请求不重复模型。暂停时间采用 Retry-After，
  未提供时为 60 秒。共享套餐额度只用于展示，不能保证换模型后就有余额。
- 整体超时沿用 `retry.timeout_seconds`。`rotation.first_output_seconds` 默认
  30 秒，限制每次等待首个有效输出的时间，不限制已经开始的正常流式回答。
- 流式 start/role/usage 前导事件会等到有效输出或正常结束后再提交。提交前错误
  可重试；提交后中断会保留已输出内容、追加协议对应的 SSE 错误并结束，随后
  推进当前位置，下一次请求使用下一个模型。不会在同一响应里拼接替代答案。
  调用方取消不会触发暂停或切换。
- 响应 JSON / SSE 的 `model` 保持请求别名，`X-Resolved-Model` 标记实际模型。
  用量按实际上游模型记录。轮转日志与管理页面仅保留分类后的错误原因，
  不记录上游错误正文、请求正文或密钥。最近保留 100 条尝试和每组 30 条切换。

配置保存于 `config.json` 的 `rotation` 字段：`sota` / `sweet` 各有 `alias`
和 `order`；状态单独原子写入 `config.json.rotation.json`（0600），包括当前
位置、版本、动态追加的顺序、暂停期限和最近记录。修改 Key 或其他配置时
沿用同一个轮转状态管理器。页面展示状态写入失败提示，首次无法写入则拒绝启动。

管理接口均需要现有管理 Session，写入另需 CSRF：

- `GET /api/rotation`：分组配置、可用状态、暂停期限、切换和尝试记录。
- `PUT /api/rotation`：提交 `sota` / `sweet` 的 `alias`、完整 `order`，以及
  `first_output_seconds`；别名不得重复或与真实模型 ID 冲突，成员不得跨组。
- `POST /api/rotation/current`：`{"group":"sota","model":"glm-5.3"}`；立即
  修改当前模型并解除该模型的临时暂停。

验证：`go test -race ./...`、`go vet ./...`、`npm run check:web`。
模拟上游测试覆盖 9 次普通错误上限、超过 3 个额度跳过、混合错误预算、
Retry-After、单个模型故障不影响 Key、三种入口协议的跨协议转换、成功保持、
别名发现与用量归属、畸形响应、输出前错误、首个输出超时、输出后故障、
调用方取消、在途回答不受手动切换影响、并发版本保护、下架和重启恢复。
上线前建议使用独立配置及端口验证管理登录、CSRF、页面排序与保存、别名冲突、
Key 配置热更新和真实 Go 请求。测试与正式配置、凭证、日志及运行状态不应提交到仓库。
