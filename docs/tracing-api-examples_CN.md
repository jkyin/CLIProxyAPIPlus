# CLIProxyAPI Tracing API 响应样例

本文档提供给对接 CLIProxyAPI tracing 能力的消费端项目，用于理解接口返回结构。

注意：

- 以下 JSON 是说明结构的示例，不保证字段值完全一致
- 权威增量接口仍然是 `/v0/management/tracing/events`
- SSE 只用于通知，不是唯一事实源

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

## 2. 增量事件接口

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

## 3. request 详情接口

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

## 4. attempts 详情接口

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

## 5. usage 详情接口

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

## 6. blob 接口

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

## 7. SSE 通知接口

`GET /v0/management/tracing/sse`

```text
event: ready
data: {"latest_seq":12834}

event: seq
data: {"latest_seq":12835,"ts":"2026-03-12T14:00:01.234567Z"}
```
