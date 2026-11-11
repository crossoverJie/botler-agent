// Command install installs botler-agent from source: clone (or update) the repository, build the
// binary, and generate the user config templates (the Go replacement for install.sh).
//
// Usage:
//
//	go run ./scripts/install          # or: go build -o /tmp/botler-install ./scripts/install && /tmp/botler-install
//
// Environment:
//
//	BOTLER_INSTALL_DIR   install directory (default ~/.local/share/botler-agent)
//	BOTLER_REPO          git remote (default https://github.com/crossoverJie/botler-agent.git)
//	BOTLER_REF           branch or tag to build (default main)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	dir := envOr("BOTLER_INSTALL_DIR", filepath.Join(home, ".local", "share", "botler-agent"))
	repo := envOr("BOTLER_REPO", "https://github.com/crossoverJie/botler-agent.git")
	ref := envOr("BOTLER_REF", "main")

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		fmt.Printf("==> Updating existing install at %s\n", dir)
		must(dir, "git", "fetch", "origin", ref)
		must(dir, "git", "checkout", "-f", "FETCH_HEAD")
	} else {
		fmt.Printf("==> Cloning botler-agent into %s\n", dir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			fail(err)
		}
		must("", "git", "clone", "--branch", ref, repo, dir)
	}

	fmt.Println("==> Building botler")
	binary := "botler"
	if strings.HasPrefix(strings.ToLower(os.Getenv("OS")), "windows") {
		binary += ".exe"
	}
	must(dir, "go", "build", "-trimpath", "-o", binary, "./cmd/botler")

	fmt.Println("==> Initializing user config (~/.botler-agent/)")
	must(dir, filepath.Join(dir, binary), "init")

	fmt.Printf(`
Done. Next steps:
  1. Edit ~/.botler-agent/.env — fill in DATA_ROOT, model selection (PI_PROVIDER / PI_MODEL), channel credentials.
  2. Edit ~/.botler-agent/providers.json with your model providers (baseUrl / apiKey / models).
  3. Run from this directory:  cd %s && ./%s
     (or put %s on your PATH)
`, dir, binary, filepath.Join(dir, binary))
}

func must(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail(fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err))
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "install: %v\n", err)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
