package wechat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// quietAPI answers every iLink endpoint successfully without doing anything, which is enough for
// the lifecycle (start / stop) tests.
func quietAPI(t *testing.T, requests *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// resetMonitorState clears the process-global monitor/reminder flags between tests.
func resetMonitorState(t *testing.T) {
	t.Helper()
	monitorMu.Lock()
	cancel := monitorCancel
	monitorCancel = nil
	monitorRunning = false
	monitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
	reminderMu.Lock()
	reminderStarted = false
	reminderLastAt = nil
	reminderNextAt = nil
	reminderMu.Unlock()
	t.Cleanup(func() {
		monitorMu.Lock()
		if monitorCancel != nil {
			monitorCancel()
		}
		monitorCancel = nil
		monitorRunning = false
		monitorMu.Unlock()
		reminderMu.Lock()
		reminderStarted = false
		reminderMu.Unlock()
	})
}

func TestStartRequiresEnabledAndLogin(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatEnabled = false })
	err := Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "WECHAT_ENABLED=1") {
		t.Fatalf("expected the enabled gate to fire, got %v", err)
	}

	// Enabled but not logged in: starting would only produce an unauthenticated long poll.
	withWechatConfig(t, nil)
	err = Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected the login gate to fire, got %v", err)
	}
}

func TestStartStartsMonitorAndStopStopsIt(t *testing.T) {
	resetMonitorState(t)
	var requests int32
	srv := quietAPI(t, &requests)
	withWechatConfig(t, nil)
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})

	if err := Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !MonitorRunning() {
		t.Fatal("Start must mark the monitor as running")
	}
	// The goroutine really is polling: the server sees getUpdates.
	waitFor(t, func() bool { return atomic.LoadInt32(&requests) > 0 })

	Stop()
	waitFor(t, func() bool { return !MonitorRunning() })
	// Stopping twice must be harmless.
	Stop()
}

func TestEnsureMonitorIsGatedAndIdempotent(t *testing.T) {
	resetMonitorState(t)
	var requests int32
	srv := quietAPI(t, &requests)
	withWechatConfig(t, func(c *config.Config) { c.WeChatEnabled = false })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})

	// Disabled: a late pair must not start anything.
	EnsureMonitor(context.Background())
	if MonitorRunning() {
		t.Fatal("a disabled channel must not start the monitor")
	}

	// Enabled but not configured: same.
	config.Set(&config.Config{WeChatEnabled: true})
	withWechatConfig(t, nil)
	if err := os.RemoveAll(filepath.Join(config.UserConfigDir(), "wechat")); err != nil {
		t.Fatal(err)
	}
	EnsureMonitor(context.Background())
	if MonitorRunning() {
		t.Fatal("an unconfigured channel must not start the monitor")
	}

	// Enabled and configured: starts, and a second call does not start a second loop.
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})
	EnsureMonitor(context.Background())
	if !MonitorRunning() {
		t.Fatal("a late pair must bring the monitor online")
	}
	// Idempotent: the second call must leave the existing loop (and its cancel func) untouched.
	first := reflect.ValueOf(monitorCancel).Pointer()
	EnsureMonitor(context.Background())
	if got := reflect.ValueOf(monitorCancel).Pointer(); got != first {
		t.Fatal("EnsureMonitor must not restart a running monitor")
	}
	Stop()
}

func TestLogoutClearsEverything(t *testing.T) {
	withWechatConfig(t, nil)
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	UpdateContext("owner", "ct")
	push.RecordContact(push.SourceWeChat, "owner")
	SaveSyncBuf("cursor")

	Logout()

	if ResolveAccount().Configured {
		t.Fatal("the account must be gone after logout")
	}
	if _, ok := GetContext("owner"); ok {
		t.Fatal("context tokens must be cleared")
	}
	if got := push.Contacts(push.SourceWeChat); len(got) != 0 {
		t.Fatalf("push contacts must be cleared: %v", got)
	}
	if LoadSyncBuf() != "" {
		t.Fatal("the sync cursor must be cleared")
	}
}

func TestStatusSnapshot(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 2 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	UpdateContext("owner", "ct")

	status := Status()
	if !status.Configured {
		t.Fatal("the snapshot must report the account as configured")
	}
	if status.Owner == nil || *status.Owner != "owner" {
		t.Fatalf("unexpected owner: %v", status.Owner)
	}
	if !status.OwnerHasContext {
		t.Fatal("the owner has a context token, the snapshot must say so")
	}
	if status.LoginAt == nil || status.LoginAgeMs == nil {
		t.Fatal("a login timestamp must be reported")
	}
	if status.ThresholdHours != 2 || !status.ReminderEnabled {
		t.Fatalf("unexpected threshold: %d enabled=%v", status.ThresholdHours, status.ReminderEnabled)
	}
	if status.QuietMs == nil || status.NextReminderAt == nil {
		t.Fatal("the quiet window must be reported")
	}
	if status.RemindedThisStretch {
		t.Fatal("nothing was reminded yet")
	}

	// No account at all: nothing is claimed.
	withWechatConfig(t, nil)
	empty := Status()
	if empty.Configured || empty.Owner != nil || empty.OwnerHasContext {
		t.Fatalf("an empty account must produce an empty snapshot: %+v", empty)
	}
}

func TestReminderTextClamps(t *testing.T) {
	if got := ReminderText(0); !strings.Contains(got, "over 0 hours") || !strings.Contains(got, "about 24 hours") {
		t.Fatalf("unexpected copy: %q", got)
	}
	// Over 24h: the remaining window is clamped to 0 instead of going negative.
	if got := ReminderText(30.4); !strings.Contains(got, "over 30 hours") || !strings.Contains(got, "about 0 hours") {
		t.Fatalf("unexpected clamped copy: %q", got)
	}
	if got := ReminderText(-5); !strings.Contains(got, "over 0 hours") {
		t.Fatalf("a negative elapsed time must clamp to 0: %q", got)
	}
}

// reminderClient captures what the reminder tick delivers.
type reminderClient struct {
	sends int32
	fail  bool
}

func (r *reminderClient) Configured() bool { return true }
func (r *reminderClient) Send(string, push.Payload, bool) error {
	atomic.AddInt32(&r.sends, 1)
	if r.fail {
		return errors.New("send failed")
	}
	return nil
}

// seedContext writes a context entry directly so the test can control the quiet window.
func seedContext(t *testing.T, userID, token string, lastMsgAt int64, lastRemindedAt *int64) {
	t.Helper()
	contextsMu.Lock()
	defer contextsMu.Unlock()
	m := loadContexts()
	m[userID] = ContextEntry{Token: token, LastMsgAt: lastMsgAt, LastRemindedAt: lastRemindedAt}
	saveContexts(m)
}

func withReminderSender(t *testing.T) *reminderClient {
	t.Helper()
	rc := &reminderClient{}
	saved, had := push.SenderFor(push.SourceWeChat)
	push.RegisterSender(push.SourceWeChat, rc)
	t.Cleanup(func() {
		if had {
			push.RegisterSender(push.SourceWeChat, saved)
			return
		}
		push.RegisterSender(push.SourceWeChat, unavailableSender{})
	})
	return rc
}

type unavailableSender struct{}

func (unavailableSender) Configured() bool { return false }
func (unavailableSender) Send(string, push.Payload, bool) error {
	return errors.New("unavailable")
}

func TestReminderTickSendsOncePerQuietStretch(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 1 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	rc := withReminderSender(t)

	// Quiet for 3 hours with a valid context token: exactly one reminder.
	seedContext(t, "owner", "ct", nowMs()-3*3600*1000, nil)
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 1 {
		t.Fatalf("expected one reminder, got %d", got)
	}
	// The attempt is recorded even on success, so the same quiet stretch is never nudged twice.
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 1 {
		t.Fatalf("a second tick must not re-send, got %d", got)
	}

	// A fresh message starts a new stretch: the next quiet window reminds again.
	seedContext(t, "owner", "ct", nowMs()-3*3600*1000, nil)
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 2 {
		t.Fatalf("a new quiet stretch must remind again, got %d", got)
	}
}

func TestReminderTickSkipsWhenNotDue(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 2 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	rc := withReminderSender(t)

	// Still inside the window.
	seedContext(t, "owner", "ct", nowMs()-30*60*1000, nil)
	reminderTick(context.Background())
	// Already reminded for this stretch.
	reminded := nowMs()
	seedContext(t, "owner", "ct", nowMs()-3*3600*1000, &reminded)
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 0 {
		t.Fatalf("expected no reminder, got %d", got)
	}
}

func TestReminderTickRequiresOwnerContextAndEnabled(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 1 })
	rc := withReminderSender(t)

	// Disabled: nothing happens even with a stale owner.
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 0 {
		t.Fatal("a disabled reminder must not send")
	}

	// No owner at all.
	config.Set(&config.Config{WeChatReminderHours: 1})
	reminderTick(context.Background())

	// Owner known but never messaged the bot: there is no token to renew.
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 0 {
		t.Fatalf("a missing context must not send, got %d", got)
	}
}

func TestReminderTickRecordsFailureWithoutSpamming(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 1 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	rc := withReminderSender(t)
	rc.fail = true

	seedContext(t, "owner", "ct", nowMs()-3*3600*1000, nil)
	reminderTick(context.Background())
	// A failed delivery still marks the stretch: retrying on every tick would be spam.
	entry, ok := GetContext("owner")
	if !ok || entry.LastRemindedAt == nil {
		t.Fatalf("the attempt must be recorded: %+v", entry)
	}
	reminderTick(context.Background())
	if got := atomic.LoadInt32(&rc.sends); got != 1 {
		t.Fatalf("a recorded stretch must not be retried, got %d sends", got)
	}
}

func TestReminderTickSkipsWhenAnotherTickIsRunning(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 1 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	rc := withReminderSender(t)
	seedContext(t, "owner", "ct", nowMs()-3*3600*1000, nil)

	// Simulate an in-flight tick: the overlapping one must return immediately.
	reminderRunning.Lock()
	reminderTick(context.Background())
	reminderRunning.Unlock()

	if got := atomic.LoadInt32(&rc.sends); got != 0 {
		t.Fatalf("an overlapping tick must not send, got %d", got)
	}
}

func TestStartReminderLoopGating(t *testing.T) {
	resetMonitorState(t)

	// WECHAT_REMINDER_HOURS=0 disables the loop entirely.
	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 0 })
	StartReminderLoop(context.Background())
	if ReminderStatus().Started {
		t.Fatal("a zero threshold must not start the loop")
	}

	withWechatConfig(t, func(c *config.Config) { c.WeChatReminderHours = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartReminderLoop(ctx)
	status := ReminderStatus()
	if !status.Started || status.TickMs != ReminderTickMs {
		t.Fatalf("unexpected reminder status: %+v", status)
	}
	// The first tick runs immediately, so the last-tick timestamp is stamped.
	waitFor(t, func() bool { return ReminderStatus().LastTickAt != nil })

	// A second call must not spawn another loop.
	StartReminderLoop(ctx)
	StartReminderLoop(ctx)
	cancel()
}

func TestPushSenderReportsMissingState(t *testing.T) {
	withWechatConfig(t, nil)
	sender := pushSender{}
	if sender.Configured() {
		t.Fatal("no account means not configured")
	}
	if err := sender.Send("owner", push.Payload{Text: "hi"}, false); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected a not-configured error, got %v", err)
	}

	// Configured but the user never messaged: there is no context token to echo.
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	if !sender.Configured() {
		t.Fatal("a saved token means configured")
	}
	if err := sender.Send("someone", push.Payload{Text: "hi"}, false); err == nil ||
		!strings.Contains(err.Error(), "context_token") {
		t.Fatalf("expected a missing-context error, got %v", err)
	}
}

func TestPushSenderSendsStrippedText(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	withWechatConfig(t, nil)
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})
	UpdateContext("owner", "ct")

	if err := (pushSender{}).Send("owner", push.Payload{Text: "**bold** text"}, false); err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatal("no sendmessage request reached the API")
	}
	joined := strings.Join(bodies, "\n")
	if strings.Contains(joined, "**") {
		t.Fatalf("markdown must be stripped for WeChat: %q", joined)
	}
	if !strings.Contains(joined, "bold") {
		t.Fatalf("the text body was lost: %q", joined)
	}
}

func TestPushSenderImageWithoutLocalFileFails(t *testing.T) {
	// Images are only sent on the primary WeChat channel; a missing file must surface as an error
	// rather than a silent drop.
	withWechatConfig(t, nil)
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: "http://127.0.0.1:1", UserID: "owner"})
	UpdateContext("owner", "ct")

	err := (pushSender{}).Send("owner", push.Payload{
		Text:   "",
		Images: []string{filepath.Join(t.TempDir(), "missing.png")},
	}, true)
	if err == nil {
		t.Fatal("expected a read error for a missing image")
	}
}
