package wechat

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// The WeChat context_token is tied to a rolling 24h window; each inbound message refreshes it.
// This loop checks ONLY the account owner: if they have been quiet for WECHAT_REMINDER_HOURS and
// this quiet stretch hasn't been reminded yet, it sends a reminder (primary WeChat channel,
// falling back to other channels via push.Deliver). It must not spam: lastRemindedAt is updated
// whether the send succeeded or failed, so each quiet stretch is reminded at most once.

// ReminderTickMs is the renewal-reminder check interval (exported for the WebUI status panel).
const ReminderTickMs int64 = 10 * 60 * 1000

// ReminderState is the process-local reminder loop state (read-only, for the WebUI).
type ReminderState struct {
	Started    bool   `json:"started"`
	TickMs     int64  `json:"tickMs"`
	LastTickAt *int64 `json:"lastTickAt"`
	NextTickAt *int64 `json:"nextTickAt"`
}

var (
	reminderMu      sync.Mutex
	reminderStarted bool
	reminderLastAt  *int64
	reminderNextAt  *int64
)

// ReminderStatus returns the current renewal-reminder loop state.
func ReminderStatus() ReminderState {
	reminderMu.Lock()
	defer reminderMu.Unlock()
	return ReminderState{
		Started:    reminderStarted,
		TickMs:     ReminderTickMs,
		LastTickAt: reminderLastAt,
		NextTickAt: reminderNextAt,
	}
}

// StartReminderLoop starts the renewal reminder loop (idempotent). No-op unless
// WECHAT_REMINDER_HOURS >= 1.
func StartReminderLoop(ctx context.Context) {
	hours := config.C().WeChatReminderHours
	if hours < 1 {
		return
	}
	reminderMu.Lock()
	if reminderStarted {
		reminderMu.Unlock()
		return
	}
	reminderStarted = true
	reminderMu.Unlock()

	logf("[wechat-reminder] started (threshold %dh, tick %dmin)", hours, ReminderTickMs/60_000)
	go reminderLoop(ctx)
}

func reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(ReminderTickMs) * time.Millisecond)
	defer ticker.Stop()
	reminderTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			setNextTick()
			reminderTick(ctx)
		}
	}
}

func setNextTick() {
	next := nowMs() + ReminderTickMs
	reminderMu.Lock()
	reminderNextAt = &next
	reminderMu.Unlock()
}

// reminderRunning guards against overlapping ticks (a slow deliver must not stack).
var reminderRunning sync.Mutex

func reminderTick(ctx context.Context) {
	if !reminderRunning.TryLock() {
		return
	}
	defer reminderRunning.Unlock()

	last := nowMs()
	reminderMu.Lock()
	reminderLastAt = &last
	reminderMu.Unlock()

	hours := config.C().WeChatReminderHours
	if hours < 1 {
		return // disabled
	}
	owner := ResolveAccount().UserID
	if owner == "" {
		return
	}
	entry, ok := GetContext(owner)
	if !ok {
		return // owner never messaged the bot (no context yet) — nothing to renew
	}
	now := nowMs()
	quiet := now - entry.LastMsgAt
	if quiet < int64(hours)*3600*1000 {
		return // still inside the window
	}
	if entry.LastRemindedAt != nil && *entry.LastRemindedAt >= entry.LastMsgAt {
		return // this quiet stretch was already nudged
	}
	res := push.Deliver(
		push.Payload{Text: ReminderText(float64(quiet) / 3600e3)},
		push.Recipient{Source: push.SourceWeChat, UserID: owner},
	)
	if !res.OK {
		warnf("[wechat-reminder] deliver failed: %s", res.Error)
	}
	// Record the attempt regardless of outcome so we don't retry-spam on the same quiet stretch.
	MarkReminded(owner)
	_ = ctx
}

// ReminderText renders the reminder copy; values are defensively clamped (negative hours / over
// 24 → remaining hours = 0).
func ReminderText(elapsedHours float64) string {
	h := int(math.Max(0, math.Round(elapsedHours)))
	remaining := 24 - h
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("[System reminder] Your bot has not received a message for over %d hours; the session expires in about %d hours."+
		"Please reply with any message in WeChat to refresh the 24-hour window.", h, remaining)
}
