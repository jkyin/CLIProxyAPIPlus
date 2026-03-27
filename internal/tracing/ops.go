package tracing

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func RecordRequestStart(ctx context.Context, start RequestStart) *RequestState {
	recorder := DefaultRecorder()
	if recorder == nil || !recorder.Enabled() {
		return nil
	}
	state := NewRequestState(recorder, start.RequestID, start.LegacyRequestID, start.StartedAt)
	if err := recorder.StartRequest(ctx, start); err != nil {
		return nil
	}
	_ = recorder.RecordRequestEvent(ctx, start.RequestID, "", EventRequestStarted, start.StartedAt, map[string]any{
		"is_stream":       start.IsStream,
		"handler_type":    start.HandlerType,
		"requested_model": start.RequestedModel,
	})
	state.routeInfo = RequestRouteInfo{
		IsStream:            start.IsStream,
		HandlerType:         start.HandlerType,
		RequestedModel:      start.RequestedModel,
		ClientCorrelationID: start.ClientCorrelationID,
	}
	return state
}

func UpdateRequestRoute(ctx context.Context, route RequestRouteInfo) {
	state := RequestStateFromContext(ctx)
	if state == nil {
		return
	}
	_ = state.SetRouteInfo(ctx, route)
}

func StartAttempt(ctx context.Context, start AttemptStart) (context.Context, *AttemptState, error) {
	state := RequestStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() {
		return ctx, nil, nil
	}
	attempt, err := state.BeginAttempt(ctx, start)
	if err != nil {
		return ctx, nil, err
	}
	_ = state.recorder.RecordRequestEvent(ctx, state.requestID, attempt.id, EventAttemptStarted, start.StartedAt, map[string]any{
		"attempt_no":     attempt.no,
		"retry_scope":    start.RetryScope,
		"provider":       start.Provider,
		"auth_id":        start.AuthID,
		"route_model":    start.RouteModel,
		"upstream_model": start.UpstreamModel,
	})
	return WithAttemptState(ctx, attempt), attempt, nil
}

func FinishAttempt(ctx context.Context, finish AttemptFinish) {
	state := RequestStateFromContext(ctx)
	attempt := AttemptStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() || attempt == nil {
		return
	}
	if finish.AttemptID == "" {
		finish.AttemptID = attempt.id
	}
	if finish.FinishedAt.IsZero() {
		finish.FinishedAt = time.Now().UTC()
	}
	if finish.Outcome == "" {
		finish.Outcome = AttemptOutcomeFailed
	}
	state.RecordAttemptOutcome(finish.AttemptID, finish.Outcome)
	if attempt.respCollector != nil && finish.Outcome != AttemptOutcomeRunning {
		record, err := attempt.respCollector.Close(finish.Outcome == AttemptOutcomeSucceeded)
		if err == nil && record != nil {
			_ = state.recorder.SaveBlob(ctx, record)
			_ = state.recorder.UpdateAttemptResponse(ctx, AttemptResponse{
				AttemptID:          attempt.id,
				FirstByteAt:        attempt.FirstByteAt(),
				ResponseBodyBlobID: record.BlobID,
			})
		}
	}
	_ = state.recorder.FinishAttempt(ctx, finish)
	_ = state.recorder.RecordRequestEvent(ctx, state.requestID, attempt.id, EventAttemptFinished, finish.FinishedAt, map[string]any{
		"attempt_no":    attempt.no,
		"outcome":       finish.Outcome,
		"status_code":   finish.StatusCode,
		"error_code":    finish.ErrorCode,
		"error_message": finish.ErrorMessage,
	})
}

func RecordAttemptRequest(ctx context.Context, request AttemptRequest) {
	state := RequestStateFromContext(ctx)
	attempt := AttemptStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() || attempt == nil {
		return
	}
	if request.AttemptID == "" {
		request.AttemptID = attempt.id
	}
	attempt.mu.Lock()
	attempt.requestUpdated = true
	attempt.mu.Unlock()
	state.SetActiveAttempt(attempt.id)
	_ = state.recorder.UpdateAttemptRequest(ctx, request)
}

func RecordAttemptResponseMeta(ctx context.Context, statusCode int, headers http.Header) {
	state := RequestStateFromContext(ctx)
	attempt := AttemptStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() || attempt == nil {
		return
	}
	_ = state.recorder.UpdateAttemptResponse(ctx, AttemptResponse{
		AttemptID:       attempt.id,
		StatusCode:      statusCode,
		HeadersAt:       time.Now().UTC(),
		ResponseHeaders: headers,
	})
}

func ObserveUsage(ctx context.Context, obs UsageObservation) {
	state := RequestStateFromContext(ctx)
	attempt := AttemptStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() {
		return
	}
	if obs.RequestID == "" {
		obs.RequestID = state.requestID
	}
	if attempt != nil && obs.AttemptID == "" {
		obs.AttemptID = attempt.id
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	if obs.CompletenessHint == "" {
		obs.CompletenessHint = UsageCompletenessPartial
	}
	state.ObserveUsage(obs)
	_ = state.recorder.RecordRequestEvent(ctx, state.requestID, obs.AttemptID, EventUsageObserved, obs.ObservedAt, usageObservationPayload(obs))
}

func RecordHandlerBootstrapRetry(ctx context.Context, payload map[string]any) {
	state := RequestStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() {
		return
	}
	_ = state.recorder.RecordRequestEvent(ctx, state.requestID, "", EventHandlerBootstrapRetry, time.Now().UTC(), payload)
}

func FinalizeRequest(ctx context.Context, finish RequestFinish) {
	state := RequestStateFromContext(ctx)
	if state == nil || state.recorder == nil || !state.recorder.Enabled() {
		return
	}
	if finish.RequestID == "" {
		finish.RequestID = state.requestID
	}
	if finish.FinishedAt.IsZero() {
		finish.FinishedAt = time.Now().UTC()
	}
	if finish.Status == "" {
		finish.Status = RequestStatusSucceeded
	}
	if finish.FirstByteAt.IsZero() {
		finish.FirstByteAt = state.ResponseFirstByteAt()
	}
	if state.respCollector != nil && finish.ResponseBodyBlobID == "" {
		record, err := state.respCollector.Close(finish.Status == RequestStatusSucceeded)
		if err == nil && record != nil {
			_ = state.recorder.SaveBlob(ctx, record)
			finish.ResponseBodyBlobID = record.BlobID
		}
	}
	_ = state.recorder.FinalizeRequest(ctx, finish)
	eventType := EventRequestFinished
	if finish.Status == RequestStatusInterrupted {
		eventType = EventRequestInterrupted
	}
	_ = state.recorder.RecordRequestEvent(ctx, state.requestID, "", eventType, finish.FinishedAt, map[string]any{
		"status":                 finish.Status,
		"downstream_status_code": finish.StatusCode,
	})
	if usage := state.FinalizeUsage(finish.Status); usage != nil {
		_ = state.recorder.FinalizeUsage(ctx, *usage)
		_ = state.recorder.RecordRequestEvent(ctx, state.requestID, usage.AttemptID, EventUsageFinalized, usage.FinalizedAt, usageFinalPayload(*usage))
	}
}

func usageObservationPayload(obs UsageObservation) map[string]any {
	return map[string]any{
		"input_tokens":      usageMetricPayloadValue(obs.CompletenessHint, obs.InputTokens),
		"output_tokens":     usageMetricPayloadValue(obs.CompletenessHint, obs.OutputTokens),
		"reasoning_tokens":  usageMetricPayloadValue(obs.CompletenessHint, obs.ReasoningTokens),
		"cached_tokens":     usageMetricPayloadValue(obs.CompletenessHint, obs.CachedTokens),
		"total_tokens":      usageMetricPayloadValue(obs.CompletenessHint, obs.TotalTokens),
		"is_terminal":       obs.IsTerminal,
		"completeness_hint": obs.CompletenessHint,
	}
}

func usageFinalPayload(usage UsageFinal) map[string]any {
	return map[string]any{
		"completeness":     usage.Completeness,
		"input_tokens":     usageMetricPayloadValue(usage.Completeness, usage.InputTokens),
		"output_tokens":    usageMetricPayloadValue(usage.Completeness, usage.OutputTokens),
		"reasoning_tokens": usageMetricPayloadValue(usage.Completeness, usage.ReasoningTokens),
		"cached_tokens":    usageMetricPayloadValue(usage.Completeness, usage.CachedTokens),
		"total_tokens":     usageMetricPayloadValue(usage.Completeness, usage.TotalTokens),
	}
}

func usageMetricPayloadValue(completeness string, value int64) any {
	if completeness == UsageCompletenessMissing {
		return nil
	}
	return value
}

func AuthSnapshotJSON(provider, authID, authIndex, label, accountType, account, sourcePath, sourceKind, status string, unavailable bool, nextRetryAt time.Time) []byte {
	payload := map[string]any{
		"provider":     provider,
		"auth_id":      authID,
		"auth_index":   authIndex,
		"label":        label,
		"account_type": accountType,
		"account":      account,
		"path":         sourcePath,
		"source":       sourceKind,
		"status":       status,
		"unavailable":  unavailable,
	}
	if !nextRetryAt.IsZero() {
		payload["next_retry_after"] = nextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	for key, value := range payload {
		if str, ok := value.(string); ok && strings.TrimSpace(str) == "" {
			delete(payload, key)
		}
	}
	return MarshalJSONMap(payload)
}
