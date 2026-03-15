package tracing

import (
	"context"
	"time"
)

var defaultRecorder Recorder = disabledRecorder{}

type disabledRecorder struct{}

func (disabledRecorder) Enabled() bool                                    { return false }
func (disabledRecorder) BootID() string                                   { return "" }
func (disabledRecorder) DBPath() string                                   { return "" }
func (disabledRecorder) BodiesDir() string                                { return "" }
func (disabledRecorder) StartRequest(context.Context, RequestStart) error { return nil }
func (disabledRecorder) UpdateRequestRoute(context.Context, string, RequestRouteInfo) error {
	return nil
}
func (disabledRecorder) RecordRequestEvent(context.Context, string, string, string, time.Time, any) error {
	return nil
}
func (disabledRecorder) BeginAttempt(context.Context, AttemptStart) error             { return nil }
func (disabledRecorder) UpdateAttemptRequest(context.Context, AttemptRequest) error   { return nil }
func (disabledRecorder) UpdateAttemptResponse(context.Context, AttemptResponse) error { return nil }
func (disabledRecorder) FinishAttempt(context.Context, AttemptFinish) error           { return nil }
func (disabledRecorder) FinalizeRequest(context.Context, RequestFinish) error         { return nil }
func (disabledRecorder) FinalizeUsage(context.Context, UsageFinal) error              { return nil }
func (disabledRecorder) SaveBlob(context.Context, *BlobRecord) error                  { return nil }
func (disabledRecorder) LatestSeq() int64                                             { return 0 }
func (disabledRecorder) Subscribe() (<-chan int64, func()) {
	ch := make(chan int64)
	close(ch)
	return ch, func() {}
}
func (disabledRecorder) Status(context.Context) (StatusRecord, error) { return StatusRecord{}, nil }
func (disabledRecorder) ListEvents(context.Context, int64, int) ([]EventRecord, error) {
	return nil, nil
}
func (disabledRecorder) ListRequestSummaries(context.Context, RequestSummaryFilter) (RequestSummaryPage, error) {
	return RequestSummaryPage{}, nil
}
func (disabledRecorder) GetRequest(context.Context, string) (*RequestRecord, error) { return nil, nil }
func (disabledRecorder) GetRequestDetail(context.Context, string) (*RequestDetailRecord, error) {
	return nil, nil
}
func (disabledRecorder) ListAttempts(context.Context, string) ([]AttemptRecord, error) {
	return nil, nil
}
func (disabledRecorder) GetUsage(context.Context, string) (*UsageFinalRecord, error) { return nil, nil }
func (disabledRecorder) GetBlob(context.Context, string) (*BlobRecord, error)        { return nil, nil }
func (disabledRecorder) DeleteRequest(context.Context, string) (bool, error)         { return false, nil }
func (disabledRecorder) SubscribeRequestSummaries() (<-chan RequestSummaryEvent, func()) {
	ch := make(chan RequestSummaryEvent)
	close(ch)
	return ch, func() {}
}
func (disabledRecorder) Close() error { return nil }

func SetDefaultRecorder(recorder Recorder) {
	if recorder == nil {
		defaultRecorder = disabledRecorder{}
		return
	}
	defaultRecorder = recorder
}

func DefaultRecorder() Recorder { return defaultRecorder }

func Enabled() bool { return defaultRecorder != nil && defaultRecorder.Enabled() }
