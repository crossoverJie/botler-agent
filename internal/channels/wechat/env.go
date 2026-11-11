package wechat

import (
	"fmt"
	"os"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
)

// agentrtImage is the inbound image type shared with dispatch / the runner (data + mime type;
// the file extension is re-sniffed when the photo is persisted).
type agentrtImage = agentrt.InboundImage

// wechatAllowFrom is the extra ilink_user_id allowlist (comma-separated); the account owner is
// always allowed on top of it.
func wechatAllowFrom() string { return config.C().WeChatAllowFrom }

// wechatImageBatchWindowMs is the image batching window in milliseconds
// (WECHAT_IMAGE_BATCH_SECONDS × 1000; 0 disables batching).
func wechatImageBatchWindowMs() int { return config.C().WeChatImageBatchSeconds * 1000 }

// logf / warnf are the channel's console sinks. They are variables so tests can silence them.
var logf = func(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

var warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
