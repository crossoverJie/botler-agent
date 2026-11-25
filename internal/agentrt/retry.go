package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model/anthropicmodel"
	"google.golang.org/adk/v2/model/openaimodel"
)

// RetryEvent describes one retry, fallback, give-up, or budget-exhausted decision.
// agentrt stays free of monitor and logging dependencies: callers wire callbacks via OnEvent and
// context-scoped RetryObserver.
type RetryEvent struct {
	// Kind is one of "retry", "fallback", "give-up", or "budget-exhausted".
	Kind string
	// From is the Describe() label of the runtime that failed (or the whole chain for budget-exhausted).
	From string
	// To is the Describe() label of the next runtime in the chain (empty except for "fallback").
	To string
	// Attempt is the 1-based retry index (only set for "retry").
	Attempt int
	// MaxAttempts is RetryingRuntime.MaxRetries (only set for "retry").
	MaxAttempts int
	// DelayMs is the backoff sleep before the next attempt ("retry") or the budget that expired ("budget-exhausted").
	DelayMs int64
	Err     error
}

// Describer is an optional interface implemented by runtimes so diagnostics and notices can report
// "provider/model" without widening Runtime itself.
type Describer interface {
	Describe() string
}

func describe(rt Runtime) string {
	if d, ok := rt.(Describer); ok {
		return d.Describe()
	}
	return fmt.Sprintf("%T", rt)
}

// RetryObserver receives retry events for a single task. dispatch installs it through the context
// so the process-wide cached runtime chain never holds per-task mutable state.
type RetryObserver func(ctx context.Context, ev RetryEvent)

type retryObserverKey struct{}

// WithRetryObserver attaches a task-scoped RetryObserver to ctx (passing nil clears an inherited observer).
func WithRetryObserver(ctx context.Context, o RetryObserver) context.Context {
	return context.WithValue(ctx, retryObserverKey{}, o)
}

// RetryObserverFrom returns the task-scoped RetryObserver installed on ctx, or nil when absent.
func RetryObserverFrom(ctx context.Context) RetryObserver {
	if ctx == nil {
		return nil
	}
	o, _ := ctx.Value(retryObserverKey{}).(RetryObserver)
	return o
}

// notify fans an event out to both the process-wide sink (stderr + monitor) and any task-scoped
// observer on ctx.
func notify(ctx context.Context, fn func(RetryEvent), ev RetryEvent) {
	if fn != nil {
		fn(ev)
	}
	if o := RetryObserverFrom(ctx); o != nil {
		o(ctx, ev)
	}
}

// StreamError is a failure reported inside an SSE stream. It carries no HTTP status code, so
// Classify applies the exact same text rules (blacklist before whitelist) to Message and refuses
// to retry unrecognized stream errors.
type StreamError struct {
	Model   string
	Message string
}

func (e *StreamError) Error() string { return e.Message }
func (e *StreamError) Unwrap() error { return nil }

var (
	badRequestPattern = regexp.MustCompile(`(?i)(invalid_request|bad request)`)

	nonRetryableLimitPattern = regexp.MustCompile(`(?i)(` + strings.Join([]string{
		`GoUsageLimitError`,
		`FreeUsageLimitError`,
		`Monthly usage limit reached`,
		`usage limit`,
		`available balance`,
		`insufficient_quota`,
		`out of budget`,
		`quota exceeded`,
		`billing`,
		`subscription_sharing_usage_limit_exceeded`,
	}, "|") + `)`)

	contextLengthPattern = regexp.MustCompile(`(?i)(context_length_exceeded|too many tokens|maximum context)`)

	retryableTransportPattern = regexp.MustCompile(`(?i)(` + strings.Join([]string{
		`overloaded`,
		`server_busy`,
		`servers are currently busy`,
		`currently experiencing high demand`,
		`model is at capacity`,
		`rate.?limit`,
		`too many requests`,
		`\b(?:429|500|502|503|504|520|524|529)\b`,
		`service.?unavailable`,
		`server.?error`,
		`internal.?error`,
		`provider.?returned.?(?:an.?)?error`,
		`exceeded request buffer limit while retrying upstream`,
		`network.?error`,
		`connection.?error`,
		`connection.?refused`,
		`connection.?reset`,
		`connection.?lost`,
		`broken pipe`,
		`other side closed`,
		`fetch failed`,
		`getaddrinfo`,
		`no such host`,
		`ENOTFOUND`,
		`EAI_AGAIN`,
		`upstream.?connect`,
		`reset before headers`,
		`socket hang up`,
		`socket connection was closed`,
		`timed? out`,
		`timeout`,
		`deadline exceeded`,
		`terminated`,
		`websocket.?closed`,
		`websocket.?error`,
		`unexpected eof`,
		`unexpected end of json input`,
		`\beof\b`,
		`stream ended`,
		`ended without`,
		`http2`,
		`pending stream has been canceled`,
		`retry delay`,
		`you can retry your request`,
		`try your request again`,
		`please retry your request`,
		`ResourceExhausted`,
		`subscription_sharing_usage_unavailable`,
		`subscription_sharing_user_unavailable`,
	}, "|") + `)`)
)

// Classify reports whether err should be retried on the same provider and whether the fallback
// chain should advance to the next provider.
func Classify(ctx context.Context, err error) (retrySame bool, tryNext bool) {
	if err == nil {
		return false, false
	}
	// 1. Caller cancellation or outer retry-budget exhaustion: stop immediately.
	// Note that per-request timeouts leave the outer ctx alive, so they fall through to step 7.
	if (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled) {
		return false, false
	}

	var apiErr *anthropicmodel.APIError
	hasAnth := errors.As(err, &apiErr) && apiErr != nil
	var oaiErr *openai.Error
	hasOAI := errors.As(err, &oaiErr) && oaiErr != nil

	text := err.Error()
	if hasOAI {
		// openai.Error.Error() prints only the HTTP status summary, omitting Code/Type/Message.
		text = text + " " + oaiErr.Code + " " + oaiErr.Type + " " + oaiErr.Message
	}

	var se *StreamError
	if errors.As(err, &se) && se != nil {
		// StreamError carries no HTTP status or sentinel; let the ordered text rules below decide.
		text = se.Message
	}

	// 2. Authentication / permission failure: same provider cannot recover, next provider might.
	if errors.Is(err, anthropicmodel.ErrAuthentication) ||
		(hasAnth && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403)) ||
		(hasOAI && (oaiErr.StatusCode == 401 || oaiErr.StatusCode == 403)) {
		return false, true
	}

	// 3. Malformed request / prompt validation failure: switching providers rarely helps.
	if errors.Is(err, anthropicmodel.ErrBadRequest) ||
		(hasAnth && apiErr.StatusCode == 400) ||
		(hasOAI && oaiErr.StatusCode == 400) ||
		badRequestPattern.MatchString(text) {
		return false, false
	}

	// 4. Account-level quota / billing exhaustion (checked BEFORE 429 status so a 429 carrying
	// insufficient_quota advances to the next provider without wasting same-provider retries).
	if nonRetryableLimitPattern.MatchString(text) {
		return false, true
	}

	// 5. Transient HTTP statuses and provider overload/rate-limit sentinels.
	if errors.Is(err, anthropicmodel.ErrRateLimited) || errors.Is(err, anthropicmodel.ErrOverloaded) {
		return true, true
	}
	if hasAnth && isRetryableStatus(apiErr.StatusCode) {
		return true, true
	}
	if hasOAI && isRetryableStatus(oaiErr.StatusCode) {
		return true, true
	}

	// 6. Context window overflow: surface the real error to the user.
	if contextLengthPattern.MatchString(text) {
		return false, false
	}

	// 7. Transport / mid-stream / per-request timeout errors.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, true
	}
	// An SSE `data:` frame cut mid-JSON by a proxy surfaces from ssestream as *json.SyntaxError
	// (e.g. "invalid character '\n' in string literal" or "unexpected end of JSON input"). Exclude
	// ErrFunctionCallArgs, where the stream finished cleanly and the model itself emitted bad JSON.
	var synErr *json.SyntaxError
	if errors.As(err, &synErr) && !errors.Is(err, openaimodel.ErrFunctionCallArgs) {
		return true, true
	}
	if retryableTransportPattern.MatchString(text) {
		return true, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, true
	}

	// 8. OpenAI SDK error with no HTTP response (connect-phase failure).
	if hasOAI && oaiErr.StatusCode == 0 {
		return true, true
	}

	// 9. Unknown errors are not retried conservatively.
	return false, false
}

func isRetryableStatus(code int) bool {
	return code == 408 || code == 409 || code == 429 || code >= 500
}

// RetryingRuntime retries a failed Complete call on the same inner runtime with exponential
// backoff and full jitter.
type RetryingRuntime struct {
	Inner      Runtime
	MaxRetries int // 0 = single attempt (no retry)
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	OnEvent    func(RetryEvent)
	// sleepFn overrides timer waiting in tests; returns false when ctx ended before d elapsed.
	sleepFn func(ctx context.Context, d time.Duration) bool
}

// Complete executes Inner.Complete and retries transient failures up to MaxRetries times.
func (r *RetryingRuntime) Complete(ctx context.Context, req Request) (*Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 0; ; attempt++ {
		reply, err := r.Inner.Complete(ctx, req)
		if err == nil {
			return reply, nil
		}
		if retrySame, _ := Classify(ctx, err); !retrySame {
			return nil, err
		}
		if attempt >= r.MaxRetries {
			return nil, err
		}
		d := r.backoff(attempt + 1)
		notify(ctx, r.OnEvent, RetryEvent{
			Kind:        "retry",
			From:        describe(r.Inner),
			Attempt:     attempt + 1,
			MaxAttempts: r.MaxRetries,
			DelayMs:     d.Milliseconds(),
			Err:         err,
		})
		if !r.sleep(ctx, d) {
			// Return the last real upstream error rather than ctx.Err().
			return nil, err
		}
	}
}

// Describe forwards the inner runtime's description.
func (r *RetryingRuntime) Describe() string { return describe(r.Inner) }

// backoff computes base * 2^(attempt-1) capped at MaxDelay, scaled by jitter in [0.5, 1.5), and
// clamped to MaxDelay again so LLM_RETRY_MAX_MS is a hard upper bound on any single sleep.
func (r *RetryingRuntime) backoff(attempt int) time.Duration {
	if r.BaseDelay <= 0 || r.MaxDelay <= 0 {
		return 0
	}
	var d time.Duration
	if attempt >= 63 {
		d = r.MaxDelay
	} else {
		d = r.BaseDelay << (attempt - 1)
		if d <= 0 || d > r.MaxDelay {
			d = r.MaxDelay
		}
	}
	jittered := time.Duration(float64(d) * (0.5 + rand.Float64()))
	if jittered > r.MaxDelay {
		return r.MaxDelay
	}
	return jittered
}

func (r *RetryingRuntime) sleep(ctx context.Context, d time.Duration) bool {
	if r.sleepFn != nil {
		return r.sleepFn(ctx, d)
	}
	if d <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// FallbackRuntime tries Chain in order; each member carries its own retry policy.
// Even a single-provider setup uses FallbackRuntime so "give-up" events and metrics have one home.
type FallbackRuntime struct {
	Chain   []Runtime
	OnEvent func(RetryEvent)
}

// Complete tries each runtime in Chain until one succeeds or an error cannot advance.
func (f *FallbackRuntime) Complete(ctx context.Context, req Request) (*Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for i, rt := range f.Chain {
		callReq := req
		if i > 0 {
			callReq = stripThinking(req)
		}
		reply, err := rt.Complete(ctx, callReq)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		if _, tryNext := Classify(ctx, err); !tryNext {
			if ctx == nil || ctx.Err() == nil {
				notify(ctx, f.OnEvent, RetryEvent{Kind: "give-up", From: describe(rt), Err: err})
			}
			return nil, err
		}
		if i+1 < len(f.Chain) {
			notify(ctx, f.OnEvent, RetryEvent{
				Kind: "fallback",
				From: describe(rt),
				To:   describe(f.Chain[i+1]),
				Err:  err,
			})
		}
	}
	notify(ctx, f.OnEvent, RetryEvent{Kind: "give-up", From: describe(f.Chain[len(f.Chain)-1]), Err: lastErr})
	return nil, lastErr
}

// Describe derives a static, race-free label from Chain. Single-element chains omit the wrapper.
func (f *FallbackRuntime) Describe() string {
	if len(f.Chain) == 1 {
		return describe(f.Chain[0])
	}
	names := make([]string, len(f.Chain))
	for i, rt := range f.Chain {
		names[i] = describe(rt)
	}
	return "chain(" + strings.Join(names, ">") + ")"
}

func stripThinking(req Request) Request {
	out := req
	out.Messages = make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		parts := make([]Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			if p.Type == PartThinking {
				continue
			}
			parts = append(parts, p)
		}
		m.Parts = parts
		out.Messages[i] = m
	}
	return out
}

type budgetRuntime struct {
	inner   Runtime
	d       time.Duration
	OnEvent func(RetryEvent)
}

// WrapBudget caps the wall-clock duration of one Complete call across the entire chain.
// Non-positive durations return rt unchanged.
func WrapBudget(rt Runtime, d time.Duration, onEvent func(RetryEvent)) Runtime {
	if d <= 0 {
		return rt
	}
	return &budgetRuntime{inner: rt, d: d, OnEvent: onEvent}
}

func (b *budgetRuntime) Complete(ctx context.Context, req Request) (*Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callerAlive := ctx.Err() == nil
	bctx, cancel := context.WithTimeout(ctx, b.d)
	defer cancel()
	msg, err := b.inner.Complete(bctx, req)
	if err != nil && callerAlive && ctx.Err() == nil && errors.Is(bctx.Err(), context.DeadlineExceeded) {
		notify(bctx, b.OnEvent, RetryEvent{
			Kind:    "budget-exhausted",
			From:    describe(b.inner),
			DelayMs: b.d.Milliseconds(),
			Err:     err,
		})
	}
	return msg, err
}

func (b *budgetRuntime) Describe() string { return describe(b.inner) }
