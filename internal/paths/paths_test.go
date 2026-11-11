package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// withRoot builds a temp DATA_ROOT with the given first-level entries and installs it as the
// process config, resetting the lazily computed allowlist.
func withRoot(t *testing.T, dirs ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Config{DataRoot: root})
	resetForTest()
	t.Cleanup(resetForTest)
	return root
}

func TestValidateRejectsMissingOrEmptyRoot(t *testing.T) {
	config.Set(&config.Config{DataRoot: ""})
	resetForTest()
	if err := Validate(); err == nil {
		t.Fatal("expected an error when DATA_ROOT is unset")
	}

	empty := t.TempDir()
	config.Set(&config.Config{DataRoot: empty})
	resetForTest()
	if err := Validate(); err == nil {
		t.Fatal("expected an error when DATA_ROOT has no subprojects")
	}

	config.Set(&config.Config{DataRoot: filepath.Join(empty, "nope")})
	resetForTest()
	if err := Validate(); err == nil {
		t.Fatal("expected an error when DATA_ROOT does not exist")
	}
}

func TestSafePathResolvesRelativeToDataRoot(t *testing.T) {
	root := withRoot(t, "notes", "vocab")
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := SafePath("notes/a.json", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "notes", "a.json")
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSafePathKeepsAbsolutePathsInside(t *testing.T) {
	root := withRoot(t, "notes")
	abs := filepath.Join(root, "notes", "b.json")
	got, err := SafePath(abs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("got %s, want %s", got, abs)
	}
	if _, err := SafePath(filepath.Join(root, "other", "x.json"), Options{}); err == nil {
		t.Fatal("expected out-of-bounds error")
	}
	if _, err := SafePath("/etc/passwd", Options{}); err == nil {
		t.Fatal("expected out-of-bounds error for /etc/passwd")
	}
}

// A sibling whose name shares a prefix must not slip through (the /agent2 vs /agent bug).
func TestSafePathRejectsSiblingPrefixTrick(t *testing.T) {
	root := withRoot(t, "agent")
	outside := filepath.Join(root, "agent-bak")
	if _, err := SafePath(filepath.Join(outside, "x.json"), Options{}); err == nil {
		t.Fatal("expected sibling directory to be rejected")
	}
}

func TestSafePathExcludesHiddenDirectories(t *testing.T) {
	root := withRoot(t, "notes", ".secret")
	if _, err := SafePath(filepath.Join(root, ".secret", "k.json"), Options{}); err == nil {
		t.Fatal("expected hidden directory to be outside the allowlist")
	}
}

func TestSafePathAllowsNewFileViaDeepestExistingAncestor(t *testing.T) {
	root := withRoot(t, "notes")
	got, err := SafePath(filepath.Join(root, "notes", "deep", "new", "f.json"), Options{})
	if err != nil {
		t.Fatalf("new file should be allowed: %v", err)
	}
	want := filepath.Join(root, "notes", "deep", "new", "f.json")
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSafePathBlocksSymlinkEscape(t *testing.T) {
	root := withRoot(t, "notes")
	outside := t.TempDir()
	link := filepath.Join(root, "notes", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := SafePath(filepath.Join(link, "passwd"), Options{}); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestSafePathEnforcesTaskProjectSubset(t *testing.T) {
	root := withRoot(t, "notes", "vocab")
	if _, err := SafePath("notes/a.json", Options{Projects: []string{"vocab"}}); err == nil {
		t.Fatal("expected task subset rejection")
	}
	if _, err := SafePath("vocab/a.json", Options{Projects: []string{"vocab"}}); err != nil {
		t.Fatalf("selected project must be allowed: %v", err)
	}
	if _, err := SafePath(filepath.Join(root, "vocab", "a.json"), Options{Projects: []string{"vocab"}}); err != nil {
		t.Fatalf("selected project must be allowed (absolute): %v", err)
	}
}

func TestProjectOf(t *testing.T) {
	root := withRoot(t, "notes", "vocab")
	if name, ok := ProjectOf(filepath.Join(root, "notes", "x.json")); !ok || name != "notes" {
		t.Fatalf("got %q ok=%v", name, ok)
	}
	if _, ok := ProjectOf(root); ok {
		t.Fatal("DATA_ROOT itself does not belong to a project")
	}
}

func TestListProjectsSorted(t *testing.T) {
	withRoot(t, "zeta", "alpha", ".hidden")
	got := ListProjects()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("unexpected list: %v", got)
	}
}

// An empty (non-nil) subset means "nothing is allowed": v1 passes an empty array for a
// schedule-only route, and every path must be rejected rather than falling back to the full
// allowlist.
func TestSafePathEmptySubsetRejectsEverything(t *testing.T) {
	withRoot(t, "notes", "vocab")
	if _, err := SafePath("notes/a.json", Options{Projects: []string{}}); err == nil {
		t.Fatal("expected an empty subset to reject every path")
	}
	if _, err := SafePath("notes/a.json", Options{Projects: nil}); err != nil {
		t.Fatalf("a nil subset means no restriction: %v", err)
	}
}
