package wechat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
)

// TestMain points the config directory and DATA_ROOT at throwaway temp dirs: this package reads and
// writes <config>/wechat/*.json and persists photos through paths.SafePath (which needs a real
// DATA_ROOT with at least one subproject).
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "botler-wechat-test-")
	if err != nil {
		panic(err)
	}
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		root = tmp
	}
	cfgDir := filepath.Join(root, "cfg")
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(dataRoot, "cook"), 0o755); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		panic(err)
	}
	_ = os.Setenv("BOTLER_CONFIG_DIR", cfgDir)
	_ = os.Setenv("DATA_ROOT", dataRoot)
	_ = os.Setenv("WECHAT_ENABLED", "1")
	_ = os.Setenv("WECHAT_IMAGE_BATCH_SECONDS", "60")
	config.Set(config.Load())
	logf = func(string, ...any) {}
	warnf = func(string, ...any) {}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func testImage(seed string) agentrt.InboundImage {
	return agentrt.InboundImage{Data: []byte("fake-image-" + seed), MimeType: "image/jpeg"}
}

func TestImageMessageIsBufferedNotDispatched(t *testing.T) {
	c, err := NewImageBatchCoordinator(60_000)
	if err != nil {
		t.Fatal(err)
	}
	action := c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	if action.Kind != ActionBuffer {
		t.Fatalf("want buffer, got %v", action.Kind)
	}
	if !c.HasPending("alice") || c.PendingCount() != 1 {
		t.Fatal("expected one pending batch")
	}
}

func TestSecondImageExtendsTheSameBatch(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("b")}})
	if got := c.PendingImageCount("alice"); got != 2 {
		t.Fatalf("pending images = %d, want 2", got)
	}
	batch, ok := c.Flush("alice")
	if !ok || len(batch.Images) != 2 {
		t.Fatalf("flushed batch = %+v", batch)
	}
}

func TestTextJoinsPendingBatchAndDispatches(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	action := c.OnMessage("alice", PendingPiece{Text: "记一下这顿", ContextToken: "tok2"})
	if action.Kind != ActionDispatch {
		t.Fatalf("want dispatch, got %v", action.Kind)
	}
	if action.Dispatch.Text != "记一下这顿" {
		t.Fatalf("text = %q", action.Dispatch.Text)
	}
	if len(action.Dispatch.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(action.Dispatch.Images))
	}
	if action.Dispatch.ContextToken != "tok2" {
		t.Fatalf("contextToken = %q", action.Dispatch.ContextToken)
	}
	if c.HasPending("alice") || c.PendingCount() != 0 {
		t.Fatal("batch should be consumed by the join")
	}
}

func TestFlushWithoutCaptionUsesPlaceholder(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}, ContextToken: "tok1"})
	batch, ok := c.Flush("alice")
	if !ok {
		t.Fatal("expected a batch")
	}
	if batch.Text != "[图片]" {
		t.Fatalf("text = %q, want [图片]", batch.Text)
	}
	if len(batch.Images) != 1 || batch.ContextToken != "tok1" {
		t.Fatalf("batch = %+v", batch)
	}
	if c.HasPending("alice") {
		t.Fatal("flush must consume the batch")
	}
}

func TestStandaloneTextDispatchesImmediately(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	action := c.OnMessage("alice", PendingPiece{Text: "查一下英语"})
	if action.Kind != ActionDispatch || action.Dispatch.Text != "查一下英语" {
		t.Fatalf("action = %+v", action)
	}
	if c.HasPending("alice") {
		t.Fatal("text-only message must not buffer")
	}
}

func TestFlushWithNoPendingBatch(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	if _, ok := c.Flush("nobody"); ok {
		t.Fatal("flush of an unknown sender must report no batch")
	}
}

func TestZeroWindowDisablesBatching(t *testing.T) {
	c, _ := NewImageBatchCoordinator(0)
	action := c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	if action.Kind != ActionDispatch {
		t.Fatalf("want dispatch, got %v", action.Kind)
	}
	if action.Dispatch.Text != "[图片]" || len(action.Dispatch.Images) != 1 {
		t.Fatalf("dispatch = %+v", action.Dispatch)
	}
	if c.HasPending("alice") {
		t.Fatal("batching disabled must not buffer")
	}
}

func TestBatchesForDifferentSendersAreIndependent(t *testing.T) {
	c, _ := NewImageBatchCoordinator(60_000)
	c.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	c.OnMessage("bob", PendingPiece{Images: []agentrt.InboundImage{testImage("b")}})
	if c.PendingCount() != 2 {
		t.Fatalf("pending = %d, want 2", c.PendingCount())
	}
	action := c.OnMessage("bob", PendingPiece{Text: "caption"})
	if len(action.Dispatch.Images) != 1 {
		t.Fatalf("bob images = %d", len(action.Dispatch.Images))
	}
	if !c.HasPending("alice") || c.HasPending("bob") {
		t.Fatal("only bob's batch should be consumed")
	}
}

func TestBatchIDIsDeterministicAndContentBased(t *testing.T) {
	c1, _ := NewImageBatchCoordinator(60_000)
	c1.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	d1, _ := c1.Flush("alice")
	c2, _ := NewImageBatchCoordinator(60_000)
	c2.OnMessage("alice", PendingPiece{Images: []agentrt.InboundImage{testImage("a")}})
	d2, _ := c2.Flush("alice")
	if d1.ID != d2.ID {
		t.Fatalf("ids differ: %q vs %q", d1.ID, d2.ID)
	}
	if !strings.HasPrefix(d1.ID, "alice:batch:") {
		t.Fatalf("id = %q", d1.ID)
	}
}

func TestNegativeWindowIsRejected(t *testing.T) {
	if _, err := NewImageBatchCoordinator(-1); err == nil {
		t.Fatal("want an error for a negative window")
	}
}
