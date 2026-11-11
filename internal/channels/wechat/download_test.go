package wechat

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
)

func TestSniffMime(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
		ext  string
	}{
		{"png", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, "image/png", "png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg", "jpg"},
		{"gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}, "image/gif", "gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp", "webp"},
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03}, "image/jpeg", "jpg"},
	}
	for _, c := range cases {
		mimeType, ext := SniffMime(c.data)
		if mimeType != c.mime || ext != c.ext {
			t.Fatalf("%s: got (%s,%s), want (%s,%s)", c.name, mimeType, ext, c.mime, c.ext)
		}
	}
}

var persistedNameRe = regexp.MustCompile(`^cook/photos/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}\.jpg$`)

func TestPersistInboundImageWritesUnderDataRootPhotos(t *testing.T) {
	data := []byte("fake-jpeg-bytes")
	rel, err := PersistInboundImage(agentrt.InboundImage{Data: data, MimeType: "image/jpeg"}, "cook")
	if err != nil {
		t.Fatal(err)
	}
	if !persistedNameRe.MatchString(rel) {
		t.Fatalf("relative path = %q", rel)
	}
	abs := filepath.Join(config.C().DataRoot, filepath.FromSlash(rel))
	written, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}
	if string(written) != string(data) {
		t.Fatal("persisted bytes differ")
	}
}

func TestPersistInboundImageRejectsProjectOutsideAllowlist(t *testing.T) {
	if _, err := PersistInboundImage(agentrt.InboundImage{Data: []byte("x")}, "../evil"); err == nil {
		t.Fatal("want an error for a project outside the allowlist")
	}
}

func TestPersistInboundImageUsesPngExtensionForPngBytes(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	rel, err := PersistInboundImage(agentrt.InboundImage{Data: png, MimeType: "image/png"}, "cook")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(rel) != ".png" {
		t.Fatalf("path = %q, want a .png extension", rel)
	}
}
