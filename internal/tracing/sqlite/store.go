package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
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
	readDB    *sql.DB
	bootID    string
	dbPath    string
	bodiesDir string
	emitSSE   bool

	latestSeq atomic.Int64

	summarySubMu sync.Mutex
	summarySubs  map[chan tracing.RequestSummaryEvent]struct{}
}

const currentSchemaVersion = 3

func New(ctx context.Context, cfg Config) (*Store, error) {
	baseDir := strings.TrimSpace(cfg.BaseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("tracing sqlite: base dir is empty")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(baseDir, "tracing.db")
	db, err := openDB(dbPath, 1, 1)
	if err != nil {
		return nil, err
	}
	readDB, err := openDB(dbPath, 8, 8)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	store := &Store{
		db:          db,
		readDB:      readDB,
		bootID:      tracing.MustNewID(),
		dbPath:      dbPath,
		bodiesDir:   filepath.Join(baseDir, "bodies"),
		emitSSE:     cfg.EmitSSE,
		summarySubs: make(map[chan tracing.RequestSummaryEvent]struct{}),
	}
	if err := os.MkdirAll(store.bodiesDir, 0o755); err != nil {
		_ = db.Close()
		_ = readDB.Close()
		return nil, err
	}
	previousVersion, err := store.init(ctx)
	if err != nil {
		_ = db.Close()
		_ = readDB.Close()
		return nil, err
	}
	if cfg.PruneDays > 0 {
		if err := store.prune(ctx, cfg.PruneDays); err != nil {
			_ = db.Close()
			_ = readDB.Close()
			return nil, err
		}
	}
	if err := store.recoverRunning(ctx); err != nil {
		_ = db.Close()
		_ = readDB.Close()
		return nil, err
	}
	if previousVersion > 0 && previousVersion < currentSchemaVersion {
		if err := store.rebuildRequestSummaries(ctx); err != nil {
			_ = db.Close()
			_ = readDB.Close()
			return nil, err
		}
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

func (s *Store) SubscribeRequestSummaries() (<-chan tracing.RequestSummaryEvent, func()) {
	ch := make(chan tracing.RequestSummaryEvent, 32)
	if s == nil || !s.emitSSE {
		close(ch)
		return ch, func() {}
	}
	s.summarySubMu.Lock()
	s.summarySubs[ch] = struct{}{}
	s.summarySubMu.Unlock()
	cancel := func() {
		s.summarySubMu.Lock()
		delete(s.summarySubs, ch)
		s.summarySubMu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (s *Store) StartRequest(ctx context.Context, start tracing.RequestStart) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishRequestSummary(ctx, start.RequestID, s.publishStartedRequestSummary, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
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
	})
}

func (s *Store) UpdateRequestRoute(ctx context.Context, requestID string, route tracing.RequestRouteInfo) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishRequestSummary(ctx, requestID, s.publishUpdatedRequestSummary, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE trace_request
			SET is_stream = ?, handler_type = ?, requested_model = ?, client_correlation_id = ?
			WHERE request_id = ?
		`, boolInt(route.IsStream), route.HandlerType, route.RequestedModel, route.ClientCorrelationID, requestID)
		return err
	})
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
	}
	return nil
}

func (s *Store) BeginAttempt(ctx context.Context, start tracing.AttemptStart) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishRequestSummary(ctx, start.RequestID, s.publishUpdatedRequestSummary, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
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
	})
}

func (s *Store) UpdateAttemptRequest(ctx context.Context, request tracing.AttemptRequest) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishAttemptSummary(ctx, request.AttemptID, s.publishUpdatedRequestSummary, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE trace_attempt
			SET upstream_url = ?, upstream_method = ?, upstream_protocol = ?,
				request_headers_json = ?, request_body_blob_id = ?
			WHERE attempt_id = ?
		`, request.UpstreamURL, request.UpstreamMethod, request.UpstreamProtocol,
			tracing.HeaderJSON(request.RequestHeaders), nullableString(request.RequestBodyBlobID), request.AttemptID)
		return err
	})
}

func (s *Store) UpdateAttemptResponse(ctx context.Context, response tracing.AttemptResponse) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishAttemptSummary(ctx, response.AttemptID, s.publishUpdatedRequestSummary, func(tx *sql.Tx) error {
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
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

func (s *Store) FinishAttempt(ctx context.Context, finish tracing.AttemptFinish) error {
	if s == nil {
		return nil
	}
	return s.writeAndPublishAttemptSummary(ctx, finish.AttemptID, s.publishUpdatedRequestSummary, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE trace_attempt
			SET finished_at_ns = ?, outcome = ?, status_code = CASE WHEN ? > 0 THEN ? ELSE status_code END,
				error_code = ?, error_message = ?
			WHERE attempt_id = ?
		`, unixNS(finish.FinishedAt), finish.Outcome, finish.StatusCode, finish.StatusCode,
			finish.ErrorCode, finish.ErrorMessage, finish.AttemptID)
		return err
	})
}

func (s *Store) FinalizeRequest(ctx context.Context, finish tracing.RequestFinish) error {
	if s == nil {
		return nil
	}
	state := finish.Status
	if state == "" {
		state = tracing.RequestStatusSucceeded
	}
	return s.writeAndPublishRequestSummary(ctx, finish.RequestID, s.publishEndedRequestSummaryIfReady, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE trace_request
			SET finished_at_ns = ?, status = ?, downstream_status_code = ?,
				downstream_first_byte_at_ns = ?, response_headers_json = ?, response_body_blob_id = ?
			WHERE request_id = ?
		`, unixNS(finish.FinishedAt), state, finish.StatusCode, unixNS(finish.FirstByteAt),
			tracing.HeaderJSON(finish.ResponseHeaders), nullableString(finish.ResponseBodyBlobID), finish.RequestID)
		return err
	})
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
	return s.writeAndPublishRequestSummary(ctx, final.RequestID, s.publishSummaryAfterUsageFinalize, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
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
	})
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
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_request WHERE status = ?`, tracing.RequestStatusRunning).Scan(&requestsRunning); err != nil {
		return tracing.StatusRecord{}, err
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_attempt WHERE outcome = ?`, tracing.AttemptOutcomeRunning).Scan(&attemptsRunning); err != nil {
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
	rows, err := s.readDB.QueryContext(ctx, `
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

func (s *Store) ListRequestSummaries(ctx context.Context, filter tracing.RequestSummaryFilter) (tracing.RequestSummaryPage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	whereClause, args := makeSummaryWhereClause(filter)

	var totalCount int
	countQuery := `SELECT COUNT(*) FROM trace_request_summary ` + whereClause
	if err := s.readDB.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return tracing.RequestSummaryPage{}, err
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT
			request_id, started_at_ns, COALESCE(provider, ''), COALESCE(requested_model, ''),
			COALESCE(route_model, ''), COALESCE(upstream_model, ''), COALESCE(auth_label, ''),
			COALESCE(auth_account, ''), COALESCE(auth_path, ''), COALESCE(auth_index, ''),
			http_method, http_path, COALESCE(http_query, ''), status_code, duration_ms,
			input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens,
			request_status, COALESCE(usage_completeness, ''), COALESCE(is_stream, 0), updated_at_ns
		FROM trace_request_summary
	`+whereClause+`
		ORDER BY started_at_ns DESC
		LIMIT ? OFFSET ?
	`, pageArgs...)
	if err != nil {
		return tracing.RequestSummaryPage{}, err
	}
	defer rows.Close()

	items := make([]tracing.RequestSummaryRecord, 0, limit)
	for rows.Next() {
		item, scanErr := scanRequestSummary(rows)
		if scanErr != nil {
			return tracing.RequestSummaryPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return tracing.RequestSummaryPage{}, err
	}

	var nextOffset *int
	if offset+len(items) < totalCount {
		value := offset + len(items)
		nextOffset = &value
	}

	return tracing.RequestSummaryPage{
		Items:      items,
		TotalCount: totalCount,
		NextOffset: nextOffset,
	}, nil
}

func (s *Store) GetRequestSummary(ctx context.Context, requestID string) (*tracing.RequestSummaryRecord, error) {
	return s.getRequestSummary(ctx, requestID)
}

func (s *Store) GetRequestDetail(ctx context.Context, requestID string) (*tracing.RequestDetailRecord, error) {
	requestRecord, err := s.GetRequest(ctx, requestID)
	if err != nil || requestRecord == nil {
		return nil, err
	}

	attempts, err := s.ListAttempts(ctx, requestID)
	if err != nil {
		return nil, err
	}
	usageRecord, err := s.GetUsage(ctx, requestID)
	if err != nil {
		return nil, err
	}

	detail := &tracing.RequestDetailRecord{
		Request:    requestRecord,
		Attempts:   attempts,
		UsageFinal: usageRecord,
	}

	if requestRecord.RequestBodyBlobID != "" {
		if record, blobErr := s.getBlobMetadata(ctx, requestRecord.RequestBodyBlobID); blobErr != nil {
			return nil, blobErr
		} else {
			detail.RequestBlob = record
		}
	}
	if requestRecord.ResponseBodyBlobID != "" {
		if record, blobErr := s.getBlobMetadata(ctx, requestRecord.ResponseBodyBlobID); blobErr != nil {
			return nil, blobErr
		} else {
			detail.ResponseBlob = record
		}
	}

	for _, attempt := range attempts {
		if attempt.RequestBodyBlobID != "" {
			record, blobErr := s.getBlobMetadata(ctx, attempt.RequestBodyBlobID)
			if blobErr != nil {
				return nil, blobErr
			}
			if record != nil {
				if detail.AttemptRequestBlobs == nil {
					detail.AttemptRequestBlobs = make(map[string]tracing.BlobRecord)
				}
				detail.AttemptRequestBlobs[attempt.AttemptID] = *record
			}
		}
		if attempt.ResponseBodyBlobID != "" {
			record, blobErr := s.getBlobMetadata(ctx, attempt.ResponseBodyBlobID)
			if blobErr != nil {
				return nil, blobErr
			}
			if record != nil {
				if detail.AttemptResponseBlobs == nil {
					detail.AttemptResponseBlobs = make(map[string]tracing.BlobRecord)
				}
				detail.AttemptResponseBlobs[attempt.AttemptID] = *record
			}
		}
	}

	return detail, nil
}

func (s *Store) GetRequest(ctx context.Context, requestID string) (*tracing.RequestRecord, error) {
	var record tracing.RequestRecord
	var isStream int
	err := s.readDB.QueryRowContext(ctx, `
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
	rows, err := s.readDB.QueryContext(ctx, `
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
	err := s.readDB.QueryRowContext(ctx, `
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
	err := s.readDB.QueryRowContext(ctx, `
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
	if s == nil {
		return nil
	}
	var closeErr error
	if s.readDB != nil {
		closeErr = s.readDB.Close()
	}
	if s.db != nil {
		if err := s.db.Close(); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (s *Store) init(ctx context.Context) (int, error) {
	pragmas := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=NORMAL;`,
		`PRAGMA foreign_keys=ON;`,
		`PRAGMA busy_timeout=5000;`,
	}
	for _, stmt := range pragmas {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return 0, err
		}
		if _, err := s.readDB.ExecContext(ctx, stmt); err != nil {
			return 0, err
		}
	}
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.createSchema(ctx); err != nil {
		return 0, err
	}
	if err := s.migrateSchema(ctx, version); err != nil {
		return 0, err
	}
	return version, s.setSchemaVersion(ctx, currentSchemaVersion)
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
		`CREATE TABLE IF NOT EXISTS trace_request_summary (
			request_id TEXT PRIMARY KEY,
			started_at_ns INTEGER NOT NULL,
			provider TEXT,
			requested_model TEXT,
			route_model TEXT,
			upstream_model TEXT,
			auth_label TEXT,
			auth_account TEXT,
			auth_path TEXT,
			auth_index TEXT,
			http_method TEXT NOT NULL,
			http_path TEXT NOT NULL,
			http_query TEXT,
			status_code INTEGER,
			duration_ms INTEGER,
			input_tokens INTEGER,
			output_tokens INTEGER,
			total_tokens INTEGER,
			reasoning_tokens INTEGER,
			cached_tokens INTEGER,
			request_status TEXT NOT NULL,
			usage_completeness TEXT,
			is_stream INTEGER NOT NULL DEFAULT 0,
			updated_at_ns INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_started_at ON trace_request_summary(started_at_ns DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_status_started ON trace_request_summary(request_status, started_at_ns DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_model_started ON trace_request_summary(requested_model, started_at_ns DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_provider_started ON trace_request_summary(provider, started_at_ns DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_stream_started ON trace_request_summary(is_stream, started_at_ns DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_trace_request_summary_updated_at ON trace_request_summary(updated_at_ns DESC);`,
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
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trace_request_summary WHERE request_id NOT IN (
		SELECT request_id FROM trace_request
	)`); err != nil {
		return err
	}
	for _, rel := range filesToRemove {
		_ = os.Remove(filepath.Join(filepath.Dir(s.dbPath), rel))
	}
	return nil
}

func (s *Store) DeleteRequest(ctx context.Context, requestID string) (bool, error) {
	if s == nil {
		return false, nil
	}

	var (
		found         bool
		filesToRemove []string
	)
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		record, err := getRequestByQueryer(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if record == nil {
			return nil
		}
		found = true

		blobRefs, err := listRequestBlobRefs(ctx, tx, requestID)
		if err != nil {
			return err
		}

		statements := []string{
			`DELETE FROM trace_event WHERE request_id = ?`,
			`DELETE FROM trace_usage_final WHERE request_id = ?`,
			`DELETE FROM trace_attempt WHERE request_id = ?`,
			`DELETE FROM trace_request_summary WHERE request_id = ?`,
			`DELETE FROM trace_request WHERE request_id = ?`,
		}
		for _, stmt := range statements {
			if _, err := tx.ExecContext(ctx, stmt, requestID); err != nil {
				return err
			}
		}

		filesToRemove, err = deleteOrphanBlobs(ctx, tx, blobRefs)
		return err
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	for _, relPath := range filesToRemove {
		_ = os.Remove(filepath.Join(filepath.Dir(s.dbPath), relPath))
	}
	s.broadcastRequestSummaryEvent(tracing.RequestSummaryEvent{
		Type:      tracing.RequestSummaryEventDeleted,
		RequestID: requestID,
		LatestSeq: s.latestSeq.Load(),
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	})
	return true, nil
}

func (s *Store) withWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
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
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

type requestSummaryPublisher func(context.Context, string)

func (s *Store) writeAndPublishRequestSummary(ctx context.Context, requestID string, publish requestSummaryPublisher, fn func(tx *sql.Tx) error) error {
	if err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return upsertRequestSummary(ctx, tx, requestID)
	}); err != nil {
		return err
	}
	if publish != nil {
		publish(ctx, requestID)
	}
	return nil
}

func (s *Store) writeAndPublishAttemptSummary(ctx context.Context, attemptID string, publish requestSummaryPublisher, fn func(tx *sql.Tx) error) error {
	var requestID string
	if err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		resolvedRequestID, err := requestIDForAttempt(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		requestID = resolvedRequestID
		if requestID == "" {
			return nil
		}
		return upsertRequestSummary(ctx, tx, requestID)
	}); err != nil {
		return err
	}
	if requestID != "" {
		if publish != nil {
			publish(ctx, requestID)
		}
	}
	return nil
}

type requestSummaryEventSnapshot struct {
	request *tracing.RequestRecord
	summary *tracing.RequestSummaryRecord
}

func (s *Store) loadRequestSummaryEventSnapshot(ctx context.Context, requestID string) (*requestSummaryEventSnapshot, error) {
	requestRecord, err := s.GetRequest(ctx, requestID)
	if err != nil || requestRecord == nil {
		return nil, err
	}
	summary, err := s.getRequestSummary(ctx, requestID)
	if err != nil || summary == nil {
		return nil, err
	}
	return &requestSummaryEventSnapshot{
		request: requestRecord,
		summary: summary,
	}, nil
}

func (s *Store) newRequestSummaryEvent(snapshot *requestSummaryEventSnapshot, eventType string) *tracing.RequestSummaryEvent {
	if snapshot == nil || snapshot.request == nil || snapshot.summary == nil {
		return nil
	}
	event := &tracing.RequestSummaryEvent{
		Type:      eventType,
		RequestID: snapshot.summary.RequestID,
		Summary:   snapshot.summary,
		LatestSeq: s.latestSeq.Load(),
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	switch eventType {
	case tracing.RequestSummaryEventStarted:
		event.StartedAtNS = snapshot.request.StartedAtNS
	case tracing.RequestSummaryEventEnded:
		event.RequestStatus = snapshot.summary.RequestStatus
		event.EndedAtNS = snapshot.request.FinishedAtNS
	}
	return event
}

func (s *Store) publishStartedRequestSummary(ctx context.Context, requestID string) {
	s.publishRequestSummaryEvent(ctx, requestID, tracing.RequestSummaryEventStarted)
}

func (s *Store) publishUpdatedRequestSummary(ctx context.Context, requestID string) {
	s.publishRequestSummaryEvent(ctx, requestID, tracing.RequestSummaryEventUpdated)
}

func (s *Store) publishEndedRequestSummaryIfReady(ctx context.Context, requestID string) {
	snapshot, err := s.loadRequestSummaryEventSnapshot(ctx, requestID)
	if err != nil || snapshot == nil {
		return
	}
	if snapshot.request.FinishedAtNS == 0 || strings.TrimSpace(snapshot.summary.RequestStatus) == tracing.RequestStatusRunning {
		return
	}
	if strings.TrimSpace(snapshot.summary.UsageCompleteness) == "" {
		return
	}
	event := s.newRequestSummaryEvent(snapshot, tracing.RequestSummaryEventEnded)
	if event == nil {
		return
	}
	s.broadcastRequestSummaryEvent(*event)
}

func (s *Store) publishSummaryAfterUsageFinalize(ctx context.Context, requestID string) {
	snapshot, err := s.loadRequestSummaryEventSnapshot(ctx, requestID)
	if err != nil || snapshot == nil {
		return
	}
	eventType := tracing.RequestSummaryEventUpdated
	if snapshot.request.FinishedAtNS != 0 && strings.TrimSpace(snapshot.summary.RequestStatus) != tracing.RequestStatusRunning {
		eventType = tracing.RequestSummaryEventEnded
	}
	event := s.newRequestSummaryEvent(snapshot, eventType)
	if event == nil {
		return
	}
	s.broadcastRequestSummaryEvent(*event)
}

func (s *Store) publishRequestSummaryEvent(ctx context.Context, requestID, eventType string) {
	snapshot, err := s.loadRequestSummaryEventSnapshot(ctx, requestID)
	if err != nil || snapshot == nil {
		return
	}
	event := s.newRequestSummaryEvent(snapshot, eventType)
	if event == nil {
		return
	}
	s.broadcastRequestSummaryEvent(*event)
}

func (s *Store) rebuildRequestSummaries(ctx context.Context) error {
	rows, err := s.readDB.QueryContext(ctx, `SELECT request_id FROM trace_request ORDER BY started_at_ns ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var requestIDs []string
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			return err
		}
		requestIDs = append(requestIDs, requestID)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, requestID := range requestIDs {
			if err := upsertRequestSummary(ctx, tx, requestID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) getRequestSummary(ctx context.Context, requestID string) (*tracing.RequestSummaryRecord, error) {
	row := s.readDB.QueryRowContext(ctx, `
		SELECT
			request_id, started_at_ns, COALESCE(provider, ''), COALESCE(requested_model, ''),
			COALESCE(route_model, ''), COALESCE(upstream_model, ''), COALESCE(auth_label, ''),
			COALESCE(auth_account, ''), COALESCE(auth_path, ''), COALESCE(auth_index, ''),
			http_method, http_path, COALESCE(http_query, ''), status_code, duration_ms,
			input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens,
			request_status, COALESCE(usage_completeness, ''), COALESCE(is_stream, 0), updated_at_ns
		FROM trace_request_summary
		WHERE request_id = ?
	`, requestID)
	record, err := scanRequestSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func upsertRequestSummary(ctx context.Context, tx *sql.Tx, requestID string) error {
	requestRecord, err := getRequestByQueryer(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if requestRecord == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM trace_request_summary WHERE request_id = ?`, requestID)
		return err
	}

	attemptRecord, err := getLatestAttemptByRequest(ctx, tx, requestID)
	if err != nil {
		return err
	}
	usageRecord, err := getUsageByQueryer(ctx, tx, requestID)
	if err != nil {
		return err
	}

	summary := buildRequestSummary(requestRecord, attemptRecord, usageRecord)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO trace_request_summary (
			request_id, started_at_ns, provider, requested_model, route_model, upstream_model,
			auth_label, auth_account, auth_path, auth_index, http_method, http_path, http_query,
			status_code, duration_ms, input_tokens, output_tokens, total_tokens, reasoning_tokens,
			cached_tokens, request_status, usage_completeness, is_stream, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id) DO UPDATE SET
			started_at_ns = excluded.started_at_ns,
			provider = excluded.provider,
			requested_model = excluded.requested_model,
			route_model = excluded.route_model,
			upstream_model = excluded.upstream_model,
			auth_label = excluded.auth_label,
			auth_account = excluded.auth_account,
			auth_path = excluded.auth_path,
			auth_index = excluded.auth_index,
			http_method = excluded.http_method,
			http_path = excluded.http_path,
			http_query = excluded.http_query,
			status_code = excluded.status_code,
			duration_ms = excluded.duration_ms,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			total_tokens = excluded.total_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			cached_tokens = excluded.cached_tokens,
			request_status = excluded.request_status,
			usage_completeness = excluded.usage_completeness,
			is_stream = excluded.is_stream,
			updated_at_ns = excluded.updated_at_ns
	`, summary.RequestID, summary.StartedAtNS, emptyToNull(summary.Provider), emptyToNull(summary.RequestedModel),
		emptyToNull(summary.RouteModel), emptyToNull(summary.UpstreamModel), emptyToNull(summary.AuthLabel),
		emptyToNull(summary.AuthAccount), emptyToNull(summary.AuthPath), emptyToNull(summary.AuthIndex),
		summary.HTTPMethod, summary.HTTPPath, emptyToNull(summary.HTTPQuery), summary.StatusCode, summary.DurationMS,
		summary.InputTokens, summary.OutputTokens, summary.TotalTokens, summary.ReasoningTokens, summary.CachedTokens,
		summary.RequestStatus, emptyToNull(summary.UsageCompleteness), boolInt(summary.IsStream), summary.UpdatedAtNS)
	return err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getRequestByQueryer(ctx context.Context, q queryer, requestID string) (*tracing.RequestRecord, error) {
	var record tracing.RequestRecord
	var isStream int
	err := q.QueryRowContext(ctx, `
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
		&isStream, &record.HandlerType, &record.RequestedModel, &record.ClientCorrelationID,
		&record.DownstreamStatusCode, &record.DownstreamFirstByteNS, &record.RequestHeadersJSON,
		&record.RequestBodyBlobID, &record.ResponseHeadersJSON, &record.ResponseBodyBlobID,
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

func getUsageByQueryer(ctx context.Context, q queryer, requestID string) (*tracing.UsageFinalRecord, error) {
	var record tracing.UsageFinalRecord
	var derivedTotal int
	var inputTokens, outputTokens, reasoningTokens, cachedTokens, totalTokens sql.NullInt64
	err := q.QueryRowContext(ctx, `
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

func getLatestAttemptByRequest(ctx context.Context, q queryer, requestID string) (*tracing.AttemptRecord, error) {
	var record tracing.AttemptRecord
	err := q.QueryRowContext(ctx, `
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
		ORDER BY attempt_no DESC
		LIMIT 1
	`, requestID).Scan(
		&record.AttemptID, &record.RequestID, &record.AttemptNo, &record.RetryScope, &record.Provider, &record.ExecutorID,
		&record.AuthID, &record.AuthIndex, &record.AuthSnapshotJSON, &record.RouteModel, &record.UpstreamModel,
		&record.UpstreamURL, &record.UpstreamMethod, &record.UpstreamProtocol, &record.StartedAtNS, &record.HeadersAtNS,
		&record.FirstByteAtNS, &record.FinishedAtNS, &record.StatusCode, &record.Outcome, &record.ErrorCode,
		&record.ErrorMessage, &record.RequestHeadersJSON, &record.RequestBodyBlobID, &record.ResponseHeadersJSON,
		&record.ResponseBodyBlobID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func requestIDForAttempt(ctx context.Context, q queryer, attemptID string) (string, error) {
	var requestID string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(request_id, '') FROM trace_attempt WHERE attempt_id = ?`, attemptID).Scan(&requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return requestID, err
}

func (s *Store) getBlobMetadata(ctx context.Context, blobID string) (*tracing.BlobRecord, error) {
	return getBlobMetadataByQueryer(ctx, s.readDB, blobID)
}

func getBlobMetadataByQueryer(ctx context.Context, q queryer, blobID string) (*tracing.BlobRecord, error) {
	var record tracing.BlobRecord
	var complete, truncated int
	err := q.QueryRowContext(ctx, `
		SELECT blob_id, storage_kind, size_bytes, COALESCE(content_type, ''), COALESCE(content_encoding, ''),
			COALESCE(complete, 0), COALESCE(truncated, 0), COALESCE(sha256, ''), COALESCE(file_relpath, '')
		FROM trace_blob
		WHERE blob_id = ?
	`, blobID).Scan(
		&record.BlobID, &record.StorageKind, &record.SizeBytes, &record.ContentType, &record.ContentEncoding,
		&complete, &truncated, &record.SHA256, &record.FileRelPath,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.Complete = complete != 0
	record.Truncated = truncated != 0
	record.InlineBytes = nil
	return &record, nil
}

type blobRef struct {
	BlobID      string
	FileRelPath string
}

func listRequestBlobRefs(ctx context.Context, q queryer, requestID string) ([]blobRef, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT b.blob_id, COALESCE(b.file_relpath, '')
		FROM trace_blob b
		WHERE b.blob_id IN (
			SELECT request_body_blob_id FROM trace_request WHERE request_id = ? AND request_body_blob_id IS NOT NULL
			UNION
			SELECT response_body_blob_id FROM trace_request WHERE request_id = ? AND response_body_blob_id IS NOT NULL
			UNION
			SELECT request_body_blob_id FROM trace_attempt WHERE request_id = ? AND request_body_blob_id IS NOT NULL
			UNION
			SELECT response_body_blob_id FROM trace_attempt WHERE request_id = ? AND response_body_blob_id IS NOT NULL
		)
	`, requestID, requestID, requestID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []blobRef
	for rows.Next() {
		var ref blobRef
		if err := rows.Scan(&ref.BlobID, &ref.FileRelPath); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func deleteOrphanBlobs(ctx context.Context, tx *sql.Tx, refs []blobRef) ([]string, error) {
	var filesToRemove []string
	for _, ref := range refs {
		if strings.TrimSpace(ref.BlobID) == "" {
			continue
		}
		var refCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM trace_request WHERE request_body_blob_id = ? OR response_body_blob_id = ?) +
				(SELECT COUNT(*) FROM trace_attempt WHERE request_body_blob_id = ? OR response_body_blob_id = ?)
		`, ref.BlobID, ref.BlobID, ref.BlobID, ref.BlobID).Scan(&refCount); err != nil {
			return nil, err
		}
		if refCount > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM trace_blob WHERE blob_id = ?`, ref.BlobID); err != nil {
			return nil, err
		}
		if ref.FileRelPath != "" {
			filesToRemove = append(filesToRemove, ref.FileRelPath)
		}
	}
	return filesToRemove, nil
}

func buildRequestSummary(
	requestRecord *tracing.RequestRecord,
	attemptRecord *tracing.AttemptRecord,
	usageRecord *tracing.UsageFinalRecord,
) tracing.RequestSummaryRecord {
	summary := tracing.RequestSummaryRecord{
		RequestID:      requestRecord.RequestID,
		StartedAtNS:    requestRecord.StartedAtNS,
		RequestedModel: strings.TrimSpace(requestRecord.RequestedModel),
		HTTPMethod:     requestRecord.HTTPMethod,
		HTTPPath:       requestRecord.HTTPPath,
		HTTPQuery:      strings.TrimSpace(requestRecord.HTTPQuery),
		RequestStatus:  requestRecord.Status,
		IsStream:       requestRecord.IsStream,
		UpdatedAtNS:    summaryUpdatedAtNS(requestRecord, attemptRecord, usageRecord),
	}

	statusCode := nullablePositiveInt(requestRecord.DownstreamStatusCode)
	summary.StatusCode = statusCode
	durationMS := requestDurationMS(requestRecord)
	summary.DurationMS = &durationMS

	if attemptRecord != nil {
		summary.Provider = strings.TrimSpace(attemptRecord.Provider)
		summary.RouteModel = strings.TrimSpace(attemptRecord.RouteModel)
		summary.UpstreamModel = strings.TrimSpace(attemptRecord.UpstreamModel)
		authSummary := decodeAuthSnapshot(attemptRecord.AuthSnapshotJSON)
		summary.AuthLabel = authSummary.Label
		summary.AuthAccount = authSummary.Account
		summary.AuthPath = authSummary.Path
		summary.AuthIndex = firstNonEmpty(strings.TrimSpace(attemptRecord.AuthIndex), authSummary.AuthIndex)
	}

	if usageRecord != nil {
		summary.UsageCompleteness = strings.TrimSpace(usageRecord.Completeness)
		summary.InputTokens = usageRecord.InputTokens
		summary.OutputTokens = usageRecord.OutputTokens
		summary.TotalTokens = usageRecord.TotalTokens
		summary.ReasoningTokens = usageRecord.ReasoningTokens
		summary.CachedTokens = usageRecord.CachedTokens
	}

	return summary
}

type authSnapshotSummary struct {
	Label     string `json:"label"`
	Account   string `json:"account"`
	Path      string `json:"path"`
	AuthIndex string `json:"auth_index"`
}

func decodeAuthSnapshot(raw []byte) authSnapshotSummary {
	if len(raw) == 0 {
		return authSnapshotSummary{}
	}
	var snapshot authSnapshotSummary
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return authSnapshotSummary{}
	}
	snapshot.Label = strings.TrimSpace(snapshot.Label)
	snapshot.Account = strings.TrimSpace(snapshot.Account)
	snapshot.Path = strings.TrimSpace(snapshot.Path)
	snapshot.AuthIndex = strings.TrimSpace(snapshot.AuthIndex)
	return snapshot
}

func requestDurationMS(record *tracing.RequestRecord) int64 {
	if record == nil || record.StartedAtNS <= 0 {
		return 0
	}
	finishedAtNS := record.FinishedAtNS
	if finishedAtNS <= 0 {
		return 0
	}
	if finishedAtNS < record.StartedAtNS {
		return 0
	}
	return (finishedAtNS - record.StartedAtNS) / int64(time.Millisecond)
}

func summaryUpdatedAtNS(
	requestRecord *tracing.RequestRecord,
	attemptRecord *tracing.AttemptRecord,
	usageRecord *tracing.UsageFinalRecord,
) int64 {
	values := []int64{
		requestRecord.StartedAtNS,
		requestRecord.FinishedAtNS,
		requestRecord.DownstreamFirstByteNS,
	}
	if attemptRecord != nil {
		values = append(values, attemptRecord.StartedAtNS, attemptRecord.HeadersAtNS, attemptRecord.FirstByteAtNS, attemptRecord.FinishedAtNS)
	}
	if usageRecord != nil {
		values = append(values, usageRecord.FinalizedAtNS)
	}
	var maxValue int64
	for _, value := range values {
		if value > maxValue {
			maxValue = value
		}
	}
	if maxValue == 0 {
		return time.Now().UTC().UnixNano()
	}
	return maxValue
}

func nullablePositiveInt(value int) *int {
	if value <= 0 {
		return nil
	}
	copyValue := value
	return &copyValue
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func makeSummaryWhereClause(filter tracing.RequestSummaryFilter) (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if provider := strings.TrimSpace(filter.Provider); provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, provider)
	}
	if requestedModel := strings.TrimSpace(filter.RequestedModel); requestedModel != "" {
		clauses = append(clauses, "(requested_model = ? OR route_model = ? OR upstream_model = ?)")
		args = append(args, requestedModel, requestedModel, requestedModel)
	}

	switch strings.TrimSpace(filter.Status) {
	case "", "all":
	case "success":
		clauses = append(clauses, "request_status = ?")
		args = append(args, tracing.RequestStatusSucceeded)
	case "failure":
		clauses = append(clauses, "(request_status = ? OR request_status = ?)")
		args = append(args, tracing.RequestStatusFailed, tracing.RequestStatusInterrupted)
	default:
		clauses = append(clauses, "request_status = ?")
		args = append(args, strings.TrimSpace(filter.Status))
	}

	if filter.HasUsageOnly {
		clauses = append(clauses, "usage_completeness IS NOT NULL")
	}
	if filter.StreamOnly != nil {
		clauses = append(clauses, "is_stream = ?")
		args = append(args, boolInt(*filter.StreamOnly))
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		like := "%" + search + "%"
		searchColumns := []string{
			"request_id",
			"http_path",
			"http_query",
			"requested_model",
			"provider",
			"route_model",
			"upstream_model",
			"auth_label",
			"auth_account",
			"auth_path",
			"auth_index",
		}
		var fragments []string
		for _, column := range searchColumns {
			fragments = append(fragments, column+" LIKE ?")
			args = append(args, like)
		}
		clauses = append(clauses, "("+strings.Join(fragments, " OR ")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequestSummary(scanner rowScanner) (tracing.RequestSummaryRecord, error) {
	var (
		record                                  tracing.RequestSummaryRecord
		provider, requestedModel                string
		routeModel, upstreamModel               string
		authLabel, authAccount, authPath        string
		authIndex, httpQuery, usageCompleteness string
		statusCode                              sql.NullInt64
		durationMS                              sql.NullInt64
		inputTokens, outputTokens               sql.NullInt64
		totalTokens, reasoningTokens            sql.NullInt64
		cachedTokens                            sql.NullInt64
		isStream                                int
	)
	err := scanner.Scan(
		&record.RequestID, &record.StartedAtNS, &provider, &requestedModel,
		&routeModel, &upstreamModel, &authLabel, &authAccount, &authPath, &authIndex,
		&record.HTTPMethod, &record.HTTPPath, &httpQuery, &statusCode, &durationMS,
		&inputTokens, &outputTokens, &totalTokens, &reasoningTokens, &cachedTokens,
		&record.RequestStatus, &usageCompleteness, &isStream, &record.UpdatedAtNS,
	)
	if err != nil {
		return tracing.RequestSummaryRecord{}, err
	}
	record.Provider = strings.TrimSpace(provider)
	record.RequestedModel = strings.TrimSpace(requestedModel)
	record.RouteModel = strings.TrimSpace(routeModel)
	record.UpstreamModel = strings.TrimSpace(upstreamModel)
	record.AuthLabel = strings.TrimSpace(authLabel)
	record.AuthAccount = strings.TrimSpace(authAccount)
	record.AuthPath = strings.TrimSpace(authPath)
	record.AuthIndex = strings.TrimSpace(authIndex)
	record.HTTPQuery = strings.TrimSpace(httpQuery)
	record.UsageCompleteness = strings.TrimSpace(usageCompleteness)
	record.StatusCode = nullIntPtr(statusCode)
	record.DurationMS = nullInt64Ptr(durationMS)
	record.InputTokens = nullInt64Ptr(inputTokens)
	record.OutputTokens = nullInt64Ptr(outputTokens)
	record.TotalTokens = nullInt64Ptr(totalTokens)
	record.ReasoningTokens = nullInt64Ptr(reasoningTokens)
	record.CachedTokens = nullInt64Ptr(cachedTokens)
	record.IsStream = isStream != 0
	return record, nil
}

func (s *Store) broadcastRequestSummaryEvent(event tracing.RequestSummaryEvent) {
	if s == nil || !s.emitSSE {
		return
	}
	s.summarySubMu.Lock()
	defer s.summarySubMu.Unlock()
	for ch := range s.summarySubs {
		select {
		case ch <- event:
		default:
		}
	}
}

func openDB(path string, maxOpenConns, maxIdleConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	return db, nil
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

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	value := int(v.Int64)
	return &value
}
