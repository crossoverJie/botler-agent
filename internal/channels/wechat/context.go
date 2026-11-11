package wechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ContextEntry is the persisted per-user context token plus the timestamps the renewal reminder
// loop needs. context_token is the protocol handle that must be echoed on replies; it is tied to
// a rolling 24h window that every inbound message refreshes.
type ContextEntry struct {
	Token          string `json:"token"`
	LastMsgAt      int64  `json:"lastMsgAt"`
	LastRemindedAt *int64 `json:"lastRemindedAt,omitempty"`
}

type contextsMap map[string]ContextEntry

// contextsMu serialises read-modify-write cycles (the monitor, the reminder loop and the WebUI
// all touch this file).
var contextsMu sync.Mutex

func contextsPath() string {
	return filepath.Join(wechatDir(), "contexts.json")
}

func loadContexts() contextsMap {
	out := contextsMap{}
	raw, err := os.ReadFile(contextsPath())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return contextsMap{}
	}
	return out
}

func saveContexts(m contextsMap) {
	if err := os.MkdirAll(wechatDir(), 0o700); err != nil {
		return
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	path := contextsPath()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		warnf("[wechat] save contexts failed: %v", err)
		return
	}
	_ = os.Chmod(path, 0o600)
}

// UpdateContext refreshes a user's token and last-message time (preserving lastRemindedAt).
func UpdateContext(userID, token string) {
	if userID == "" || token == "" {
		return
	}
	contextsMu.Lock()
	defer contextsMu.Unlock()
	m := loadContexts()
	prev := m[userID]
	m[userID] = ContextEntry{Token: token, LastMsgAt: nowMs(), LastRemindedAt: prev.LastRemindedAt}
	saveContexts(m)
}

// MarkReminded records that a renewal reminder was sent for this user now (lastMsgAt untouched).
func MarkReminded(userID string) {
	contextsMu.Lock()
	defer contextsMu.Unlock()
	m := loadContexts()
	prev, ok := m[userID]
	if !ok {
		return
	}
	ts := nowMs()
	prev.LastRemindedAt = &ts
	m[userID] = prev
	saveContexts(m)
}

// GetContext returns the persisted context entry for a user.
func GetContext(userID string) (ContextEntry, bool) {
	contextsMu.Lock()
	defer contextsMu.Unlock()
	m := loadContexts()
	entry, ok := m[userID]
	return entry, ok
}

// ClearContexts removes all persisted context tokens.
func ClearContexts() {
	contextsMu.Lock()
	defer contextsMu.Unlock()
	removeFile(contextsPath())
}

// nowMs is the timestamp used for every persisted lastMsgAt / lastRemindedAt value.
func nowMs() int64 { return timeNow().UnixMilli() }
