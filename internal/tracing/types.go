package tracing

import (
	"context"
	"net/http"
	"time"
)

const (
	EventRequestStarted        = "request.started"
	EventRequestFinished       = "request.finished"
	EventRequestInterrupted    = "request.interrupted"
	EventAttemptStarted        = "attempt.started"
	EventAttemptFinished       = "attempt.finished"
	EventHandlerBootstrapRetry = "handler.bootstrap_retry"
	EventUsageObserved         = "usage.observed"
	EventUsageFinalized        = "usage.finalized"
)

const (
	RequestStatusRunning     = "running"
	RequestStatusSucceeded   = "succeeded"
	RequestStatusFailed      = "failed"
	RequestStatusInterrupted = "interrupted"
)

const (
	AttemptOutcomeRunning     = "running"
	AttemptOutcomeSucceeded   = "succeeded"
	AttemptOutcomeFailed      = "failed"
	AttemptOutcomeInterrupted = "interrupted"
)

const (
	UsageCompletenessComplete = "complete"
	UsageCompletenessPartial  = "partial"
	UsageCompletenessMissing  = "missing"
)

type RequestStart struct {
	RequestID           string
	LegacyRequestID     string
	StartedAt           time.Time
	Method              string
	Scheme              string
	Host                string
	Path                string
	Query               string
	RequestHeaders      http.Header
	RequestBodyBlobID   string
	IsStream            bool
	HandlerType         string
	RequestedModel      string
	ClientCorrelationID string
}

type RequestRouteInfo struct {
	IsStream            bool
	HandlerType         string
	RequestedModel      string
	ClientCorrelationID string
}

type RequestFinish struct {
	RequestID          string
	StatusCode         int
	Status             string
	FinishedAt         time.Time
	FirstByteAt        time.Time
	ResponseHeaders    http.Header
	ResponseBodyBlobID string
}

type AttemptStart struct {
	RequestID        string
	AttemptID        string
	AttemptNo        int
	RetryScope       string
	Provider         string
	ExecutorID       string
	AuthID           string
	AuthIndex        string
	AuthSnapshotJSON []byte
	RouteModel       string
	UpstreamModel    string
	StartedAt        time.Time
}

type AttemptRequest struct {
	AttemptID         string
	UpstreamURL       string
	UpstreamMethod    string
	UpstreamProtocol  string
	RequestHeaders    http.Header
	RequestBodyBlobID string
}

type AttemptResponse struct {
	AttemptID          string
	StatusCode         int
	HeadersAt          time.Time
	ResponseHeaders    http.Header
	FirstByteAt        time.Time
	ResponseBodyBlobID string
}

type AttemptFinish struct {
	AttemptID    string
	Outcome      string
	FinishedAt   time.Time
	StatusCode   int
	ErrorCode    string
	ErrorMessage string
}

type UsageObservation struct {
	RequestID          string
	AttemptID          string
	Provider           string
	ObservedAt         time.Time
	InputTokens        int64
	OutputTokens       int64
	ReasoningTokens    int64
	CachedTokens       int64
	TotalTokens        int64
	DerivedTotal       bool
	IsTerminal         bool
	CompletenessHint   string
	ProviderUsageJSON  []byte
	ProviderNativeJSON []byte
}

type UsageFinal struct {
	RequestID          string
	AttemptID          string
	FinalizedAt        time.Time
	Status             string
	Completeness       string
	InputTokens        int64
	OutputTokens       int64
	ReasoningTokens    int64
	CachedTokens       int64
	TotalTokens        int64
	DerivedTotal       bool
	ProviderUsageJSON  []byte
	ProviderNativeJSON []byte
}

type EventRecord struct {
	Seq       int64  `json:"seq"`
	RequestID string `json:"request_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	TSUnixNS  int64  `json:"ts_ns"`
	EventType string `json:"event_type"`
	Payload   []byte `json:"payload_json"`
	BootID    string `json:"boot_id"`
}

type RequestRecord struct {
	RequestID             string `json:"request_id"`
	LegacyRequestID       string `json:"legacy_request_id,omitempty"`
	StartedAtNS           int64  `json:"started_at_ns"`
	FinishedAtNS          int64  `json:"finished_at_ns"`
	Status                string `json:"status"`
	HTTPMethod            string `json:"http_method"`
	HTTPScheme            string `json:"http_scheme"`
	HTTPHost              string `json:"http_host"`
	HTTPPath              string `json:"http_path"`
	HTTPQuery             string `json:"http_query"`
	IsStream              bool   `json:"is_stream"`
	HandlerType           string `json:"handler_type,omitempty"`
	RequestedModel        string `json:"requested_model,omitempty"`
	ClientCorrelationID   string `json:"client_correlation_id,omitempty"`
	DownstreamStatusCode  int    `json:"downstream_status_code"`
	DownstreamFirstByteNS int64  `json:"downstream_first_byte_at_ns"`
	RequestHeadersJSON    []byte `json:"request_headers_json,omitempty"`
	RequestBodyBlobID     string `json:"request_body_blob_id,omitempty"`
	ResponseHeadersJSON   []byte `json:"response_headers_json,omitempty"`
	ResponseBodyBlobID    string `json:"response_body_blob_id,omitempty"`
}

type AttemptRecord struct {
	AttemptID           string `json:"attempt_id"`
	RequestID           string `json:"request_id"`
	AttemptNo           int    `json:"attempt_no"`
	RetryScope          string `json:"retry_scope"`
	Provider            string `json:"provider"`
	ExecutorID          string `json:"executor_id,omitempty"`
	AuthID              string `json:"auth_id,omitempty"`
	AuthIndex           string `json:"auth_index,omitempty"`
	AuthSnapshotJSON    []byte `json:"auth_snapshot_json,omitempty"`
	RouteModel          string `json:"route_model,omitempty"`
	UpstreamModel       string `json:"upstream_model,omitempty"`
	UpstreamURL         string `json:"upstream_url,omitempty"`
	UpstreamMethod      string `json:"upstream_method,omitempty"`
	UpstreamProtocol    string `json:"upstream_protocol,omitempty"`
	StartedAtNS         int64  `json:"started_at_ns"`
	HeadersAtNS         int64  `json:"headers_at_ns"`
	FirstByteAtNS       int64  `json:"first_byte_at_ns"`
	FinishedAtNS        int64  `json:"finished_at_ns"`
	StatusCode          int    `json:"status_code"`
	Outcome             string `json:"outcome"`
	ErrorCode           string `json:"error_code,omitempty"`
	ErrorMessage        string `json:"error_message,omitempty"`
	RequestHeadersJSON  []byte `json:"request_headers_json,omitempty"`
	RequestBodyBlobID   string `json:"request_body_blob_id,omitempty"`
	ResponseHeadersJSON []byte `json:"response_headers_json,omitempty"`
	ResponseBodyBlobID  string `json:"response_body_blob_id,omitempty"`
}

type UsageFinalRecord struct {
	RequestID          string `json:"request_id"`
	AttemptID          string `json:"attempt_id,omitempty"`
	FinalizedAtNS      int64  `json:"finalized_at_ns"`
	Status             string `json:"status"`
	Completeness       string `json:"completeness"`
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	ReasoningTokens    *int64 `json:"reasoning_tokens"`
	CachedTokens       *int64 `json:"cached_tokens"`
	TotalTokens        *int64 `json:"total_tokens"`
	DerivedTotal       bool   `json:"derived_total"`
	ProviderUsageJSON  []byte `json:"provider_usage_json,omitempty"`
	ProviderNativeJSON []byte `json:"provider_native_json,omitempty"`
}

type BlobRecord struct {
	BlobID          string `json:"blob_id"`
	StorageKind     string `json:"storage_kind"`
	SizeBytes       int64  `json:"size_bytes"`
	ContentType     string `json:"content_type,omitempty"`
	ContentEncoding string `json:"content_encoding,omitempty"`
	Complete        bool   `json:"complete"`
	Truncated       bool   `json:"truncated"`
	SHA256          string `json:"sha256,omitempty"`
	FileRelPath     string `json:"file_relpath,omitempty"`
	InlineBytes     []byte `json:"inline_bytes,omitempty"`
}

type StatusRecord struct {
	Enabled         bool   `json:"enabled"`
	BootID          string `json:"boot_id,omitempty"`
	LatestSeq       int64  `json:"latest_seq"`
	RequestsRunning int64  `json:"requests_running"`
	AttemptsRunning int64  `json:"attempts_running"`
	DBPath          string `json:"db_path,omitempty"`
	BodiesDir       string `json:"bodies_dir,omitempty"`
}

type Recorder interface {
	Enabled() bool
	BootID() string
	DBPath() string
	BodiesDir() string
	StartRequest(ctx context.Context, start RequestStart) error
	UpdateRequestRoute(ctx context.Context, requestID string, route RequestRouteInfo) error
	RecordRequestEvent(ctx context.Context, requestID, attemptID, eventType string, ts time.Time, payload any) error
	BeginAttempt(ctx context.Context, start AttemptStart) error
	UpdateAttemptRequest(ctx context.Context, request AttemptRequest) error
	UpdateAttemptResponse(ctx context.Context, response AttemptResponse) error
	FinishAttempt(ctx context.Context, finish AttemptFinish) error
	FinalizeRequest(ctx context.Context, finish RequestFinish) error
	FinalizeUsage(ctx context.Context, final UsageFinal) error
	SaveBlob(ctx context.Context, blob *BlobRecord) error
	LatestSeq() int64
	Subscribe() (<-chan int64, func())
	Status(ctx context.Context) (StatusRecord, error)
	ListEvents(ctx context.Context, afterSeq int64, limit int) ([]EventRecord, error)
	GetRequest(ctx context.Context, requestID string) (*RequestRecord, error)
	ListAttempts(ctx context.Context, requestID string) ([]AttemptRecord, error)
	GetUsage(ctx context.Context, requestID string) (*UsageFinalRecord, error)
	GetBlob(ctx context.Context, blobID string) (*BlobRecord, error)
	Close() error
}
