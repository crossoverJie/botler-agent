package wechat

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	ilink "github.com/openilink/openilink-sdk-go"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// pngMagic is a valid PNG header, enough for SniffMime to report image/png.
var pngMagic = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// silentLogs silences the channel's console sinks for the duration of a test.
func silentLogs(t *testing.T) {
	t.Helper()
	out, err := logf, warnf
	logf, warnf = func(string, ...any) {}, func(string, ...any) {}
	t.Cleanup(func() { logf, warnf = out, err })
}

func TestDownloadOnceStatusHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write(pngMagic)
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	body, err := downloadOnce(context.Background(), srv.URL+"/ok")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(pngMagic) {
		t.Fatalf("unexpected body: %x", body)
	}

	// 4xx is reported as a client error, which the retry loop must treat as terminal.
	if _, err := downloadOnce(context.Background(), srv.URL+"/notfound"); err == nil ||
		!strings.Contains(err.Error(), "client error 404") {
		t.Fatalf("expected a client error, got %v", err)
	}
	// 5xx is a plain status error (retryable).
	if _, err := downloadOnce(context.Background(), srv.URL+"/boom"); err == nil ||
		!strings.Contains(err.Error(), "status 500") {
		t.Fatalf("expected a status error, got %v", err)
	}
	// A malformed URL fails at request construction, before any network call.
	if _, err := downloadOnce(context.Background(), "://nope"); err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
}

func TestDownloadWithRetryRetriesServerErrors(t *testing.T) {
	silentLogs(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(pngMagic)
	}))
	defer srv.Close()

	body, err := downloadWithRetry(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(pngMagic) {
		t.Fatalf("unexpected body: %x", body)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected exactly one retry, got %d requests", got)
	}
}

func TestDownloadWithRetryAbortsOnClientError(t *testing.T) {
	silentLogs(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := downloadWithRetry(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("a 4xx must not be retried, got %d requests", got)
	}
}

func TestDownloadWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	silentLogs(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := downloadWithRetry(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error after the retries are exhausted")
	}
	if got := atomic.LoadInt32(&hits); got != cdnDownloadMaxRetries {
		t.Fatalf("expected %d attempts, got %d", cdnDownloadMaxRetries, got)
	}
}

// imageItem builds an inbound image item with the key in image_item.aeskey (the preferred field).
func imageItem(hexKey string, media *ilink.CDNMedia) ilink.MessageItem {
	return ilink.MessageItem{
		Type:      ilink.ItemImage,
		ImageItem: &ilink.ImageItem{Media: media, AESKey: hexKey},
	}
}

// rewriteTransport sends requests that target the real CDN host to a local test server, so the
// production URL-building path is exercised without any network access.
type rewriteTransport struct{ base string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	base := r.base + u.Path
	if u.RawQuery != "" {
		base += "?" + u.RawQuery
	}
	clone := req.Clone(req.Context())
	parsed, err := http.NewRequestWithContext(req.Context(), req.Method, base, req.Body)
	if err != nil {
		return nil, err
	}
	clone.URL = parsed.URL
	return http.DefaultTransport.RoundTrip(clone)
}

// withCDNClient swaps the package HTTP client for the duration of a test.
func withCDNClient(t *testing.T, client *http.Client) {
	t.Helper()
	saved := cdnHTTPClient
	cdnHTTPClient = client
	t.Cleanup(func() { cdnHTTPClient = saved })
}

func TestDownloadInboundImageDecryptsAndSniffs(t *testing.T) {
	silentLogs(t)
	key := mustHex("0123456789abcdef0123456789abcdef")
	plain := append(append([]byte{}, pngMagic...), []byte("payload")...)
	cipher, err := ilink.EncryptAESECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cipher)
	}))
	defer srv.Close()

	item := imageItem(hex.EncodeToString(key), &ilink.CDNMedia{FullURL: srv.URL})
	img, err := DownloadInboundImage(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if string(img.Data) != string(plain) {
		t.Fatalf("decrypt mismatch: %x", img.Data)
	}
	if img.MimeType != "image/png" {
		t.Fatalf("unexpected mime: %q", img.MimeType)
	}
}

// The key may arrive only in media.aes_key (base64), as the second choice of ParseInboundAESKey.
func TestDownloadInboundImageAcceptsBase64KeyFromMedia(t *testing.T) {
	silentLogs(t)
	key := mustHex("0123456789abcdef0123456789abcdef")
	cipher, err := ilink.EncryptAESECB(pngMagic, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cipher)
	}))
	defer srv.Close()

	// No FullURL: the URL is built from EncryptQueryParam against the fixed CDN root, so the
	// transport redirects that host to the local test server.
	withCDNClient(t, &http.Client{Transport: rewriteTransport{base: srv.URL}})
	item := imageItem("", &ilink.CDNMedia{
		EncryptQueryParam: "qp",
		AESKey:            base64.StdEncoding.EncodeToString(key),
	})
	img, err := DownloadInboundImage(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if img.MimeType != "image/png" {
		t.Fatalf("unexpected mime: %q", img.MimeType)
	}
}

func TestDownloadInboundImageRejectsMalformedItems(t *testing.T) {
	silentLogs(t)
	cases := map[string]ilink.MessageItem{
		"not an image": {Type: ilink.ItemText, TextItem: &ilink.TextItem{Text: "hi"}},
		"empty image":  {Type: ilink.ItemImage},
		"no media":     {Type: ilink.ItemImage, ImageItem: &ilink.ImageItem{}},
		"no url": {Type: ilink.ItemImage, ImageItem: &ilink.ImageItem{
			Media: &ilink.CDNMedia{AESKey: "x"}, AESKey: "0123456789abcdef0123456789abcdef",
		}},
		"bad key": {Type: ilink.ItemImage, ImageItem: &ilink.ImageItem{
			Media: &ilink.CDNMedia{FullURL: "http://127.0.0.1:1/x", AESKey: "not-a-key"}, AESKey: "zz",
		}},
	}
	for name, item := range cases {
		if _, err := DownloadInboundImage(context.Background(), item); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestDownloadRemoteImageToTemp(t *testing.T) {
	silentLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".png") {
			_, _ = w.Write(pngMagic)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	path, err := downloadRemoteImageToTemp(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if !strings.HasSuffix(path, ".png") {
		t.Fatalf("the extension must be carried over from the URL: %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(pngMagic) {
		t.Fatalf("unexpected file content: %x", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("temp image must be 0600, got %o", mode)
	}

	if _, err := downloadRemoteImageToTemp(context.Background(), srv.URL+"/missing"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if _, err := downloadRemoteImageToTemp(context.Background(), "://nope"); err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
}

func TestRandomHex(t *testing.T) {
	a := randomHex(4)
	if len(a) != 8 {
		t.Fatalf("expected 8 hex chars for 4 bytes, got %q", a)
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("not hex: %q", a)
	}
	if a == randomHex(4) {
		t.Fatal("randomHex must not be constant")
	}
}

func TestWechatEnvGetters(t *testing.T) {
	saved := config.C()
	config.Set(&config.Config{WeChatAllowFrom: "a,b", WeChatImageBatchSeconds: 3})
	t.Cleanup(func() { config.Set(saved) })
	if got := wechatAllowFrom(); got != "a,b" {
		t.Fatalf("unexpected allow-from: %q", got)
	}
	// Seconds are converted to milliseconds: ImageBatchCoordinator works in ms.
	if got := wechatImageBatchWindowMs(); got != 3000 {
		t.Fatalf("unexpected window: %d", got)
	}
}
