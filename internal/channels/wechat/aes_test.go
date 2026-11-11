package wechat

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

var key16 = mustHex("0123456789abcdef0123456789abcdef")

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseInboundAESKeyAcceptsHex(t *testing.T) {
	got, err := ParseInboundAESKey("0123456789abcdef0123456789abcdef", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key16) {
		t.Fatalf("key = %x", got)
	}
}

func TestParseInboundAESKeyAcceptsBase64OfRawBytes(t *testing.T) {
	got, err := ParseInboundAESKey("", base64.StdEncoding.EncodeToString(key16))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key16) {
		t.Fatalf("key = %x", got)
	}
}

func TestParseInboundAESKeyAcceptsBase64OfHexString(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	got, err := ParseInboundAESKey("", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key16) {
		t.Fatalf("key = %x", got)
	}
}

func TestParseInboundAESKeyPrefersHex(t *testing.T) {
	got, err := ParseInboundAESKey("0123456789abcdef0123456789abcdef", base64.StdEncoding.EncodeToString([]byte("short")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key16) {
		t.Fatalf("key = %x", got)
	}
}

func TestParseInboundAESKeyRejectsBadKeys(t *testing.T) {
	cases := []struct{ hex, b64 string }{
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", ""}, // non-hex
		{"abcd", ""},                             // too short
		{"", base64.StdEncoding.EncodeToString([]byte("hello"))},                            // 5 bytes
		{"", base64.StdEncoding.EncodeToString([]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"))}, // 32 non-hex bytes
		{"", ""}, // no key at all
	}
	for _, c := range cases {
		if _, err := ParseInboundAESKey(c.hex, c.b64); err == nil {
			t.Fatalf("want error for hex=%q b64=%q", c.hex, c.b64)
		}
	}
}
