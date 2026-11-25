package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/push"
	"github.com/crossoverJie/botler-agent/internal/runner"
)

type recordingSender struct {
	mu    sync.Mutex
	texts []string
	delay time.Duration
	fail  error
	// blockUntilCtxDone blocks every Send until ctx is cancelled, recording cancellation.
	blockUntilCtxDone bool
	startedCh         chan struct{}
	cancelledCount    atomic.Int32
	ctxErrOnEntry     atomic.Value
}

func (r *recordingSender) Configured() bool { return true }

func (r *recordingSender) Send(ctx context.Context, _ string, p push.Payload, _ bool) error {
	if err := ctx.Err(); err != nil {
		r.ctxErrOnEntry.Store(err)
	}
	if r.startedCh != nil {
		select {
		case r.startedCh <- struct{}{}:
		default:
		}
	}
	if r.blockUntilCtxDone {
		<-ctx.Done()
		r.cancelledCount.Add(1)
		return ctx.Err()
	}
	if r.delay > 0 {
		t := time.NewTimer(r.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			r.cancelledCount.Add(1)
			return ctx.Err()
		}
	}
	if r.fail != nil {
		return r.fail
	}
	r.mu.Lock()
	r.texts = append(r.texts, p.Text)
	r.mu.Unlock()
	return nil
}

func (r *recordingSender) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.texts))
	copy(out, r.texts)
	return out
}

func withWeChatSender(t *testing.T, s push.Sender) {
	t.Helper()
	saved, had := push.SenderFor(push.SourceWeChat)
	push.RegisterSender(push.SourceWeChat, s)
	t.Cleanup(func() {
		if had {
			push.RegisterSender(push.SourceWeChat, saved)
		}
	})
}

// observerEmittingRuntime fires RetryEvents via RetryObserverFrom(ctx) during Complete.
type observerEmittingRuntime struct {
	events []agentrt.RetryEvent
	reply  string
}

func (o *observerEmittingRuntime) Complete(ctx context.Context, _ agentrt.Request) (*agentrt.Message, error) {
	if obs := agentrt.RetryObserverFrom(ctx); obs != nil {
		for _, ev := range o.events {
			obs(ctx, ev)
		}
	}
	return &agentrt.Message{
		Role:       agentrt.RoleAssistant,
		StopReason: agentrt.StopStop,
		Parts:      []agentrt.Part{{Type: agentrt.PartText, Text: o.reply}},
		Usage:      &agentrt.Usage{},
	}, nil
}

func TestDispatchNoticeSourceGateAndModes(t *testing.T) {
	setup(t, 20)
	sender := &recordingSender{}
	withWeChatSender(t, sender)

	rt := &observerEmittingRuntime{
		events: []agentrt.RetryEvent{
			{Kind: "retry", From: "custom/m1", Attempt: 1, MaxAttempts: 2, Err: errors.New("eof")},
			{Kind: "retry", From: "custom/m1", Attempt: 2, MaxAttempts: 2, Err: errors.New("eof")},
			{Kind: "fallback", From: "custom/m1", To: "ark/ark-code-latest", Err: errors.New("eof")},
		},
		reply: "final answer",
	}
	runner.SetRuntimeForTest(rt)
	rec := &push.Recipient{Source: push.SourceWeChat, UserID: "wx-user"}

	// CLI and scheduler sources must emit ZERO notices even when Recipient is provided.
	config.C().RetryNotify = config.RetryNotifyAll
	Dispatch(context.Background(), "记一下", Options{Source: "cli", Recipient: rec})
	Dispatch(context.Background(), "记一下", Options{Source: "scheduler", Recipient: rec})
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("cli/scheduler must never send notices, got %v", got)
	}

	// Off mode installs no notifier at all.
	config.C().RetryNotify = config.RetryNotifyOff
	if newNotifier(context.Background(), *rec, config.RetryNotifyOff) != nil {
		t.Fatal("off mode must return nil notifier")
	}
	Dispatch(context.Background(), "记一下", Options{Source: "wechat", Recipient: rec, SessionKey: conversation.IMSessionKey})
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("off mode must send 0 notices, got %v", got)
	}

	// Default "fallback" mode announces only provider switches.
	config.C().RetryNotify = config.RetryNotifyFallback
	res := Dispatch(context.Background(), "记一下", Options{Source: "wechat", Recipient: rec, SessionKey: conversation.IMSessionKey})
	if res.Text != "final answer" {
		t.Fatalf("Result.Text must stay untouched, got %q", res.Text)
	}
	got := sender.snapshot()
	if len(got) != 1 || !strings.Contains(got[0], "ark/ark-code-latest") {
		t.Fatalf("fallback mode expected 1 switch notice, got %v", got)
	}

	// "all" mode announces the FIRST retry per provider (deduping the 2nd) + every fallback, in FIFO order.
	sender.texts = nil
	config.C().RetryNotify = config.RetryNotifyAll
	Dispatch(context.Background(), "记一下", Options{Source: "wechat", Recipient: rec, SessionKey: conversation.IMSessionKey})
	got = sender.snapshot()
	if len(got) != 2 {
		t.Fatalf("all mode expected 2 notices (1 deduped retry + 1 fallback), got %v", got)
	}
	if !strings.Contains(got[0], "第 1/2 次") || !strings.Contains(got[1], "ark/ark-code-latest") {
		t.Fatalf("unexpected FIFO order or copy: %v", got)
	}
}

func TestNotifierCapLateEventFailureAndParentCancelSurvival(t *testing.T) {
	rec := push.Recipient{Source: push.SourceWeChat, UserID: "wx-1"}

	// 1. Cap at maxNoticesPerTask (3) + FIFO ordering under slow sends + late event after close() does not panic.
	sender := &recordingSender{delay: 15 * time.Millisecond}
	withWeChatSender(t, sender)

	n := newNotifier(context.Background(), rec, config.RetryNotifyAll)
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "retry", From: "p1/m1", Attempt: 1, MaxAttempts: 2})
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p1/m1", To: "p2/m2"})
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "retry", From: "p2/m2", Attempt: 1, MaxAttempts: 2})
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p2/m2", To: "p3/m3"}) // 4th -> dropped by cap
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "give-up", From: "p3/m3"})               // ignored
	n.close()

	// Late event after close() must be dropped cleanly without panicking on closed channel.
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p3/m3", To: "p4/m4"})

	got := sender.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected cap at 3 notices, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "正在重试") || !strings.Contains(got[1], "p2/m2") || !strings.Contains(got[2], "正在重试") {
		t.Fatalf("FIFO order violated: %v", got)
	}

	// 2. Send failure only warns and never aborts.
	var warns []string
	var warnMu sync.Mutex
	failSender := &recordingSender{fail: errors.New("channel broken")}
	withWeChatSender(t, failSender)
	nFail := newNotifier(context.Background(), rec, config.RetryNotifyFallback)
	nFail.warnFn = func(s string) {
		warnMu.Lock()
		warns = append(warns, s)
		warnMu.Unlock()
	}
	nFail.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p1/m1", To: "p2/m2"})
	nFail.close()
	if len(warns) != 1 || !strings.Contains(warns[0], "retry notice failed: channel broken") {
		t.Fatalf("expected retry notice failed warning, got %v", warns)
	}

	// 3. Parent task ctx cancelled right after observe() still delivers queued notice via detached base ctx.
	gate := make(chan struct{})
	okSender := &recordingSender{startedCh: make(chan struct{}, 1)}
	withWeChatSender(t, okSender)
	taskCtx, taskCancel := context.WithCancel(context.Background())
	nDetached := newNotifier(taskCtx, rec, config.RetryNotifyFallback)
	taskCancel() // cancel task ctx BEFORE observe/send!
	close(gate)
	_ = gate
	nDetached.observe(taskCtx, agentrt.RetryEvent{Kind: "fallback", From: "p1/m1", To: "p2/m2"})
	nDetached.close()
	if len(okSender.snapshot()) != 1 {
		t.Fatalf("detached base ctx must still deliver notice after parent ctx cancellation, got %v", okSender.snapshot())
	}
	if v := okSender.ctxErrOnEntry.Load(); v != nil {
		t.Fatalf("sender received an already-cancelled ctx: %v", v)
	}
}

func TestNotifierDrainBoundAbortsBlockedAndSlowSenders(t *testing.T) {
	rec := push.Recipient{Source: push.SourceWeChat, UserID: "wx-1"}

	// Case 1 (r4): permanently blocked sender returns within notifyDrainTimeout (~2s), cancels in-flight ctx,
	// drops remaining queued notices, and logs the "dropped" diagnostic line.
	blocked := &recordingSender{blockUntilCtxDone: true, startedCh: make(chan struct{}, 1)}
	withWeChatSender(t, blocked)

	var warns []string
	var warnMu sync.Mutex
	n := newNotifier(context.Background(), rec, config.RetryNotifyAll)
	n.warnFn = func(s string) {
		warnMu.Lock()
		warns = append(warns, s)
		warnMu.Unlock()
	}
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "retry", From: "p1/m1", Attempt: 1, MaxAttempts: 2})
	n.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p1/m1", To: "p2/m2"})
	<-blocked.startedCh

	start := time.Now()
	n.close()
	elapsed := time.Since(start)
	if elapsed < notifyDrainTimeout-200*time.Millisecond || elapsed > notifyDrainTimeout+400*time.Millisecond {
		t.Fatalf("close() elapsed = %s, want ~%s", elapsed, notifyDrainTimeout)
	}
	if blocked.cancelledCount.Load() != 1 {
		t.Fatalf("expected in-flight send to receive ctx cancellation once (2nd queued dropped without sending), got %d", blocked.cancelledCount.Load())
	}
	warnMu.Lock()
	gotWarns := append([]string{}, warns...)
	warnMu.Unlock()
	if len(gotWarns) != 1 || !strings.Contains(gotWarns[0], "retry notices dropped: drain exceeded 2s") {
		t.Fatalf("expected single dropped warning (no extra 'failed' warning), got %v", gotWarns)
	}

	// Case 2 (r4): 3 notices × 1.5s slow channel still finishes at the fixed 2s bound rather than 4.5s.
	slow := &recordingSender{delay: 1500 * time.Millisecond, startedCh: make(chan struct{}, 1)}
	withWeChatSender(t, slow)
	nSlow := newNotifier(context.Background(), rec, config.RetryNotifyAll)
	nSlow.warnFn = func(string) {}
	nSlow.observe(context.Background(), agentrt.RetryEvent{Kind: "retry", From: "p1/m1", Attempt: 1, MaxAttempts: 2})
	nSlow.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p1/m1", To: "p2/m2"})
	nSlow.observe(context.Background(), agentrt.RetryEvent{Kind: "fallback", From: "p2/m2", To: "p3/m3"})
	<-slow.startedCh

	startSlow := time.Now()
	nSlow.close()
	elapsedSlow := time.Since(startSlow)
	if elapsedSlow > notifyDrainTimeout+400*time.Millisecond {
		t.Fatalf("3 slow notices must still obey fixed %s drain bound, took %s", notifyDrainTimeout, elapsedSlow)
	}
	if got := len(slow.snapshot()); got != 1 {
		t.Fatalf("expected 1 completed send and remaining dropped at 2s bound, got %d", got)
	}
}
