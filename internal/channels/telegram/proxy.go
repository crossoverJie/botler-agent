package telegram

import (
	"net/http"
	"net/url"
	"os"
)

// ResolveProxy returns the configured proxy address, in v1 priority order:
// TG_PROXY > HTTPS_PROXY > https_proxy > HTTP_PROXY > http_proxy.
//
// TG_PROXY is a custom key: http.ProxyFromEnvironment does not understand it, so the proxy must be
// injected into the transport by hand.
func ResolveProxy() string {
	for _, key := range []string{"TG_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := envValue(key); v != "" {
			return v
		}
	}
	return ""
}

// newHTTPClient builds the Bot API client. There is no client-wide timeout: long polling needs a
// deadline longer than the server-side poll window, so every request carries its own context
// deadline instead.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy := ResolveProxy()
	// Without a configured proxy the Bot API is reached directly (v1 does the same: Node's fetch
	// ignores HTTP(S)_PROXY unless an agent is attached, which only happens when resolveProxy hits).
	transport.Proxy = nil
	if proxy != "" {
		if u, err := url.Parse(proxy); err != nil {
			errf("[telegram] Ignoring invalid proxy %q: %v", proxy, err)
		} else if u.Scheme == "http" || u.Scheme == "https" {
			transport.Proxy = http.ProxyURL(u)
			logf("[telegram] Using proxy: %s", proxy)
		} else {
			errf("[telegram] Ignoring unsupported proxy scheme %q (only http/https proxies are supported)", u.Scheme)
		}
	}
	return &http.Client{Transport: transport}
}

// envValue reads a process env var (overridable in tests).
func envValue(key string) string { return os.Getenv(key) }
