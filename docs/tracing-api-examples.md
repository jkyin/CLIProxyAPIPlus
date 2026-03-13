# CLIProxyAPI Tracing API Response Examples

This document provides sample response shapes for projects that integrate with CLIProxyAPI tracing.

## 1. Status

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

## 2. Incremental events

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

## 3. Request detail

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

## 4. Attempts

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

## 5. Usage

`GET /v0/management/tracing/requests/:request_id/usage`

When `completeness=missing`, the token fields below are returned as `null`, not `0`.

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

## 6. Blob

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

## 7. SSE

`GET /v0/management/tracing/sse`

```text
event: ready
data: {"latest_seq":12834}

event: seq
data: {"latest_seq":12835,"ts":"2026-03-12T14:00:01.234567Z"}
```
