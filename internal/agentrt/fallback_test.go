package agentrt

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func TestFallbackRuntimePrimarySuccessTouchesNoBackup(t *testing.T) {
	p1 := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{text: "from-p1"}}}
	p2 := &scriptedRuntime{label: "p2/m2", results: []scriptedStep{{text: "from-p2"}}}
	var events []RetryEvent
	fb := &FallbackRuntime{Chain: []Runtime{p1, p2}, OnEvent: func(ev RetryEvent) { events = append(events, ev) }}

	msg, err := fb.Complete(context.Background(), Request{})
	if err != nil || msg.Text() != "from-p1" {
		t.Fatalf("unexpected result: %+v %v", msg, err)
	}
	if len(p2.reqs) != 0 || len(events) != 0 {
		t.Fatalf("backup must not be touched: p2=%d events=%+v", len(p2.reqs), events)
	}
}

func TestFallbackRuntimeFatalStopsWithoutConsumingBackup(t *testing.T) {
	badReq := &openai.Error{StatusCode: 400, Message: "invalid schema"}
	p1 := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{err: badReq}}}
	p2 := &scriptedRuntime{label: "p2/m2", results: []scriptedStep{{text: "from-p2"}}}
	var events []RetryEvent
	fb := &FallbackRuntime{Chain: []Runtime{p1, p2}, OnEvent: func(ev RetryEvent) { events = append(events, ev) }}

	_, err := fb.Complete(context.Background(), Request{})
	if !errors.Is(err, badReq) {
		t.Fatalf("expected 400 error, got %v", err)
	}
	if len(p2.reqs) != 0 {
		t.Fatalf("400 must not fall back to p2, got %d calls", len(p2.reqs))
	}
	if len(events) != 1 || events[0].Kind != "give-up" || events[0].From != "p1/m1" {
		t.Fatalf("expected single give-up at p1/m1, got %+v", events)
	}
}

func TestFallbackRuntimeExhaustsRetriesPerMemberAndStripsThinking(t *testing.T) {
	p1Raw := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{err: io.ErrUnexpectedEOF}}}
	p2Raw := &scriptedRuntime{label: "p2/m2", results: []scriptedStep{
		{err: io.ErrUnexpectedEOF},
		{text: "backup-ok"},
	}}
	var events []RetryEvent
	onEv := func(ev RetryEvent) { events = append(events, ev) }
	noSleep := func(context.Context, time.Duration) bool { return true }

	fb := &FallbackRuntime{
		Chain: []Runtime{
			&RetryingRuntime{Inner: p1Raw, MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, OnEvent: onEv, sleepFn: noSleep},
			&RetryingRuntime{Inner: p2Raw, MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, OnEvent: onEv, sleepFn: noSleep},
		},
		OnEvent: onEv,
	}

	req := Request{
		Messages: []Message{
			{Role: RoleAssistant, Parts: []Part{
				{Type: PartThinking, Text: "secret thought", Signature: []byte("sig")},
				{Type: PartText, Text: "visible text"},
			}},
		},
	}
	msg, err := fb.Complete(context.Background(), req)
	if err != nil || msg.Text() != "backup-ok" {
		t.Fatalf("unexpected result: %+v %v", msg, err)
	}
	// Primary used all 1 + MaxRetries = 3 attempts before switching.
	if len(p1Raw.reqs) != 3 {
		t.Fatalf("primary calls = %d, want 3", len(p1Raw.reqs))
	}
	// Primary kept PartThinking; secondary received stripped history.
	if len(p1Raw.reqs[0].Messages[0].Parts) != 2 {
		t.Fatalf("primary request should retain thinking, got %+v", p1Raw.reqs[0].Messages[0].Parts)
	}
	if len(p2Raw.reqs) != 2 {
		t.Fatalf("secondary calls = %d, want 2", len(p2Raw.reqs))
	}
	for _, part := range p2Raw.reqs[0].Messages[0].Parts {
		if part.Type == PartThinking {
			t.Fatalf("secondary request must have PartThinking stripped: %+v", p2Raw.reqs[0].Messages[0].Parts)
		}
	}
	// Original caller slice was not mutated in place.
	if len(req.Messages[0].Parts) != 2 {
		t.Fatal("stripThinking mutated caller's Request")
	}
}

func TestFallbackRuntimeSingleElementGiveUpAndDescribe(t *testing.T) {
	lastErr := errors.New("503 upstream down")
	only := &scriptedRuntime{label: "custom/only-model", results: []scriptedStep{{err: lastErr}}}
	var events []RetryEvent
	fb := &FallbackRuntime{
		Chain:   []Runtime{only},
		OnEvent: func(ev RetryEvent) { events = append(events, ev) },
	}
	if got := fb.Describe(); got != "custom/only-model" {
		t.Fatalf("single-provider Describe() must not wrap with chain(...), got %q", got)
	}
	_, err := fb.Complete(context.Background(), Request{})
	if !errors.Is(err, lastErr) {
		t.Fatalf("expected %v, got %v", lastErr, err)
	}
	if len(events) != 1 || events[0].Kind != "give-up" || events[0].From != "custom/only-model" {
		t.Fatalf("single-element chain must still emit exactly one give-up, got %+v", events)
	}
}

func TestFallbackRuntimeAllMembersFailReturnsLastError(t *testing.T) {
	err1 := &openai.Error{StatusCode: 401}
	err2 := errors.New("502 second gateway down")
	p1 := &scriptedRuntime{label: "p1/m1", results: []scriptedStep{{err: err1}}}
	p2 := &scriptedRuntime{label: "p2/m2", results: []scriptedStep{{err: err2}}}
	var events []RetryEvent
	fb := &FallbackRuntime{Chain: []Runtime{p1, p2}, OnEvent: func(ev RetryEvent) { events = append(events, ev) }}

	_, err := fb.Complete(context.Background(), Request{})
	if !errors.Is(err, err2) {
		t.Fatalf("expected last error %v, got %v", err2, err)
	}
	if len(events) != 2 || events[0].Kind != "fallback" || events[1].Kind != "give-up" || events[1].From != "p2/m2" {
		t.Fatalf("unexpected event sequence: %+v", events)
	}
}
