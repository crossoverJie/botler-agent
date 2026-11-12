package initcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withConfigDir points the externalized config dir at a temp directory.
func withConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOTLER_CONFIG_DIR", filepath.Join(dir, "cfg"))
	return filepath.Join(dir, "cfg")
}

func TestRunScaffoldsEveryTemplate(t *testing.T) {
	// `botler init` must work on a machine with no DATA_ROOT at all (startup-order contract).
	dir := withConfigDir(t)
	t.Setenv("DATA_ROOT", "")

	if err := Run(); err != nil {
		t.Fatal(err)
	}

	for name, wantContains := range map[string]string{
		".env":             "DATA_ROOT",
		"system-prompt.md": "__PROJECT_CONTEXT__",
		"providers.json":   "openai-completions",
		"schedules.json":   `"schedules"`,
	} {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
		if !strings.Contains(string(raw), wantContains) {
			t.Fatalf("%s does not look like the template (missing %q)", name, wantContains)
		}
	}

	// Credentials-bearing templates must not be world readable.
	for name, want := range map[string]os.FileMode{
		".env":             0o600,
		"providers.json":   0o600,
		"schedules.json":   0o600,
		"system-prompt.md": 0o644,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", name, got, want)
		}
	}
}

func TestRunNeverOverwritesExistingFiles(t *testing.T) {
	dir := withConfigDir(t)
	if err := Run(); err != nil {
		t.Fatal(err)
	}

	// A hand-edited config must survive a second `init` run byte for byte.
	envPath := filepath.Join(dir, ".env")
	custom := "DATA_ROOT=/my/data\n"
	if err := os.WriteFile(envPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Run(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != custom {
		t.Fatalf("init must never overwrite an existing .env, got %q", raw)
	}
}

// A directory where the file should go makes the write fail; init reports it and keeps going
// instead of aborting the remaining templates.
func TestWriteIfAbsentReportsErrorsWithoutAborting(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "system-prompt.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeIfAbsent(filepath.Join(blocked, "system-prompt.md"), "x", 0o644, "created %s", "exists %s")
	writeIfAbsent(filepath.Join(blocked, "ok.txt"), "content", 0o644, "created %s", "exists %s")

	raw, err := os.ReadFile(filepath.Join(blocked, "ok.txt"))
	if err != nil {
		t.Fatalf("the later write must still happen: %v", err)
	}
	if string(raw) != "content" {
		t.Fatalf("unexpected content: %q", raw)
	}
}
