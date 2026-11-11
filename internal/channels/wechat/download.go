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
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/prompt"
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

// SniffMime identifies the image format from magic bytes; unrecognized data falls back to JPEG.
func SniffMime(data []byte) (mimeType, ext string) {
	if len(data) >= 8 && string(data[0:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png", "png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg", "jpg"
	}
	if len(data) >= 4 && string(data[0:4]) == "GIF8" {
		return "image/gif", "gif"
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", "webp"
	}
	return "image/jpeg", "jpg"
}

// DownloadInboundImage downloads + decrypts one inbound IMAGE item. It throws when the item is
// not an image or cannot be decoded, so the caller can warn and continue with the rest.
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

const photosSubdir = "photos"

// inboundExts is the inbound-only extension whitelist (no bmp, unlike the outbound reply path).
var inboundExts = map[string]bool{"jpg": true, "png": true, "gif": true, "webp": true}

// PersistInboundImage writes a decoded image under DATA_ROOT/<project>/photos/<date>-<rand>.<ext>
// through paths.SafePath (first-level subdir allowlist) plus an image-extension whitelist, so the
// file can never escape DATA_ROOT or be written with a non-image extension. The date uses the
// agent's local-timezone today(), matching the filenames the agent writes into its records.
// Returns the RELATIVE path for the agent to cite.
func PersistInboundImage(img agentrt.InboundImage, project string) (string, error) {
	_, ext := SniffMime(img.Data)
	if !inboundExts[ext] {
		ext = "jpg"
	}
	randBytes := make([]byte, 4)
	if _, err := rand.Read(randBytes); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.%s", prompt.Today(), hex.EncodeToString(randBytes), ext)
	rel := filepath.ToSlash(filepath.Join(project, photosSubdir, name))
	abs, err := paths.SafePath(rel, paths.Options{Projects: []string{project}})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, img.Data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// downloadRemoteImageToTemp downloads an https image referenced by a reply so it can go through
// the same upload pipeline as a local file.
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
