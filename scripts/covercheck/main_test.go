package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A merged profile produced with -coverpkg repeats every block once per test binary; the same
// statement must be counted once, and hit if ANY copy hit it.
func TestSummariseMergesDuplicateBlocks(t *testing.T) {
	path := writeProfile(t, `mode: atomic
m/internal/a/a.go:1.1,2.2 2 0
m/internal/a/a.go:1.1,2.2 2 0
m/internal/a/a.go:1.1,2.2 2 5
m/internal/a/a.go:3.1,4.2 1 0
`)

	stats, total, stmts, err := summarise(path)
	if err != nil {
		t.Fatal(err)
	}
	if stmts != 3 {
		t.Fatalf("duplicated blocks were counted more than once: %d statements", stmts)
	}
	if got := total; got < 66.6 || got > 66.7 {
		t.Fatalf("unexpected total: %v", got)
	}
	if len(stats) != 1 || stats[0].Name != "m/internal/a" {
		t.Fatalf("unexpected package stats: %+v", stats)
	}
	if stats[0].Hit != 2 || stats[0].Stmts != 3 {
		t.Fatalf("unexpected per-package numbers: %+v", stats[0])
	}
}

func TestSummariseSplitsByPackage(t *testing.T) {
	path := writeProfile(t, `mode: set
m/internal/a/a.go:1.1,2.2 4 1
m/internal/b/b.go:1.1,2.2 1 0
m/internal/b/sub/c.go:1.1,2.2 5 5
`)

	stats, _, stmts, err := summarise(path)
	if err != nil {
		t.Fatal(err)
	}
	if stmts != 10 {
		t.Fatalf("unexpected statement count: %d", stmts)
	}
	got := map[string]float64{}
	for _, s := range stats {
		got[s.Name] = s.Pct
	}
	want := map[string]float64{
		"m/internal/a":     100,
		"m/internal/b":     0, // b.go below the subpackage is its own package
		"m/internal/b/sub": 100,
	}
	for name, pct := range want {
		if got[name] != pct {
			t.Fatalf("%s = %v%%, want %v%% (all: %v)", name, got[name], pct, got)
		}
	}
}

func TestSummariseRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"no statements": "mode: set\n",
		"malformed":     "mode: set\nnot-a-line\n",
		"bad count":     "mode: set\nm/a.go:1.1,2.2 x 1\n",
	} {
		if _, _, _, err := summarise(writeProfile(t, body)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestPackageOf(t *testing.T) {
	cases := map[string]string{
		"m/internal/tools/run.go:61.41,63.2": "m/internal/tools",
		"m/internal/tools/a/b.go:1.1,2.2":    "m/internal/tools/a",
		"m/x.go:1.1,2.2":                     "m",
	}
	for in, want := range cases {
		if got := packageOf(in); got != want {
			t.Fatalf("packageOf(%q) = %q, want %q", in, got, want)
		}
	}
}
