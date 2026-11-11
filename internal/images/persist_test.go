package images

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

func withDataRoot(t *testing.T, projects ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Config{DataRoot: root})
	paths.ResetCache()
	t.Cleanup(paths.ResetCache)
	return root
}

var jpegBytes = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}

var persistedNameRe = regexp.MustCompile(`^cook/photos/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}\.jpg$`)

func TestPersistInboundImageWritesUnderPhotos(t *testing.T) {
	root := withDataRoot(t, "cook")
	rel, err := PersistInboundImage(agentrt.InboundImage{Data: jpegBytes, MimeType: "image/jpeg"}, "cook")
	if err != nil {
		t.Fatal(err)
	}
	if !persistedNameRe.MatchString(rel) {
		t.Fatalf("unexpected relative path %q", rel)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("expected the file to exist: %v", err)
	}
	if string(raw) != string(jpegBytes) {
		t.Fatal("persisted bytes differ from the received image")
	}
}

func TestPersistInboundImageRejectsEscapingProject(t *testing.T) {
	withDataRoot(t, "cook")
	if _, err := PersistInboundImage(agentrt.InboundImage{Data: jpegBytes}, "../evil"); err == nil {
		t.Fatal("expected an out-of-bounds rejection")
	}
	if _, err := PersistInboundImage(agentrt.InboundImage{Data: jpegBytes}, "other"); err == nil {
		t.Fatal("expected the unlisted project to be rejected")
	}
}

func TestPersistInboundImageSniffsExtension(t *testing.T) {
	withDataRoot(t, "cook")
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	rel, err := PersistInboundImage(agentrt.InboundImage{Data: png}, "cook")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rel, ".png") {
		t.Fatalf("expected a png extension, got %q", rel)
	}
}

// The inbound whitelist must not allow bmp (the outbound reply path does).
func TestSniffMimeFallsBackToJPEG(t *testing.T) {
	mime, ext := SniffMime([]byte("BM\x00\x00"))
	if mime != "image/jpeg" || ext != "jpg" {
		t.Fatalf("unexpected fallback (%s,%s)", mime, ext)
	}
}
