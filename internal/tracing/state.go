package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type requestStateKey struct{}
type attemptStateKey struct{}

const ginRequestStateKey = "__trace_request_state__"

type RequestState struct {
	recorder        Recorder
	requestID       string
	legacyRequestID string
	startedAt       time.Time
	blobConfig      BlobConfig

	mu              sync.Mutex
	attemptNo       int
	attempts        map[string]*AttemptState
	activeAttemptID string
	usage           *UsageAccumulator
	routeInfo       RequestRouteInfo
	routeFlushed    bool
	respCollector   *BlobCollector
	respFirstByteAt time.Time
}

type AttemptState struct {
	id            string
	no            int
	retryScope    string
	provider      string
	executorID    string
	authID        string
	authIndex     string
	routeModel    string
	upstreamModel string
	startedAt     time.Time

	mu             sync.Mutex
	requestUpdated bool
	responseMeta   bool
	responseFirst  time.Time
	respCollector  *BlobCollector
}

type AttemptSnapshot struct {
	RetryScope    string
	Provider      string
	ExecutorID    string
	AuthID        string
	AuthIndex     string
	RouteModel    string
	UpstreamModel string
	RequestBound  bool
}

func NewRequestState(recorder Recorder, requestID, legacyRequestID string, startedAt time.Time) *RequestState {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &RequestState{
		recorder:        recorder,
		requestID:       requestID,
		legacyRequestID: legacyRequestID,
		startedAt:       startedAt,
		attempts:        make(map[string]*AttemptState),
		usage:           NewUsageAccumulator(),
	}
}

func (s *RequestState) RequestID() string            { return s.requestID }
func (s *RequestState) LegacyRequestID() string      { return s.legacyRequestID }
func (s *RequestState) Recorder() Recorder           { return s.recorder }
func (s *RequestState) SetBlobConfig(cfg BlobConfig) { s.blobConfig = cfg }
func (s *RequestState) BlobConfig() BlobConfig       { return s.blobConfig }

func (s *RequestState) EnsureResponseCollector(config BlobConfig) (*BlobCollector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respCollector != nil {
		return s.respCollector, nil
	}
	collector, err := NewBlobCollector(config)
	if err != nil {
		return nil, err
	}
	s.respCollector = collector
	return collector, nil
}

func (s *RequestState) MarkResponseFirstByte(at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	if s.respFirstByteAt.IsZero() {
		s.respFirstByteAt = at
	}
	s.mu.Unlock()
}

func (s *RequestState) ResponseFirstByteAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.respFirstByteAt
}

func (s *RequestState) SetRouteInfo(ctx context.Context, route RequestRouteInfo) error {
	if s == nil || s.recorder == nil || !s.recorder.Enabled() {
		return nil
	}
	s.mu.Lock()
	s.routeInfo = route
	s.routeFlushed = true
	s.mu.Unlock()
	return s.recorder.UpdateRequestRoute(ctx, s.requestID, route)
}

func (s *RequestState) BeginAttempt(ctx context.Context, start AttemptStart) (*AttemptState, error) {
	if s == nil || s.recorder == nil || !s.recorder.Enabled() {
		return nil, nil
	}
	if start.AttemptID == "" {
		start.AttemptID = MustNewID()
	}
	if start.StartedAt.IsZero() {
		start.StartedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.attemptNo++
	start.AttemptNo = s.attemptNo
	start.RequestID = s.requestID
	state := &AttemptState{
		id:            start.AttemptID,
		no:            start.AttemptNo,
		retryScope:    start.RetryScope,
		provider:      start.Provider,
		executorID:    start.ExecutorID,
		authID:        start.AuthID,
		authIndex:     start.AuthIndex,
		routeModel:    start.RouteModel,
		upstreamModel: start.UpstreamModel,
		startedAt:     start.StartedAt,
	}
	s.attempts[start.AttemptID] = state
	s.activeAttemptID = start.AttemptID
	s.mu.Unlock()
	if err := s.recorder.BeginAttempt(ctx, start); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *RequestState) ActiveAttempt() *AttemptState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeAttemptID == "" {
		return nil
	}
	return s.attempts[s.activeAttemptID]
}

func (s *RequestState) SetActiveAttempt(attemptID string) {
	s.mu.Lock()
	s.activeAttemptID = attemptID
	s.mu.Unlock()
}

func (s *RequestState) ObserveUsage(obs UsageObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.Observe(obs)
}

func (s *RequestState) FinalizeUsage() *UsageFinal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage.Finalize(s.requestID)
}

func (a *AttemptState) EnsureResponseCollector(config BlobConfig) (*BlobCollector, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.respCollector != nil {
		return a.respCollector, nil
	}
	collector, err := NewBlobCollector(config)
	if err != nil {
		return nil, err
	}
	a.respCollector = collector
	return collector, nil
}

func (a *AttemptState) MarkFirstByte(at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	a.mu.Lock()
	if a.responseFirst.IsZero() {
		a.responseFirst = at
	}
	a.mu.Unlock()
}

func (a *AttemptState) FirstByteAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.responseFirst
}

func (a *AttemptState) Snapshot() AttemptSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AttemptSnapshot{
		RetryScope:    a.retryScope,
		Provider:      a.provider,
		ExecutorID:    a.executorID,
		AuthID:        a.authID,
		AuthIndex:     a.authIndex,
		RouteModel:    a.routeModel,
		UpstreamModel: a.upstreamModel,
		RequestBound:  a.requestUpdated,
	}
}

func AttachRequestState(c *gin.Context, state *RequestState) {
	if c == nil || state == nil {
		return
	}
	c.Set(ginRequestStateKey, state)
	ctx := context.WithValue(c.Request.Context(), requestStateKey{}, state)
	c.Request = c.Request.WithContext(ctx)
}

func RequestStateFromContext(ctx context.Context) *RequestState {
	if ctx == nil {
		return nil
	}
	if state, ok := ctx.Value(requestStateKey{}).(*RequestState); ok {
		return state
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		return RequestStateFromGin(ginCtx)
	}
	return nil
}

func RequestStateFromGin(c *gin.Context) *RequestState {
	if c == nil {
		return nil
	}
	if state, exists := c.Get(ginRequestStateKey); exists {
		if typed, ok := state.(*RequestState); ok {
			return typed
		}
	}
	return nil
}

func WithAttemptState(ctx context.Context, attempt *AttemptState) context.Context {
	if ctx == nil || attempt == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptStateKey{}, attempt)
}

func AttemptStateFromContext(ctx context.Context) *AttemptState {
	if ctx == nil {
		return nil
	}
	if state := RequestStateFromContext(ctx); state != nil {
		if attempt := state.ActiveAttempt(); attempt != nil {
			return attempt
		}
	}
	if attempt, ok := ctx.Value(attemptStateKey{}).(*AttemptState); ok {
		return attempt
	}
	return nil
}

func MustNewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

type UsageAccumulator struct {
	latestCandidate *UsageObservation
	latestTerminal  *UsageObservation
	failed          bool
}

func NewUsageAccumulator() *UsageAccumulator { return &UsageAccumulator{} }

func (u *UsageAccumulator) Observe(obs UsageObservation) {
	copyObs := obs
	if copyObs.ObservedAt.IsZero() {
		copyObs.ObservedAt = time.Now().UTC()
	}
	if copyObs.TotalTokens == 0 {
		total := copyObs.InputTokens + copyObs.OutputTokens + copyObs.ReasoningTokens
		if total > 0 {
			copyObs.TotalTokens = total
			copyObs.DerivedTotal = true
		}
	}
	u.latestCandidate = &copyObs
	if copyObs.IsTerminal {
		u.latestTerminal = &copyObs
	}
}

func (u *UsageAccumulator) MarkFailed() { u.failed = true }

func (u *UsageAccumulator) Finalize(requestID string) *UsageFinal {
	finalizedAt := time.Now().UTC()
	if u.latestTerminal != nil {
		return usageFinalFromObservation(requestID, *u.latestTerminal, UsageCompletenessComplete, finalizedAt)
	}
	if u.latestCandidate != nil {
		if u.latestCandidate.CompletenessHint == UsageCompletenessMissing &&
			u.latestCandidate.InputTokens == 0 &&
			u.latestCandidate.OutputTokens == 0 &&
			u.latestCandidate.ReasoningTokens == 0 &&
			u.latestCandidate.CachedTokens == 0 &&
			u.latestCandidate.TotalTokens == 0 {
			return &UsageFinal{
				RequestID:    requestID,
				AttemptID:    u.latestCandidate.AttemptID,
				FinalizedAt:  finalizedAt,
				Status:       boolToUsageStatus(!u.failed),
				Completeness: UsageCompletenessMissing,
			}
		}
		completeness := UsageCompletenessComplete
		if u.failed {
			completeness = UsageCompletenessPartial
		}
		return usageFinalFromObservation(requestID, *u.latestCandidate, completeness, finalizedAt)
	}
	return &UsageFinal{
		RequestID:    requestID,
		FinalizedAt:  finalizedAt,
		Status:       boolToUsageStatus(!u.failed),
		Completeness: UsageCompletenessMissing,
	}
}

func usageFinalFromObservation(requestID string, obs UsageObservation, completeness string, finalizedAt time.Time) *UsageFinal {
	status := boolToUsageStatus(completeness == UsageCompletenessComplete || completeness == UsageCompletenessPartial)
	return &UsageFinal{
		RequestID:          requestID,
		AttemptID:          obs.AttemptID,
		FinalizedAt:        finalizedAt,
		Status:             status,
		Completeness:       completeness,
		InputTokens:        obs.InputTokens,
		OutputTokens:       obs.OutputTokens,
		ReasoningTokens:    obs.ReasoningTokens,
		CachedTokens:       obs.CachedTokens,
		TotalTokens:        obs.TotalTokens,
		DerivedTotal:       obs.DerivedTotal,
		ProviderUsageJSON:  cloneBytes(obs.ProviderUsageJSON),
		ProviderNativeJSON: cloneBytes(obs.ProviderNativeJSON),
	}
}

func boolToUsageStatus(success bool) string {
	if success {
		return "success"
	}
	return "failed"
}

func MarshalJSONMap(value any) []byte {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func HeaderJSON(headers http.Header) []byte {
	if len(headers) == 0 {
		return nil
	}
	data, err := json.Marshal(headers)
	if err != nil {
		return nil
	}
	return data
}

func NormalizedRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	return bytes.Clone(body)
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out
}
