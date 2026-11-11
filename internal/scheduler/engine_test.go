package scheduler

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/push"
)

func stubDispatch(t *testing.T, messageFn FireFunc, scriptFn ScriptFireFunc) {
	t.Helper()
	SetDispatch(messageFn, scriptFn)
	t.Cleanup(func() { SetDispatch(nil, nil) })
}

func TestFireWithoutWiringDoesNotPanic(t *testing.T) {
	newTestConfig(t)
	SetDispatch(nil, nil)
	e := &Engine{lastFire: map[string]int64{}}
	// Should warn and return without firing or panicking.
	e.fire(context.Background(), Schedule{ID: "x", Enabled: true, Timezone: "UTC", Message: "m", At: "08:00"}, 0)
	if e.lastFireID != "" {
		t.Fatalf("an unwired engine must not record a fire: %q", e.lastFireID)
	}
}

func TestFireUnknownProjectUsesMessageAsReminder(t *testing.T) {
	newTestConfig(t)
	stubDispatch(t, func(_ context.Context, _, _, _ string) FireResult {
		return FireResult{Text: "😅 routing failed", Status: "unknown-project"}
	}, nil)

	var delivered atomic.Value
	push.RegisterSender(push.SourceTelegram, fakeSender{onSend: func(address string, p push.Payload) {
		delivered.Store(address + "|" + p.Text)
	}})

	e := &Engine{lastFire: map[string]int64{}}
	entry := Schedule{ID: "remind", Enabled: true, Timezone: "UTC", Message: "该喝水了",
		At: "08:00", Recipient: &push.Recipient{Source: push.SourceTelegram, UserID: "42"}}
	e.fire(context.Background(), entry, 0)

	got, _ := delivered.Load().(string)
	if got != "42|该喝水了" {
		t.Fatalf("delivered = %q, want the entry message as the reminder text", got)
	}
	if e.lastFireID != "remind" {
		t.Fatalf("lastFireID = %q", e.lastFireID)
	}
}

func TestFireScriptValidationFailedIsNotRetried(t *testing.T) {
	newTestConfig(t)
	var calls atomic.Int32
	stubDispatch(t, nil, func(_ context.Context, _, _ string, _ []string, _ *bool) FireResult {
		calls.Add(1)
		return FireResult{Text: "bad json", Status: "validation-failed"}
	})

	e := &Engine{lastFire: map[string]int64{}}
	entry := Schedule{ID: "script1", Enabled: true, Timezone: "UTC", Script: "notes/scripts/build.py",
		At: "02:00", Retry: &Retry{Max: 2, BackoffMs: 0}}
	e.fire(context.Background(), entry, 0)
	time.Sleep(100 * time.Millisecond) // give a (wrong) retry the chance to fire
	if got := calls.Load(); got != 1 {
		t.Fatalf("script entry fired %d times, want 1 (validation-failed must not retry scripts)", got)
	}
}

func TestFireMessageValidationFailedRetries(t *testing.T) {
	newTestConfig(t)
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	stubDispatch(t, func(_ context.Context, _, _, _ string) FireResult {
		if calls.Add(1) == 1 {
			return FireResult{Text: "bad json", Status: "validation-failed"}
		}
		return FireResult{Text: "ok", Status: "success"}
	}, nil)
	// The retry path re-reads schedules.json to verify the entry still exists and is unchanged.
	if _, err := SaveSchedules([]Schedule{{ID: "msg1", Enabled: true, Timezone: "UTC", Message: "m",
		At: "08:00", Retry: &Retry{Max: 1, BackoffMs: 0}}}); err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}

	e := &Engine{lastFire: map[string]int64{}}
	entry, ok := FindSchedule("msg1")
	if !ok {
		t.Fatal("entry not found after save")
	}
	e.fire(context.Background(), entry, 0)
	go func() {
		for calls.Load() < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("message entry did not retry (calls = %d)", calls.Load())
	}
}

func TestStartIsNoOpWhenDisabled(t *testing.T) {
	c := newTestConfig(t)
	c.SchedulerEnabled = false
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if Current() != nil {
		t.Fatal("a disabled scheduler must not create an engine")
	}
}

func TestEngineFiresDueOnceEntry(t *testing.T) {
	c := newTestConfig(t)
	c.SchedulerEnabled = true
	// An unreachable calendar URL keeps the background refresh instantaneous instead of hitting the net.
	c.HolidayAPIURL = "http://127.0.0.1:1/{year}"
	year := time.Now().Year()
	if err := SaveHolidays(HolidayData{
		strconv.Itoa(year):     {Holidays: []string{}, Workdays: []string{}},
		strconv.Itoa(year + 1): {Holidays: []string{}, Workdays: []string{}},
	}); err != nil {
		t.Fatalf("SaveHolidays: %v", err)
	}
	// `once` has second granularity, so anchor it a couple of seconds out (still in the future of
	// the watermark the first loop round records).
	fireAt := time.Now().Add(2 * time.Second).Truncate(time.Second).UTC().Format(time.RFC3339)
	if _, err := SaveSchedules([]Schedule{{ID: "once1", Enabled: true, Timezone: "UTC",
		Message: "fired", Once: fireAt}}); err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}
	fired := make(chan string, 1)
	stubDispatch(t, func(_ context.Context, _, message, _ string) FireResult {
		select {
		case fired <- message:
		default:
		}
		return FireResult{Text: "ok", Status: "success"}
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if e := Current(); e != nil {
			e.Stop()
		}
	})
	select {
	case msg := <-fired:
		if msg != "fired" {
			t.Fatalf("fired message = %q", msg)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the once entry did not fire within 6s")
	}
}

// fakeSender is a push.Sender used to observe delivery attempts.
type fakeSender struct {
	onSend func(address string, p push.Payload)
}

func (f fakeSender) Configured() bool { return true }

func (f fakeSender) Send(address string, p push.Payload, _ bool) error {
	if f.onSend != nil {
		f.onSend(address, p)
	}
	return nil
}
