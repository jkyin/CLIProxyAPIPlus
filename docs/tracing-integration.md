# CLIProxyAPI Request Tracing Integration Guide

This document is intended to be handed to another project so it can integrate with CLIProxyAPI's persistent request tracing.

## What is authoritative

CLIProxyAPI now persists tracing data to:

- `tracing.db`: the authoritative structured event store
- `tracing/bodies/`: large and streaming body payloads

These are not authoritative:

- `/usage`: summary / aggregate usage only
- `/auth-files`: current auth inventory only
- `/tracing/sse`: real-time notification only

## Configuration

Enable tracing in `config.yaml`:

```yaml
tracing:
  enabled: true
  dir: "./tracing"
  body-inline-max-bytes: 65536
  max-body-bytes: 0
  emit-sse: true
  prune-days: 0
```

Notes:

- `tracing.dir` is the base directory for `tracing.db` and `tracing/bodies/`
- `max-body-bytes: 0` means no truncation
- changes to tracing config currently require a CLIProxyAPI restart

## APIs

Authoritative incremental API:

- `GET /v0/management/tracing/events?after_seq=<n>&limit=<m>`

Request detail APIs:

- `GET /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/requests/:request_id/attempts`
- `GET /v0/management/tracing/requests/:request_id/usage`

Body APIs:

- `GET /v0/management/tracing/blobs/:blob_id`
- `GET /v0/management/tracing/blobs/:blob_id?raw=1`

Real-time notification API:

- `GET /v0/management/tracing/sse`

Status API:

- `GET /v0/management/tracing/status`

## Recommended consumer flow

1. Persist a local `last_seq`
2. On startup, call `/tracing/events?after_seq=<last_seq>`
3. Apply events in ascending `seq`
4. Advance `last_seq` only after the local transaction commits
5. After catch-up, connect `/tracing/sse`
6. When SSE signals a new `latest_seq`, go back to `/tracing/events`

Do not treat SSE as the only source of truth.

## Semantics

### Request

Each downstream request maps to exactly one `trace_request`.

Key fields:

- `request_id`
- `legacy_request_id`
- `http_method/http_scheme/http_host/http_path/http_query`
- `is_stream`
- `handler_type`
- `requested_model`
- `client_correlation_id`
- `downstream_status_code`
- `status`

Request status values:

- `running`
- `succeeded`
- `failed`
- `interrupted`

### Attempt

Each real upstream HTTP / WebSocket attempt maps to exactly one `trace_attempt`.

Key fields:

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

Retry scopes:

- `initial`
- `handler_bootstrap_retry`
- `auth_retry`
- `model_pool_retry`
- `executor_internal_retry`
- `websocket_resend`

### Usage

Each request has at most one `trace_usage_final`.

Key fields:

- `input_tokens`
- `output_tokens`
- `reasoning_tokens`
- `cached_tokens`
- `total_tokens`
- `derived_total`
- `completeness`

Completeness values:

- `complete`
- `partial`
- `missing`

Rules:

- `missing` is not the same as zero tokens
- When `completeness=missing`, token columns in `trace_usage_final` are stored as `NULL`
- The management usage API and `usage.finalized` event payload also emit `null` token fields for `missing`
- `derived_total=true` means `total_tokens` was computed locally

### Auth snapshot

`auth_snapshot_json` contains safe historical context only.

Typical fields:

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

## Prompt template for another project

```text
Integrate with CLIProxyAPI request tracing.

Use CLIProxyAPI as the authoritative source of request / attempt / usage facts.
Do not use /usage, /auth-files, external HTTP capture, or heuristic matching as the main path.

Requirements:
1. Use GET /v0/management/tracing/events?after_seq=<n>&limit=<m> as the only incremental source
2. Use request_id as the request primary key and attempt_id as the upstream-attempt primary key
3. Use the request / attempts / usage / blob endpoints to hydrate details
4. Treat GET /v0/management/tracing/sse as a notification layer only
5. Treat trace_usage_final as the final token usage result
6. Respect completeness=complete|partial|missing
7. Never convert missing usage into zero tokens
8. Treat /usage as summary only
9. Treat /auth-files as current-state inventory only

Design and implement:
- a tracing ingest module
- persistent last_seq checkpoint storage
- local request / attempt / usage / blob models
- startup catch-up + steady-state SSE notification flow
- idempotent event application and crash recovery

Constraints:
- last_seq advances only after local commit
- blob fetch failures must not block the main event pipeline
- request / attempt / usage must be queryable independently
```
