// Package telegram is the Telegram channel adapter: Bot API long polling with only net/http, a
// proxy-aware transport, the shared sender allowlist, and push delivery.
//
// A received text message becomes one dispatch call; the reply is sent back as plain text (the
// channel has no image support, so an images-only reply is replaced by a fixed placeholder).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/channels/allowlist"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/dispatch"
	"github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// DefaultAPIBase is the public Bot API endpoint.
const DefaultAPIBase = "https://api.telegram.org"

// imageOnlyPlaceholder is sent when the agent's reply consists of images only: Telegram rejects an
// empty message body, so the text-only channel needs a non-empty body (v1 verbatim).
const imageOnlyPlaceholder = "(image generated, but the Telegram channel does not support sending images yet)"

// processErrorReply is sent when handling a message fails unexpectedly (v1 verbatim).
const processErrorReply = "⚠️ Something went wrong; please try again later."

const (
	// longPollTimeoutSec is the server-side long-poll window.
	longPollTimeoutSec = 50
	// requestTimeout bounds a single non-long-poll API call.
	requestTimeout = 30 * time.Second
	// maxBodyBytes caps an API response body.
	maxBodyBytes = 1 << 20
	// maxBackoff caps the retry delay after transient polling failures.
	maxBackoff = 30 * time.Second
)

// Channel is the Telegram adapter. It implements push.Sender so scheduled results can be pushed
// back to the user.
type Channel struct {
	token     string
	allowFrom string
	apiBase   string
	client    *http.Client

	// dispatchFn is indirected for tests (dispatch.Dispatch otherwise).
	dispatchFn func(ctx context.Context, text string, opts dispatch.Options) dispatch.Result

	offset int64
}

// New builds a channel from the process configuration. It fails when TELEGRAM_BOT_TOKEN is
// missing ("Starting Telegram requires TELEGRAM_BOT_TOKEN", v1 verbatim).
func New(c *config.Config) (*Channel, error) {
	if c.TelegramToken == "" {
		return nil, fmt.Errorf("Starting Telegram requires TELEGRAM_BOT_TOKEN")
	}
	ch := &Channel{
		token:      c.TelegramToken,
		allowFrom:  c.TelegramAllowFrom,
		apiBase:    DefaultAPIBase,
		client:     newHTTPClient(),
		dispatchFn: dispatch.Dispatch,
	}
	return ch, nil
}

// Start builds the channel from the process configuration, registers it for push delivery and
// runs long polling in the background. Polling failures never crash the process.
func Start(ctx context.Context) error {
	ch, err := New(config.C())
	if err != nil {
		return err
	}
	Register(ch)
	go ch.Loop(ctx)
	return nil
}

// Register wires the channel into the push fallback chain.
func Register(ch *Channel) {
	push.RegisterSender(push.SourceTelegram, ch)
}

// Configured reports whether the channel has credentials to send.
func (c *Channel) Configured() bool { return c.token != "" }

// Send delivers text to one chat id (push.Sender). Images are not supported by this channel.
func (c *Channel) Send(address string, p push.Payload, withImages bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return c.SendMessage(ctx, address, p.Text)
}

// SendMessage posts a plain-text message to a chat id.
func (c *Channel) SendMessage(ctx context.Context, chatID, text string) error {
	payload := map[string]any{"chat_id": chatID, "text": text}
	var out json.RawMessage
	return c.post(ctx, "sendMessage", payload, &out)
}

// Loop runs long polling until ctx is cancelled (or the token is definitively rejected).
func (c *Channel) Loop(ctx context.Context) {
	logf("[telegram] Starting long polling…")
	if name, err := c.getMe(ctx); err != nil {
		errf("[telegram] getMe failed: %s", c.redact(err.Error()))
	} else {
		logf("[telegram] @%s online", name)
		monitor.SetChannelUp("telegram")
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := c.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var apiErr *apiError
			if ok := asAPIError(err, &apiErr); ok && (apiErr.code == 401 || apiErr.code == 404) {
				// A rejected token can never recover: mark the channel down and stop polling
				// (the process itself keeps running, as in v1).
				monitor.SetChannelDown("telegram", err)
				errf("[telegram] fatal: %s", c.redact(err.Error()))
				return
			}
			errf("[telegram] getUpdates failed: %s; retrying in %s", c.redact(err.Error()), backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.UpdateID >= c.offset {
				c.offset = u.UpdateID + 1
			}
			c.handleUpdate(ctx, u)
		}
	}
}

// handleUpdate applies the v1 message filter and dispatches one text message.
func (c *Channel) handleUpdate(ctx context.Context, u update) {
	msg := u.Message
	chatID := ""
	text := ""
	var messageID int64
	fromID := ""
	username := ""
	if msg != nil {
		messageID = msg.MessageID
		text = msg.Text
		chatID = strconv.FormatInt(msg.Chat.ID, 10)
		if msg.From != nil {
			fromID = strconv.FormatInt(msg.From.ID, 10)
			username = msg.From.Username
		}
	}
	logf("[tg] update=%s chat=%s from=%s text=%s", updateType(u), chatID, firstNonEmpty(username, fromID), quoteShort(text, 60))

	if msg == nil || text == "" || msg.MessageID == 0 || chatID == "0" || chatID == "" {
		logf("[tg] Ignored non-text / no-chat message")
		return
	}
	if !allowlist.Allowed(c.allowFrom, []string{fromID, username, chatID}) {
		logf("[tg] Ignoring sender outside allowlist: from=%s chat=%s", firstNonEmpty(fromID, username, "(unknown)"), chatID)
		return
	}
	logf("[tg] Processing chat=%s msg=%d: %s", chatID, messageID, quoteShort(text, 60))

	// Remember this address so push delivery can fall back to Telegram.
	push.RecordContact(push.SourceTelegram, chatID)

	// A panic in the dispatch chain must not kill the long-poll loop (same guard as the WeChat monitor).
	reply := dispatchSafely(ctx, c.dispatchFn, text, dispatch.Options{
		ID:         fmt.Sprintf("%s:%d", chatID, messageID),
		Source:     "telegram",
		Recipient:  &push.Recipient{Source: push.SourceTelegram, UserID: chatID},
		SessionKey: conversation.IMSessionKey,
	})
	body := reply.Text
	if body == "" {
		body = imageOnlyPlaceholder
	}
	logf("[tg] Reply length=%d, sending…", len([]rune(body)))
	if err := c.SendMessage(ctx, chatID, body); err != nil {
		errf("[telegram] Error processing message: %s", c.redact(err.Error()))
		_ = c.SendMessage(ctx, chatID, processErrorReply)
		return
	}
	// A successful reply means the channel is reachable — recover from any transient blip.
	monitor.SetChannelUp("telegram")
	logf("[tg] Replied to chat=%s", chatID)
}

// ---- Bot API wire types ----

type update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *message `json:"message"`
	EditedMessage *message `json:"edited_message"`
	ChannelPost   *message `json:"channel_post"`
	CallbackQuery any      `json:"callback_query"`
	InlineQuery   any      `json:"inline_query"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From *struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

// apiError is a Bot API level failure (HTTP 200 with ok=false, or a non-2xx status).
type apiError struct {
	method      string
	code        int
	description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.method, e.code, e.description)
}

func asAPIError(err error, target **apiError) bool {
	if e, ok := err.(*apiError); ok {
		*target = e
		return true
	}
	return false
}

func (c *Channel) getMe(ctx context.Context) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var me struct {
		Username string `json:"username"`
	}
	if err := c.get(reqCtx, "getMe", nil, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

func (c *Channel) getUpdates(ctx context.Context) ([]update, error) {
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(longPollTimeoutSec))
	if c.offset > 0 {
		q.Set("offset", strconv.FormatInt(c.offset, 10))
	}
	reqCtx, cancel := context.WithTimeout(ctx, (longPollTimeoutSec+15)*time.Second)
	defer cancel()
	var updates []update
	if err := c.get(reqCtx, "getUpdates", q, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Channel) get(ctx context.Context, method string, query url.Values, out any) error {
	endpoint := fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, method)
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return c.do(req, method, out)
}

func (c *Channel) post(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, method, out)
}

func (c *Channel) do(req *http.Request, method string, out any) error {
	res, err := c.client.Do(req)
	if err != nil {
		// net/http embeds the full request URL in transport errors, and that URL contains the bot
		// token. Redact before the error can reach a log line.
		return errors.New(c.redact(err.Error()))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	var parsed apiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("telegram %s: invalid response (HTTP %d): %w", method, res.StatusCode, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || !parsed.OK {
		code := parsed.ErrorCode
		if code == 0 {
			code = res.StatusCode
		}
		return &apiError{method: method, code: code, description: parsed.Description}
	}
	if out != nil && len(parsed.Result) > 0 {
		if err := json.Unmarshal(parsed.Result, out); err != nil {
			return fmt.Errorf("telegram %s: cannot decode result: %w", method, err)
		}
	}
	return nil
}

// dispatchSafely calls dispatch with a panic guard, so one bad task cannot stop polling.
func dispatchSafely(ctx context.Context, fn func(context.Context, string, dispatch.Options) dispatch.Result, text string, opts dispatch.Options) (out dispatch.Result) {
	defer func() {
		if r := recover(); r != nil {
			errf("[telegram] dispatch panicked: %v", r)
			out = dispatch.Result{Text: processErrorReply}
		}
	}()
	return fn(ctx, text, opts)
}

// redact removes the bot token from text that may be logged. The Bot API puts the token in the
// URL path, so every transport error and every %v of a request carries it.
func (c *Channel) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "***")
}

func updateType(u update) string {
	switch {
	case u.Message != nil:
		return "message"
	case u.EditedMessage != nil:
		return "edited_message"
	case u.ChannelPost != nil:
		return "channel_post"
	case u.CallbackQuery != nil:
		return "callback_query"
	case u.InlineQuery != nil:
		return "inline_query"
	}
	return "unknown"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// quoteShort JSON-quotes the first n runes of s (v1 logs `JSON.stringify(text.slice(0, 60))`).
func quoteShort(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	encoded, err := json.Marshal(string(r))
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

func errf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

var _ push.Sender = (*Channel)(nil)
