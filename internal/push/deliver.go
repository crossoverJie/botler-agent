package push

import (
	"errors"
	"fmt"
	"os"
)

// fallbackOrder is the fixed degradation chain. No Feishu sender is registered in this build, so
// channelConfigured reports false for it and it is skipped: the effective v3.0 chain is
// telegram → wechat (design §2.6.1). Registering a sender is all that is needed to restore it.
var fallbackOrder = []Source{SourceTelegram, SourceFeishu, SourceWeChat}

func channelConfigured(src Source) bool {
	s, ok := SenderFor(src)
	return ok && s.Configured()
}

func deliverToChannel(src Source, p Payload, address string, withImages bool) error {
	s, ok := SenderFor(src)
	if !ok {
		return fmt.Errorf("%s: channel not available", src)
	}
	return s.Send(address, p, withImages)
}

// DeliverResult reports the outcome of Deliver.
type DeliverResult struct {
	OK    bool
	Via   Source
	Error string
}

// Deliver sends the payload to the recipient on its primary channel; on failure it degrades to
// the other configured channels in fallbackOrder, trying only channels that are configured and
// have a recorded contact address, and each recorded address until one succeeds. Degraded
// channels receive plain text only; images go out only on the primary WeChat channel.
func Deliver(p Payload, recipient Recipient) DeliverResult {
	primary := recipient.Source
	if recipient.UserID != "" {
		if err := deliverToChannel(primary, p, recipient.UserID, primary == SourceWeChat); err == nil {
			return DeliverResult{OK: true, Via: primary}
		} else {
			fmt.Fprintf(os.Stderr, "[push] primary %s failed: %v\n", primary, err)
			return degrade(p, primary, err)
		}
	}
	return degrade(p, primary, errors.New("recipient address is empty"))
}

func degrade(p Payload, primary Source, primaryErr error) DeliverResult {
	textOnly := Payload{Text: p.Text}
	for _, src := range fallbackOrder {
		if src == primary || !channelConfigured(src) {
			continue
		}
		for _, addr := range Contacts(src) {
			if err := deliverToChannel(src, textOnly, addr, false); err != nil {
				fmt.Fprintf(os.Stderr, "[push] fallback %s -> %s failed: %v\n", src, addr, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "[push] degraded to %s -> %s\n", src, addr)
			return DeliverResult{OK: true, Via: src}
		}
	}
	return DeliverResult{OK: false, Error: primaryErr.Error()}
}
