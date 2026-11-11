// Package push delivers a payload to a recipient, degrading across channels when the primary
// channel fails. It also stores the per-channel known contact addresses used by that fallback.
package push

// Source is a channel name.
type Source string

// Known channels.
const (
	SourceWeChat   Source = "wechat"
	SourceTelegram Source = "telegram"
	SourceFeishu   Source = "feishu"
)

// Recipient is who to push to: `Source` is the channel, `UserID` the address that channel
// understands (Telegram chatId, Feishu chat_id, WeChat ilink_user_id).
type Recipient struct {
	Source Source `json:"source"`
	UserID string `json:"userId"`
}

// Payload is what to push.
type Payload struct {
	Text   string
	Images []string
}

// Sender is implemented by each channel adapter and registered at startup, so push never
// imports the channels (and the channels never import push's delivery logic).
type Sender interface {
	// Configured reports whether the channel has enough credentials to send.
	Configured() bool
	// Send delivers the payload to one address. It must return an error on failure so the
	// caller can fall back to another channel.
	Send(address string, p Payload, withImages bool) error
}

var senders = map[Source]Sender{}

// RegisterSender wires a channel adapter into the fallback chain.
func RegisterSender(src Source, s Sender) { senders[src] = s }

// SenderFor returns the registered sender for a channel.
func SenderFor(src Source) (Sender, bool) {
	s, ok := senders[src]
	return s, ok
}
