package wechat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	ilink "github.com/openilink/openilink-sdk-go"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/images"
)

// cdnBaseURL is the WeChat media CDN root (shared by the inbound download path and, in the SDK,
// the outbound upload path). Not configurable: there is no known reason to point elsewhere.
const cdnBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

const (
	cdnDownloadTimeout    = 15 * time.Second
	cdnDownloadMaxRetries = 2 // initial attempt + 1 retry
)

var cdnHTTPClient = &http.Client{}

// downloadWithRetry GETs the ciphertext with a per-attempt timeout; retries once on
// network/timeout errors. Without a timeout a hung CDN connection would block the long-poll loop.
func downloadWithRetry(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= cdnDownloadMaxRetries; attempt++ {
		body, err := downloadOnce(ctx, rawURL)
		if err == nil {
			return body, nil
		}
		lastErr = err
		// 4xx is not transient; abort immediately.
		if strings.Contains(err.Error(), "client error") {
			return nil, err
		}
		warnf("[wechat] CDN download attempt %d/%d failed: %v", attempt, cdnDownloadMaxRetries, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("CDN download failed")
	}
	return nil, lastErr
}

func downloadOnce(ctx context.Context, rawURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, cdnDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cdnHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, fmt.Errorf("CDN download client error %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN download status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// SniffMime identifies the image format from magic bytes (one implementation: internal/images).
func SniffMime(data []byte) (mimeType, ext string) { return images.SniffMime(data) }

func DownloadInboundImage(ctx context.Context, item ilink.MessageItem) (agentrt.InboundImage, error) {
	if item.Type != ilink.ItemImage {
		return agentrt.InboundImage{}, fmt.Errorf("not an image item")
	}
	img := item.ImageItem
	if img == nil {
		return agentrt.InboundImage{}, fmt.Errorf("image item is empty")
	}
	media := img.Media
	if media == nil || (media.EncryptQueryParam == "" && media.FullURL == "") {
		mediaKeys := "(none)"
		if media != nil {
			mediaKeys = fmt.Sprintf("encrypt_query_param=%q,full_url=%q,aes_key=%v", media.EncryptQueryParam, media.FullURL, media.AESKey != "")
		}
		return agentrt.InboundImage{}, fmt.Errorf("image item has no encrypt_query_param / full_url (media: %s)", mediaKeys)
	}
	key, err := ParseInboundAESKey(img.AESKey, media.AESKey)
	if err != nil {
		return agentrt.InboundImage{}, err
	}
	rawURL := media.FullURL
	if rawURL == "" {
		rawURL = ilink.BuildCDNDownloadURL(cdnBaseURL, media.EncryptQueryParam)
	}
	cipher, err := downloadWithRetry(ctx, rawURL)
	if err != nil {
		return agentrt.InboundImage{}, err
	}
	plain, err := decryptAESECB(cipher, key)
	if err != nil {
		return agentrt.InboundImage{}, err
	}
	mimeType, _ := SniffMime(plain)
	return agentrt.InboundImage{Data: plain, MimeType: mimeType}, nil
}

func downloadRemoteImageToTemp(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := cdnHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("remote image download failed: %d %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), "botler-agent", "wechat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The extension is cosmetic: the upload protocol carries bytes + md5, never a file name.
	ext := ""
	if u, err := url.Parse(rawURL); err == nil {
		ext = filepath.Ext(u.Path)
		if len(ext) > 5 {
			ext = ext[:5]
		}
	}
	name := fmt.Sprintf("%d-%s%s", timeNow().UnixMilli(), randomHex(4), ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
