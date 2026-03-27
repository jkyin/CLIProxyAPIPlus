package tracing

import "testing"

func TestUsageAccumulatorFinalizeSingleSucceededAttemptIsComplete(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		InputTokens:      10,
		OutputTokens:     4,
		CompletenessHint: UsageCompletenessPartial,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusSucceeded)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.Completeness != UsageCompletenessComplete {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessComplete)
	}
	if final.AttemptID != "attempt-1" {
		t.Fatalf("Finalize().AttemptID = %q, want attempt-1", final.AttemptID)
	}
}

func TestUsageAccumulatorFinalizeSuccessfulRetryUsageWins(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		InputTokens:      9,
		OutputTokens:     3,
		CompletenessHint: UsageCompletenessPartial,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeFailed)

	acc.Observe(UsageObservation{
		AttemptID:        "attempt-2",
		InputTokens:      12,
		OutputTokens:     5,
		IsTerminal:       true,
		CompletenessHint: UsageCompletenessComplete,
	})
	acc.RecordAttemptOutcome("attempt-2", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusSucceeded)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.Completeness != UsageCompletenessComplete {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessComplete)
	}
	if final.AttemptID != "attempt-2" {
		t.Fatalf("Finalize().AttemptID = %q, want attempt-2", final.AttemptID)
	}
	if final.TotalTokens != 17 {
		t.Fatalf("Finalize().TotalTokens = %d, want 17", final.TotalTokens)
	}
}

func TestUsageAccumulatorFinalizeSuccessfulRetryWithoutUsageStaysPartial(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		InputTokens:      8,
		OutputTokens:     2,
		CompletenessHint: UsageCompletenessPartial,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeFailed)
	acc.RecordAttemptOutcome("attempt-2", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusSucceeded)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.Completeness != UsageCompletenessPartial {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessPartial)
	}
	if final.AttemptID != "attempt-1" {
		t.Fatalf("Finalize().AttemptID = %q, want attempt-1", final.AttemptID)
	}
}

func TestUsageAccumulatorFinalizeFailedRequestWithUsageIsPartial(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		InputTokens:      7,
		OutputTokens:     1,
		CompletenessHint: UsageCompletenessPartial,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusFailed)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.Completeness != UsageCompletenessPartial {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessPartial)
	}
}

func TestUsageAccumulatorFinalizeMissingUsage(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		CompletenessHint: UsageCompletenessMissing,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusSucceeded)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.Completeness != UsageCompletenessMissing {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessMissing)
	}
	if final.Status != "success" {
		t.Fatalf("Finalize().Status = %q, want success", final.Status)
	}
}

func TestUsageAccumulatorFinalizePrefersTerminalUsage(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.Observe(UsageObservation{
		AttemptID:        "attempt-1",
		InputTokens:      10,
		OutputTokens:     2,
		IsTerminal:       true,
		CompletenessHint: UsageCompletenessComplete,
	})
	acc.RecordAttemptOutcome("attempt-1", AttemptOutcomeSucceeded)

	acc.Observe(UsageObservation{
		AttemptID:        "attempt-2",
		InputTokens:      20,
		OutputTokens:     5,
		CompletenessHint: UsageCompletenessPartial,
	})
	acc.RecordAttemptOutcome("attempt-2", AttemptOutcomeSucceeded)

	final := acc.Finalize("request-1", RequestStatusSucceeded)
	if final == nil {
		t.Fatal("Finalize() = nil, want usage")
	}
	if final.AttemptID != "attempt-1" {
		t.Fatalf("Finalize().AttemptID = %q, want attempt-1", final.AttemptID)
	}
	if final.TotalTokens != 12 {
		t.Fatalf("Finalize().TotalTokens = %d, want 12", final.TotalTokens)
	}
	if final.Completeness != UsageCompletenessComplete {
		t.Fatalf("Finalize().Completeness = %q, want %q", final.Completeness, UsageCompletenessComplete)
	}
}
