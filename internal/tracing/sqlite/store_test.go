package sqlite

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
)

func readRequestSummaryEvent(t *testing.T, ch <-chan tracing.RequestSummaryEvent) tracing.RequestSummaryEvent {
	t.Helper()

	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("summary event channel closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for summary event")
	}

	return tracing.RequestSummaryEvent{}
}

func requireNoRequestSummaryEvent(t *testing.T, ch <-chan tracing.RequestSummaryEvent) {
	t.Helper()

	select {
	case event, ok := <-ch:
		if !ok {
			return
		}
		t.Fatalf("unexpected summary event: %#v", event)
	case <-time.After(150 * time.Millisecond):
	}
}

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

func TestStoreRequestSummaryEventLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(ctx, Config{BaseDir: dir, EmitSSE: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	ch, cancel := store.SubscribeRequestSummaries()
	defer cancel()

	requestID := tracing.MustNewID()
	startedAt := time.Now().UTC().Truncate(time.Millisecond)
	finishedAt := startedAt.Add(800 * time.Millisecond)

	if err := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:      requestID,
		StartedAt:      startedAt,
		Method:         http.MethodPost,
		Scheme:         "http",
		Host:           "localhost:8317",
		Path:           "/v1/chat/completions",
		IsStream:       true,
		HandlerType:    "openai",
		RequestedModel: "gpt-5",
	}); err != nil {
		t.Fatalf("StartRequest() error = %v", err)
	}

	startedEvent := readRequestSummaryEvent(t, ch)
	if startedEvent.Type != tracing.RequestSummaryEventStarted {
		t.Fatalf("started event type = %q, want %q", startedEvent.Type, tracing.RequestSummaryEventStarted)
	}
	if startedEvent.RequestID != requestID {
		t.Fatalf("started event request_id = %q, want %q", startedEvent.RequestID, requestID)
	}
	if startedEvent.StartedAtNS != startedAt.UnixNano() {
		t.Fatalf("started event started_at_ns = %d, want %d", startedEvent.StartedAtNS, startedAt.UnixNano())
	}
	if startedEvent.Summary == nil || startedEvent.Summary.RequestStatus != tracing.RequestStatusRunning {
		t.Fatalf("started event summary = %#v, want running summary", startedEvent.Summary)
	}

	if err := store.UpdateRequestRoute(ctx, requestID, tracing.RequestRouteInfo{
		IsStream:            true,
		HandlerType:         "openai",
		RequestedModel:      "gpt-5.4",
		ClientCorrelationID: "corr-1",
	}); err != nil {
		t.Fatalf("UpdateRequestRoute() error = %v", err)
	}

	updatedEvent := readRequestSummaryEvent(t, ch)
	if updatedEvent.Type != tracing.RequestSummaryEventUpdated {
		t.Fatalf("updated event type = %q, want %q", updatedEvent.Type, tracing.RequestSummaryEventUpdated)
	}
	if updatedEvent.Summary == nil || updatedEvent.Summary.RequestedModel != "gpt-5.4" {
		t.Fatalf("updated event summary = %#v, want requested_model updated", updatedEvent.Summary)
	}

	if err := store.FinalizeRequest(ctx, tracing.RequestFinish{
		RequestID:   requestID,
		StatusCode:  200,
		Status:      tracing.RequestStatusSucceeded,
		FinishedAt:  finishedAt,
		FirstByteAt: startedAt.Add(100 * time.Millisecond),
	}); err != nil {
		t.Fatalf("FinalizeRequest() error = %v", err)
	}

	requireNoRequestSummaryEvent(t, ch)

	if err := store.FinalizeUsage(ctx, tracing.UsageFinal{
		RequestID:       requestID,
		FinalizedAt:     finishedAt,
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

	endedEvent := readRequestSummaryEvent(t, ch)
	if endedEvent.Type != tracing.RequestSummaryEventEnded {
		t.Fatalf("ended event type = %q, want %q", endedEvent.Type, tracing.RequestSummaryEventEnded)
	}
	if endedEvent.RequestStatus != tracing.RequestStatusSucceeded {
		t.Fatalf("ended event request_status = %q, want %q", endedEvent.RequestStatus, tracing.RequestStatusSucceeded)
	}
	if endedEvent.EndedAtNS != finishedAt.UnixNano() {
		t.Fatalf("ended event ended_at_ns = %d, want %d", endedEvent.EndedAtNS, finishedAt.UnixNano())
	}
	if endedEvent.Summary == nil {
		t.Fatal("ended event summary = nil, want final summary")
	}
	if endedEvent.Summary.TotalTokens == nil || *endedEvent.Summary.TotalTokens != 35 {
		t.Fatalf("ended event total_tokens = %#v, want 35", endedEvent.Summary.TotalTokens)
	}
	if endedEvent.Summary.UsageCompleteness != tracing.UsageCompletenessComplete {
		t.Fatalf("ended event usage_completeness = %q, want %q", endedEvent.Summary.UsageCompleteness, tracing.UsageCompletenessComplete)
	}

	deleted, err := store.DeleteRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("DeleteRequest() error = %v", err)
	}
	if !deleted {
		t.Fatal("DeleteRequest() = false, want true")
	}

	deletedEvent := readRequestSummaryEvent(t, ch)
	if deletedEvent.Type != tracing.RequestSummaryEventDeleted {
		t.Fatalf("deleted event type = %q, want %q", deletedEvent.Type, tracing.RequestSummaryEventDeleted)
	}
	if deletedEvent.RequestID != requestID {
		t.Fatalf("deleted event request_id = %q, want %q", deletedEvent.RequestID, requestID)
	}
	if deletedEvent.Summary != nil {
		t.Fatalf("deleted event summary = %#v, want nil", deletedEvent.Summary)
	}
}

func TestStoreRecoverRunningRefreshesRequestSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := New(ctx, Config{BaseDir: dir})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	requestID := tracing.MustNewID()
	attemptID := tracing.MustNewID()
	startedAt := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Millisecond)

	if err := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:      requestID,
		StartedAt:      startedAt,
		Method:         http.MethodPost,
		Scheme:         "http",
		Host:           "localhost:8317",
		Path:           "/v1/chat/completions",
		IsStream:       true,
		HandlerType:    "openai",
		RequestedModel: "gpt-5",
	}); err != nil {
		t.Fatalf("StartRequest() error = %v", err)
	}

	if err := store.BeginAttempt(ctx, tracing.AttemptStart{
		RequestID:        requestID,
		AttemptID:        attemptID,
		AttemptNo:        1,
		RetryScope:       "initial",
		Provider:         "openai",
		ExecutorID:       "openai",
		AuthIndex:        "idx-3",
		AuthSnapshotJSON: tracing.AuthSnapshotJSON("openai", "auth-1", "idx-3", "Work", "oauth", "jkyin@example.com", "/tmp/auth.json", "file", "ready", false, time.Time{}),
		RouteModel:       "gpt-5",
		UpstreamModel:    "gpt-5.4",
		StartedAt:        startedAt.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("BeginAttempt() error = %v", err)
	}

	if err := store.FinalizeUsage(ctx, tracing.UsageFinal{
		RequestID:       requestID,
		AttemptID:       attemptID,
		FinalizedAt:     startedAt.Add(20 * time.Millisecond),
		Status:          "success",
		Completeness:    tracing.UsageCompletenessComplete,
		InputTokens:     11,
		OutputTokens:    29,
		ReasoningTokens: 3,
		CachedTokens:    1,
		TotalTokens:     40,
	}); err != nil {
		t.Fatalf("FinalizeUsage() error = %v", err)
	}

	preRecoverySummary, err := store.GetRequestSummary(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequestSummary() before restart error = %v", err)
	}
	if preRecoverySummary == nil {
		t.Fatal("GetRequestSummary() before restart = nil, want summary")
	}
	if preRecoverySummary.RequestStatus != tracing.RequestStatusRunning {
		t.Fatalf("pre-recovery summary request_status = %q, want %q", preRecoverySummary.RequestStatus, tracing.RequestStatusRunning)
	}
	if preRecoverySummary.DurationMS == nil || *preRecoverySummary.DurationMS != 0 {
		t.Fatalf("pre-recovery summary duration_ms = %#v, want 0", preRecoverySummary.DurationMS)
	}
	preRecoveryUpdatedAtNS := preRecoverySummary.UpdatedAtNS

	if err := store.Close(); err != nil {
		t.Fatalf("Close() before restart error = %v", err)
	}

	store, err = New(ctx, Config{BaseDir: dir})
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	defer func() { _ = store.Close() }()

	requestRecord, err := store.GetRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequest() after restart error = %v", err)
	}
	if requestRecord == nil {
		t.Fatal("GetRequest() after restart = nil, want request")
	}
	if requestRecord.Status != tracing.RequestStatusInterrupted {
		t.Fatalf("request status after restart = %q, want %q", requestRecord.Status, tracing.RequestStatusInterrupted)
	}
	if requestRecord.FinishedAtNS <= requestRecord.StartedAtNS {
		t.Fatalf("request finished_at_ns after restart = %d, want > started_at_ns %d", requestRecord.FinishedAtNS, requestRecord.StartedAtNS)
	}

	attempts, err := store.ListAttempts(ctx, requestID)
	if err != nil {
		t.Fatalf("ListAttempts() after restart error = %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("ListAttempts() len after restart = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != tracing.AttemptOutcomeInterrupted {
		t.Fatalf("attempt outcome after restart = %q, want %q", attempts[0].Outcome, tracing.AttemptOutcomeInterrupted)
	}
	if attempts[0].FinishedAtNS <= attempts[0].StartedAtNS {
		t.Fatalf("attempt finished_at_ns after restart = %d, want > started_at_ns %d", attempts[0].FinishedAtNS, attempts[0].StartedAtNS)
	}

	summary, err := store.GetRequestSummary(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequestSummary() after restart error = %v", err)
	}
	if summary == nil {
		t.Fatal("GetRequestSummary() after restart = nil, want summary")
	}
	if summary.RequestStatus != tracing.RequestStatusInterrupted {
		t.Fatalf("summary request_status after restart = %q, want %q", summary.RequestStatus, tracing.RequestStatusInterrupted)
	}
	if summary.DurationMS == nil || *summary.DurationMS <= 0 {
		t.Fatalf("summary duration_ms after restart = %#v, want > 0", summary.DurationMS)
	}
	if summary.UpdatedAtNS <= preRecoveryUpdatedAtNS {
		t.Fatalf("summary updated_at_ns after restart = %d, want > %d", summary.UpdatedAtNS, preRecoveryUpdatedAtNS)
	}
	if summary.Provider != "openai" || summary.AuthLabel != "Work" || summary.UpstreamModel != "gpt-5.4" {
		t.Fatalf("summary after restart = %#v, want preserved provider/auth/attempt fields", summary)
	}
	if summary.TotalTokens == nil || *summary.TotalTokens != 40 {
		t.Fatalf("summary total_tokens after restart = %#v, want 40", summary.TotalTokens)
	}
	if summary.UsageCompleteness != tracing.UsageCompletenessComplete {
		t.Fatalf("summary usage_completeness after restart = %q, want %q", summary.UsageCompleteness, tracing.UsageCompletenessComplete)
	}

	page, err := store.ListRequestSummaries(ctx, tracing.RequestSummaryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListRequestSummaries() after restart error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("ListRequestSummaries() len after restart = %d, want 1", len(page.Items))
	}
	if page.Items[0].RequestID != requestID {
		t.Fatalf("ListRequestSummaries() request_id after restart = %q, want %q", page.Items[0].RequestID, requestID)
	}
	if page.Items[0].RequestStatus != tracing.RequestStatusInterrupted {
		t.Fatalf("ListRequestSummaries() request_status after restart = %q, want %q", page.Items[0].RequestStatus, tracing.RequestStatusInterrupted)
	}
}

func TestStoreRequestSummarySuccessfulRetryUsageIsComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(ctx, Config{BaseDir: dir, EmitSSE: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	ch, cancel := store.SubscribeRequestSummaries()
	defer cancel()

	requestID := tracing.MustNewID()
	startedAt := time.Now().UTC()
	if err := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:      requestID,
		StartedAt:      startedAt,
		Method:         http.MethodPost,
		Scheme:         "http",
		Host:           "localhost:8317",
		Path:           "/v1/chat/completions",
		RequestHeaders: http.Header{"Content-Type": {"application/json"}},
		IsStream:       true,
		HandlerType:    "openai",
		RequestedModel: "gpt-5",
	}); err != nil {
		t.Fatalf("StartRequest() error = %v", err)
	}

	reqRecorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(reqRecorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", nil)

	state := tracing.NewRequestState(store, requestID, "", startedAt)
	tracing.AttachRequestState(ginCtx, state)
	requestCtx := ginCtx.Request.Context()

	attempt1Ctx, _, err := tracing.StartAttempt(requestCtx, tracing.AttemptStart{
		RetryScope: "initial",
		Provider:   "test-provider",
		ExecutorID: "test-executor",
		StartedAt:  startedAt.Add(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("StartAttempt(first) error = %v", err)
	}
	tracing.ObserveUsage(attempt1Ctx, tracing.UsageObservation{
		InputTokens:      10,
		OutputTokens:     5,
		IsTerminal:       true,
		CompletenessHint: tracing.UsageCompletenessComplete,
	})
	tracing.FinishAttempt(attempt1Ctx, tracing.AttemptFinish{
		Outcome:    tracing.AttemptOutcomeFailed,
		FinishedAt: startedAt.Add(20 * time.Millisecond),
	})

	attempt2Ctx, _, err := tracing.StartAttempt(requestCtx, tracing.AttemptStart{
		RetryScope: "executor_internal_retry",
		Provider:   "test-provider",
		ExecutorID: "test-executor",
		StartedAt:  startedAt.Add(30 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("StartAttempt(second) error = %v", err)
	}
	tracing.ObserveUsage(attempt2Ctx, tracing.UsageObservation{
		InputTokens:      12,
		OutputTokens:     6,
		IsTerminal:       true,
		CompletenessHint: tracing.UsageCompletenessComplete,
	})
	tracing.FinishAttempt(attempt2Ctx, tracing.AttemptFinish{
		Outcome:    tracing.AttemptOutcomeSucceeded,
		FinishedAt: startedAt.Add(40 * time.Millisecond),
	})

	tracing.FinalizeRequest(requestCtx, tracing.RequestFinish{
		RequestID:  requestID,
		StatusCode: 200,
		Status:     tracing.RequestStatusSucceeded,
		FinishedAt: startedAt.Add(50 * time.Millisecond),
	})

	var endedEvent tracing.RequestSummaryEvent
	foundEnded := false
	for i := 0; i < 8; i++ {
		event := readRequestSummaryEvent(t, ch)
		if event.Type == tracing.RequestSummaryEventEnded {
			endedEvent = event
			foundEnded = true
			break
		}
	}
	if !foundEnded {
		t.Fatal("did not receive ended request summary event")
	}
	if endedEvent.RequestStatus != tracing.RequestStatusSucceeded {
		t.Fatalf("ended event request_status = %q, want %q", endedEvent.RequestStatus, tracing.RequestStatusSucceeded)
	}
	if endedEvent.Summary == nil {
		t.Fatal("ended event summary = nil, want summary")
	}
	if endedEvent.Summary.UsageCompleteness != tracing.UsageCompletenessComplete {
		t.Fatalf("ended event usage_completeness = %q, want %q", endedEvent.Summary.UsageCompleteness, tracing.UsageCompletenessComplete)
	}

	usageRecord, err := store.GetUsage(ctx, requestID)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}
	if usageRecord == nil {
		t.Fatal("GetUsage() = nil, want record")
	}
	if usageRecord.Completeness != tracing.UsageCompletenessComplete {
		t.Fatalf("GetUsage().Completeness = %q, want %q", usageRecord.Completeness, tracing.UsageCompletenessComplete)
	}

	attempts, err := store.ListAttempts(ctx, requestID)
	if err != nil {
		t.Fatalf("ListAttempts() error = %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("ListAttempts() len = %d, want 2", len(attempts))
	}
	if attempts[0].Outcome != tracing.AttemptOutcomeFailed || attempts[1].Outcome != tracing.AttemptOutcomeSucceeded {
		t.Fatalf("ListAttempts() outcomes = [%q, %q], want [failed, succeeded]", attempts[0].Outcome, attempts[1].Outcome)
	}
	if usageRecord.AttemptID != attempts[1].AttemptID {
		t.Fatalf("GetUsage().AttemptID = %q, want %q", usageRecord.AttemptID, attempts[1].AttemptID)
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

func TestStoreRequestSummariesDetailAndDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(ctx, Config{BaseDir: dir, EmitSSE: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	requestID := tracing.MustNewID()
	attemptID := tracing.MustNewID()
	startedAt := time.Now().UTC()
	requestBlob := &tracing.BlobRecord{
		BlobID:      tracing.MustNewID(),
		StorageKind: "inline",
		SizeBytes:   7,
		Complete:    true,
		InlineBytes: []byte(`{"a":1}`),
	}
	responseBlob := &tracing.BlobRecord{
		BlobID:      tracing.MustNewID(),
		StorageKind: "inline",
		SizeBytes:   4,
		Complete:    true,
		InlineBytes: []byte("pong"),
	}
	if err := store.SaveBlob(ctx, requestBlob); err != nil {
		t.Fatalf("SaveBlob(request) error = %v", err)
	}
	if err := store.SaveBlob(ctx, responseBlob); err != nil {
		t.Fatalf("SaveBlob(response) error = %v", err)
	}

	if err := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:         requestID,
		StartedAt:         startedAt,
		Method:            http.MethodPost,
		Scheme:            "http",
		Host:              "localhost:8317",
		Path:              "/v1/responses",
		Query:             "debug=1",
		RequestHeaders:    http.Header{"Content-Type": {"application/json"}},
		RequestBodyBlobID: requestBlob.BlobID,
		IsStream:          true,
		HandlerType:       "openai",
		RequestedModel:    "gpt-5",
	}); err != nil {
		t.Fatalf("StartRequest() error = %v", err)
	}

	if err := store.BeginAttempt(ctx, tracing.AttemptStart{
		RequestID:        requestID,
		AttemptID:        attemptID,
		AttemptNo:        1,
		RetryScope:       "initial",
		Provider:         "openai",
		AuthIndex:        "idx-7",
		AuthSnapshotJSON: tracing.AuthSnapshotJSON("openai", "auth-1", "idx-7", "Work", "oauth", "jkyin@example.com", "/tmp/auth.json", "file", "ready", false, time.Time{}),
		RouteModel:       "gpt-5",
		UpstreamModel:    "gpt-5.4",
		StartedAt:        startedAt,
	}); err != nil {
		t.Fatalf("BeginAttempt() error = %v", err)
	}

	if err := store.UpdateAttemptRequest(ctx, tracing.AttemptRequest{
		AttemptID:         attemptID,
		UpstreamURL:       "https://api.openai.com/v1/responses",
		UpstreamMethod:    http.MethodPost,
		UpstreamProtocol:  "https",
		RequestHeaders:    http.Header{"Authorization": {"Bearer masked"}},
		RequestBodyBlobID: requestBlob.BlobID,
	}); err != nil {
		t.Fatalf("UpdateAttemptRequest() error = %v", err)
	}

	if err := store.UpdateAttemptResponse(ctx, tracing.AttemptResponse{
		AttemptID:          attemptID,
		StatusCode:         200,
		HeadersAt:          startedAt.Add(10 * time.Millisecond),
		FirstByteAt:        startedAt.Add(20 * time.Millisecond),
		ResponseHeaders:    http.Header{"Content-Type": {"application/json"}},
		ResponseBodyBlobID: responseBlob.BlobID,
	}); err != nil {
		t.Fatalf("UpdateAttemptResponse() error = %v", err)
	}

	if err := store.FinishAttempt(ctx, tracing.AttemptFinish{
		AttemptID:  attemptID,
		Outcome:    tracing.AttemptOutcomeSucceeded,
		StatusCode: 200,
		FinishedAt: startedAt.Add(40 * time.Millisecond),
	}); err != nil {
		t.Fatalf("FinishAttempt() error = %v", err)
	}

	if err := store.FinalizeUsage(ctx, tracing.UsageFinal{
		RequestID:       requestID,
		AttemptID:       attemptID,
		FinalizedAt:     startedAt.Add(50 * time.Millisecond),
		Status:          "success",
		Completeness:    tracing.UsageCompletenessComplete,
		InputTokens:     11,
		OutputTokens:    29,
		ReasoningTokens: 3,
		CachedTokens:    1,
		TotalTokens:     40,
	}); err != nil {
		t.Fatalf("FinalizeUsage() error = %v", err)
	}

	if err := store.FinalizeRequest(ctx, tracing.RequestFinish{
		RequestID:          requestID,
		StatusCode:         200,
		Status:             tracing.RequestStatusSucceeded,
		FinishedAt:         startedAt.Add(60 * time.Millisecond),
		FirstByteAt:        startedAt.Add(25 * time.Millisecond),
		ResponseHeaders:    http.Header{"Content-Type": {"application/json"}},
		ResponseBodyBlobID: responseBlob.BlobID,
	}); err != nil {
		t.Fatalf("FinalizeRequest() error = %v", err)
	}

	page, err := store.ListRequestSummaries(ctx, tracing.RequestSummaryFilter{
		Limit:          10,
		Provider:       "openai",
		RequestedModel: "gpt-5",
		HasUsageOnly:   true,
	})
	if err != nil {
		t.Fatalf("ListRequestSummaries() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("ListRequestSummaries() len = %d, want 1", len(page.Items))
	}
	summary := page.Items[0]
	if summary.RequestID != requestID || summary.Provider != "openai" || summary.AuthLabel != "Work" || summary.AuthAccount != "jkyin@example.com" || summary.AuthPath != "/tmp/auth.json" {
		t.Fatalf("summary = %#v, want populated provider/auth fields", summary)
	}
	if summary.StatusCode == nil || *summary.StatusCode != 200 {
		t.Fatalf("summary.StatusCode = %#v, want 200", summary.StatusCode)
	}
	if summary.TotalTokens == nil || *summary.TotalTokens != 40 {
		t.Fatalf("summary.TotalTokens = %#v, want 40", summary.TotalTokens)
	}
	summaryByID, err := store.GetRequestSummary(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequestSummary() error = %v", err)
	}
	if summaryByID == nil {
		t.Fatal("GetRequestSummary() = nil, want summary")
	}
	if !reflect.DeepEqual(*summaryByID, summary) {
		t.Fatalf("GetRequestSummary() = %#v, want %#v", *summaryByID, summary)
	}

	detail, err := store.GetRequestDetail(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequestDetail() error = %v", err)
	}
	if detail == nil || detail.Request == nil || len(detail.Attempts) != 1 || detail.UsageFinal == nil {
		t.Fatalf("GetRequestDetail() = %#v, want populated detail", detail)
	}
	if detail.RequestBlob == nil || detail.RequestBlob.BlobID != requestBlob.BlobID || len(detail.RequestBlob.InlineBytes) != 0 {
		t.Fatalf("detail.RequestBlob = %#v, want metadata-only request blob", detail.RequestBlob)
	}
	if detail.AttemptResponseBlobs[attemptID].BlobID != responseBlob.BlobID {
		t.Fatalf("detail.AttemptResponseBlobs = %#v, want attempt response blob", detail.AttemptResponseBlobs)
	}

	deleted, err := store.DeleteRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("DeleteRequest() error = %v", err)
	}
	if !deleted {
		t.Fatal("DeleteRequest() = false, want true")
	}
	deletedSummaryPage, err := store.ListRequestSummaries(ctx, tracing.RequestSummaryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListRequestSummaries(after delete) error = %v", err)
	}
	if len(deletedSummaryPage.Items) != 0 {
		t.Fatalf("ListRequestSummaries(after delete) = %#v, want empty", deletedSummaryPage.Items)
	}
	deletedSummary, err := store.GetRequestSummary(ctx, requestID)
	if err != nil {
		t.Fatalf("GetRequestSummary(after delete) error = %v", err)
	}
	if deletedSummary != nil {
		t.Fatalf("GetRequestSummary(after delete) = %#v, want nil", deletedSummary)
	}
	blobRecord, err := store.GetBlob(ctx, responseBlob.BlobID)
	if err != nil {
		t.Fatalf("GetBlob(after delete) error = %v", err)
	}
	if blobRecord != nil {
		t.Fatalf("GetBlob(after delete) = %#v, want nil for orphan blob", blobRecord)
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
	if schemaVersion != "3" {
		t.Fatalf("schema_version = %q, want 3", schemaVersion)
	}
}
