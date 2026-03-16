package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
	tracingsqlite "github.com/router-for-me/CLIProxyAPI/v6/internal/tracing/sqlite"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetTracingRequestSummary_ReturnsSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	store, err := tracingsqlite.New(ctx, tracingsqlite.Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("failed to create tracing store: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("failed to close tracing store: %v", errClose)
		}
	}()

	requestID := "req-summary-1"
	startedAt := time.Unix(1741750401, 0).UTC()
	if errStart := store.StartRequest(ctx, tracing.RequestStart{
		RequestID:      requestID,
		StartedAt:      startedAt,
		Method:         http.MethodPost,
		Scheme:         "http",
		Host:           "127.0.0.1:8317",
		Path:           "/v1/chat/completions",
		IsStream:       true,
		HandlerType:    "openai",
		RequestedModel: "gpt-5",
	}); errStart != nil {
		t.Fatalf("StartRequest() error = %v", errStart)
	}
	if errFinish := store.FinalizeRequest(ctx, tracing.RequestFinish{
		RequestID:   requestID,
		StatusCode:  200,
		Status:      tracing.RequestStatusSucceeded,
		FinishedAt:  startedAt.Add(800 * time.Millisecond),
		FirstByteAt: startedAt.Add(100 * time.Millisecond),
	}); errFinish != nil {
		t.Fatalf("FinalizeRequest() error = %v", errFinish)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, coreauth.NewManager(nil, nil, nil))
	h.SetTracingRecorder(store)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Params = gin.Params{{Key: "request_id", Value: requestID}}
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/tracing/requests/"+requestID+"/summary", nil)

	h.GetTracingRequestSummary(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Summary tracing.RequestSummaryRecord `json:"summary"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("failed to decode response: %v", errDecode)
	}
	if payload.Summary.RequestID != requestID {
		t.Fatalf("summary.RequestID = %q, want %q", payload.Summary.RequestID, requestID)
	}
	if payload.Summary.RequestStatus != tracing.RequestStatusSucceeded {
		t.Fatalf("summary.RequestStatus = %q, want %q", payload.Summary.RequestStatus, tracing.RequestStatusSucceeded)
	}
	if payload.Summary.StatusCode == nil || *payload.Summary.StatusCode != 200 {
		t.Fatalf("summary.StatusCode = %#v, want 200", payload.Summary.StatusCode)
	}
}

func TestGetTracingRequestSummary_ReturnsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	store, err := tracingsqlite.New(ctx, tracingsqlite.Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("failed to create tracing store: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("failed to close tracing store: %v", errClose)
		}
	}()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, coreauth.NewManager(nil, nil, nil))
	h.SetTracingRecorder(store)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Params = gin.Params{{Key: "request_id", Value: "missing-request"}}
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/tracing/requests/missing-request/summary", nil)

	h.GetTracingRequestSummary(ginCtx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" {
		t.Fatal("expected error response body")
	}
}
