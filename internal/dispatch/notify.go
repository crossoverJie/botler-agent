package dispatch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// maxNoticesPerTask caps user-facing retry/fallback hints per task and sizes the FIFO buffer so
// observe() never blocks the retry loop.
const maxNoticesPerTask = 3

// notifyDrainTimeout is the fixed upper bound on how long close() may delay the final reply,
// regardless of how many notices are queued or how long the provider chain is.
const notifyDrainTimeout = 2 * time.Second

// isInteractiveSource reports whether a human is actively waiting on an IM channel for this reply.
// Hardcoded to "wechat"/"telegram" on purpose: scheduler entries also carry a Recipient in
// schedules.json, and background tasks must never emit transient "retrying..." noise.
func isInteractiveSource(src string) bool {
	return src == "wechat" || src == "telegram"
}

type notice struct {
	text string
}

type notifier struct {
	r       push.Recipient
	mode    config.RetryNotifyMode
	retried map[string]bool
	sent    int
	closed  bool
	mu      sync.Mutex

	// base is detached from the task's cancellation (WithoutCancel) so a notice the user is owed
	// still goes out when the task hits its retry budget, while close() retains cancel to abort
	// an in-flight HTTP request if draining exceeds notifyDrainTimeout.
	base   context.Context
	cancel context.CancelFunc

	queue  chan notice
	done   chan struct{}
	warnFn func(string)
}

func newNotifier(ctx context.Context, r push.Recipient, mode config.RetryNotifyMode) *notifier {
	if mode == config.RetryNotifyOff {
		return nil
	}
	if mode == "" {
		mode = config.RetryNotifyFallback
	}
	if ctx == nil {
		ctx = context.Background()
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &notifier{
		r:       r,
		mode:    mode,
		retried: map[string]bool{},
		base:    base,
		cancel:  cancel,
		queue:   make(chan notice, maxNoticesPerTask),
		done:    make(chan struct{}),
	}
	go n.sender()
	return n
}

func (n *notifier) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if n.warnFn != nil {
		n.warnFn(msg)
		return
	}
	fmt.Fprintln(os.Stderr, msg)
}

func (n *notifier) sender() {
	defer close(n.done)
	for m := range n.queue {
		if n.base.Err() != nil {
			break
		}
		if err := push.Notify(n.base, n.r, m.text); err != nil {
			if n.base.Err() != nil {
				break
			}
			n.warnf("[dispatch] retry notice failed: %v", err)
		}
	}
}

func (n *notifier) observe(_ context.Context, ev agentrt.RetryEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	text := n.renderLocked(ev)
	if text == "" {
		return
	}
	n.queue <- notice{text: text}
}

func (n *notifier) close() {
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue)
	}
	n.mu.Unlock()

	t := time.NewTimer(notifyDrainTimeout)
	defer t.Stop()
	select {
	case <-n.done:
	case <-t.C:
		n.cancel()
		<-n.done
		n.warnf("[dispatch] retry notices dropped: drain exceeded %s", notifyDrainTimeout)
	}
	n.cancel()
}

func (n *notifier) renderLocked(ev agentrt.RetryEvent) string {
	if n.sent >= maxNoticesPerTask {
		return ""
	}
	switch ev.Kind {
	case "retry":
		if n.mode != config.RetryNotifyAll {
			return ""
		}
		if n.retried[ev.From] {
			return ""
		}
		n.retried[ev.From] = true
	case "fallback":
		// Every provider switch is announced so the user knows another model answered.
	default:
		return ""
	}
	n.sent++
	if ev.Kind == "retry" {
		return fmt.Sprintf("⏳ 模型响应异常，正在重试（第 %d/%d 次），请稍候…", ev.Attempt, ev.MaxAttempts)
	}
	return fmt.Sprintf("🔄 主模型不可用，已切换到备用模型（%s），请稍候…", ev.To)
}
