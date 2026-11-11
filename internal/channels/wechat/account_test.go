package wechat

import (
	"testing"

	"github.com/crossoverJie/botler-agent/internal/push"
)

func TestApplyNewAccountDropsOldOwnerStateButKeepsNewAccount(t *testing.T) {
	// Pre-populate the OLD owner's persisted state.
	SaveAccount(AccountUpdate{Token: "old-token", BaseURL: "https://old", UserID: "old-user"})
	SaveSyncBuf("cursor-123")
	UpdateContext("old-user", "old-context-token")
	push.RecordContact(push.SourceWeChat, "old-user")

	if got := ResolveAccount().Token; got != "old-token" {
		t.Fatalf("token = %q", got)
	}
	if got := LoadSyncBuf(); got != "cursor-123" {
		t.Fatalf("sync buf = %q", got)
	}
	if _, ok := GetContext("old-user"); !ok {
		t.Fatal("old context should exist before re-pair")
	}
	if got := push.Contacts(push.SourceWeChat); len(got) != 1 || got[0] != "old-user" {
		t.Fatalf("contacts = %v", got)
	}

	ApplyNewAccount(AccountUpdate{Token: "new-token", BaseURL: "https://new", UserID: "new-user"})

	// The new account is persisted (and stamped), not deleted.
	saved := LoadAccount()
	if saved == nil {
		t.Fatal("account must still exist")
	}
	if saved.Token != "new-token" || saved.UserID != "new-user" || saved.BaseURL != "https://new" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.SavedAt == "" {
		t.Fatal("savedAt must be stamped")
	}
	if got := ResolveAccount().Token; got != "new-token" {
		t.Fatalf("token = %q", got)
	}

	// The old owner's stale state is dropped.
	if _, ok := GetContext("old-user"); ok {
		t.Fatal("old context token must be cleared")
	}
	if got := push.Contacts(push.SourceWeChat); len(got) != 0 {
		t.Fatalf("contacts = %v, want empty", got)
	}
	if got := LoadSyncBuf(); got != "" {
		t.Fatalf("sync buf = %q, want empty", got)
	}

	ClearAccount()
}

func TestSaveAccountKeepsExistingTokenWhenUpdateOmitsIt(t *testing.T) {
	SaveAccount(AccountUpdate{Token: "tok-1", BaseURL: "https://a", UserID: "u1"})
	SaveAccount(AccountUpdate{BaseURL: "https://b"})
	saved := LoadAccount()
	if saved == nil || saved.Token != "tok-1" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.BaseURL != "https://b" {
		t.Fatalf("baseUrl = %q", saved.BaseURL)
	}
	ClearAccount()
}

func TestResolveAccountDefaults(t *testing.T) {
	ClearAccount()
	resolved := ResolveAccount()
	if resolved.BaseURL != DefaultBaseURL {
		t.Fatalf("baseUrl = %q", resolved.BaseURL)
	}
	if resolved.Configured || resolved.Token != "" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestUpdateContextAndMarkReminded(t *testing.T) {
	ClearContexts()
	UpdateContext("u1", "tok-1")
	entry, ok := GetContext("u1")
	if !ok || entry.Token != "tok-1" || entry.LastMsgAt == 0 {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.LastRemindedAt != nil {
		t.Fatal("lastRemindedAt must start nil")
	}
	MarkReminded("u1")
	entry, _ = GetContext("u1")
	if entry.LastRemindedAt == nil || *entry.LastRemindedAt < entry.LastMsgAt {
		t.Fatalf("entry = %+v", entry)
	}
	ClearContexts()
	if _, ok := GetContext("u1"); ok {
		t.Fatal("contexts must be cleared")
	}
}
