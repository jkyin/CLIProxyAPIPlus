# CLIProxyAPI Tracing API 响应样例

本文档提供给对接 CLIProxyAPI tracing 能力的消费端项目，用于理解接口返回结构。

注意：

- 以下 JSON 只是结构示例，不保证字段值完全一致
- `/tracing/requests` 和 `/tracing/requests/:request_id/detail` 是当前推荐的 live 查询接口
- `/tracing/events` 仍然保留给需要自建 durable local projection 的消费端

## 1. 状态接口

`GET /v0/management/tracing/status`

```json
{
  "enabled": true,
  "boot_id": "01958f9a-2b34-7f1a-8d95-3d79f9d5e902",
  "latest_seq": 12834,
  "requests_running": 2,
  "attempts_running": 3,
  "db_path": "/path/to/tracing/tracing.db",
  "bodies_dir": "/path/to/tracing/bodies"
}
```

## 2. request summary 列表接口

`GET /v0/management/tracing/requests?limit=2&offset=0&status=all`

```json
{
  "items": [
    {
      "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
      "started_at_ns": 1741750401000000000,
      "provider": "codex",
      "requested_model": "gpt-5",
      "route_model": "gpt-5",
      "upstream_model": "gpt-5-codex",
      "auth_label": "codex-main",
      "auth_account": "user@example.com",
      "auth_path": "/Users/demo/.codex/auth.json",
      "auth_index": "a8f239bc12d013fe",
      "http_method": "POST",
      "http_path": "/v1/chat/completions",
      "http_query": "",
      "status_code": 200,
      "duration_ms": 812,
      "input_tokens": 1280,
      "output_tokens": 412,
      "total_tokens": 1784,
      "reasoning_tokens": 92,
      "cached_tokens": 256,
      "request_status": "succeeded",
      "usage_completeness": "complete",
      "is_stream": true,
      "updated_at_ns": 1741750401812000000
    }
  ],
  "total_count": 42,
  "next_offset": 2
}
```

## 3. 聚合详情接口

`GET /v0/management/tracing/requests/:request_id/detail`

```json
{
  "request": {
    "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
    "legacy_request_id": "a1b2c3d4",
    "started_at_ns": 1741750401000000000,
    "finished_at_ns": 1741750401800000000,
    "status": "succeeded",
    "http_method": "POST",
    "http_scheme": "http",
    "http_host": "127.0.0.1:8317",
    "http_path": "/v1/chat/completions",
    "http_query": "",
    "is_stream": true,
    "handler_type": "openai",
    "requested_model": "gpt-5",
    "client_correlation_id": "req-20260312-001",
    "downstream_status_code": 200,
    "downstream_first_byte_at_ns": 1741750401300000000,
    "request_body_blob_id": "01958f9c-4b09-70a9-8be9-d5c6d5e82f1a",
    "response_body_blob_id": "01958f9c-4b52-714e-a6a4-31457fa53a89"
  },
  "attempts": [
    {
      "attempt_id": "01958f9c-4b13-7ed0-b0d2-39613d11d88a",
      "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
      "attempt_no": 1,
      "retry_scope": "initial",
      "provider": "codex",
      "executor_id": "codex",
      "auth_id": "auth-codex-01",
      "auth_index": "a8f239bc12d013fe",
      "route_model": "gpt-5",
      "upstream_model": "gpt-5-codex",
      "upstream_url": "https://chatgpt.com/backend-api/codex/responses",
      "upstream_method": "POST",
      "upstream_protocol": "http",
      "status_code": 200,
      "outcome": "succeeded"
    }
  ],
  "usage_final": {
    "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
    "attempt_id": "01958f9c-4b13-7ed0-b0d2-39613d11d88a",
    "finalized_at_ns": 1741750401800000000,
    "status": "success",
    "completeness": "complete",
    "input_tokens": 1280,
    "output_tokens": 412,
    "reasoning_tokens": 92,
    "cached_tokens": 256,
    "total_tokens": 1784,
    "derived_total": false
  },
  "request_blob": {
    "blob_id": "01958f9c-4b09-70a9-8be9-d5c6d5e82f1a",
    "storage_kind": "inline",
    "size_bytes": 512,
    "content_type": "application/json",
    "complete": true,
    "truncated": false
  },
  "response_blob": {
    "blob_id": "01958f9c-4b52-714e-a6a4-31457fa53a89",
    "storage_kind": "file",
    "size_bytes": 48211,
    "content_type": "text/event-stream",
    "complete": true,
    "truncated": false,
    "file_relpath": "bodies/01/01958f9c-4b52-714e-a6a4-31457fa53a89.bin"
  },
  "attempt_request_blobs": {
    "01958f9c-4b13-7ed0-b0d2-39613d11d88a": {
      "blob_id": "01958f9c-4b1e-7d94-a912-9fda4c5b7dc0",
      "storage_kind": "inline",
      "size_bytes": 600,
      "content_type": "application/json",
      "complete": true,
      "truncated": false
    }
  },
  "attempt_response_blobs": {
    "01958f9c-4b13-7ed0-b0d2-39613d11d88a": {
      "blob_id": "01958f9c-4b2e-792c-a7da-ae772ee2758d",
      "storage_kind": "file",
      "size_bytes": 48211,
      "content_type": "text/event-stream",
      "complete": true,
      "truncated": false,
      "file_relpath": "bodies/01/01958f9c-4b2e-792c-a7da-ae772ee2758d.bin"
    }
  }
}
```

## 4. 增量事件接口

`GET /v0/management/tracing/events?after_seq=12800&limit=3`

```json
{
  "events": [
    {
      "seq": 12801,
      "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
      "ts_ns": 1741750401000000000,
      "event_type": "request.started",
      "payload_json": {
        "is_stream": true,
        "handler_type": "openai",
        "requested_model": "gpt-5"
      },
      "boot_id": "01958f9a-2b34-7f1a-8d95-3d79f9d5e902"
    }
  ],
  "latest_seq": 12834
}
```

## 5. 原始 request 详情接口

`GET /v0/management/tracing/requests/:request_id`

```json
{
  "request": {
    "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
    "legacy_request_id": "a1b2c3d4",
    "started_at_ns": 1741750401000000000,
    "finished_at_ns": 1741750401800000000,
    "status": "succeeded",
    "http_method": "POST",
    "http_scheme": "http",
    "http_host": "127.0.0.1:8317",
    "http_path": "/v1/chat/completions",
    "http_query": "",
    "is_stream": true,
    "handler_type": "openai",
    "requested_model": "gpt-5",
    "client_correlation_id": "req-20260312-001",
    "downstream_status_code": 200,
    "downstream_first_byte_at_ns": 1741750401300000000,
    "request_body_blob_id": "01958f9c-4b09-70a9-8be9-d5c6d5e82f1a",
    "response_body_blob_id": "01958f9c-4b52-714e-a6a4-31457fa53a89"
  }
}
```

## 6. attempts 详情接口

`GET /v0/management/tracing/requests/:request_id/attempts`

```json
{
  "attempts": [
    {
      "attempt_id": "01958f9c-4b13-7ed0-b0d2-39613d11d88a",
      "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
      "attempt_no": 1,
      "retry_scope": "initial",
      "provider": "codex",
      "executor_id": "codex",
      "auth_id": "auth-codex-01",
      "auth_index": "a8f239bc12d013fe",
      "route_model": "gpt-5",
      "upstream_model": "gpt-5-codex",
      "upstream_url": "https://chatgpt.com/backend-api/codex/responses",
      "upstream_method": "POST",
      "upstream_protocol": "http",
      "status_code": 200,
      "outcome": "succeeded"
    }
  ]
}
```

## 7. usage 详情接口

`GET /v0/management/tracing/requests/:request_id/usage`

当 `completeness=missing` 时，下面这些 token 字段会返回 `null`，不是 `0`。

```json
{
  "usage": {
    "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7",
    "attempt_id": "01958f9c-4b13-7ed0-b0d2-39613d11d88a",
    "finalized_at_ns": 1741750401800000000,
    "status": "success",
    "completeness": "complete",
    "input_tokens": 1280,
    "output_tokens": 412,
    "reasoning_tokens": 92,
    "cached_tokens": 256,
    "total_tokens": 1784,
    "derived_total": false
  }
}
```

## 8. blob 接口

`GET /v0/management/tracing/blobs/:blob_id`

```json
{
  "blob": {
    "blob_id": "01958f9c-4b2e-792c-a7da-ae772ee2758d",
    "storage_kind": "file",
    "size_bytes": 48211,
    "content_type": "text/event-stream",
    "complete": true,
    "truncated": false,
    "file_relpath": "bodies/01/01958f9c-4b2e-792c-a7da-ae772ee2758d.bin"
  },
  "data": "event: response.output_text.delta\ndata: ..."
}
```

## 9. request summary 实时流

`GET /v0/management/tracing/requests/stream`

```text
event: ready
data: {"type":"ready","latest_seq":12834,"ts":"2026-03-12T14:00:00.100000Z"}

event: upsert
data: {"type":"upsert","request_id":"01958f9c-4b01-7869-a4de-4d2c1dcad4c7","summary":{"request_id":"01958f9c-4b01-7869-a4de-4d2c1dcad4c7","started_at_ns":1741750401000000000,"provider":"codex","requested_model":"gpt-5","http_method":"POST","http_path":"/v1/chat/completions","request_status":"running","is_stream":true,"updated_at_ns":1741750401050000000},"latest_seq":12834,"ts":"2026-03-12T14:00:00.150000Z"}

event: delete
data: {"type":"delete","request_id":"01958f9c-4b01-7869-a4de-4d2c1dcad4c7","latest_seq":12840,"ts":"2026-03-12T14:00:05.000000Z"}
```

## 10. 旧的 seq SSE 通知流

`GET /v0/management/tracing/sse`

```text
event: ready
data: {"latest_seq":12834}

event: seq
data: {"latest_seq":12835,"ts":"2026-03-12T14:00:01.234567Z"}
```

## 11. 删除请求接口

`DELETE /v0/management/tracing/requests/:request_id`

```json
{
  "deleted": true,
  "request_id": "01958f9c-4b01-7869-a4de-4d2c1dcad4c7"
}
```
