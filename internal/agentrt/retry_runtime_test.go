package agentrt

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// scriptedRuntime replays a scripted sequence of (msg, err) outcomes.
type scriptedRuntime struct {
	label   string
	results []scriptedStep
	reqs    []Request
}

type scriptedStep struct {
	text string
	err  error
	fn   func(ctx context.Context) (*Message, error)
}

func (s *scriptedRuntime) Describe() string { return s.label }

func (s *scriptedRuntime) Complete(ctx context.Context, req Request) (*Message, error) {
	s.reqs = append(s.reqs, req)
	idx := len(s.reqs) - 1
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	step := s.results[idx]
	if step.fn != nil {
		return step.fn(ctx)
	}
	if step.err != nil {
		return nil, step.err
	}
	return &Message{
		Role:       RoleAssistant,
		StopReason: StopStop,
		Parts:      []Part{{Type: PartText, Text: step.text}},
	}, nil
}

func TestRetryingRuntimeSucceedsFirstTry(t *testing.T) {
	inner := &scriptedRuntime{label: "custom/m1", results: []scriptedStep{{text: "ok"}}}
	var events []RetryEvent
	rt := &RetryingRuntime{
		Inner:      inner,
		MaxRetries: 2,
		BaseDelay:  time.Second,
		MaxDelay:   15 * time.Second,
		OnEvent:    func(ev RetryEvent) { events = append(events, ev) },
		sleepFn:    func(context.Context, time.Duration) bool { return true },
	}
	if rt.Describe() != "custom/m1" {
		t.Fatalf("Describe() = %q", rt.Describe())
	}
	msg, err := rt.Complete(context.Background(), Request{})
	if err != nil || msg.Text() != "ok" {
		t.Fatalf("unexpected result: msg=%+v err=%v", msg, err)
	}
	if len(inner.reqs) != 1 || len(events) != 0 {
		t.Fatalf("expected 1 call and 0 events, got %d calls and %d events", len(inner.reqs), len(events))
	}
}

func TestRetryingRuntimeRetriesTwiceThenSucceeds(t *testing.T) {
	inner := &scriptedRuntime{
		label: "custom/m1",
		results: []scriptedStep{
			{err: io.ErrUnexpectedEOF},
			{err: errors.New("503 service unavailable")},
			{text: "recovered"},
		},
	}
	var events []RetryEvent
	var obsEvents []RetryEvent
	rt := &RetryingRuntime{
		Inner:      inner,
		MaxRetries: 2,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   500 * time.Millisecond,
		OnEvent:    func(ev RetryEvent) { events = append(events, ev) },
		sleepFn:    func(context.Context, time.Duration) bool { return true },
	}

	ctx := WithRetryObserver(context.Background(), func(_ context.Context, ev RetryEvent) {
		obsEvents = append(obsEvents, ev)
	})
	msg, err := rt.Complete(ctx, Request{})
	if err != nil || msg.Text() != "recovered" {
		t.Fatalf("unexpected result: msg=%+v err=%v", msg, err)
	}
	if len(inner.reqs) != 3 {
		t.Fatalf("calls = %d, want 3", len(inner.reqs))
	}
	if len(events) != 2 || len(obsEvents) != 2 {
		t.Fatalf("events=%d obs=%d, want 2 each", len(events), len(obsEvents))
	}
	if events[0].Kind != "retry" || events[0].Attempt != 1 || events[0].MaxAttempts != 2 || events[0].From != "custom/m1" {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
	if events[1].Attempt != 2 || events[1].MaxAttempts != 2 {
		t.Fatalf("unexpected second event: %+v", events[1])
	}
}

func TestRetryingRuntimeExhaustsAndAbortsBackoffPreservingUpstreamError(t *testing.T) {
	lastUpstream := errors.New("502 bad gateway")
	inner := &scriptedRuntime{
		label: "custom/m1",
		results: []scriptedStep{
			{err: io.ErrUnexpectedEOF},
			{err: lastUpstream},
		},
	}
	rt := &RetryingRuntime{
		Inner:      inner,
		MaxRetries: 1,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
		sleepFn:    func(context.Context, time.Duration) bool { return true },
	}
	_, err := rt.Complete(context.Background(), Request{})
	if !errors.Is(err, lastUpstream) {
		t.Fatalf("expected last upstream error %v, got %v", lastUpstream, err)
	}

	// Backoff cancelled mid-sleep must still return the upstream error rather than context.Canceled.
	upstream := errors.New("503 overloaded")
	inner2 := &scriptedRuntime{label: "custom/m1", results: []scriptedStep{{err: upstream}}}
	ctx, cancel := context.WithCancel(context.Background())
	rt2 := &RetryingRuntime{
		Inner:      inner2,
		MaxRetries: 3,
		BaseDelay:  5 * time.Second,
		MaxDelay:   15 * time.Second,
	}
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	_, err2 := rt2.Complete(ctx, Request{})
	if !errors.Is(err2, upstream) {
		t.Fatalf("cancelled backoff must preserve upstream error, got %v", err2)
	}
	if len(inner2.reqs) != 1 {
		t.Fatalf("must not attempt again after cancelled sleep, got %d calls", len(inner2.reqs))
	}
}

func TestWrapBudgetStopsChainAndPreservesUpstreamError(t *testing.T) {
	upstream := errors.New("503 service unavailable")
	primary := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{err: upstream}}}
	secondary := &scriptedRuntime{label: "p2/m2", results: []scriptedStep{{text: "should not run"}}}

	var events []RetryEvent
	onEv := func(ev RetryEvent) { events = append(events, ev) }

	chain := &FallbackRuntime{
		Chain: []Runtime{
			&RetryingRuntime{
				Inner:      primary,
				MaxRetries: 3,
				BaseDelay:  5 * time.Second,
				MaxDelay:   15 * time.Second,
				OnEvent:    onEv,
			},
			secondary,
		},
		OnEvent: onEv,
	}
	wrapped := WrapBudget(chain, 35*time.Millisecond, onEv)
	if wrapped.(Describer).Describe() != "chain(p1/m1>p2/m2)" {
		t.Fatalf("unexpected Describe(): %q", wrapped.(Describer).Describe())
	}

	_, err := wrapped.Complete(context.Background(), Request{})
	if !errors.Is(err, upstream) {
		t.Fatalf("budget exhaustion must return last real upstream error, got %v", err)
	}
	if len(secondary.reqs) != 0 {
		t.Fatalf("secondary provider must not be tried after budget expired, got %d calls", len(secondary.reqs))
	}
	var sawBudget bool
	for _, ev := range events {
		if ev.Kind == "give-up" {
			t.Fatalf("give-up must not be emitted alongside budget-exhausted: %+v", events)
		}
		if ev.Kind == "budget-exhausted" {
			sawBudget = true
			if ev.From != "chain(p1/m1>p2/m2)" {
				t.Fatalf("unexpected budget-exhausted From: %q", ev.From)
			}
		}
	}
	if !sawBudget {
		t.Fatalf("expected budget-exhausted event, got %+v", events)
	}

	// WrapBudget(<=0) returns inner untouched.
	if WrapBudget(primary, 0, nil) != primary {
		t.Fatal("zero budget must return inner unchanged")
	}

	// A nil context must not panic inside RetryingRuntime, FallbackRuntime, or budgetRuntime.
	nilInner := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{text: "ok"}}}
	nilWrapped := WrapBudget(&FallbackRuntime{
		Chain: []Runtime{&RetryingRuntime{Inner: nilInner, MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}},
	}, time.Second, nil)
	var nilCtx context.Context
	if msg, err := nilWrapped.Complete(nilCtx, Request{}); err != nil || msg.Text() != "ok" {
		t.Fatalf("nil ctx failed: msg=%+v err=%v", msg, err)
	}
}

func TestRetryingRuntimeBackoffNeverExceedsMaxDelay(t *testing.T) {
	rt := &RetryingRuntime{
		BaseDelay: time.Second,
		MaxDelay:  15 * time.Second,
	}
	for _, attempt := range []int{1, 2, 4, 5, 10, 62, 63, 100} {
		for i := 0; i < 200; i++ {
			d := rt.backoff(attempt)
			if d <= 0 || d > rt.MaxDelay {
				t.Fatalf("attempt=%d produced backoff %s outside (0, %s]", attempt, d, rt.MaxDelay)
			}
			if attempt >= 5 && d < rt.MaxDelay/2 {
				t.Fatalf("attempt=%d saturated backoff %s fell below 0.5*MaxDelay (%s)", attempt, d, rt.MaxDelay/2)
			}
		}
	}

	// BaseDelay == MaxDelay still respects MaxDelay on attempt 1.
	equalCap := &RetryingRuntime{BaseDelay: 500 * time.Millisecond, MaxDelay: 500 * time.Millisecond}
	for i := 0; i < 200; i++ {
		if d := equalCap.backoff(1); d > equalCap.MaxDelay {
			t.Fatalf("backoff(1) = %s exceeded MaxDelay %s", d, equalCap.MaxDelay)
		}
	}

	if (&RetryingRuntime{BaseDelay: 0, MaxDelay: time.Second}).backoff(1) != 0 {
		t.Fatal("zero BaseDelay must yield 0 backoff")
	}
	if (&RetryingRuntime{BaseDelay: time.Second, MaxDelay: 0}).backoff(1) != 0 {
		t.Fatal("zero MaxDelay must yield 0 backoff")
	}
}
