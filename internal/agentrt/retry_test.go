package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model/anthropicmodel"
	"google.golang.org/adk/v2/model/openaimodel"
)

func TestClassifyOrderedRules(t *testing.T) {
	bg := context.Background()
	cancelledCtx, cancel := context.WithCancel(bg)
	cancel()
	var truncatedJSONErr error
	_ = json.Unmarshal([]byte("{\"delta\":\"hel\n"), &struct{}{})
	truncatedJSONErr = json.Unmarshal([]byte("{\"delta\":\"hel\n"), &map[string]any{})

	cases := []struct {
		name      string
		ctx       context.Context
		err       error
		retrySame bool
		tryNext   bool
	}{
		{"nil error", bg, nil, false, false},
		{"cancelled caller ctx", cancelledCtx, errors.New("rate limit 429"), false, false},
		{"context.Canceled error", bg, context.Canceled, false, false},
		{"per-request DeadlineExceeded with live caller ctx", bg, context.DeadlineExceeded, true, true},

		// Auth (401/403) -> tryNext only.
		{"anthropic ErrAuthentication", bg, fmt.Errorf("wrap: %w", anthropicmodel.ErrAuthentication), false, true},
		{"openai 401", bg, &openai.Error{StatusCode: 401}, false, true},
		{"openai 403", bg, &openai.Error{StatusCode: 403}, false, true},

		// Bad request (400) -> neither.
		{"anthropic ErrBadRequest", bg, fmt.Errorf("wrap: %w", anthropicmodel.ErrBadRequest), false, false},
		{"openai 400", bg, &openai.Error{StatusCode: 400, Message: "invalid tool schema"}, false, false},
		{"invalid_request text", bg, errors.New("provider failed: invalid_request_error"), false, false},

		// Blacklist beats whitelist (e.g. 429 or "rate limit" carrying insufficient_quota).
		{"quota blacklist beats rate limit text", bg, errors.New("rate limit exceeded: insufficient_quota"), false, true},
		{"openai 429 with insufficient_quota code", bg, &openai.Error{StatusCode: 429, Code: "insufficient_quota", Message: "Rate limit: quota exceeded"}, false, true},
		{"billing text", bg, errors.New("account billing issue: out of budget"), false, true},

		// Transient statuses and sentinels.
		{"anthropic ErrRateLimited", bg, fmt.Errorf("wrap: %w", anthropicmodel.ErrRateLimited), true, true},
		{"anthropic ErrOverloaded", bg, fmt.Errorf("wrap: %w", anthropicmodel.ErrOverloaded), true, true},
		{"anthropic 502 APIError", bg, &anthropicmodel.APIError{StatusCode: 502, Err: anthropicmodel.ErrAPIStatus}, true, true},
		{"openai 429", bg, &openai.Error{StatusCode: 429}, true, true},
		{"openai 503", bg, &openai.Error{StatusCode: 503}, true, true},
		{"openai connect failure StatusCode 0", bg, &openai.Error{StatusCode: 0}, true, true},

		// Context window overflow -> neither.
		{"context_length_exceeded", bg, errors.New("context_length_exceeded: maximum context is 128000 tokens"), false, false},
		{"too many tokens", bg, errors.New("prompt has too many tokens"), false, false},

		// Transport / mid-stream drops.
		{"io.ErrUnexpectedEOF", bg, io.ErrUnexpectedEOF, true, true},
		{"truncated SSE frame json.SyntaxError", bg, truncatedJSONErr, true, true},
		{"model bad tool args is NOT retried", bg, fmt.Errorf("%w: %w", openaimodel.ErrFunctionCallArgs, truncatedJSONErr), false, false},
		{"connection reset text", bg, errors.New("read tcp: connection reset by peer"), true, true},
		{"stream ended without message_stop", bg, errors.New("Anthropic stream ended before message_stop"), true, true},
		{"net.OpError", bg, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(" custom dial error ")}, true, true},

		// StreamError uses the exact same ordered text rules (never default-retryable).
		{"StreamError invalid_request", bg, &StreamError{Model: "m", Message: "invalid_request_error: empty block"}, false, false},
		{"StreamError insufficient_quota", bg, &StreamError{Model: "m", Message: "insufficient_quota: rate limit"}, false, true},
		{"StreamError context_length_exceeded", bg, &StreamError{Model: "m", Message: "context_length_exceeded"}, false, false},
		{"StreamError stream ended", bg, &StreamError{Model: "m", Message: "stream ended before message_stop"}, true, true},
		{"StreamError unknown text is not retried", bg, &StreamError{Model: "m", Message: "messages: text content blocks must be non-empty"}, false, false},

		// Unknown error -> conservative false/false.
		{"unknown deterministic error", bg, errors.New("something completely unrecognized"), false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSame, gotNext := Classify(tc.ctx, tc.err)
			if gotSame != tc.retrySame || gotNext != tc.tryNext {
				t.Fatalf("Classify(%v) = (%v, %v), want (%v, %v)",
					tc.err, gotSame, gotNext, tc.retrySame, tc.tryNext)
			}
		})
	}

	se := &StreamError{Model: "m", Message: "boom"}
	if se.Error() != "boom" || se.Unwrap() != nil {
		t.Fatalf("unexpected StreamError methods: %q %v", se.Error(), se.Unwrap())
	}
}
