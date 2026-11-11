package webui

import (
	"context"
	"sync"
)

// WeChatBackend is the WeChat surface the WebUI needs (pairing + the read-only status panel).
//
// Like SchedulesBackend it is injected by the wiring layer so this package never imports the
// channel runtime (which pulls in the dispatcher): the WebUI keeps compiling and testing on its
// own, and the JSON shapes stay the ones the UI already understands.
type WeChatBackend struct {
	// Status renders the read-only WeChat panel; use BuildWeChatStatus for the v1 shape.
	Status func() any
	// StartPairing starts a pairing session and renders the QR data URL.
	StartPairing func(ctx context.Context) PairStartResult
	// PairStatus returns the v1 pairing status shape ({state} when idle).
	PairStatus func() any
	// CancelPairing aborts the current pairing session.
	CancelPairing func()
}

// PairStartResult is the outcome of starting a pairing session. State "error" means the session
// failed (Error carries the message); otherwise QRImage is the data URL to display.
type PairStartResult struct {
	State   string
	QRImage string
	Error   string
}

// WeChatAccount is the account part of the status panel.
type WeChatAccount struct {
	Configured bool
	Owner      string
	// LoginAtMs is the parsed account savedAt (nil when unknown).
	LoginAtMs *int64
}

// WeChatReminder is the renewal-reminder loop state.
type WeChatReminder struct {
	Started    bool
	TickMs     int64
	LastTickAt *int64
	NextTickAt *int64
}

// WeChatContext is the owner's persisted context entry.
type WeChatContext struct {
	LastMsgAt      int64
	LastRemindedAt *int64
}

// BuildWeChatStatus renders the WebUI's WeChat panel exactly as v1 does (keys, nullability and
// derived booleans included). All inputs are plain values, so the logic is unit-testable without
// a WeChat account on disk.
func BuildWeChatStatus(nowMs int64, acct WeChatAccount, thresholdHours int, rem WeChatReminder, ctx *WeChatContext) map[string]any {
	var owner any
	if acct.Owner != "" {
		owner = acct.Owner
	}
	var loginAgeMs *int64
	if acct.LoginAtMs != nil {
		v := nowMs - *acct.LoginAtMs
		loginAgeMs = &v
	}
	out := map[string]any{
		"configured":          acct.Configured,
		"owner":               owner,
		"loginAt":             acct.LoginAtMs,
		"loginAgeMs":          loginAgeMs,
		"thresholdHours":      thresholdHours,
		"reminderEnabled":     thresholdHours >= 1,
		"reminderLoopStarted": rem.Started,
		"reminderTickMs":      rem.TickMs,
		"lastTickAt":          rem.LastTickAt,
		"nextTickAt":          rem.NextTickAt,
		"ownerHasContext":     false,
		"lastMsgAt":           nil,
		"quietMs":             nil,
		"lastRemindedAt":      nil,
		"remindedThisStretch": false,
		"nextReminderAt":      nil,
	}
	if ctx == nil {
		return out
	}
	quiet := nowMs - ctx.LastMsgAt
	nextReminder := ctx.LastMsgAt + int64(thresholdHours)*3600e3
	out["ownerHasContext"] = true
	out["lastMsgAt"] = ctx.LastMsgAt
	out["quietMs"] = quiet
	out["lastRemindedAt"] = ctx.LastRemindedAt
	out["remindedThisStretch"] = ctx.LastRemindedAt != nil && *ctx.LastRemindedAt >= ctx.LastMsgAt
	out["nextReminderAt"] = nextReminder
	return out
}

var (
	wechatMu sync.RWMutex
	wechatBk *WeChatBackend
)

// SetWeChatBackend wires the WeChat channel into the WebUI (called once by cmd/botler).
func SetWeChatBackend(b WeChatBackend) {
	wechatMu.Lock()
	wechatBk = &b
	wechatMu.Unlock()
}

func wechatBackend() (*WeChatBackend, bool) {
	wechatMu.RLock()
	defer wechatMu.RUnlock()
	return wechatBk, wechatBk != nil
}
