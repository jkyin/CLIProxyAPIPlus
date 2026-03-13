package sqlite

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
)

func TestStorePersistsRequestAttemptAndUsage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(ctx, Config{BaseDir: dir, EmitSSE: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	requestID := tracing.MustNewID()
	startedAt := time.Now().UTC()
	if err := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:      requestID,
		StartedAt:      startedAt,
		Method:         http.MethodPost,
		Scheme:         "http",
		Host:           "localhost:8317",
		Path:           "/v1/chat/completions",
		Query:          "foo=bar",
		RequestHeaders: http.Header{"Content-Type": {"application/json"}},
		IsStream:       true,
		HandlerType:    "openai",
		RequestedModel: "gpt-5",
	}); err != nil {
		t.Fatalf("StartRequest() error = %v", err)
	}

	if err := store.RecordRequestEvent(ctx, requestID, "", tracing.EventRequestStarted, startedAt, map[string]any{"foo": "bar"}); err != nil {
		t.Fatalf("RecordRequestEvent() error = %v", err)
	}

	attemptID := tracing.MustNewID()
	if err := store.BeginAttempt(ctx, tracing.AttemptStart{
		RequestID:        requestID,
		AttemptID:        attemptID,
		AttemptNo:        1,
		RetryScope:       "initial",
		Provider:         "codex",
		ExecutorID:       "codex",
		AuthID:           "auth-1",
		AuthIndex:        "idx-1",
		AuthSnapshotJSON: []byte(`{"auth_id":"auth-1"}`),
		RouteModel:       "gpt-5",
		UpstreamModel:    "gpt-5-codex",
		StartedAt:        startedAt,
	}); err != nil {
		t.Fatalf("BeginAttempt() error = %v", err)
	}

	if err := store.UpdateAttemptRequest(ctx, tracing.AttemptRequest{
		AttemptID:        attemptID,
		UpstreamURL:      "https://example.test/v1/responses",
		UpstreamMethod:   http.MethodPost,
		UpstreamProtocol: "http",
		RequestHeaders:   http.Header{"Authorization": {"Bearer masked"}},
	}); err != nil {
		t.Fatalf("UpdateAttemptRequest() error = %v", err)
	}

	blob := &tracing.BlobRecord{
		BlobID:      tracing.MustNewID(),
		StorageKind: "inline",
		SizeBytes:   4,
		Complete:    true,
		InlineBytes: []byte("pong"),
	}
	if err := store.SaveBlob(ctx, blob); err != nil {
		t.Fatalf("SaveBlob() error = %v", err)
	}

	if err := store.UpdateAttemptResponse(ctx, tracing.AttemptResponse{
		AttemptID:          attemptID,
		StatusCode:         200,
		HeadersAt:          startedAt.Add(10 * time.Millisecond),
		FirstByteAt:        startedAt.Add(20 * time.Millisecond),
		ResponseHeaders:    http.Header{"Content-Type": {"text/event-stream"}},
		ResponseBodyBlobID: blob.BlobID,
	}); err != nil {
		t.Fatalf("UpdateAttemptResponse() error = %v", err)
	}

	if err := store.FinishAttempt(ctx, tracing.AttemptFinish{
		AttemptID:  attemptID,
		Outcome:    tracing.AttemptOutcomeSucceeded,
		FinishedAt: startedAt.Add(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("FinishAttempt() error = %v", err)
	}

	if err := store.FinalizeRequest(ctx, tracing.RequestFinish{
		RequestID:          requestID,
		StatusCode:         200,
		Status:             tracing.RequestStatusSucceeded,
		FinishedAt:         startedAt.Add(60 * time.Millisecond),
		FirstByteAt:        startedAt.Add(30 * time.Millisecond),
		ResponseHeaders:    http.Header{"Content-Type": {"text/event-stream"}},
		ResponseBodyBlobID: blob.BlobID,
	}); err != nil {
		t.Fatalf("FinalizeRequest() error = %v", err)
	}

	if err := store.FinalizeUsage(ctx, tracing.UsageFinal{
		RequestID:       requestID,
		AttemptID:       attemptID,
		FinalizedAt:     startedAt.Add(60 * time.Millisecond),
		Status:          "success",
		Completeness:    tracing.UsageCompletenessComplete,
		InputTokens:     10,
		OutputTokens:    20,
		ReasoningTokens: 5,
		CachedTokens:    3,
		TotalTokens:     35,
	}); err != nil {
		t.Fatalf("FinalizeUsage() error = %v", err)
	}

	requestRecord, err := store.GetRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequest() error = %v", err)
	}
	if requestRecord == nil || requestRecord.Status != tracing.RequestStatusSucceeded {
		t.Fatalf("GetRequest() = %#v, want succeeded request", requestRecord)
	}

	attempts, err := store.ListAttempts(ctx, requestID)
	if err != nil {
		t.Fatalf("ListAttempts() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != tracing.AttemptOutcomeSucceeded {
		t.Fatalf("ListAttempts() = %#v, want one succeeded attempt", attempts)
	}

	usageRecord, err := store.GetUsage(ctx, requestID)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}
	if usageRecord == nil || usageRecord.TotalTokens == nil || *usageRecord.TotalTokens != 35 {
		t.Fatalf("GetUsage() = %#v, want total_tokens=35", usageRecord)
	}

	events, err := store.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].RequestID != requestID {
		t.Fatalf("ListEvents() = %#v, want one event for request", events)
	}

	blobRecord, err := store.GetBlob(ctx, blob.BlobID)
	if err != nil {
		t.Fatalf("GetBlob() error = %v", err)
	}
	if blobRecord == nil || string(blobRecord.InlineBytes) != "pong" {
		t.Fatalf("GetBlob() = %#v, want inline body", blobRecord)
	}

	if filepath.Base(store.DBPath()) != "tracing.db" {
		t.Fatalf("DBPath() = %q, want tracing.db suffix", store.DBPath())
	}
}

func TestStoreUsageMissingTokensAreNull(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(ctx, Config{BaseDir: dir})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	requestID := tracing.MustNewID()
	if err := store.FinalizeUsage(ctx, tracing.UsageFinal{
		RequestID:    requestID,
		FinalizedAt:  time.Now().UTC(),
		Status:       "success",
		Completeness: tracing.UsageCompletenessMissing,
	}); err != nil {
		t.Fatalf("FinalizeUsage() error = %v", err)
	}

	usageRecord, err := store.GetUsage(ctx, requestID)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}
	if usageRecord == nil {
		t.Fatal("GetUsage() = nil, want record")
	}
	if usageRecord.InputTokens != nil || usageRecord.OutputTokens != nil || usageRecord.ReasoningTokens != nil || usageRecord.CachedTokens != nil || usageRecord.TotalTokens != nil {
		t.Fatalf("GetUsage() = %#v, want nil token fields for missing usage", usageRecord)
	}

	var inputTokens, totalTokens sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `
		SELECT input_tokens, total_tokens
		FROM trace_usage_final
		WHERE request_id = ?
	`, requestID).Scan(&inputTokens, &totalTokens); err != nil {
		t.Fatalf("raw usage query error = %v", err)
	}
	if inputTokens.Valid || totalTokens.Valid {
		t.Fatalf("raw usage row = input=%v total=%v, want NULL token columns", inputTokens, totalTokens)
	}
}

func TestStoreMigratesMissingUsageZerosToNull(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "tracing.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, execErr := db.ExecContext(ctx, query, args...); execErr != nil {
			t.Fatalf("ExecContext(%q) error = %v", query, execErr)
		}
	}

	mustExec(`CREATE TABLE trace_usage_final (
		request_id TEXT PRIMARY KEY,
		attempt_id TEXT,
		finalized_at_ns INTEGER NOT NULL,
		status TEXT NOT NULL,
		completeness TEXT NOT NULL,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		derived_total INTEGER NOT NULL DEFAULT 0,
		provider_usage_json BLOB,
		provider_native_json BLOB
	)`)
	mustExec(`CREATE TABLE trace_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	mustExec(`INSERT INTO trace_meta(key, value) VALUES ('schema_version', '1')`)
	requestID := tracing.MustNewID()
	mustExec(`
		INSERT INTO trace_usage_final (
			request_id, finalized_at_ns, status, completeness, input_tokens,
			output_tokens, reasoning_tokens, cached_tokens, total_tokens, derived_total
		) VALUES (?, ?, 'success', ?, 0, 0, 0, 0, 0, 0)
	`, requestID, time.Now().UTC().UnixNano(), tracing.UsageCompletenessMissing)
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	store, err := New(ctx, Config{BaseDir: dir})
	if err != nil {
		t.Fatalf("New() after legacy schema error = %v", err)
	}
	defer func() { _ = store.Close() }()

	usageRecord, err := store.GetUsage(ctx, requestID)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}
	if usageRecord == nil {
		t.Fatal("GetUsage() = nil, want migrated record")
	}
	if usageRecord.InputTokens != nil || usageRecord.OutputTokens != nil || usageRecord.ReasoningTokens != nil || usageRecord.CachedTokens != nil || usageRecord.TotalTokens != nil {
		t.Fatalf("GetUsage() = %#v, want migrated nil token fields", usageRecord)
	}

	var rawInputTokens sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `
		SELECT input_tokens
		FROM trace_usage_final
		WHERE request_id = ?
	`, requestID).Scan(&rawInputTokens); err != nil {
		t.Fatalf("migrated raw usage query error = %v", err)
	}
	if rawInputTokens.Valid {
		t.Fatalf("migrated input_tokens = %v, want NULL", rawInputTokens)
	}

	var schemaVersion string
	if err := store.db.QueryRowContext(ctx, `
		SELECT value
		FROM trace_meta
		WHERE key = 'schema_version'
	`).Scan(&schemaVersion); err != nil {
		t.Fatalf("schema version query error = %v", err)
	}
	if schemaVersion != "2" {
		t.Fatalf("schema_version = %q, want 2", schemaVersion)
	}
}
