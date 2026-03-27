# CLIProxyAPI Request Tracing Integration Guide

This document is intended to be handed to another project so it can integrate with CLIProxyAPI's persistent request tracing.

## What is authoritative

CLIProxyAPI persists tracing data to:

- `tracing.db`: the authoritative structured tracing store
- `tracing/bodies/`: large and streaming body payloads referenced by blob ID

These are not authoritative:

- `/usage`: aggregate usage only
- `/auth-files`: current auth inventory only
- `/tracing/request-summaries/stream`: live request-summary projection transport only

The authoritative facts still come from the persisted tracing tables and blobs.

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
- `body-inline-max-bytes` controls when a blob stays inline in SQLite vs spills to file
- `max-body-bytes: 0` means no truncation
- `emit-sse` controls whether `/tracing/request-summaries/stream` emits live summary events
- changes to tracing config currently require a CLIProxyAPI restart

## APIs

### Live query APIs

These are the preferred APIs for request-list UIs and live inspection tools:

- `GET /v0/management/tracing/requests?limit=<n>&offset=<n>&search=<q>&status=<all|success|failure>&provider=<name>&requested_model=<name>&has_usage_only=<bool>&stream_only=<bool>`
- `GET /v0/management/tracing/requests/:request_id/summary`
- `GET /v0/management/tracing/requests/:request_id/detail`
- `DELETE /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/request-summaries/stream`

`/tracing/requests` returns a projected summary page optimized for list queries.

`/tracing/requests/:request_id/summary` returns the current projected summary row for one request.

`/tracing/requests/:request_id/detail` returns a single aggregated payload that includes:

- `request`
- `attempts`
- `usage_final`
- request / response blob metadata
- attempt request / response blob metadata

`/tracing/request-summaries/stream` emits:

- `ready`
- `started`
- `updated`
- `ended`
- `deleted`

- `started` carries the first full summary snapshot for a request row
- `updated` carries an in-flight full-row replacement snapshot
- `ended` carries the single terminal full-row snapshot after final usage has been merged
- `deleted` removes a row by `request_id`

### Durable incremental APIs

This remains available for consumers that maintain their own local projection and checkpoint:

- `GET /v0/management/tracing/events?after_seq=<n>&limit=<m>`

Clients that need durable mirroring should poll `/tracing/events`; there is no separate sequence-notification stream.

### Record detail APIs

These remain available for compatibility and low-level hydration:

- `GET /v0/management/tracing/requests/:request_id`
- `GET /v0/management/tracing/requests/:request_id/attempts`
- `GET /v0/management/tracing/requests/:request_id/usage`

### Body APIs

- `GET /v0/management/tracing/blobs/:blob_id`
- `GET /v0/management/tracing/blobs/:blob_id?raw=1`

### Status API

- `GET /v0/management/tracing/status`

## Recommended consumer flows

### Flow A: live UI consumer

Use this for request lists, detail pages, and live operator tools.

1. Load the initial request list from `/tracing/requests`
2. Open request detail on demand via `/tracing/requests/:request_id/detail`
3. Fetch blob payloads only when the UI actually needs them
4. Connect `/tracing/request-summaries/stream`
5. Apply `started`, `updated`, `ended`, and `deleted` by `request_id`
6. Treat `started`, `updated`, and `ended` as full-row replacements; treat `ended` as the only completion signal

Important notes:

- `ready` only means the stream is live; it is not a historical replay
- the same request can emit multiple `updated` events while it is still running
- `ended` is the only authoritative terminal event for a request row
- if the stream disconnects and you only need current live state, reload `/tracing/requests`
- if you need exact catch-up semantics, use Flow B instead

### Flow B: durable local mirror consumer

Use this only if your project needs its own persistent local projection with crash recovery.

1. Persist a local `last_seq`
2. On startup, call `/tracing/events?after_seq=<last_seq>`
3. Apply events in ascending `seq`
4. Advance `last_seq` only after the local transaction commits
5. Poll `/tracing/events` again on your own schedule

Do not treat request-summary SSE as the only source of truth.

## Summary stream semantics

The request-summary stream is derived from the authoritative tracing tables. It is a projection, not a separate fact store.

Important behaviors:

- every request row begins with `started`
- multiple `updated` events for the same request are expected while it is in progress
- request finalization does not publish a pre-terminal summary; the terminal snapshot is emitted once as `ended` after final usage is merged
- `latest_seq` may repeat across multiple summary events because it tracks the latest committed row in `trace_event`, not the number of summary publishes
- consumers should always key summary rows by `request_id`

## Client migration

Clients must upgrade in lockstep with the backend for this protocol change.

- stream URL changes from `/tracing/requests/stream` to `/tracing/request-summaries/stream`
- replace `upsert` handling with:
  - `started`: create row
  - `updated`: replace row
  - `ended`: replace row and mark terminal
  - `deleted`: remove row
- stop assuming the first event is `upsert`
- stop inferring completion from the last `upsert` or `request_status != running` inside an update
- treat `ended` as the only reliable completion signal

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

- `complete` means the final request succeeded and the selected usage observation belongs to the successful attempt, or is an explicit terminal usage observation
- `partial` means usage was observed, but it cannot be proven to represent the final successful request's complete usage conclusion
- `missing` is not the same as zero tokens
- when `completeness=missing`, token columns in `trace_usage_final` are stored as `NULL`
- the management usage API and `usage.finalized` payload also emit `null` token fields for `missing`
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

Choose one integration mode:

1. Live UI mode
   - Use GET /v0/management/tracing/requests for list queries
   - Use GET /v0/management/tracing/requests/:request_id/summary when you only need the projected summary row
   - Use GET /v0/management/tracing/requests/:request_id/detail for detail pages
   - Use GET /v0/management/tracing/request-summaries/stream for ready/started/updated/ended/deleted live updates
   - Treat `ended` as the only completion signal and all non-delete payloads as full-row replacements keyed by request_id

2. Durable mirror mode
   - Use GET /v0/management/tracing/events?after_seq=<n>&limit=<m> as the only incremental replay source
   - Poll `/tracing/events` at a cadence that matches your freshness requirements
   - Persist last_seq and advance it only after local commit

Requirements:
- Use request_id as the request primary key and attempt_id as the upstream-attempt primary key
- Treat trace_usage_final as the final token usage result
- Respect completeness=complete|partial|missing
- Never convert missing usage into zero tokens
- Treat /usage as summary only
- Treat /auth-files as current-state inventory only
- Blob fetch failures must not block the main event pipeline
```
