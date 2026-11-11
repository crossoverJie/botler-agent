// Package wechat implements the WeChat (iLink) channel: QR login, long-poll monitoring, inbound
// image handling, outbound text/image replies, the 24h-window renewal reminder and the WebUI
// pairing state machine.
//
// The wire protocol layer (HTTP endpoints, CDN crypto, message structs) comes from
// github.com/openilink/openilink-sdk-go; everything the framework decides (allowlist, context
// token persistence, batching, markdown stripping, photo persistence, pairing) lives here and
// mirrors v1's src/channels/wechat business layer.
package wechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// DefaultBaseURL is the iLink Bot API endpoint used until a login returns a specific one.
const DefaultBaseURL = "https://ilinkai.weixin.qq.com"

func wechatDir() string {
	return filepath.Join(config.UserConfigDir(), "wechat")
}

func accountPath() string { return filepath.Join(wechatDir(), "account.json") }
func syncPath() string    { return filepath.Join(wechatDir(), "sync.json") }

// AccountData is the persisted account (wechat/account.json).
type AccountData struct {
	Token   string `json:"token,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	// UserID is the ilink_user_id of the person who scanned the QR (the account owner).
	UserID  string `json:"userId,omitempty"`
	SavedAt string `json:"savedAt,omitempty"`
}

// AccountUpdate is a login result to merge into the persisted account.
type AccountUpdate struct {
	Token   string
	BaseURL string
	UserID  string
}

// LoadAccount reads the persisted account, or nil when absent/corrupt.
func LoadAccount() *AccountData {
	raw, err := os.ReadFile(accountPath())
	if err != nil {
		return nil
	}
	var data AccountData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}
	return &data
}

// SaveAccount persists account data after QR login (merges into the existing file).
// Mirrors v1: a non-empty token also stamps savedAt, and the token falls back to the existing
// one; baseUrl / userId are only written when non-empty.
func SaveAccount(update AccountUpdate) {
	if err := os.MkdirAll(wechatDir(), 0o700); err != nil {
		warnf("[wechat] create config dir failed: %v", err)
		return
	}
	existing := LoadAccount()
	data := AccountData{}
	if existing != nil {
		data = *existing
	}
	if t := strings.TrimSpace(update.Token); t != "" {
		data.Token = t
		data.SavedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if u := strings.TrimSpace(update.BaseURL); u != "" {
		data.BaseURL = u
	}
	if id := strings.TrimSpace(update.UserID); id != "" {
		data.UserID = id
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	path := accountPath()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		warnf("[wechat] save account failed: %v", err)
		return
	}
	_ = os.Chmod(path, 0o600)
}

// ClearAccount removes stored credentials, the sync cursor, the cached context tokens and the
// channel's push contact addresses (a re-login must not keep the old user's state).
func ClearAccount() {
	removeFile(accountPath())
	removeFile(syncPath())
	ClearContexts()
	push.RemoveContacts(push.SourceWeChat)
}

// ClearSyncBuf drops the persisted get_updates_buf cursor without touching account.json.
func ClearSyncBuf() { removeFile(syncPath()) }

// ApplyNewAccount applies a fresh login (re-pair) while dropping the old owner's stale state:
// the saved account is kept, but the old context tokens, the old push contact address and the
// old sync cursor are cleared.
func ApplyNewAccount(update AccountUpdate) {
	SaveAccount(update)
	ClearContexts()
	push.RemoveContacts(push.SourceWeChat)
	ClearSyncBuf()
}

// ResolvedAccount is the effective account used by the monitor / senders.
type ResolvedAccount struct {
	BaseURL    string
	Token      string
	UserID     string
	Configured bool
}

// ResolveAccount resolves baseUrl + token + owner userId from the stored account.
func ResolveAccount() ResolvedAccount {
	data := LoadAccount()
	out := ResolvedAccount{BaseURL: DefaultBaseURL}
	if data == nil {
		return out
	}
	if b := strings.TrimSpace(data.BaseURL); b != "" {
		out.BaseURL = b
	}
	out.Token = strings.TrimSpace(data.Token)
	out.UserID = strings.TrimSpace(data.UserID)
	out.Configured = out.Token != ""
	return out
}

// LoadSyncBuf loads the persisted get_updates_buf cursor.
func LoadSyncBuf() string {
	raw, err := os.ReadFile(syncPath())
	if err != nil {
		return ""
	}
	var data struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.GetUpdatesBuf
}

// SaveSyncBuf persists the get_updates_buf cursor.
func SaveSyncBuf(buf string) {
	if err := os.MkdirAll(wechatDir(), 0o700); err != nil {
		return
	}
	body, err := json.Marshal(struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{GetUpdatesBuf: buf})
	if err != nil {
		return
	}
	path := syncPath()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		warnf("[wechat] save sync buf failed: %v", err)
		return
	}
	_ = os.Chmod(path, 0o600)
}

func removeFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		warnf("[wechat] remove %s failed: %v", path, err)
	}
}
