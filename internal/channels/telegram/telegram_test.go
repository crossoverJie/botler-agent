package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/dispatch"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// newTestChannel builds a channel pointing at a stub Bot API server.
func newTestChannel(t *testing.T, baseURL, allowFrom string) *Channel {
	t.Helper()
	return &Channel{
		token:      "TEST",
		allowFrom:  allowFrom,
		apiBase:    baseURL,
		client:     &http.Client{Timeout: 5 * time.Second},
		dispatchFn: dispatch.Dispatch,
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(&config.Config{}); err == nil {
		t.Fatal("expected an error when TELEGRAM_BOT_TOKEN is missing")
	} else if err.Error() != "Starting Telegram requires TELEGRAM_BOT_TOKEN" {
		t.Fatalf("unexpected error text: %q", err.Error())
	}
}

func TestHandleUpdateDispatchesWithDedupIDAndRecipient(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, body.ChatID+"|"+body.Text)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	var got dispatch.Options
	ch.dispatchFn = func(_ context.Context, text string, opts dispatch.Options) dispatch.Result {
		got = opts
		return dispatch.Result{Text: "hello back"}
	}

	ch.handleUpdate(context.Background(), update{
		UpdateID: 7,
		Message:  testMessage(42, 99, "hi"),
	})

	if got.ID != "42:99" {
		t.Fatalf("dedup id = %q, want 42:99", got.ID)
	}
	if got.Source != "telegram" {
		t.Fatalf("source = %q", got.Source)
	}
	if got.SessionKey != conversation.IMSessionKey {
		t.Fatalf("sessionKey = %q", got.SessionKey)
	}
	if got.Recipient == nil || got.Recipient.Source != push.SourceTelegram || got.Recipient.UserID != "42" {
		t.Fatalf("recipient = %+v", got.Recipient)
	}
	if len(sent) != 1 || sent[0] != "42|hello back" {
		t.Fatalf("sent = %v", sent)
	}
}

func TestHandleUpdateUsesImagePlaceholderForEmptyReply(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, body.Text)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	ch.dispatchFn = func(_ context.Context, _ string, _ dispatch.Options) dispatch.Result {
		return dispatch.Result{Images: []string{"/data/p/x.png"}}
	}
	ch.handleUpdate(context.Background(), update{Message: testMessage(42, 1, "chart")})

	want := "(image generated, but the Telegram channel does not support sending images yet)"
	if len(sent) != 1 || sent[0] != want {
		t.Fatalf("sent = %v, want placeholder %q", sent, want)
	}
}

func TestHandleUpdateIgnoresSendersOutsideAllowlist(t *testing.T) {
	calls := 0
	ch := newTestChannel(t, "http://127.0.0.1:0", "12345")
	ch.dispatchFn = func(_ context.Context, _ string, _ dispatch.Options) dispatch.Result {
		calls++
		return dispatch.Result{Text: "should not happen"}
	}

	msg := testMessage(42, 1, "hi")
	msg.From = &struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}{ID: 7, Username: "stranger"}
	ch.handleUpdate(context.Background(), update{Message: msg})

	if calls != 0 {
		t.Fatalf("dispatch must not run for a sender outside the allowlist")
	}
}

func TestHandleUpdateAllowsConfiguredUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "owner")
	called := false
	ch.dispatchFn = func(_ context.Context, _ string, _ dispatch.Options) dispatch.Result {
		called = true
		return dispatch.Result{Text: "ok"}
	}
	msg := testMessage(42, 1, "hi")
	msg.From = &struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}{ID: 7, Username: "owner"}
	ch.handleUpdate(context.Background(), update{Message: msg})

	if !called {
		t.Fatal("dispatch should run for an allowlisted username")
	}
}

func TestHandleUpdateIgnoresNonTextMessage(t *testing.T) {
	ch := newTestChannel(t, "http://127.0.0.1:0", "")
	called := false
	ch.dispatchFn = func(_ context.Context, _ string, _ dispatch.Options) dispatch.Result {
		called = true
		return dispatch.Result{}
	}
	ch.handleUpdate(context.Background(), update{Message: testMessage(42, 1, "")})
	if called {
		t.Fatal("a message without text must be ignored")
	}
}

func TestLoopProcessesUpdateAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatched := make(chan struct{}, 1)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"testbot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			served := false
			once.Do(func() { served = true })
			if served {
				res, _ := json.Marshal(map[string]any{
					"ok":     true,
					"result": []any{map[string]any{"update_id": 11, "message": json.RawMessage(testMessageJSON(42, 5, "hello"))}},
				})
				_, _ = w.Write(res)
				return
			}
			// Subsequent long polls stay open until the client goes away.
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	ch.dispatchFn = func(_ context.Context, text string, opts dispatch.Options) dispatch.Result {
		if text != "hello" || opts.ID != "42:5" {
			t.Errorf("unexpected dispatch: text=%q id=%q", text, opts.ID)
		}
		dispatched <- struct{}{}
		return dispatch.Result{Text: "reply"}
	}

	done := make(chan struct{})
	go func() {
		ch.Loop(ctx)
		close(done)
	}()

	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not dispatch the received message")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after context cancellation")
	}
}

func TestLoopMarksChannelDownOnUnauthorizedToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	done := make(chan struct{})
	go func() {
		ch.Loop(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop must stop on a rejected token instead of retrying forever")
	}
	if calls != 1 {
		t.Fatalf("getUpdates calls = %d, want 1", calls)
	}
}

func TestSendMessagePostsChatIDAndText(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	if err := ch.Send("12345", push.Payload{Text: "hi"}, false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/botTEST/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["chat_id"] != "12345" || gotBody["text"] != "hi" {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestSendReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"chat not found"}`))
	}))
	defer srv.Close()

	ch := newTestChannel(t, srv.URL, "")
	err := ch.SendMessage(context.Background(), "1", "hi")
	if err == nil {
		t.Fatal("expected an error so push can fall back")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigured(t *testing.T) {
	if (&Channel{}).Configured() {
		t.Fatal("a channel without a token must not be configured")
	}
	if !(&Channel{token: "x"}).Configured() {
		t.Fatal("a channel with a token must be configured")
	}
}

func TestResolveProxyPriority(t *testing.T) {
	for _, key := range []string{"TG_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("http_proxy", "http://p5:1")
	if got := ResolveProxy(); got != "http://p5:1" {
		t.Fatalf("ResolveProxy = %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://p2:1")
	if got := ResolveProxy(); got != "http://p2:1" {
		t.Fatalf("ResolveProxy = %q", got)
	}
	t.Setenv("TG_PROXY", "http://p1:1")
	if got := ResolveProxy(); got != "http://p1:1" {
		t.Fatalf("ResolveProxy = %q", got)
	}
}

func TestNewHTTPClientInstallsProxy(t *testing.T) {
	for _, key := range []string{"TG_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("TG_PROXY", "http://127.0.0.1:7890")
	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("expected a proxy-aware transport")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.telegram.org/botX/getMe", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil || proxyURL == nil || proxyURL.Host != "127.0.0.1:7890" {
		t.Fatalf("proxy = %v, err = %v", proxyURL, err)
	}
}

func TestNewHTTPClientWithoutProxy(t *testing.T) {
	for _, key := range []string{"TG_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(key, "")
	}
	transport := newHTTPClient().Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("no proxy must be configured when no proxy env var is set")
	}
}

func TestUpdateType(t *testing.T) {
	if got := updateType(update{Message: &message{}}); got != "message" {
		t.Fatalf("updateType = %q", got)
	}
	if got := updateType(update{}); got != "unknown" {
		t.Fatalf("updateType = %q", got)
	}
}

func testMessage(chatID, messageID int64, text string) *message {
	var m message
	if err := json.Unmarshal(testMessageJSON(chatID, messageID, text), &m); err != nil {
		panic(err)
	}
	return &m
}

func testMessageJSON(chatID, messageID int64, text string) []byte {
	return []byte(fmt.Sprintf(
		`{"message_id":%d,"text":%q,"chat":{"id":%d},"from":{"id":7,"username":"tester"}}`,
		messageID, text, chatID,
	))
}
