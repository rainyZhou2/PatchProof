package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelayMonotonicAndCapped(t *testing.T) {
	t.Parallel()
	policy := Policy{MaxAttempts: 10, BaseDelay: 10 * time.Millisecond, MaxDelay: 100 * time.Millisecond}
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
	}
	previous := time.Duration(-1)
	for attempt := 1; attempt <= len(want); attempt++ {
		got := Delay(policy, attempt)
		if got != want[attempt-1] {
			t.Fatalf("Delay(attempt=%d) = %v, want %v", attempt, got, want[attempt-1])
		}
		if got < previous {
			t.Fatalf("Delay decreased at attempt %d: %v after %v", attempt, got, previous)
		}
		if got > policy.MaxDelay {
			t.Fatalf("Delay(attempt=%d) = %v exceeds max %v", attempt, got, policy.MaxDelay)
		}
		previous = got
	}

	if got := Delay(policy, 0); got != want[0] {
		t.Fatalf("Delay(attempt=0) = %v, want %v", got, want[0])
	}
	if got := Delay(policy, -5); got != want[0] {
		t.Fatalf("Delay(attempt=-5) = %v, want %v", got, want[0])
	}

	lowMax := Policy{BaseDelay: time.Second, MaxDelay: 100 * time.Millisecond}
	if got := Delay(lowMax, 1); got != 100*time.Millisecond {
		t.Fatalf("Delay with max below base = %v, want 100ms", got)
	}
}

func TestDelayJitterBounds(t *testing.T) {
	t.Parallel()
	policy := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Jitter: 0.2}
	for i := 0; i < 200; i++ {
		got := Delay(policy, 1)
		if got < 80*time.Millisecond || got > 120*time.Millisecond {
			t.Fatalf("jittered Delay = %v, want within [80ms, 120ms]", got)
		}
	}

	large := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Jitter: 1}
	for i := 0; i < 200; i++ {
		got := Delay(large, 1)
		if got < 0 || got > 200*time.Millisecond {
			t.Fatalf("full-jitter Delay = %v, want within [0, 200ms]", got)
		}
	}

	// Jitter is clamped into [0, 1]; negative jitter behaves like no jitter.
	clamped := Policy{BaseDelay: 50 * time.Millisecond, MaxDelay: time.Second, Jitter: -3}
	if got := Delay(clamped, 1); got != 50*time.Millisecond {
		t.Fatalf("negative jitter Delay = %v, want 50ms", got)
	}
}

func TestDoSucceedsAfterTransientErrors(t *testing.T) {
	t.Parallel()
	transient := errors.New("transient")
	var calls int
	err := Do(context.Background(), Policy{
		MaxAttempts: 4,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		IsRetryable: func(error) bool { return true },
	}, func(context.Context) error {
		calls++
		if calls < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Fatalf("Do called fn %d times, want 3", calls)
	}
}

func TestDoStopsOnNonRetryableError(t *testing.T) {
	t.Parallel()
	fatal := errors.New("fatal")
	var calls int
	err := Do(context.Background(), Policy{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		IsRetryable: func(err error) bool { return !errors.Is(err, fatal) },
	}, func(context.Context) error {
		calls++
		return fatal
	})
	if !errors.Is(err, fatal) {
		t.Fatalf("Do error = %v, want fatal", err)
	}
	if calls != 1 {
		t.Fatalf("Do called fn %d times, want 1", calls)
	}
}

func TestDoExhaustsAttempts(t *testing.T) {
	t.Parallel()
	transient := errors.New("transient")
	var calls int
	err := Do(context.Background(), Policy{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
	}, func(context.Context) error {
		calls++
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("Do error = %v, want transient", err)
	}
	if calls != 3 {
		t.Fatalf("Do called fn %d times, want 3", calls)
	}
}

func TestDoRespectsCancelledContextBeforeFirstAttempt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	err := Do(ctx, Policy{MaxAttempts: 3, BaseDelay: time.Millisecond}, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fn called %d times with a cancelled context", got)
	}
}

func TestDoCancelsDuringBackoff(t *testing.T) {
	t.Parallel()
	transient := errors.New("transient")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	err := Do(ctx, Policy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Second,
		MaxDelay:    10 * time.Second,
	}, func(context.Context) error {
		calls++
		cancel()
		return transient
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, transient) {
		t.Fatalf("Do error = %v, want it to retain the last error", err)
	}
	if calls != 1 {
		t.Fatalf("Do called fn %d times, want 1", calls)
	}
}

func TestDoFirstAttemptSuccess(t *testing.T) {
	t.Parallel()
	var calls int
	err := Do(context.Background(), Policy{
		MaxAttempts: 2,
		BaseDelay:   time.Hour,
	}, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Do called fn %d times, want 1", calls)
	}
}
