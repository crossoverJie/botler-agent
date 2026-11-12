package wechat

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ilink "github.com/openilink/openilink-sdk-go"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/dispatch"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// fakeClient implements ilinkSender and records what the reply path tried to send.
type fakeClient struct {
	texts  []sentText
	images []sentImage
	// failText/failUpload/failImage fail the first N calls, then start succeeding.
	failText   int
	failUpload int
	failImage  int
}

type sentText struct{ to, text, contextToken string }
type sentImage struct{ to, contextToken, fileKey string }

func (f *fakeClient) SendText(_ context.Context, to, text, contextToken string) (string, error) {
	if f.failText > 0 {
		f.failText--
		return "", errors.New("send text failed")
	}
	f.texts = append(f.texts, sentText{to: to, text: text, contextToken: contextToken})
	return "", nil
}

func (f *fakeClient) UploadFile(_ context.Context, plaintext []byte, toUserID string, _ ilink.UploadMediaType) (*ilink.UploadResult, error) {
	if f.failUpload > 0 {
		f.failUpload--
		return nil, errors.New("upload failed")
	}
	return &ilink.UploadResult{FileKey: "fk-" + toUserID, FileSize: int64(len(plaintext))}, nil
}

func (f *fakeClient) SendImage(_ context.Context, to, contextToken string, uploaded *ilink.UploadResult) (string, error) {
	if f.failImage > 0 {
		f.failImage--
		return "", errors.New("send image failed")
	}
	f.images = append(f.images, sentImage{to: to, contextToken: contextToken, fileKey: uploaded.FileKey})
	return "", nil
}

// withDispatch swaps the pipeline entry point so the monitor can be driven without a model.
func withDispatch(t *testing.T, fn func(context.Context, string, dispatch.Options) dispatch.Result) *[]dispatch.Options {
	t.Helper()
	saved := dispatchTask
	calls := &[]dispatch.Options{}
	dispatchTask = func(ctx context.Context, text string, opts dispatch.Options) dispatch.Result {
		*calls = append(*calls, opts)
		return fn(ctx, text, opts)
	}
	t.Cleanup(func() { dispatchTask = saved })
	return calls
}

// withWechatConfig installs a hermetic config dir and a WeChat-enabled config.
func withWechatConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOTLER_CONFIG_DIR", dir)
	saved := config.C()
	cfg := &config.Config{DataRoot: dir, WeChatEnabled: true}
	if mutate != nil {
		mutate(cfg)
	}
	config.Set(cfg)
	t.Cleanup(func() { config.Set(saved) })
	silentLogs(t)
	return cfg
}

// cdnImage returns an image item whose ciphertext is served by a local test CDN, so processMessage
// runs the real decode path (download -> AES-ECB decrypt -> mime sniff).
func cdnImage(t *testing.T) ilink.MessageItem {
	t.Helper()
	key := mustHex("0123456789abcdef0123456789abcdef")
	cipher, err := ilink.EncryptAESECB(pngMagic, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cipher)
	}))
	t.Cleanup(srv.Close)
	return ilink.MessageItem{
		Type:      ilink.ItemImage,
		ImageItem: &ilink.ImageItem{Media: &ilink.CDNMedia{FullURL: srv.URL}, AESKey: hex.EncodeToString(key)},
	}
}

func sessionFor(fake *fakeClient, owner string) *session {
	return &session{client: fake, acct: ResolvedAccount{UserID: owner, Token: "tok", BaseURL: "http://127.0.0.1:1", Configured: true}}
}

func textMessage(from, contextToken, text string) ilink.WeixinMessage {
	return ilink.WeixinMessage{
		FromUserID: from, ContextToken: contextToken, MessageID: 7,
		ItemList: []ilink.MessageItem{{Type: ilink.ItemText, TextItem: &ilink.TextItem{Text: text}}},
	}
}

func TestProcessMessageIgnoresMessagesWithoutSender(t *testing.T) {
	withWechatConfig(t, nil)
	calls := withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: "reply"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	// Our own outbound echoes carry an empty from_user_id, so an empty sender is never a user.
	m.processMessage(context.Background(), textMessage("", "ct", "hello"), sessionFor(fake, "owner"))

	if len(*calls) != 0 {
		t.Fatalf("an empty sender must not dispatch: %+v", *calls)
	}
	if len(fake.texts) != 0 {
		t.Fatal("an empty sender must not be replied to")
	}
}

func TestProcessMessageEnforcesAllowlist(t *testing.T) {
	withWechatConfig(t, func(c *config.Config) { c.WeChatAllowFrom = "friend-1" })
	calls := withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: "reply"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	m.processMessage(context.Background(), textMessage("stranger", "ct", "hi"), sessionFor(fake, "owner"))
	if len(*calls) != 0 {
		t.Fatal("a sender outside the allowlist must not reach the agent")
	}
	// An unauthorized sender must never get a context_token recorded (that would be a push target).
	if _, ok := GetContext("stranger"); ok {
		t.Fatal("an unauthorized sender must not have a context token recorded")
	}

	// The allowlisted friend is accepted, and the owner always is.
	m.processMessage(context.Background(), textMessage("friend-1", "ct", "hi"), sessionFor(fake, "owner"))
	m.processMessage(context.Background(), textMessage("owner", "ct2", "hi"), sessionFor(fake, "owner"))
	if len(*calls) != 2 {
		t.Fatalf("expected both allowed senders to dispatch, got %d", len(*calls))
	}
}

func TestProcessMessageDispatchesTextAndReplies(t *testing.T) {
	withWechatConfig(t, nil)
	var gotText string
	calls := withDispatch(t, func(_ context.Context, text string, _ dispatch.Options) dispatch.Result {
		gotText = text
		return dispatch.Result{Text: "**done** [x](http://a.b)"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}
	m.processMessage(context.Background(), textMessage("owner", "ct-1", "记一下"), sessionFor(fake, "owner"))

	if gotText != "记一下" {
		t.Fatalf("unexpected prompt: %q", gotText)
	}
	opts := (*calls)[0]
	if opts.Source != "wechat" || opts.ID != "7" {
		t.Fatalf("unexpected dispatch options: %+v", opts)
	}
	if opts.Recipient == nil || opts.Recipient.UserID != "owner" || opts.Recipient.Source != push.SourceWeChat {
		t.Fatalf("the sender must be injected as the push recipient: %+v", opts.Recipient)
	}
	if len(fake.texts) != 1 || fake.texts[0].contextToken != "ct-1" {
		t.Fatalf("the reply must echo the inbound context_token: %+v", fake.texts)
	}
	// Replies are markdown-stripped: WeChat renders plain text only.
	if strings.Contains(fake.texts[0].text, "**") || strings.Contains(fake.texts[0].text, "](") {
		t.Fatalf("reply was not stripped of markdown: %q", fake.texts[0].text)
	}
	// The token is cached so the push chain and the renewal reminder can reuse it.
	entry, ok := GetContext("owner")
	if !ok || entry.Token != "ct-1" || entry.LastMsgAt == 0 {
		t.Fatalf("context token was not recorded: %+v (ok=%v)", entry, ok)
	}
}

func TestProcessMessageIDSelection(t *testing.T) {
	withWechatConfig(t, nil)
	calls := withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	msg := textMessage("owner", "ct", "a")
	msg.MessageID = 0
	msg.ClientID = "client-9"
	m.processMessage(context.Background(), msg, sessionFor(fake, "owner"))

	msg2 := textMessage("owner", "ct", "b")
	msg2.MessageID = 0
	msg2.ClientID = ""
	m.processMessage(context.Background(), msg2, sessionFor(fake, "owner"))

	// message_id wins, then client_id; both feed the dedup key so a replayed message is dropped.
	if (*calls)[0].ID != "client-9" {
		t.Fatalf("expected the client id fallback, got %q", (*calls)[0].ID)
	}
	if (*calls)[1].ID == "" {
		t.Fatal("a message with neither id must still get a dedup key")
	}
}

func TestProcessMessageSkipsEmptyAndBotMessages(t *testing.T) {
	withWechatConfig(t, nil)
	calls := withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: "reply"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	// No text and no image: nothing to answer.
	m.processMessage(context.Background(), ilink.WeixinMessage{FromUserID: "owner", ContextToken: "ct"}, sessionFor(fake, "owner"))
	if len(*calls) != 0 {
		t.Fatal("a message with neither text nor image must be skipped")
	}

	// message_type=BOT is an echo candidate, but it is processed (the allowlist still gates it).
	bot := textMessage("owner", "ct", "echo")
	bot.MessageType = ilink.MsgTypeBot
	m.processMessage(context.Background(), bot, sessionFor(fake, "owner"))
	if len(*calls) != 1 {
		t.Fatalf("a BOT message from an allowed sender must still run, got %d", len(*calls))
	}
}

func TestProcessMessageBuffersImageUntilWindowExpires(t *testing.T) {
	withWechatConfig(t, nil)
	dispatched := make(chan string, 1)
	withDispatch(t, func(_ context.Context, text string, _ dispatch.Options) dispatch.Result {
		dispatched <- text
		return dispatch.Result{Text: "ok"}
	})
	m, err := newMonitor(30) // 30ms window
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	// An image with no caption is held so a caption arriving next joins the same task.
	msg := ilink.WeixinMessage{
		FromUserID: "owner", ContextToken: "ct", MessageID: 1,
		ItemList: []ilink.MessageItem{cdnImage(t)},
	}
	m.processMessage(context.Background(), msg, sessionFor(fake, "owner"))
	if len(dispatched) != 0 {
		t.Fatal("an uncaptioned image must be buffered, not dispatched immediately")
	}

	select {
	case <-dispatched:
	case <-time.After(3 * time.Second):
		t.Fatal("the flush timer never fired")
	}
	// The timer map entry is removed once it fired.
	m.mu.Lock()
	pending := len(m.timers)
	m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("the fired timer must be removed, %d left", pending)
	}
	m.stop()
}

func TestProcessMessageDispatchesImageImmediatelyWhenBatchingDisabled(t *testing.T) {
	withWechatConfig(t, nil)
	ch := make(chan dispatch.Options, 1)
	withDispatch(t, func(_ context.Context, _ string, opts dispatch.Options) dispatch.Result {
		ch <- opts
		return dispatch.Result{}
	})
	m, err := newMonitor(0) // 0 disables batching
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	m.processMessage(context.Background(), ilink.WeixinMessage{
		FromUserID: "owner", ContextToken: "ct", MessageID: 2,
		ItemList: []ilink.MessageItem{cdnImage(t)},
	}, sessionFor(fake, "owner"))

	select {
	case opts := <-ch:
		if opts.ID != "2" {
			t.Fatalf("unexpected id: %q", opts.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("with batching disabled the image must dispatch immediately")
	}
}

func TestProcessMessageCaptionJoinsBufferedImage(t *testing.T) {
	withWechatConfig(t, nil)
	type call struct {
		text   string
		images int
	}
	got := make(chan call, 1)
	withDispatch(t, func(ctx context.Context, text string, opts dispatch.Options) dispatch.Result {
		got <- call{text: text, images: len(opts.InboundImages)}
		return dispatch.Result{}
	})
	m, err := newMonitor(60_000) // long window: only the caption can trigger the dispatch
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}
	s := sessionFor(fake, "owner")

	m.processMessage(context.Background(), ilink.WeixinMessage{
		FromUserID: "owner", ContextToken: "ct", MessageID: 1,
		ItemList: []ilink.MessageItem{cdnImage(t)},
	}, s)

	// The caption arrives while the image is still buffered: it must join, not start a new task.
	m.processMessage(context.Background(), textMessage("owner", "ct", "这个多少钱"), s)

	select {
	case c := <-got:
		if c.text != "这个多少钱" || c.images != 1 {
			t.Fatalf("caption and image were not merged: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the caption did not dispatch the buffered image")
	}
	// Dispatch cancels the pending timer instead of leaving it to fire a duplicate task.
	m.mu.Lock()
	pending := len(m.timers)
	m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("the flush timer must be cancelled, %d left", pending)
	}
	m.stop()
}

func TestProcessMessageDropsUndecodableImage(t *testing.T) {
	withWechatConfig(t, nil)
	var images int
	withDispatch(t, func(_ context.Context, _ string, opts dispatch.Options) dispatch.Result {
		images = len(opts.InboundImages)
		return dispatch.Result{}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	// An image item with no media cannot be downloaded; the text still goes through.
	msg := ilink.WeixinMessage{
		FromUserID: "owner", ContextToken: "ct", MessageID: 3,
		ItemList: []ilink.MessageItem{
			{Type: ilink.ItemImage, ImageItem: &ilink.ImageItem{}},
			{Type: ilink.ItemText, TextItem: &ilink.TextItem{Text: "看图"}},
		},
	}
	m.processMessage(context.Background(), msg, sessionFor(fake, "owner"))
	if images != 0 {
		t.Fatalf("an undecodable image must be dropped, got %d", images)
	}
}

func TestDispatchAndReplySuppressesDuplicateSentinel(t *testing.T) {
	withWechatConfig(t, nil)
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: dispatch.DuplicateSentinel}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}
	m.dispatchAndReply(context.Background(), sessionFor(fake, "owner"), "owner", "ct", "hi", nil, "1")
	if len(fake.texts) != 0 {
		t.Fatalf("the dedup sentinel must never be echoed: %+v", fake.texts)
	}
}

func TestDispatchAndReplySendsFallbackCopyOnFailure(t *testing.T) {
	withWechatConfig(t, nil)
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: "a reply"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{failText: 1} // the reply fails, the fallback copy succeeds

	m.dispatchAndReply(context.Background(), sessionFor(fake, "owner"), "owner", "ct", "hi", nil, "1")

	if len(fake.texts) != 1 {
		t.Fatalf("expected exactly the fallback copy, got %+v", fake.texts)
	}
	if !strings.Contains(fake.texts[0].text, "出错了") && !strings.Contains(fake.texts[0].text, "wrong") {
		t.Fatalf("unexpected fallback copy: %q", fake.texts[0].text)
	}
}

func TestDispatchAndReplyWithoutContextTokenCannotReply(t *testing.T) {
	withWechatConfig(t, nil)
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Text: "a reply"}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}
	// Without a context_token the protocol has no way to associate a reply; nothing must be sent.
	m.dispatchAndReply(context.Background(), sessionFor(fake, "owner"), "owner", "", "hi", nil, "1")
	if len(fake.texts) != 0 {
		t.Fatalf("a missing context_token must not produce a send call: %+v", fake.texts)
	}
}

func TestRunTaskSendsLocalAndRemoteImages(t *testing.T) {
	withWechatConfig(t, nil)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(local, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}

	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngMagic)
	}))
	defer remote.Close()
	withCDNClient(t, remote.Client())

	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Images: []string{local, remote.URL + "/a.png"}}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{}

	if err := m.runTask(context.Background(), sessionFor(fake, "owner"), "owner", "ct", "hi", nil, "1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.images) != 2 {
		t.Fatalf("expected both images to be uploaded and sent, got %d", len(fake.images))
	}
	for _, img := range fake.images {
		if img.contextToken != "ct" {
			t.Fatalf("image sends must echo the context token: %+v", img)
		}
	}
}

func TestRunTaskRecoversFromPanic(t *testing.T) {
	withWechatConfig(t, nil)
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		panic("boom")
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	// A panic in the dispatch chain must not kill the long-poll loop.
	if err := m.runTask(context.Background(), sessionFor(&fakeClient{}, "owner"), "owner", "ct", "hi", nil, "1"); err == nil {
		t.Fatal("expected the panic to surface as an error")
	}
}

func TestRunTaskUploadFailureIsReported(t *testing.T) {
	withWechatConfig(t, nil)
	dir := t.TempDir()
	local := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(local, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{Images: []string{local}}
	})
	m, err := newMonitor(0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{failUpload: 1}
	err = m.runTask(context.Background(), sessionFor(fake, "owner"), "owner", "ct", "hi", nil, "1")
	if err == nil || !strings.Contains(err.Error(), "upload image") {
		t.Fatalf("expected an upload error, got %v", err)
	}
	if len(fake.images) != 0 {
		t.Fatal("nothing must be sent when the upload failed")
	}
}

func TestStopClearsPendingTimers(t *testing.T) {
	withWechatConfig(t, nil)
	withDispatch(t, func(context.Context, string, dispatch.Options) dispatch.Result {
		return dispatch.Result{}
	})
	m, err := newMonitor(60_000)
	if err != nil {
		t.Fatal(err)
	}
	s := sessionFor(&fakeClient{}, "owner")
	m.armTimer(context.Background(), "owner", s)
	m.armTimer(context.Background(), "owner", s) // re-arming replaces the timer
	if n := pendingTimers(m); n != 1 {
		t.Fatalf("expected a single timer per sender, got %d", n)
	}
	m.cancelTimer("nobody") // unknown sender is a no-op
	m.stop()
	if n := pendingTimers(m); n != 0 {
		t.Fatalf("stop must cancel every timer, %d left", n)
	}
	if _, ok := m.coordinator.Flush("owner"); ok {
		t.Fatal("nothing should have been buffered")
	}
}

func pendingTimers(m *wechatMonitor) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.timers)
}

func TestSleepBackoff(t *testing.T) {
	// A cancelled context makes the sleep return immediately, so this asserts the counter logic
	// without waiting out the real 2s / 30s backoffs.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := sleepBackoff(ctx, 1); got != 1 {
		t.Fatalf("a single failure must be kept, got %d", got)
	}
	// Reaching the threshold resets the counter after the long backoff.
	if got := sleepBackoff(ctx, maxConsecutiveFailures); got != 0 {
		t.Fatalf("the counter must reset at the threshold, got %d", got)
	}
}

func TestSleepCtxReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Minute)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a cancelled context must not sleep the full duration (%v)", elapsed)
	}
}

// apiServer emulates the iLink endpoints the long-poll loop touches.
func apiServer(t *testing.T, body string, requests *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(requests, 1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "sendmessage") {
			_, _ = w.Write([]byte(`{"ret":0}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunMonitorProcessesInboundAndStopsOnCancel(t *testing.T) {
	silentLogs(t)
	var requests int32
	srv := apiServer(t, `{"ret":0,"get_updates_buf":"b1","msgs":[
		{"from_user_id":"owner","context_token":"ct","message_id":7,
		 "item_list":[{"type":1,"text_item":{"text":"hi there"}}]}]}`, &requests)
	withWechatConfig(t, func(c *config.Config) { c.WeChatImageBatchSeconds = 0 })
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})

	dispatched := make(chan string, 4)
	withDispatch(t, func(_ context.Context, text string, _ dispatch.Options) dispatch.Result {
		dispatched <- text
		return dispatch.Result{Text: "pong"}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunMonitor(ctx) }()

	select {
	case text := <-dispatched:
		if text != "hi there" {
			t.Fatalf("unexpected prompt: %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the inbound message never reached the pipeline")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunMonitor did not return after the context was cancelled")
	}
	// The cursor returned by the server is persisted so a restart resumes where it left off.
	if got := LoadSyncBuf(); got != "b1" {
		t.Fatalf("sync buffer was not saved: %q", got)
	}
}

func TestRunMonitorRequiresALogin(t *testing.T) {
	withWechatConfig(t, nil)
	err := RunMonitor(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected a not-logged-in error, got %v", err)
	}
}

func TestRunMonitorSessionExpiredBacksOff(t *testing.T) {
	silentLogs(t)
	var requests int32
	srv := apiServer(t, `{"ret":0,"errcode":-14,"errmsg":"session expired"}`, &requests)
	withWechatConfig(t, nil)
	SaveAccount(AccountUpdate{Token: "tok", BaseURL: srv.URL, UserID: "owner"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunMonitor(ctx) }()

	// An expired session must be reported once and then wait, not hammer the API.
	waitFor(t, func() bool { return atomic.LoadInt32(&requests) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&requests); got > 2 {
		t.Fatalf("an expired session must back off, %d requests in 100ms", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunMonitor did not return after cancel")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}
