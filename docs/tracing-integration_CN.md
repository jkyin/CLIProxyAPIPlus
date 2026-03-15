# CLIProxyAPI 请求追踪集成指南

本文档用于给另一个项目提供接入 CLIProxyAPI 高精度请求追踪功能的上下文。

## 什么是权威数据源

CLIProxyAPI 会把 tracing 数据持久化到：

- `tracing.db`：权威结构化 tracing 库
- `tracing/bodies/`：通过 blob ID 引用的大 body 和 streaming body

以下接口都不是权威事实源：

- `/usage`：聚合 usage 汇总接口
- `/auth-files`：当前 auth 清单接口
- `/tracing/sse`：只发最新 `latest_seq` 的通知流
- `/tracing/requests/stream`：只发 request summary 投影视图的实时流

真正的权威事实仍然来自持久化 tracing 表和 blob 存储。

## 配置方式

在 CLIProxyAPI 的 `config.yaml` 中启用：

```yaml
tracing:
  enabled: true
  dir: "./tracing"
  body-inline-max-bytes: 65536
  max-body-bytes: 0
  emit-sse: true
  prune-days: 0
```

字段说明：

- `enabled`：是否启用 tracing
- `dir`：tracing 根目录；相对路径相对于 `config.yaml`
- `body-inline-max-bytes`：小 body 直接写 SQLite，超过后落文件
- `max-body-bytes`：单个 body 的最大捕获字节数；`0` 表示不截断
- `emit-sse`：是否在 tracing 提交后发实时流事件
- `prune-days`：自动清理天数；`0` 表示不自动清理

补充说明：

- `tracing.dir` 是 `tracing.db` 和 `tracing/bodies/` 的根目录
- 修改 tracing 配置后需要重启 CLIProxyAPI 才会生效

## 接口分类

### Live 查询接口

这组接口适合请求列表、详情页和 live 观察工具：

- `GET /v0/management/tracing/requests?limit=<n>&offset=<n>&search=<q>&status=<all|success|failure>&provider=<name>&requested_model=<name>&has_usage_only=<bool>&stream_only=<bool>`
- `GET /v0/management/tracing/requests/:request_id/detail`
- `DELETE /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/requests/stream`

其中：

- `/tracing/requests` 返回列表查询优化过的 summary page
- `/tracing/requests/:request_id/detail` 一次返回聚合详情：
  - `request`
  - `attempts`
  - `usage_final`
  - request / response blob metadata
  - attempt request / response blob metadata
- `/tracing/requests/stream` 会发三类事件：
  - `ready`
  - `upsert`
  - `delete`

每条 `upsert` 都表示某个 `request_id` 当前最新的一整行 summary 快照。

### Durable 增量接口

如果消费端需要自己维护本地持久 projection 和 checkpoint，继续使用：

- `GET /v0/management/tracing/events?after_seq=<n>&limit=<m>`
- `GET /v0/management/tracing/sse`

这里的 `/tracing/sse` 只负责通知“有更新了”，不是历史回放接口。

### 低层详情接口

以下接口继续保留，适合兼容旧消费端或按层补 detail：

- `GET /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/requests/:request_id/attempts`
- `GET /v0/management/tracing/requests/:request_id/usage`

### body 接口

- `GET /v0/management/tracing/blobs/:blob_id`
- `GET /v0/management/tracing/blobs/:blob_id?raw=1`

### 状态接口

- `GET /v0/management/tracing/status`

## 推荐消费方式

### 方案 A：live UI 消费端

适合请求列表、详情页、运维观察工具。

1. 首屏通过 `/tracing/requests` 拉当前列表
2. 详情页按需调用 `/tracing/requests/:request_id/detail`
3. payload 只有在界面真正需要时再按 blob ID 拉取
4. 建立 `/tracing/requests/stream`
5. 按 `request_id` 应用 `upsert` 和 `delete`
6. 把每条 `upsert` 当成该请求 summary 的整行替换

注意：

- `ready` 只代表实时流已建立，不代表历史 replay
- 同一个请求在运行期间出现多条 `upsert` 是正常的
- 如果流断开且你只关心“当前状态”，直接重新拉一次 `/tracing/requests`
- 如果你需要严格补齐断线窗口，请改用方案 B

### 方案 B：本地 durable mirror 消费端

只有在你的项目需要自建本地持久 projection 和崩溃恢复时，才使用这套方案。

1. 本地持久化一个 `last_seq`
2. 启动时先调用 `/tracing/events?after_seq=<last_seq>`
3. 按 `seq` 升序处理事件
4. 只有在本地事务提交成功后才推进 `last_seq`
5. 再建立 `/tracing/sse`
6. 当 `/tracing/sse` 提示 `latest_seq` 变大时，再回到 `/tracing/events`

不要把 SSE 当作唯一事实源。

## Summary 实时流语义

request summary 流是从权威 tracing 表派生出来的投影视图，不是另一套事实库。

几个关键语义：

- 同一个 request 出现多条 `upsert` 是预期行为
- request start、route 变化、attempt 生命周期、响应元数据变化、request finalize、usage finalize 都可能重新触发 summary 发布
- 多条 summary `upsert` 共享同一个 `latest_seq` 也是正常的，因为 `latest_seq` 跟踪的是 `trace_event` 最新提交序号，不是 summary publish 次数
- 消费端必须始终用 `request_id` 作为 summary 行主键

## 数据语义

### request

每个下游请求最终对应一条 `trace_request`。

关键字段：

- `request_id`
- `legacy_request_id`
- `http_method/http_scheme/http_host/http_path/http_query`
- `is_stream`
- `handler_type`
- `requested_model`
- `client_correlation_id`
- `downstream_status_code`
- `status`

`status` 取值：

- `running`
- `succeeded`
- `failed`
- `interrupted`

### attempt

每个真实上游 HTTP / WebSocket 尝试最终对应一条 `trace_attempt`。

关键字段：

- `attempt_id`
- `request_id`
- `attempt_no`
- `retry_scope`
- `provider`
- `executor_id`
- `auth_id`
- `auth_index`
- `auth_snapshot_json`
- `route_model`
- `upstream_model`
- `upstream_url`
- `status_code`
- `outcome`

`retry_scope` 取值：

- `initial`
- `handler_bootstrap_retry`
- `auth_retry`
- `model_pool_retry`
- `executor_internal_retry`
- `websocket_resend`

### usage

每个 request 最终最多有一条 `trace_usage_final`。

关键字段：

- `input_tokens`
- `output_tokens`
- `reasoning_tokens`
- `cached_tokens`
- `total_tokens`
- `derived_total`
- `completeness`

`completeness` 取值：

- `complete`
- `partial`
- `missing`

语义约束：

- `missing` 绝不等于 0 token
- 当 `completeness=missing` 时，`trace_usage_final` 的 token 列会存成 `NULL`
- management usage API 和 `usage.finalized` payload 也会把这些 token 字段输出为 `null`
- `derived_total=true` 说明 `total_tokens` 是本地派生的

### auth snapshot

`auth_snapshot_json` 只保存安全字段，不保存 token / cookie / bearer 原文。

典型内容：

- `provider`
- `auth_id`
- `auth_index`
- `label`
- `account_type`
- `account`
- `path`
- `source`
- `status`
- `unavailable`
- `next_retry_after`

## 给另一个项目的 Prompt 模板

```text
你要接入 CLIProxyAPI 的高精度 tracing 能力。

请把 CLIProxyAPI 视为权威事实源，不要使用 /usage、/auth-files、外部 HTTP 抓包或启发式 matching 作为主路径。

你需要先选择一种接入模式：

1. Live UI 模式
   - 用 GET /v0/management/tracing/requests 做列表查询
   - 用 GET /v0/management/tracing/requests/:request_id/detail 做详情查询
   - 用 GET /v0/management/tracing/requests/stream 接 ready/upsert/delete 实时事件
   - 每条 upsert 都按 request_id 整行替换

2. Durable mirror 模式
   - 用 GET /v0/management/tracing/events?after_seq=<n>&limit=<m> 做唯一增量回放来源
   - 用 GET /v0/management/tracing/sse 只做通知
   - 本地持久化 last_seq，并且只在本地事务提交后推进

共同约束：
- request 主键使用 request_id，attempt 主键使用 attempt_id
- trace_usage_final 是最终 usage 结论
- completeness=complete|partial|missing 必须保留
- missing usage 不能当作 0 token
- /usage 只是 summary，不是 request history
- /auth-files 只是当前状态清单，不是历史归因
- blob 拉取失败不能阻塞主流程
```
