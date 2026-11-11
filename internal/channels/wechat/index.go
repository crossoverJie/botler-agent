package wechat

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	statsmon "github.com/crossoverJie/botler-agent/internal/monitor"
)

var (
	monitorMu      sync.Mutex
	monitorRunning bool
	monitorCancel  context.CancelFunc
)

// MonitorRunning reports whether the long-poll monitor goroutine is running.
func MonitorRunning() bool {
	monitorMu.Lock()
	defer monitorMu.Unlock()
	return monitorRunning
}

// Start starts the WeChat long-poll monitor (fire-and-forget) and the renewal reminder loop.
// It requires WECHAT_ENABLED=1 and a completed QR login.
func Start(ctx context.Context) error {
	if !config.C().WeChatEnabled {
		return errors.New("Starting WeChat requires WECHAT_ENABLED=1")
	}
	if !ResolveAccount().Configured {
		return errors.New("WeChat account not logged in; please run: botler wechat-login")
	}
	startMonitor(ctx)
	logf("[wechat] long polling listening")
	return nil
}

// EnsureMonitor brings the monitor (and reminder loop) online after a late pair, e.g. a WebUI
// re-pair when the process booted WeChat-disabled/unconfigured. Idempotent.
func EnsureMonitor(ctx context.Context) {
	if !config.C().WeChatEnabled {
		return
	}
	if !ResolveAccount().Configured {
		return
	}
	if !MonitorRunning() {
		startMonitor(ctx)
		logf("[wechat] long polling listening (started after web pair)")
	}
	StartReminderLoop(ctx)
}

func startMonitor(parent context.Context) {
	monitorMu.Lock()
	if monitorRunning {
		monitorMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	monitorCancel = cancel
	monitorRunning = true
	monitorMu.Unlock()

	statsmon.SetChannelUp("wechat")
	go func() {
		defer func() {
			monitorMu.Lock()
			monitorRunning = false
			monitorMu.Unlock()
		}()
		if err := RunMonitor(ctx); err != nil && !errors.Is(err, context.Canceled) {
			warnf("[wechat] monitor exited: %v", err)
		}
	}()
	StartReminderLoop(ctx)
}

// Stop stops the long-poll monitor and the reminder loop.
func Stop() {
	monitorMu.Lock()
	cancel := monitorCancel
	monitorCancel = nil
	monitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Logout removes stored credentials, the sync cursor, the cached context tokens and this channel's
// push contact addresses.
func Logout() { ClearAccount() }

// StatusInfo is the read-only WeChat status shown by the WebUI (GET /api/config → wechat).
type StatusInfo struct {
	Configured          bool
	Owner               *string
	LoginAt             *int64
	LoginAgeMs          *int64
	ThresholdHours      int
	ReminderEnabled     bool
	ReminderLoopStarted bool
	ReminderTickMs      int64
	LastTickAt          *int64
	NextTickAt          *int64
	OwnerHasContext     bool
	LastMsgAt           *int64
	QuietMs             *int64
	LastRemindedAt      *int64
	RemindedThisStretch bool
	NextReminderAt      *int64
}

// Status builds the read-only WebUI status snapshot (never writes anything).
func Status() StatusInfo {
	now := nowMs()
	resolved := ResolveAccount()
	account := LoadAccount()
	owner := resolved.UserID
	var entry ContextEntry
	hasContext := false
	if owner != "" {
		entry, hasContext = GetContext(owner)
	}
	threshold := config.C().WeChatReminderHours
	reminder := ReminderStatus()

	out := StatusInfo{
		Configured:          resolved.Configured,
		ThresholdHours:      threshold,
		ReminderEnabled:     threshold >= 1,
		ReminderLoopStarted: reminder.Started,
		ReminderTickMs:      reminder.TickMs,
		LastTickAt:          reminder.LastTickAt,
		NextTickAt:          reminder.NextTickAt,
		OwnerHasContext:     hasContext,
	}
	if owner != "" {
		out.Owner = &owner
	}
	if account != nil && account.SavedAt != "" {
		if ts, err := time.Parse(time.RFC3339, account.SavedAt); err == nil {
			ms := ts.UnixMilli()
			age := now - ms
			out.LoginAt = &ms
			out.LoginAgeMs = &age
		}
	}
	if hasContext {
		lastMsgAt := entry.LastMsgAt
		quiet := now - entry.LastMsgAt
		next := entry.LastMsgAt + int64(threshold)*3600*1000
		out.LastMsgAt = &lastMsgAt
		out.QuietMs = &quiet
		out.LastRemindedAt = entry.LastRemindedAt
		out.RemindedThisStretch = entry.LastRemindedAt != nil && *entry.LastRemindedAt >= entry.LastMsgAt
		out.NextReminderAt = &next
	}
	return out
}
