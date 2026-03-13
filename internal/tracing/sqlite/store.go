package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
)

type Config struct {
	BaseDir   string
	EmitSSE   bool
	PruneDays int
}

type Store struct {
	db        *sql.DB
	bootID    string
	dbPath    string
	bodiesDir string
	emitSSE   bool

	latestSeq atomic.Int64

	subMu sync.Mutex
	subs  map[chan int64]struct{}
}

const currentSchemaVersion = 2

func New(ctx context.Context, cfg Config) (*Store, error) {
	baseDir := strings.TrimSpace(cfg.BaseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("tracing sqlite: base dir is empty")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(baseDir, "tracing.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{
		db:        db,
		bootID:    tracing.MustNewID(),
		dbPath:    dbPath,
		bodiesDir: filepath.Join(baseDir, "bodies"),
		emitSSE:   cfg.EmitSSE,
		subs:      make(map[chan int64]struct{}),
	}
	if err := os.MkdirAll(store.bodiesDir, 0o755); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if cfg.PruneDays > 0 {
		if err := store.prune(ctx, cfg.PruneDays); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.recoverRunning(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	var latest sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM trace_event`).Scan(&latest); err == nil {
		store.latestSeq.Store(latest.Int64)
	}
	return store, nil
}

func (s *Store) Enabled() bool     { return s != nil && s.db != nil }
func (s *Store) BootID() string    { return s.bootID }
func (s *Store) DBPath() string    { return s.dbPath }
func (s *Store) BodiesDir() string { return s.bodiesDir }
func (s *Store) LatestSeq() int64  { return s.latestSeq.Load() }

func (s *Store) Subscribe() (<-chan int64, func()) {
	ch := make(chan int64, 16)
	if s == nil || !s.emitSSE {
		close(ch)
		return ch, func() {}
	}
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	cancel := func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (s *Store) StartRequest(ctx context.Context, start tracing.RequestStart) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_request (
			request_id, legacy_request_id, started_at_ns, finished_at_ns, status,
			http_method, http_scheme, http_host, http_path, http_query, is_stream,
			handler_type, requested_model, client_correlation_id, downstream_status_code,
			downstream_first_byte_at_ns, request_headers_json, request_body_blob_id,
			response_headers_json, response_body_blob_id
		) VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, NULL, NULL)
	`, start.RequestID, start.LegacyRequestID, unixNS(start.StartedAt), tracing.RequestStatusRunning,
		start.Method, start.Scheme, start.Host, start.Path, start.Query, boolInt(start.IsStream),
		start.HandlerType, start.RequestedModel, start.ClientCorrelationID, tracing.HeaderJSON(start.RequestHeaders),
		nullableString(start.RequestBodyBlobID))
	return err
}

func (s *Store) UpdateRequestRoute(ctx context.Context, requestID string, route tracing.RequestRouteInfo) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE trace_request
		SET is_stream = ?, handler_type = ?, requested_model = ?, client_correlation_id = ?
		WHERE request_id = ?
	`, boolInt(route.IsStream), route.HandlerType, route.RequestedModel, route.ClientCorrelationID, requestID)
	return err
}

func (s *Store) RecordRequestEvent(ctx context.Context, requestID, attemptID, eventType string, ts time.Time, payload any) error {
	if s == nil {
		return nil
	}
	data := tracing.MarshalJSONMap(payload)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_event (request_id, attempt_id, ts_ns, event_type, payload_json, boot_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, requestID, emptyToNull(attemptID), unixNS(ts), eventType, data, s.bootID)
	if err != nil {
		return err
	}
	seq, err := result.LastInsertId()
	if err == nil {
		s.latestSeq.Store(seq)
		s.broadcast(seq)
	}
	return nil
}

func (s *Store) BeginAttempt(ctx context.Context, start tracing.AttemptStart) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_attempt (
			attempt_id, request_id, attempt_no, retry_scope, provider, executor_id,
			auth_id, auth_index, auth_snapshot_json, route_model, upstream_model,
			upstream_url, upstream_method, upstream_protocol,
			started_at_ns, headers_at_ns, first_byte_at_ns, finished_at_ns,
			status_code, outcome, error_code, error_message,
			request_headers_json, request_body_blob_id, response_headers_json, response_body_blob_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '',
			?, 0, 0, 0, 0, ?, '', '', NULL, NULL, NULL, NULL)
	`, start.AttemptID, start.RequestID, start.AttemptNo, start.RetryScope, start.Provider, start.ExecutorID,
		start.AuthID, start.AuthIndex, start.AuthSnapshotJSON, start.RouteModel, start.UpstreamModel,
		unixNS(start.StartedAt), tracing.AttemptOutcomeRunning)
	return err
}

func (s *Store) UpdateAttemptRequest(ctx context.Context, request tracing.AttemptRequest) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE trace_attempt
		SET upstream_url = ?, upstream_method = ?, upstream_protocol = ?,
			request_headers_json = ?, request_body_blob_id = ?
		WHERE attempt_id = ?
	`, request.UpstreamURL, request.UpstreamMethod, request.UpstreamProtocol,
		tracing.HeaderJSON(request.RequestHeaders), nullableString(request.RequestBodyBlobID), request.AttemptID)
	return err
}

func (s *Store) UpdateAttemptResponse(ctx context.Context, response tracing.AttemptResponse) error {
	if s == nil {
		return nil
	}
	sets := make([]string, 0, 5)
	args := make([]any, 0, 6)
	if response.StatusCode > 0 {
		sets = append(sets, "status_code = ?")
		args = append(args, response.StatusCode)
	}
	if !response.HeadersAt.IsZero() {
		sets = append(sets, "headers_at_ns = ?")
		args = append(args, unixNS(response.HeadersAt))
	}
	if !response.FirstByteAt.IsZero() {
		sets = append(sets, "first_byte_at_ns = ?")
		args = append(args, unixNS(response.FirstByteAt))
	}
	if len(response.ResponseHeaders) > 0 {
		sets = append(sets, "response_headers_json = ?")
		args = append(args, tracing.HeaderJSON(response.ResponseHeaders))
	}
	if response.ResponseBodyBlobID != "" {
		sets = append(sets, "response_body_blob_id = ?")
		args = append(args, response.ResponseBodyBlobID)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, response.AttemptID)
	query := "UPDATE trace_attempt SET " + strings.Join(sets, ", ") + " WHERE attempt_id = ?"
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) FinishAttempt(ctx context.Context, finish tracing.AttemptFinish) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE trace_attempt
		SET finished_at_ns = ?, outcome = ?, status_code = CASE WHEN ? > 0 THEN ? ELSE status_code END,
			error_code = ?, error_message = ?
		WHERE attempt_id = ?
	`, unixNS(finish.FinishedAt), finish.Outcome, finish.StatusCode, finish.StatusCode,
		finish.ErrorCode, finish.ErrorMessage, finish.AttemptID)
	return err
}

func (s *Store) FinalizeRequest(ctx context.Context, finish tracing.RequestFinish) error {
	if s == nil {
		return nil
	}
	state := finish.Status
	if state == "" {
		state = tracing.RequestStatusSucceeded
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE trace_request
		SET finished_at_ns = ?, status = ?, downstream_status_code = ?,
			downstream_first_byte_at_ns = ?, response_headers_json = ?, response_body_blob_id = ?
		WHERE request_id = ?
	`, unixNS(finish.FinishedAt), state, finish.StatusCode, unixNS(finish.FirstByteAt),
		tracing.HeaderJSON(finish.ResponseHeaders), nullableString(finish.ResponseBodyBlobID), finish.RequestID)
	return err
}

func (s *Store) FinalizeUsage(ctx context.Context, final tracing.UsageFinal) error {
	if s == nil {
		return nil
	}
	inputTokens := nullableUsageMetric(final.Completeness, final.InputTokens)
	outputTokens := nullableUsageMetric(final.Completeness, final.OutputTokens)
	reasoningTokens := nullableUsageMetric(final.Completeness, final.ReasoningTokens)
	cachedTokens := nullableUsageMetric(final.Completeness, final.CachedTokens)
	totalTokens := nullableUsageMetric(final.Completeness, final.TotalTokens)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_usage_final (
			request_id, attempt_id, finalized_at_ns, status, completeness, input_tokens,
			output_tokens, reasoning_tokens, cached_tokens, total_tokens, derived_total,
			provider_usage_json, provider_native_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id) DO UPDATE SET
			attempt_id = excluded.attempt_id,
			finalized_at_ns = excluded.finalized_at_ns,
			status = excluded.status,
			completeness = excluded.completeness,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			cached_tokens = excluded.cached_tokens,
			total_tokens = excluded.total_tokens,
			derived_total = excluded.derived_total,
			provider_usage_json = excluded.provider_usage_json,
			provider_native_json = excluded.provider_native_json
	`, final.RequestID, emptyToNull(final.AttemptID), unixNS(final.FinalizedAt), final.Status, final.Completeness,
		inputTokens, outputTokens, reasoningTokens, cachedTokens, totalTokens,
		boolInt(final.DerivedTotal), final.ProviderUsageJSON, final.ProviderNativeJSON)
	return err
}

func (s *Store) SaveBlob(ctx context.Context, blob *tracing.BlobRecord) error {
	if s == nil || blob == nil || blob.BlobID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_blob (
			blob_id, storage_kind, size_bytes, content_type, content_encoding,
			complete, truncated, sha256, file_relpath, inline_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(blob_id) DO UPDATE SET
			storage_kind = excluded.storage_kind,
			size_bytes = excluded.size_bytes,
			content_type = excluded.content_type,
			content_encoding = excluded.content_encoding,
			complete = excluded.complete,
			truncated = excluded.truncated,
			sha256 = excluded.sha256,
			file_relpath = excluded.file_relpath,
			inline_bytes = excluded.inline_bytes
	`, blob.BlobID, blob.StorageKind, blob.SizeBytes, blob.ContentType, blob.ContentEncoding,
		boolInt(blob.Complete), boolInt(blob.Truncated), blob.SHA256, emptyToNull(blob.FileRelPath), blob.InlineBytes)
	return err
}

func (s *Store) Status(ctx context.Context) (tracing.StatusRecord, error) {
	var requestsRunning, attemptsRunning int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_request WHERE status = ?`, tracing.RequestStatusRunning).Scan(&requestsRunning); err != nil {
		return tracing.StatusRecord{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_attempt WHERE outcome = ?`, tracing.AttemptOutcomeRunning).Scan(&attemptsRunning); err != nil {
		return tracing.StatusRecord{}, err
	}
	return tracing.StatusRecord{
		Enabled:         true,
		BootID:          s.bootID,
		LatestSeq:       s.latestSeq.Load(),
		RequestsRunning: requestsRunning,
		AttemptsRunning: attemptsRunning,
		DBPath:          s.dbPath,
		BodiesDir:       s.bodiesDir,
	}, nil
}

func (s *Store) ListEvents(ctx context.Context, afterSeq int64, limit int) ([]tracing.EventRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, COALESCE(request_id, ''), COALESCE(attempt_id, ''), ts_ns, event_type, COALESCE(payload_json, x''), boot_id
		FROM trace_event
		WHERE seq > ?
		ORDER BY seq ASC
		LIMIT ?
	`, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []tracing.EventRecord
	for rows.Next() {
		var item tracing.EventRecord
		if err := rows.Scan(&item.Seq, &item.RequestID, &item.AttemptID, &item.TSUnixNS, &item.EventType, &item.Payload, &item.BootID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetRequest(ctx context.Context, requestID string) (*tracing.RequestRecord, error) {
	var record tracing.RequestRecord
	var isStream int
	err := s.db.QueryRowContext(ctx, `
		SELECT request_id, COALESCE(legacy_request_id, ''), started_at_ns, COALESCE(finished_at_ns, 0),
			status, http_method, http_scheme, http_host, http_path, http_query,
			COALESCE(is_stream, 0), COALESCE(handler_type, ''), COALESCE(requested_model, ''),
			COALESCE(client_correlation_id, ''), COALESCE(downstream_status_code, 0),
			COALESCE(downstream_first_byte_at_ns, 0), COALESCE(request_headers_json, x''),
			COALESCE(request_body_blob_id, ''), COALESCE(response_headers_json, x''),
			COALESCE(response_body_blob_id, '')
		FROM trace_request
		WHERE request_id = ?
	`, requestID).Scan(
		&record.RequestID, &record.LegacyRequestID, &record.StartedAtNS, &record.FinishedAtNS,
		&record.Status, &record.HTTPMethod, &record.HTTPScheme, &record.HTTPHost, &record.HTTPPath, &record.HTTPQuery,
		&isStream, &record.HandlerType, &record.RequestedModel,
		&record.ClientCorrelationID, &record.DownstreamStatusCode, &record.DownstreamFirstByteNS,
		&record.RequestHeadersJSON, &record.RequestBodyBlobID, &record.ResponseHeadersJSON, &record.ResponseBodyBlobID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.IsStream = isStream != 0
	return &record, nil
}

func (s *Store) ListAttempts(ctx context.Context, requestID string) ([]tracing.AttemptRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt_id, request_id, attempt_no, retry_scope, provider, COALESCE(executor_id, ''),
			COALESCE(auth_id, ''), COALESCE(auth_index, ''), COALESCE(auth_snapshot_json, x''),
			COALESCE(route_model, ''), COALESCE(upstream_model, ''), COALESCE(upstream_url, ''),
			COALESCE(upstream_method, ''), COALESCE(upstream_protocol, ''), started_at_ns,
			COALESCE(headers_at_ns, 0), COALESCE(first_byte_at_ns, 0), COALESCE(finished_at_ns, 0),
			COALESCE(status_code, 0), outcome, COALESCE(error_code, ''), COALESCE(error_message, ''),
			COALESCE(request_headers_json, x''), COALESCE(request_body_blob_id, ''), COALESCE(response_headers_json, x''),
			COALESCE(response_body_blob_id, '')
		FROM trace_attempt
		WHERE request_id = ?
		ORDER BY attempt_no ASC
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []tracing.AttemptRecord
	for rows.Next() {
		var item tracing.AttemptRecord
		if err := rows.Scan(
			&item.AttemptID, &item.RequestID, &item.AttemptNo, &item.RetryScope, &item.Provider, &item.ExecutorID,
			&item.AuthID, &item.AuthIndex, &item.AuthSnapshotJSON, &item.RouteModel, &item.UpstreamModel,
			&item.UpstreamURL, &item.UpstreamMethod, &item.UpstreamProtocol, &item.StartedAtNS, &item.HeadersAtNS,
			&item.FirstByteAtNS, &item.FinishedAtNS, &item.StatusCode, &item.Outcome, &item.ErrorCode,
			&item.ErrorMessage, &item.RequestHeadersJSON, &item.RequestBodyBlobID, &item.ResponseHeadersJSON, &item.ResponseBodyBlobID,
		); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetUsage(ctx context.Context, requestID string) (*tracing.UsageFinalRecord, error) {
	var record tracing.UsageFinalRecord
	var derivedTotal int
	var inputTokens, outputTokens, reasoningTokens, cachedTokens, totalTokens sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT request_id, COALESCE(attempt_id, ''), finalized_at_ns, status, completeness,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			COALESCE(derived_total, 0), COALESCE(provider_usage_json, x''), COALESCE(provider_native_json, x'')
		FROM trace_usage_final
		WHERE request_id = ?
	`, requestID).Scan(
		&record.RequestID, &record.AttemptID, &record.FinalizedAtNS, &record.Status, &record.Completeness,
		&inputTokens, &outputTokens, &reasoningTokens, &cachedTokens, &totalTokens,
		&derivedTotal, &record.ProviderUsageJSON, &record.ProviderNativeJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.InputTokens = nullInt64Ptr(inputTokens)
	record.OutputTokens = nullInt64Ptr(outputTokens)
	record.ReasoningTokens = nullInt64Ptr(reasoningTokens)
	record.CachedTokens = nullInt64Ptr(cachedTokens)
	record.TotalTokens = nullInt64Ptr(totalTokens)
	record.DerivedTotal = derivedTotal != 0
	return &record, nil
}

func (s *Store) GetBlob(ctx context.Context, blobID string) (*tracing.BlobRecord, error) {
	var record tracing.BlobRecord
	var complete, truncated int
	err := s.db.QueryRowContext(ctx, `
		SELECT blob_id, storage_kind, size_bytes, COALESCE(content_type, ''), COALESCE(content_encoding, ''),
			COALESCE(complete, 0), COALESCE(truncated, 0), COALESCE(sha256, ''), COALESCE(file_relpath, ''), COALESCE(inline_bytes, x'')
		FROM trace_blob
		WHERE blob_id = ?
	`, blobID).Scan(
		&record.BlobID, &record.StorageKind, &record.SizeBytes, &record.ContentType, &record.ContentEncoding,
		&complete, &truncated, &record.SHA256, &record.FileRelPath, &record.InlineBytes,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.Complete = complete != 0
	record.Truncated = truncated != 0
	if record.StorageKind == "file" && record.FileRelPath != "" {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(s.dbPath), record.FileRelPath))
		if err != nil {
			return nil, err
		}
		record.InlineBytes = data
	}
	return &record, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init(ctx context.Context) error {
	pragmas := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=FULL;`,
		`PRAGMA foreign_keys=ON;`,
		`PRAGMA busy_timeout=5000;`,
	}
	for _, stmt := range pragmas {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if err := s.createSchema(ctx); err != nil {
		return err
	}
	if err := s.migrateSchema(ctx, version); err != nil {
		return err
	}
	return s.setSchemaVersion(ctx, currentSchemaVersion)
}

func (s *Store) createSchema(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS trace_event (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL,
			attempt_id TEXT,
			ts_ns INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			payload_json BLOB,
			boot_id TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_event_request_seq ON trace_event(request_id, seq);`,
		`CREATE TABLE IF NOT EXISTS trace_request (
			request_id TEXT PRIMARY KEY,
			legacy_request_id TEXT,
			started_at_ns INTEGER NOT NULL,
			finished_at_ns INTEGER,
			status TEXT NOT NULL,
			http_method TEXT NOT NULL,
			http_scheme TEXT,
			http_host TEXT,
			http_path TEXT,
			http_query TEXT,
			is_stream INTEGER NOT NULL DEFAULT 0,
			handler_type TEXT,
			requested_model TEXT,
			client_correlation_id TEXT,
			downstream_status_code INTEGER,
			downstream_first_byte_at_ns INTEGER,
			request_headers_json BLOB,
			request_body_blob_id TEXT,
			response_headers_json BLOB,
			response_body_blob_id TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS trace_attempt (
			attempt_id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			retry_scope TEXT NOT NULL,
			provider TEXT NOT NULL,
			executor_id TEXT,
			auth_id TEXT,
			auth_index TEXT,
			auth_snapshot_json BLOB,
			route_model TEXT,
			upstream_model TEXT,
			upstream_url TEXT,
			upstream_method TEXT,
			upstream_protocol TEXT,
			started_at_ns INTEGER NOT NULL,
			headers_at_ns INTEGER,
			first_byte_at_ns INTEGER,
			finished_at_ns INTEGER,
			status_code INTEGER,
			outcome TEXT NOT NULL,
			error_code TEXT,
			error_message TEXT,
			request_headers_json BLOB,
			request_body_blob_id TEXT,
			response_headers_json BLOB,
			response_body_blob_id TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_attempt_request_no ON trace_attempt(request_id, attempt_no);`,
		`CREATE TABLE IF NOT EXISTS trace_usage_final (
			request_id TEXT PRIMARY KEY,
			attempt_id TEXT,
			finalized_at_ns INTEGER NOT NULL,
			status TEXT NOT NULL,
			completeness TEXT NOT NULL,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cached_tokens INTEGER,
			total_tokens INTEGER,
			derived_total INTEGER NOT NULL DEFAULT 0,
			provider_usage_json BLOB,
			provider_native_json BLOB
		);`,
		`CREATE TABLE IF NOT EXISTS trace_blob (
			blob_id TEXT PRIMARY KEY,
			storage_kind TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			content_type TEXT,
			content_encoding TEXT,
			complete INTEGER NOT NULL,
			truncated INTEGER NOT NULL,
			sha256 TEXT,
			file_relpath TEXT,
			inline_bytes BLOB
		);`,
		`CREATE TABLE IF NOT EXISTS trace_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);`,
	}
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	metaExists, err := s.tableExists(ctx, "trace_meta")
	if err != nil {
		return 0, err
	}
	if !metaExists {
		legacyExists, err := s.tableExists(ctx, "trace_usage_final")
		if err != nil {
			return 0, err
		}
		if legacyExists {
			return 1, nil
		}
		return 0, nil
	}

	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM trace_meta WHERE key = 'schema_version'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		legacyExists, tableErr := s.tableExists(ctx, "trace_usage_final")
		if tableErr != nil {
			return 0, tableErr
		}
		if legacyExists {
			return 1, nil
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("tracing sqlite: invalid schema version %q: %w", raw, err)
	}
	return version, nil
}

func (s *Store) migrateSchema(ctx context.Context, version int) error {
	if version >= currentSchemaVersion {
		return nil
	}
	if version == 0 {
		return nil
	}
	if err := s.migrateUsageFinalTable(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) migrateUsageFinalTable(ctx context.Context) error {
	exists, err := s.tableExists(ctx, "trace_usage_final")
	if err != nil || !exists {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `ALTER TABLE trace_usage_final RENAME TO trace_usage_final_v1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		CREATE TABLE trace_usage_final (
			request_id TEXT PRIMARY KEY,
			attempt_id TEXT,
			finalized_at_ns INTEGER NOT NULL,
			status TEXT NOT NULL,
			completeness TEXT NOT NULL,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cached_tokens INTEGER,
			total_tokens INTEGER,
			derived_total INTEGER NOT NULL DEFAULT 0,
			provider_usage_json BLOB,
			provider_native_json BLOB
		)
	`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO trace_usage_final (
			request_id, attempt_id, finalized_at_ns, status, completeness, input_tokens,
			output_tokens, reasoning_tokens, cached_tokens, total_tokens, derived_total,
			provider_usage_json, provider_native_json
		)
		SELECT
			request_id,
			attempt_id,
			finalized_at_ns,
			status,
			completeness,
			CASE
				WHEN completeness = 'missing'
					AND COALESCE(input_tokens, 0) = 0
					AND COALESCE(output_tokens, 0) = 0
					AND COALESCE(reasoning_tokens, 0) = 0
					AND COALESCE(cached_tokens, 0) = 0
					AND COALESCE(total_tokens, 0) = 0
				THEN NULL
				ELSE input_tokens
			END,
			CASE
				WHEN completeness = 'missing'
					AND COALESCE(input_tokens, 0) = 0
					AND COALESCE(output_tokens, 0) = 0
					AND COALESCE(reasoning_tokens, 0) = 0
					AND COALESCE(cached_tokens, 0) = 0
					AND COALESCE(total_tokens, 0) = 0
				THEN NULL
				ELSE output_tokens
			END,
			CASE
				WHEN completeness = 'missing'
					AND COALESCE(input_tokens, 0) = 0
					AND COALESCE(output_tokens, 0) = 0
					AND COALESCE(reasoning_tokens, 0) = 0
					AND COALESCE(cached_tokens, 0) = 0
					AND COALESCE(total_tokens, 0) = 0
				THEN NULL
				ELSE reasoning_tokens
			END,
			CASE
				WHEN completeness = 'missing'
					AND COALESCE(input_tokens, 0) = 0
					AND COALESCE(output_tokens, 0) = 0
					AND COALESCE(reasoning_tokens, 0) = 0
					AND COALESCE(cached_tokens, 0) = 0
					AND COALESCE(total_tokens, 0) = 0
				THEN NULL
				ELSE cached_tokens
			END,
			CASE
				WHEN completeness = 'missing'
					AND COALESCE(input_tokens, 0) = 0
					AND COALESCE(output_tokens, 0) = 0
					AND COALESCE(reasoning_tokens, 0) = 0
					AND COALESCE(cached_tokens, 0) = 0
					AND COALESCE(total_tokens, 0) = 0
				THEN NULL
				ELSE total_tokens
			END,
			COALESCE(derived_total, 0),
			provider_usage_json,
			provider_native_json
		FROM trace_usage_final_v1
	`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE trace_usage_final_v1`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) setSchemaVersion(ctx context.Context, version int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trace_meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, strconv.Itoa(version))
	return err
}

func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM sqlite_master
		WHERE type = 'table' AND name = ?
	`, name).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) recoverRunning(ctx context.Context) error {
	now := unixNS(time.Now().UTC())
	if _, err := s.db.ExecContext(ctx, `
		UPDATE trace_request
		SET status = ?, finished_at_ns = CASE WHEN COALESCE(finished_at_ns, 0) = 0 THEN ? ELSE finished_at_ns END
		WHERE status = ?
	`, tracing.RequestStatusInterrupted, now, tracing.RequestStatusRunning); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE trace_attempt
		SET outcome = ?, finished_at_ns = CASE WHEN COALESCE(finished_at_ns, 0) = 0 THEN ? ELSE finished_at_ns END,
			error_code = CASE WHEN COALESCE(error_code, '') = '' THEN 'interrupted' ELSE error_code END,
			error_message = CASE WHEN COALESCE(error_message, '') = '' THEN 'process restarted before attempt finished' ELSE error_message END
		WHERE outcome = ?
	`, tracing.AttemptOutcomeInterrupted, now, tracing.AttemptOutcomeRunning); err != nil {
		return err
	}
	return nil
}

func (s *Store) prune(ctx context.Context, pruneDays int) error {
	cutoff := time.Now().UTC().Add(-time.Duration(pruneDays) * 24 * time.Hour).UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT blob_id, file_relpath
		FROM trace_blob
		WHERE blob_id IN (
			SELECT request_body_blob_id FROM trace_request WHERE started_at_ns < ? AND request_body_blob_id IS NOT NULL
			UNION
			SELECT response_body_blob_id FROM trace_request WHERE started_at_ns < ? AND response_body_blob_id IS NOT NULL
			UNION
			SELECT request_body_blob_id FROM trace_attempt WHERE started_at_ns < ? AND request_body_blob_id IS NOT NULL
			UNION
			SELECT response_body_blob_id FROM trace_attempt WHERE started_at_ns < ? AND response_body_blob_id IS NOT NULL
		)
	`, cutoff, cutoff, cutoff, cutoff)
	if err != nil {
		return err
	}
	var filesToRemove []string
	for rows.Next() {
		var blobID, relPath string
		if err := rows.Scan(&blobID, &relPath); err != nil {
			rows.Close()
			return err
		}
		if relPath != "" {
			filesToRemove = append(filesToRemove, relPath)
		}
	}
	rows.Close()
	statements := []string{
		`DELETE FROM trace_event WHERE ts_ns < ?`,
		`DELETE FROM trace_usage_final WHERE request_id IN (SELECT request_id FROM trace_request WHERE started_at_ns < ?)`,
		`DELETE FROM trace_attempt WHERE started_at_ns < ?`,
		`DELETE FROM trace_request WHERE started_at_ns < ?`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt, cutoff); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trace_blob WHERE blob_id NOT IN (
		SELECT request_body_blob_id FROM trace_request WHERE request_body_blob_id IS NOT NULL
		UNION SELECT response_body_blob_id FROM trace_request WHERE response_body_blob_id IS NOT NULL
		UNION SELECT request_body_blob_id FROM trace_attempt WHERE request_body_blob_id IS NOT NULL
		UNION SELECT response_body_blob_id FROM trace_attempt WHERE response_body_blob_id IS NOT NULL
	)`); err != nil {
		return err
	}
	for _, rel := range filesToRemove {
		_ = os.Remove(filepath.Join(filepath.Dir(s.dbPath), rel))
	}
	return nil
}

func (s *Store) broadcast(seq int64) {
	if s == nil || !s.emitSSE {
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- seq:
		default:
		}
	}
}

func unixNS(ts time.Time) int64 {
	if ts.IsZero() {
		return 0
	}
	return ts.UTC().UnixNano()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func emptyToNull(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullableUsageMetric(completeness string, value int64) any {
	if completeness == tracing.UsageCompletenessMissing {
		return nil
	}
	return value
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}
