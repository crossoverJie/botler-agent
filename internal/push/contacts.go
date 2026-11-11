package push

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/crossoverJie/botler-agent/internal/config"
)

type contactsMap map[Source][]string

func contactsPath() string {
	return filepath.Join(config.UserConfigDir(), "contacts.json")
}

func loadContacts() contactsMap {
	out := contactsMap{}
	raw, err := os.ReadFile(contactsPath())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return contactsMap{}
	}
	return out
}

func saveContacts(m contactsMap) {
	if err := os.MkdirAll(config.UserConfigDir(), 0o700); err != nil {
		return
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	path := contactsPath()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		warnf("[push] save contacts failed: %v", err)
		return
	}
	_ = os.Chmod(path, 0o600)
}

// RecordContact records that userID contacted the bot on src (deduped).
func RecordContact(src Source, userID string) {
	if userID == "" {
		return
	}
	m := loadContacts()
	for _, existing := range m[src] {
		if existing == userID {
			return
		}
	}
	m[src] = append(m[src], userID)
	saveContacts(m)
}

// Contacts returns the known addresses for a channel.
func Contacts(src Source) []string {
	return loadContacts()[src]
}

// RemoveContacts forgets all known addresses of a channel (e.g. on WeChat re-login).
func RemoveContacts(src Source) {
	m := loadContacts()
	if _, ok := m[src]; !ok {
		return
	}
	delete(m, src)
	saveContacts(m)
}

var warnf = func(format string, args ...any) {}
