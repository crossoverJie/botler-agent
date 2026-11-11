package wechat

import (
	"testing"
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
