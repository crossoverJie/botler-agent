package push

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// notifyTimeout bounds one retry/fallback notice. If the channel is slow or unreachable, the HTTP
// request is cancelled so the notice never holds up the task.
const notifyTimeout = 3 * time.Second

// Notify sends a short, best-effort notice on the recipient's OWN channel only.
// Unlike Deliver, it never degrades to another channel: a transient "retrying" hint arriving on a
// different channel than the active conversation is worse than silence.
//
// ctx comes from notifier.base, which is already detached from task cancellation while retaining
// close()'s drain-bound cancel; do NOT wrap it in context.WithoutCancel here.
func Notify(ctx context.Context, r Recipient, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s, ok := SenderFor(r.Source)
	if !ok {
		return fmt.Errorf("%s: channel not available", r.Source)
	}
	if !s.Configured() {
		return fmt.Errorf("%s: channel not configured", r.Source)
	}
	if r.UserID == "" {
		return errors.New("recipient address is empty")
	}

	sendCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	return s.Send(sendCtx, r.UserID, Payload{Text: text}, false)
}
