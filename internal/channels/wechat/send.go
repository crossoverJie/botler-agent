package wechat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	ilink "github.com/openilink/openilink-sdk-go"

	"github.com/crossoverJie/botler-agent/internal/push"
)

var httpsURLRe = regexp.MustCompile(`(?i)^https://`)

// ilinkSender is the subset of *ilink.Client the reply path uses. Narrowing it to an interface is
// what makes the monitor testable: every *ilink.Client call needs a live long poll, so a fake is the
// only way to exercise processMessage / dispatchAndReply / runTask.
type ilinkSender interface {
	SendText(ctx context.Context, to, text, contextToken string) (string, error)
	UploadFile(ctx context.Context, plaintext []byte, toUserID string, mediaType ilink.UploadMediaType) (*ilink.UploadResult, error)
	SendImage(ctx context.Context, to, contextToken string, uploaded *ilink.UploadResult) (string, error)
}

// newClient builds an SDK client bound to one resolved account.
func newClient(acct ResolvedAccount) *ilink.Client {
	client := ilink.NewClient(acct.Token)
	client.SetBaseURL(acct.BaseURL)
	return client
}

// sendText sends plain text. contextToken is required: it must echo the inbound message's
// context_token, otherwise WeChat cannot associate the reply with the conversation.
func sendText(ctx context.Context, client ilinkSender, to, text, contextToken string) error {
	if contextToken == "" {
		return errors.New("sendText: contextToken is required")
	}
	_, err := client.SendText(ctx, to, text, contextToken)
	return err
}

// sendImageFile uploads a local image file to the WeChat CDN and sends it as an image message.
func sendImageFile(ctx context.Context, client ilinkSender, to, localPath, contextToken string) error {
	if contextToken == "" {
		return errors.New("sendImageFile: contextToken is required")
	}
	plain, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read image %s: %w", localPath, err)
	}
	uploaded, err := client.UploadFile(ctx, plain, to, ilink.MediaImage)
	if err != nil {
		return fmt.Errorf("upload image: %w", err)
	}
	if _, err := client.SendImage(ctx, to, contextToken, uploaded); err != nil {
		return fmt.Errorf("send image: %w", err)
	}
	logf("[wechat] image uploaded: %s (%d bytes, filekey=%s)", localPath, len(plain), uploaded.FileKey)
	return nil
}

// pushSender is the push package's WeChat sender: proactive delivery (scheduled results, renewal
// reminders) reusing the cached context token, with the same markdown stripping as the reply path.
type pushSender struct{}

func (pushSender) Configured() bool { return ResolveAccount().Configured }

func (pushSender) Send(address string, p push.Payload, withImages bool) error {
	acct := ResolveAccount()
	if !acct.Configured {
		return errors.New("wechat: account not configured")
	}
	entry, ok := GetContext(address)
	if !ok || entry.Token == "" {
		return errors.New("wechat: no cached context_token for this user")
	}
	client := newClient(acct)
	ctx := context.Background()
	if text := MarkdownToPlainText(p.Text); len(text) > 0 {
		plain := strings.TrimSpace(text)
		if plain != "" {
			if err := sendText(ctx, client, address, plain, entry.Token); err != nil {
				return err
			}
		}
	}
	if withImages {
		for _, img := range p.Images {
			localPath := img
			if httpsURLRe.MatchString(img) {
				tmp, err := downloadRemoteImageToTemp(ctx, img)
				if err != nil {
					return err
				}
				localPath = tmp
			}
			if err := sendImageFile(ctx, client, address, localPath, entry.Token); err != nil {
				return err
			}
		}
	}
	return nil
}

// RegisterPushSender wires the WeChat sender into push's fallback chain. Called from init so a
// scheduled push can reach WeChat even when the monitor goroutine was never started.
func RegisterPushSender() { push.RegisterSender(push.SourceWeChat, pushSender{}) }

func init() { RegisterPushSender() }
