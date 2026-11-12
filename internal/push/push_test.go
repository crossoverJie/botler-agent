package push

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withConfigDir isolates contacts.json: it lives in the externalized config dir, never in DATA_ROOT.
func withConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOTLER_CONFIG_DIR", dir)
	return dir
}

// withSenders swaps the process-global registry so a test result never depends on which channel
// adapters happened to register themselves in another test's init().
func withSenders(t *testing.T) {
	t.Helper()
	saved := senders
	senders = map[Source]Sender{}
	t.Cleanup(func() { senders = saved })
}

type sendCall struct {
	address    string
	payload    Payload
	withImages bool
}

type fakeSender struct {
	configured bool
	fail       bool
	calls      []sendCall
}

func (f *fakeSender) Configured() bool { return f.configured }

func (f *fakeSender) Send(address string, p Payload, withImages bool) error {
	f.calls = append(f.calls, sendCall{address: address, payload: p, withImages: withImages})
	if f.fail {
		return errors.New("send failed")
	}
	return nil
}

func TestDeliverUsesPrimaryChannel(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	wx := &fakeSender{configured: true}
	RegisterSender(SourceWeChat, wx)

	res := Deliver(Payload{Text: "hi", Images: []string{"a.png"}},
		Recipient{Source: SourceWeChat, UserID: "wx-1"})

	if !res.OK || res.Via != SourceWeChat {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(wx.calls) != 1 || wx.calls[0].address != "wx-1" {
		t.Fatalf("unexpected calls: %+v", wx.calls)
	}
	// WeChat is the only channel that can carry images, so the primary must get them.
	if !wx.calls[0].withImages {
		t.Fatal("images must be sent on the primary WeChat channel")
	}
}

func TestDeliverSendsTextOnlyOnNonWeChatPrimary(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	tg := &fakeSender{configured: true}
	RegisterSender(SourceTelegram, tg)

	res := Deliver(Payload{Text: "hi", Images: []string{"a.png"}},
		Recipient{Source: SourceTelegram, UserID: "42"})

	if !res.OK || res.Via != SourceTelegram {
		t.Fatalf("unexpected result: %+v", res)
	}
	if tg.calls[0].withImages {
		t.Fatal("images must not be sent on Telegram")
	}
}

func TestDeliverDegradesToFallbackChannel(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	wx := &fakeSender{configured: true, fail: true}
	tg := &fakeSender{configured: true}
	RegisterSender(SourceWeChat, wx)
	RegisterSender(SourceTelegram, tg)
	RecordContact(SourceTelegram, "42")

	res := Deliver(Payload{Text: "hi", Images: []string{"a.png"}},
		Recipient{Source: SourceWeChat, UserID: "wx-1"})

	if !res.OK || res.Via != SourceTelegram {
		t.Fatalf("expected degradation to telegram, got %+v", res)
	}
	if len(tg.calls) != 1 || tg.calls[0].address != "42" {
		t.Fatalf("unexpected fallback calls: %+v", tg.calls)
	}
	// A degraded channel receives plain text only: v1 never forwards images off the primary.
	if tg.calls[0].withImages || len(tg.calls[0].payload.Images) != 0 {
		t.Fatalf("fallback must be text-only: %+v", tg.calls[0])
	}
}

func TestDeliverTriesEveryKnownContact(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	RegisterSender(SourceWeChat, &fakeSender{configured: true, fail: true})
	// The first address fails, the second succeeds: one bad address must not abort the chain.
	conditional := &conditionalSender{failTimes: 1, inner: &fakeSender{configured: true}}
	RegisterSender(SourceTelegram, conditional)
	RecordContact(SourceTelegram, "first")
	RecordContact(SourceTelegram, "second")

	res := Deliver(Payload{Text: "hi"}, Recipient{Source: SourceWeChat, UserID: "wx-1"})

	if !res.OK || res.Via != SourceTelegram {
		t.Fatalf("expected success on the second address, got %+v", res)
	}
	if len(conditional.inner.calls) != 1 || conditional.inner.calls[0].address != "second" {
		t.Fatalf("expected only the second address to be tried after the first failed: %+v", conditional.inner.calls)
	}
}

// conditionalSender fails its first N sends, then delegates.
type conditionalSender struct {
	failTimes int
	inner     *fakeSender
}

func (c *conditionalSender) Configured() bool { return true }

func (c *conditionalSender) Send(address string, p Payload, withImages bool) error {
	if c.failTimes > 0 {
		c.failTimes--
		return errors.New("transient")
	}
	return c.inner.Send(address, p, withImages)
}

func TestDeliverSkipsUnconfiguredAndUnknownContacts(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	RegisterSender(SourceWeChat, &fakeSender{configured: true, fail: true})
	RegisterSender(SourceTelegram, &fakeSender{configured: false}) // credentials missing
	RegisterSender(SourceFeishu, &fakeSender{configured: true})    // configured but nobody recorded

	res := Deliver(Payload{Text: "hi"}, Recipient{Source: SourceWeChat, UserID: "wx-1"})
	if res.OK {
		t.Fatalf("expected failure, got %+v", res)
	}
	if res.Error != "send failed" {
		t.Fatalf("the primary error must be reported, got %q", res.Error)
	}
}

func TestDeliverUnregisteredPrimaryDegrades(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	tg := &fakeSender{configured: true}
	RegisterSender(SourceTelegram, tg)
	RecordContact(SourceTelegram, "42")

	res := Deliver(Payload{Text: "hi"}, Recipient{Source: SourceFeishu, UserID: "chat-1"})
	if !res.OK || res.Via != SourceTelegram {
		t.Fatalf("expected degradation, got %+v", res)
	}
	if res.Error != "" {
		t.Fatalf("a degraded success must not carry an error, got %q", res.Error)
	}
}

func TestDeliverEmptyAddressDegrades(t *testing.T) {
	withConfigDir(t)
	withSenders(t)
	RegisterSender(SourceTelegram, &fakeSender{configured: true})

	res := Deliver(Payload{Text: "hi"}, Recipient{Source: SourceWeChat})
	if res.OK {
		t.Fatalf("expected failure, got %+v", res)
	}
	if res.Error != "recipient address is empty" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}

func TestRecordContactDedupesAndPersists(t *testing.T) {
	dir := withConfigDir(t)
	RecordContact(SourceWeChat, "") // ignored: no address to remember
	RecordContact(SourceWeChat, "wx-1")
	RecordContact(SourceWeChat, "wx-1")
	RecordContact(SourceWeChat, "wx-2")
	RecordContact(SourceTelegram, "42")

	if got := Contacts(SourceWeChat); len(got) != 2 || got[0] != "wx-1" || got[1] != "wx-2" {
		t.Fatalf("unexpected wechat contacts: %v", got)
	}
	if got := Contacts(SourceTelegram); len(got) != 1 || got[0] != "42" {
		t.Fatalf("unexpected telegram contacts: %v", got)
	}
	if got := Contacts(SourceFeishu); len(got) != 0 {
		t.Fatalf("unexpected feishu contacts: %v", got)
	}

	// The file is the hand-editable source of truth and must not be world readable.
	path := filepath.Join(dir, "contacts.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("contacts.json must be 0600, got %o", mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string][]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("contacts.json is not valid JSON: %v", err)
	}
	if len(parsed["wechat"]) != 2 || len(parsed["telegram"]) != 1 {
		t.Fatalf("unexpected file content: %v", parsed)
	}
}

func TestRemoveContacts(t *testing.T) {
	withConfigDir(t)
	RecordContact(SourceWeChat, "wx-1")
	RemoveContacts(SourceWeChat)
	if got := Contacts(SourceWeChat); len(got) != 0 {
		t.Fatalf("contacts must be gone, got %v", got)
	}
	// Removing an unknown channel is a no-op, not an error.
	RemoveContacts(SourceFeishu)
}

func TestLoadContactsToleratesCorruptFile(t *testing.T) {
	dir := withConfigDir(t)
	if err := os.WriteFile(filepath.Join(dir, "contacts.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Contacts(SourceWeChat); len(got) != 0 {
		t.Fatalf("a corrupt file must degrade to empty, got %v", got)
	}
	// A corrupt file must not block recording a fresh contact (it is simply overwritten).
	RecordContact(SourceWeChat, "wx-9")
	if got := Contacts(SourceWeChat); len(got) != 1 || got[0] != "wx-9" {
		t.Fatalf("unexpected contacts after repair: %v", got)
	}
}

func TestSenderRegistry(t *testing.T) {
	withSenders(t)
	if _, ok := SenderFor(SourceWeChat); ok {
		t.Fatal("the registry must start empty in this test")
	}
	s := &fakeSender{configured: true}
	RegisterSender(SourceWeChat, s)
	got, ok := SenderFor(SourceWeChat)
	if !ok || got != s {
		t.Fatalf("unexpected registry entry: %v %v", got, ok)
	}
}
