package safety

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
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
	return root
}

func TestValidateStateAcceptsValidJSON(t *testing.T) {
	root := withDataRoot(t, "cook")
	if err := os.WriteFile(filepath.Join(root, "cook", "a.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ValidateState([]string{"cook"}); !got.OK {
		t.Fatalf("expected valid, got %+v", got)
	}
}

func TestValidateStateReportsSelfContainedFix(t *testing.T) {
	root := withDataRoot(t, "cook")
	if err := os.WriteFile(filepath.Join(root, "cook", "bad.json"), []byte(`{"a":1,}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ValidateState([]string{"cook"})
	if got.OK {
		t.Fatal("expected invalid")
	}
	if !strings.HasPrefix(got.Fix, filepath.Join("cook", "bad.json")+" is not valid JSON (parse failed: ") {
		t.Fatalf("unexpected fix prefix: %q", got.Fix)
	}
	if !strings.HasSuffix(got.Fix, ". Read the file, fix the syntax error, and write it back. Do not create a new file.") {
		t.Fatalf("missing self-contained instruction: %q", got.Fix)
	}
}

func TestValidateStateScopedToSelectedProjects(t *testing.T) {
	root := withDataRoot(t, "cook", "vocab")
	if err := os.WriteFile(filepath.Join(root, "cook", "bad.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ValidateState([]string{"vocab"}); !got.OK {
		t.Fatalf("an unselected broken project must not block the task: %+v", got)
	}
	if got := ValidateState(nil); got.OK {
		t.Fatal("validating everything must catch the broken file")
	}
}

func TestValidateStateSkipsKnownArtifactDirs(t *testing.T) {
	root := withDataRoot(t, "cook")
	dir := filepath.Join(root, "cook", "node_modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ValidateState([]string{"cook"}); !got.OK {
		t.Fatalf("artifact dirs must be skipped: %+v", got)
	}
}

func TestValidateStateIgnoresNonJSON(t *testing.T) {
	root := withDataRoot(t, "cook")
	if err := os.WriteFile(filepath.Join(root, "cook", "notes.md"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ValidateState([]string{"cook"}); !got.OK {
		t.Fatalf("non-JSON files are not validated: %+v", got)
	}
}
