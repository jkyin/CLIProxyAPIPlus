# CLIProxyAPI 请求追踪集成指南

本文档用于给另一个项目提供接入 CLIProxyAPI 高精度请求追踪功能的上下文。

## 功能摘要

CLIProxyAPI 现在提供一套持久化 tracing 能力，用于记录：

- 下游 HTTP 请求事实
- CLIProxyAPI 内部的 provider / auth / model 路由事实
- 每次上游 attempt 的请求、响应、错误和 timing
- 最终 usage 事实

这套 tracing 的权威数据源是：

- `tracing.db`：SQLite 结构化事实库
- `tracing/bodies/`：大 body 和 streaming body 的文件存储

不是权威数据源的接口：

- `/usage`：仍然是 summary / usage 汇总接口
- `/auth-files`：仍然是当前 auth 清单接口
- `/tracing/sse`：只做实时通知，不是唯一事实源

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
- `body-inline-max-bytes`：小 body 直接写入 SQLite，超过后落文件
- `max-body-bytes`：单个 body 的最大捕获字节数；`0` 表示不截断
- `emit-sse`：是否在事件提交后发 SSE 通知
- `prune-days`：自动清理天数；`0` 表示不自动清理

目录结构：

```text
<tracing.dir>/
  tracing.db
  bodies/
    xx/
      <blob_id>.bin
```

重要约束：

- 修改 `tracing` 配置后需要重启 CLIProxyAPI 才会生效
- 如果 `max-body-bytes: 0`，磁盘增长取决于真实流量

## 核心接口

权威增量读取接口：

- `GET /v0/management/tracing/events?after_seq=<n>&limit=<m>`

单请求查询接口：

- `GET /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/requests/:request_id/attempts`
- `GET /v0/management/tracing/requests/:request_id/usage`

body 查询接口：

- `GET /v0/management/tracing/blobs/:blob_id`
- `GET /v0/management/tracing/blobs/:blob_id?raw=1`

实时通知接口：

- `GET /v0/management/tracing/sse`

状态接口：

- `GET /v0/management/tracing/status`

## 推荐消费方式

推荐消费顺序：

1. 消费端本地持久化一个 `last_seq`
2. 启动时先调用 `/tracing/events?after_seq=<last_seq>`
3. 把返回事件按 `seq` 顺序处理并落地到自己的系统
4. 只有在本地事务提交成功后才推进 `last_seq`
5. 追平后再连接 `/tracing/sse`
6. SSE 收到新 `latest_seq` 后，再回到 `/tracing/events` 拉取增量

不要直接把 SSE 当作唯一来源。

原因：

- SSE 只保证“有新事件了”
- 真正的事件内容和顺序以 SQLite 提交后的 `/tracing/events` 为准

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
- management usage API 和 `usage.finalized` 事件 payload 也会把这些 token 字段输出为 `null`
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

## 对接建议

如果你的项目要接入这套 tracing，建议做以下映射：

- 用 `request_id` 作为你自己的外部主键
- 用 `attempt_id` 作为上游尝试明细主键
- 把 `trace_usage_final` 作为最终 usage 结论，不要自己再从 stream chunk 重算
- 把 `/usage` 只用于 summary 页面，不要用作 request history
- 把 `/auth-files` 只用于“当前账号状态”页面，不要用作历史归因

## 给另一个项目的 Prompt 模板

下面这段 Prompt 可以直接给另一个项目的 agent / 工程师作为上下文：

```text
你要接入 CLIProxyAPI 的高精度 tracing 能力。

请把 CLIProxyAPI 视为权威事实源，不要再使用 /usage、/auth-files、外部 HTTP 抓包或启发式 matching 作为主路径。

接入目标：
1. 使用 GET /v0/management/tracing/events?after_seq=<n>&limit=<m> 作为唯一增量读取接口
2. 使用 request_id 作为请求主键，attempt_id 作为上游尝试主键
3. 使用 GET /v0/management/tracing/requests/:request_id、/attempts、/usage、/blobs/:blob_id 补全详情
4. 仅把 GET /v0/management/tracing/sse 作为实时通知层；SSE 断开后回退到 /events 补齐
5. 把 trace_usage_final 视为最终 usage 结论；不要自行从 stream chunk 重算最终 token
6. 把 completeness=complete|partial|missing 作为 usage 可信度等级
7. 不要把 missing usage 当作 0 token
8. 不要把 /usage 当作 request history；它只是 summary
9. 不要把 /auth-files 当作历史归因；历史归因使用 attempt 自带 auth_snapshot_json

请基于以上约束，为当前项目设计：
- tracing ingest 模块
- last_seq checkpoint 持久化
- request / attempt / usage / blob 的本地数据模型
- 首次全量同步 + 增量追平 + SSE 实时模式的状态机
- 错误恢复与幂等处理逻辑

要求：
- after_seq 增量消费必须幂等
- 只有本地事务提交后才能推进 last_seq
- request / attempt / usage 三层数据必须可独立查询
- blob 下载失败不能阻塞事件主流程
```
