package wechat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ilink "github.com/openilink/openilink-sdk-go"

	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/dispatch"
	statsmon "github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// Long-poll tuning, carried over verbatim from v1's monitor.ts. The retry/backoff numbers match
// the values the SDK's Monitor uses, but the loop itself is ours: v1 re-resolves the account on
// every iteration so a WebUI re-pair takes effect live (no restart), and it marks the channel
// up/down for /healthz. Neither is expressible through the SDK's Monitor.
const (
	defaultLongPollTimeoutMs = 35_000
	maxConsecutiveFailures   = 3
	backoffDelay             = 30 * time.Second
	retryDelay               = 2 * time.Second
	sessionExpiredErrcode    = -14
)

// session is the account snapshot one inbound message is processed against.
type session struct {
	client *ilink.Client
	acct   ResolvedAccount
}

// monitor owns the batching coordinator plus the per-sender flush timers (the coordinator itself
// stays pure).
type wechatMonitor struct {
	coordinator *ImageBatchCoordinator
	mu          sync.Mutex
	timers      map[string]*time.Timer
}

func newMonitor(windowMs int) (*wechatMonitor, error) {
	coord, err := NewImageBatchCoordinator(windowMs)
	if err != nil {
		return nil, err
	}
	return &wechatMonitor{coordinator: coord, timers: map[string]*time.Timer{}}, nil
}

// armTimer (re)arms the flush timer for a sender; the window extends whenever a new image joins.
func (m *wechatMonitor) armTimer(ctx context.Context, sender string, s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.timers[sender]; ok {
		existing.Stop()
	}
	window := time.Duration(m.coordinator.WindowMs()) * time.Millisecond
	m.timers[sender] = time.AfterFunc(window, func() {
		m.mu.Lock()
		delete(m.timers, sender)
		m.mu.Unlock()
		m.flushBatch(ctx, sender, s)
	})
}

// cancelTimer cancels a sender's flush timer (called when a caption joins and dispatches now).
func (m *wechatMonitor) cancelTimer(sender string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.timers[sender]; ok {
		existing.Stop()
		delete(m.timers, sender)
	}
}

// stop cancels every pending flush timer (monitor shutdown).
func (m *wechatMonitor) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sender, timer := range m.timers {
		timer.Stop()
		delete(m.timers, sender)
	}
}

// flushBatch dispatches a sender's buffered images when the window expires with no caption.
func (m *wechatMonitor) flushBatch(ctx context.Context, sender string, s *session) {
	batch, ok := m.coordinator.Flush(sender)
	if !ok {
		return
	}
	logf("[wechat] flushed image batch from %s: %d image(s), text=%q", sender, len(batch.Images), truncate(batch.Text, 40))
	m.dispatchAndReply(ctx, s, sender, batch.ContextToken, batch.Text, batch.Images, batch.ID)
}

// bodyFromItemList extracts plain text: the first TEXT item, else the VOICE-to-text transcript.
func bodyFromItemList(items []ilink.MessageItem) string {
	for _, item := range items {
		if item.Type == ilink.ItemText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	for _, item := range items {
		if item.Type == ilink.ItemVoice && item.VoiceItem != nil && item.VoiceItem.Text != "" {
			return item.VoiceItem.Text
		}
	}
	return ""
}

// isAllowed implements the allowlist: the account owner (the person who scanned the QR) is always
// allowed; additional ilink_user_id values come from WECHAT_ALLOW_FROM (comma-separated).
func isAllowed(sender, ownerUserID, allowFrom string) bool {
	if ownerUserID != "" && sender == ownerUserID {
		return true
	}
	for _, entry := range strings.Split(allowFrom, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" && trimmed == sender {
			return true
		}
	}
	return false
}

// processMessage handles one inbound message: allowlist → extract text → batch → dispatch → reply.
func (m *wechatMonitor) processMessage(ctx context.Context, msg ilink.WeixinMessage, s *session) {
	fromUserID := msg.FromUserID
	contextToken := msg.ContextToken
	text := bodyFromItemList(msg.ItemList)

	// First-deploy observability: log raw metadata so we can confirm what real inbound messages
	// look like before trusting any skip rule.
	logf("[wechat] inbound: from=%s type=%d text=%q", orEmpty(fromUserID), int(msg.MessageType), truncate(text, 40))

	// Robust skip: no sender. Our own outbound echoes are built with an empty from_user_id, so an
	// empty sender is never a real user message.
	if fromUserID == "" {
		return
	}

	// message_type=BOT is NOT hard-dropped (the SDK never skips by message_type); log it and keep
	// processing — the allowlist below still gates who may drive the agent.
	if msg.MessageType == ilink.MsgTypeBot {
		warnf("[wechat] message_type=BOT inbound (possible echo), processing: from=%s", fromUserID)
	}

	if !isAllowed(fromUserID, s.acct.UserID, wechatAllowFrom()) {
		logf("[wechat] ignoring sender outside allowlist: %s", fromUserID)
		return
	}

	// Remember the sender's context_token + address (for push delivery and renewal reminders).
	// Gated on the allowlist above, so unallowed senders are never recorded.
	if contextToken != "" {
		UpdateContext(fromUserID, contextToken)
		push.RecordContact(push.SourceWeChat, fromUserID)
	}

	// Decode inbound images after the allowlist check, so unauthorized senders never trigger a CDN
	// download/decrypt. One failure is warn-logged; the rest still go through.
	images := make([]agentrtImage, 0, len(msg.ItemList))
	for _, item := range msg.ItemList {
		if item.Type != ilink.ItemImage {
			continue
		}
		img, err := DownloadInboundImage(ctx, item)
		if err != nil {
			warnf("[wechat] image decode failed: %v", err)
			continue
		}
		images = append(images, img)
	}

	// v1's text-only skip becomes "no text AND no image".
	if text == "" && len(images) == 0 {
		return
	}

	action := m.coordinator.OnMessage(fromUserID, PendingPiece{Text: text, Images: images, ContextToken: contextToken})
	if action.Kind == ActionBuffer {
		m.armTimer(ctx, fromUserID, s)
		logf("[wechat] buffered %d image(s) from %s awaiting caption (%ds, batch has %d image(s))",
			len(images), fromUserID, m.coordinator.WindowMs()/1000, m.coordinator.PendingImageCount(fromUserID))
		return
	}

	// Dispatch now: standalone text, a caption that joined a pending image batch, or an image
	// dispatch when batching is disabled. A caption join cancels the flush timer.
	m.cancelTimer(fromUserID)
	id := action.Dispatch.ID
	if msg.MessageID != 0 {
		id = fmt.Sprintf("%d", msg.MessageID)
	} else if msg.ClientID != "" {
		id = msg.ClientID
	}
	m.dispatchAndReply(ctx, s, fromUserID, action.Dispatch.ContextToken, action.Dispatch.Text, action.Dispatch.Images, id)
}

// dispatchAndReply runs one task and sends the reply back to the sender (text + outbound images).
// Any failure is reported to the user with the fixed fallback copy (v1's try/catch).
func (m *wechatMonitor) dispatchAndReply(ctx context.Context, s *session, fromUserID, contextToken, text string, images []agentrtImage, id string) {
	// Protocol: replying requires echoing this message's context_token. Without it we cannot reply
	// at all — log loudly instead of silently dropping the ack.
	if contextToken == "" {
		warnf("[wechat] missing context_token, cannot reply to this message (protocol requires echoing it): from=%s", fromUserID)
	}
	if err := m.runTask(ctx, s, fromUserID, contextToken, text, images, id); err != nil {
		warnf("[wechat] processing failed: %v", err)
		if contextToken != "" {
			_ = sendText(ctx, s.client, fromUserID, "⚠️ Something went wrong, please try again later.", contextToken)
		}
	}
}

func (m *wechatMonitor) runTask(ctx context.Context, s *session, fromUserID, contextToken, text string, images []agentrtImage, id string) (err error) {
	// A panic in the dispatch chain must not kill the long-poll loop.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()

	res := dispatch.Dispatch(ctx, text, dispatch.Options{
		ID:            id,
		Source:        "wechat",
		Recipient:     &push.Recipient{Source: push.SourceWeChat, UserID: fromUserID},
		InboundImages: images,
		SessionKey:    conversation.IMSessionKey,
	})
	// A dedup hit returns a sentinel; never echo it back to the user.
	if res.Text == dispatch.DuplicateSentinel {
		return nil
	}

	client := s.client
	if plain := strings.TrimSpace(MarkdownToPlainText(res.Text)); plain != "" {
		if err := sendText(ctx, client, fromUserID, plain, contextToken); err != nil {
			return fmt.Errorf("send text: %w", err)
		}
	}
	// Images the agent produced inside a data subproject (already path-checked by the runner), or
	// remote https URLs it referenced: upload to the WeChat CDN, then send one message each.
	for _, img := range res.Images {
		localPath := img
		if httpsURLRe.MatchString(img) {
			tmp, err := downloadRemoteImageToTemp(ctx, img)
			if err != nil {
				return fmt.Errorf("download remote image: %w", err)
			}
			localPath = tmp
		}
		if err := sendImageFile(ctx, client, fromUserID, localPath, contextToken); err != nil {
			return fmt.Errorf("send image: %w", err)
		}
	}
	return nil
}

// RunMonitor runs the long-poll loop: getUpdates → process → dispatch → reply. It blocks until the
// context is cancelled, re-resolving the account on every iteration so a re-pair takes effect live.
func RunMonitor(ctx context.Context) error {
	initial := ResolveAccount()
	if !initial.Configured {
		return errors.New("WeChat account not logged in; please run: botler wechat-login")
	}

	windowMs := wechatImageBatchWindowMs()
	m, err := newMonitor(windowMs)
	if err != nil {
		return err
	}
	defer m.stop()

	token := initial.Token
	baseURL := initial.BaseURL
	ownerUserID := initial.UserID
	client := newClient(initial)

	buf := LoadSyncBuf()
	nextTimeoutMs := int64(defaultLongPollTimeoutMs)
	failures := 0

	logf("[wechat] monitor started (%s)", baseURL)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Re-resolve the account every iteration so a web re-pair takes effect live (no restart).
		// A token change means the account was swapped: reset both the in-memory and on-disk cursor
		// so the new session starts clean (see also the reqToken guard on SaveSyncBuf below).
		acct := ResolveAccount()
		if acct.Token == "" {
			// account.json was deleted out-of-band (e.g. `botler wechat-logout` in another
			// terminal): keep running with the old token rather than flipping to an
			// unauthenticated loop and mis-marking the channel as healthy.
			warnf("[wechat] account no longer configured; keeping the current session running")
		} else if acct.Token != token {
			token = acct.Token
			baseURL = acct.BaseURL
			ownerUserID = acct.UserID
			buf = ""
			ClearSyncBuf()
			failures = 0
			statsmon.SetChannelUp("wechat")
			logf("[wechat] account re-resolved (re-pair detected), restarting cursor (%s)", baseURL)
		}
		client.SetToken(token)
		client.SetBaseURL(baseURL)
		reqToken := token

		resp, err := client.GetUpdates(ctx, buf, nextTimeoutMs)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			warnf("[wechat] getUpdates error (%d/%d): %v", failures, maxConsecutiveFailures, err)
			failures = sleepBackoff(ctx, failures)
			continue
		}

		if resp.LongPollingTimeoutMs > 0 {
			nextTimeoutMs = resp.LongPollingTimeoutMs
		}

		if resp.Ret != 0 || resp.ErrCode != 0 {
			if resp.Ret == sessionExpiredErrcode || resp.ErrCode == sessionExpiredErrcode {
				warnf("[wechat] session expired (errcode %d), please re-pair from the WebUI (or re-run: botler wechat-login)", sessionExpiredErrcode)
				statsmon.SetChannelDown("wechat", errors.New("session expired"))
				failures = 0
				sleepCtx(ctx, 60*time.Second) // re-log once a minute, don't hammer the API
				continue
			}
			failures++
			warnf("[wechat] getUpdates failed: ret=%d errcode=%d errmsg=%s (%d/%d)",
				resp.Ret, resp.ErrCode, resp.ErrMsg, failures, maxConsecutiveFailures)
			failures = sleepBackoff(ctx, failures)
			continue
		}

		failures = 0
		// A clean getUpdates round means the long poll is healthy again — recover from any prior down.
		statsmon.SetChannelUp("wechat")

		// A re-pair may have landed while this request was in flight; the cursor it returned belongs
		// to the OLD account. Persist it only if the account is still the one it was issued against —
		// otherwise a stale cursor would resurrect sync.json on a later restart.
		if resp.GetUpdatesBuf != "" && ResolveAccount().Token == reqToken {
			SaveSyncBuf(resp.GetUpdatesBuf)
			buf = resp.GetUpdatesBuf
		}

		s := &session{client: client, acct: ResolvedAccount{BaseURL: baseURL, Token: token, UserID: ownerUserID, Configured: true}}
		for _, msg := range resp.Msgs {
			m.processMessage(ctx, msg, s)
		}
	}
}

// sleepBackoff sleeps retry/backoff depending on the consecutive failure count and returns the
// (possibly reset) counter.
func sleepBackoff(ctx context.Context, failures int) int {
	if failures >= maxConsecutiveFailures {
		sleepCtx(ctx, backoffDelay)
		return 0
	}
	sleepCtx(ctx, retryDelay)
	return failures
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
