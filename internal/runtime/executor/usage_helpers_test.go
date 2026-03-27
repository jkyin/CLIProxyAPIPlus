package executor

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
	tracingsqlite "github.com/router-for-me/CLIProxyAPI/v6/internal/tracing/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestEnsurePublishedDoesNotOverrideObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := t.Context()
	store, err := tracingsqlite.New(ctx, tracingsqlite.Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("tracing sqlite New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	state := tracing.NewRequestState(store, tracing.MustNewID(), "", time.Now().UTC())
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "http://example.test/v1/chat/completions", nil)
	tracing.AttachRequestState(ginCtx, state)

	requestCtx := ginCtx.Request.Context()
	reporter := newUsageReporter(requestCtx, "test-provider", "test-model", nil)
	reporter.publish(requestCtx, usageCandidate(usage.Detail{
		InputTokens:  13,
		OutputTokens: 335,
		TotalTokens:  348,
	}))
	reporter.ensurePublished(requestCtx)

	final := state.FinalizeUsage(tracing.RequestStatusSucceeded)
	if final == nil {
		t.Fatal("FinalizeUsage() = nil, want usage")
	}
	if final.Completeness != tracing.UsageCompletenessComplete {
		t.Fatalf("FinalizeUsage().Completeness = %q, want %q", final.Completeness, tracing.UsageCompletenessComplete)
	}
	if final.InputTokens != 13 || final.OutputTokens != 335 || final.TotalTokens != 348 {
		t.Fatalf("FinalizeUsage() = %+v, want prompt=13 completion=335 total=348", *final)
	}
}

func TestEnsurePublishedWithoutObservedUsageProducesMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := t.Context()
	store, err := tracingsqlite.New(ctx, tracingsqlite.Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("tracing sqlite New() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	state := tracing.NewRequestState(store, tracing.MustNewID(), "", time.Now().UTC())
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "http://example.test/v1/chat/completions", nil)
	tracing.AttachRequestState(ginCtx, state)

	requestCtx := ginCtx.Request.Context()
	reporter := newUsageReporter(requestCtx, "test-provider", "test-model", nil)
	reporter.ensurePublished(requestCtx)

	final := state.FinalizeUsage(tracing.RequestStatusSucceeded)
	if final == nil {
		t.Fatal("FinalizeUsage() = nil, want usage")
	}
	if final.Completeness != tracing.UsageCompletenessMissing {
		t.Fatalf("FinalizeUsage().Completeness = %q, want %q", final.Completeness, tracing.UsageCompletenessMissing)
	}
	if final.Status != "success" {
		t.Fatalf("FinalizeUsage().Status = %q, want success", final.Status)
	}
}

func TestUsageReporterBuildRecordIncludesLatency(t *testing.T) {
	reporter := &usageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		requestedAt: time.Now().Add(-1500 * time.Millisecond),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Latency < time.Second {
		t.Fatalf("latency = %v, want >= 1s", record.Latency)
	}
	if record.Latency > 3*time.Second {
		t.Fatalf("latency = %v, want <= 3s", record.Latency)
	}
}
