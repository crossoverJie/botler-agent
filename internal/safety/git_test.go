package safety

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// requireGit skips a test on a machine without git rather than failing: the Go build itself has no
// git dependency, only this package's behaviour does.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// dataRoot installs a temp DATA_ROOT and returns it symlink-resolved (macOS /var -> /private/var).
func dataRoot(t *testing.T, push bool) string {
	t.Helper()
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config.Set(&config.Config{DataRoot: root, GitPush: push})
	return root
}

// runGit runs git in dir and fails the test on a non-zero exit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runGitBare runs git against a bare repository. A bare repo is refused outright when
// safe.bareRepository=explicit is set (a common hardening in CI images and on dev machines), which
// is unrelated to the behaviour under test, so the policy is overridden here instead of relying on
// the machine's git config.
func runGitBare(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGit(t, dir, append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

// newRepo creates an independent data subproject repo with a hermetic config: the machine's global
// hooks (e.g. a lefthook wrapper) must not influence the test.
func newRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "agent@example.com")
	runGit(t, dir, "config", "user.name", "agent")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "config", "core.hooksPath", filepath.Join(root, ".no-hooks"))
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "rev-parse", "HEAD")
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"\n\n  hello \nworld":      "hello",
		"fatal: no upstream\nmore": "fatal: no upstream",
		"   \n\t\n":                "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Fatalf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommitIfChangedOnlyTouchesDirtyRepos(t *testing.T) {
	root := dataRoot(t, false)

	dirty := newRepo(t, root, "dirty")
	write(t, dirty, "a.json", `{"x":1}`)
	runGit(t, dirty, "add", "-A")
	runGit(t, dirty, "commit", "-q", "-m", "initial")
	write(t, dirty, "a.json", `{"x":2}`)

	clean := newRepo(t, root, "clean")
	write(t, clean, "b.json", "{}")
	runGit(t, clean, "add", "-A")
	runGit(t, clean, "commit", "-q", "-m", "initial")
	cleanHead := head(t, clean)

	plain := filepath.Join(root, "plain") // first-level dir, but not a git repo
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, plain, "c.json", "{}")

	CommitIfChanged("agent: test message", nil)

	if got := runGit(t, dirty, "log", "-1", "--pretty=%s"); got != "agent: test message" {
		t.Fatalf("dirty repo was not committed: last commit = %q", got)
	}
	if got := head(t, clean); got != cleanHead {
		t.Fatal("a clean repo must not get an empty commit")
	}
	// The non-repo directory is left completely alone (no .git is created).
	if _, err := os.Stat(filepath.Join(plain, ".git")); err == nil {
		t.Fatal("a non-repo directory must not be initialised")
	}
}

func TestCommitIfChangedRespectsProjectFilter(t *testing.T) {
	root := dataRoot(t, false)

	selected := newRepo(t, root, "selected")
	write(t, selected, "a.json", "{}")
	runGit(t, selected, "add", "-A")
	runGit(t, selected, "commit", "-q", "-m", "initial")
	write(t, selected, "a.json", `{"changed":true}`)

	other := newRepo(t, root, "other")
	write(t, other, "b.json", "{}")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "initial")
	write(t, other, "b.json", `{"changed":true}`)
	otherHead := head(t, other)

	CommitIfChanged("agent: scoped", []string{"selected"})

	if got := runGit(t, selected, "log", "-1", "--pretty=%s"); got != "agent: scoped" {
		t.Fatalf("selected repo was not committed: %q", got)
	}
	if got := head(t, other); got != otherHead {
		t.Fatal("a dirty but unselected repo must never be touched")
	}
	if status := runGit(t, other, "status", "--porcelain"); status == "" {
		t.Fatal("the unselected repo must still be dirty")
	}
}

func TestCommitIfChangedSkipsHiddenAndMissingRoot(t *testing.T) {
	root := dataRoot(t, false)
	hidden := newRepo(t, root, ".hidden")
	write(t, hidden, "a.json", "{}")
	runGit(t, hidden, "add", "-A")
	runGit(t, hidden, "commit", "-q", "-m", "initial")
	write(t, hidden, "a.json", `{"changed":true}`)
	hiddenHead := head(t, hidden)

	CommitIfChanged("agent: hidden", nil)
	if got := head(t, hidden); got != hiddenHead {
		t.Fatal("hidden first-level dirs are not data subprojects")
	}

	// No DATA_ROOT configured must be a silent no-op, not a panic.
	config.Set(&config.Config{})
	CommitIfChanged("agent: no root", nil)
}

func TestCommitIfChangedSurvivesFailingHook(t *testing.T) {
	root := dataRoot(t, false)
	repo := newRepo(t, root, "repo")
	hooks := filepath.Join(root, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")
	runGit(t, repo, "config", "core.hooksPath", hooks)
	write(t, repo, "a.json", `{"changed":true}`)
	before := head(t, repo)

	// A rejected commit is a logged warning, never a panic: the write task still returns normally.
	CommitIfChanged("agent: rejected", nil)

	if got := head(t, repo); got != before {
		t.Fatal("no commit should have been created")
	}
	if status := runGit(t, repo, "status", "--porcelain"); status == "" {
		t.Fatal("the change must still be pending")
	}
}

// newRemote creates a bare repo to push to, so the test never touches the network.
func newRemote(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name+".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitBare(t, dir, "init", "-q", "--bare", "-b", "main")
	return dir
}

func TestPushIfPendingPushesAndSetsUpstream(t *testing.T) {
	root := dataRoot(t, true)
	remote := newRemote(t, root, "origin")
	repo := newRepo(t, root, "repo")
	runGit(t, repo, "remote", "add", "origin", remote)
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")

	CommitIfChanged("agent: first", nil)

	local := head(t, repo)
	pushed := runGitBare(t, remote, "rev-parse", "main")
	if local != pushed {
		t.Fatalf("commit was not pushed: local=%s remote=%s", local, pushed)
	}
	// The first push must use -u: without it @{u} never resolves and every later round would
	// re-push nothing.
	if upstream := runGit(t, repo, "rev-parse", "--abbrev-ref", "@{u}"); upstream != "origin/main" {
		t.Fatalf("upstream not established: %q", upstream)
	}
}

func TestPushIfPendingNoopsWhenNothingAhead(t *testing.T) {
	root := dataRoot(t, true)
	remote := newRemote(t, root, "origin")
	repo := newRepo(t, root, "repo")
	runGit(t, repo, "remote", "add", "origin", remote)
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")
	CommitIfChanged("agent: first", nil)

	// Nothing new to push: rev-list reports 0, so no second push happens and nothing changes.
	pushIfPending(repo, "repo")

	if got := runGit(t, repo, "rev-list", "--count", "@{u}..HEAD"); got != "0" {
		t.Fatalf("expected an in-sync repo, got %q commits ahead", got)
	}
}

func TestPushIfPendingSkipsWithoutOrigin(t *testing.T) {
	root := dataRoot(t, true)
	repo := newRepo(t, root, "repo")
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")

	// Must not panic and must not create a remote.
	pushIfPending(repo, "repo")
	if out := runGit(t, repo, "remote"); out != "" {
		t.Fatalf("no remote should have been added: %q", out)
	}
}

func TestPushIfPendingSkipsDetachedHead(t *testing.T) {
	root := dataRoot(t, true)
	remote := newRemote(t, root, "origin")
	repo := newRepo(t, root, "repo")
	runGit(t, repo, "remote", "add", "origin", remote)
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")
	runGit(t, repo, "checkout", "-q", "--detach", "HEAD")

	pushIfPending(repo, "repo")
	// A detached HEAD has no upstream to establish; the remote stays empty.
	if out := runGitBare(t, remote, "branch"); out != "" {
		t.Fatalf("nothing should have been pushed: %q", out)
	}
}

func TestPushIfPendingSkipsUnbornBranch(t *testing.T) {
	root := dataRoot(t, true)
	remote := newRemote(t, root, "origin")
	repo := newRepo(t, root, "repo")
	runGit(t, repo, "remote", "add", "origin", remote)

	// A fresh repo with no commit: rev-list fails, and `push -u` must not be attempted.
	pushIfPending(repo, "repo")
	if out := runGitBare(t, remote, "branch"); out != "" {
		t.Fatalf("an unborn branch must not be pushed: %q", out)
	}
}

func TestPushIfPendingSkipsStaleUpstream(t *testing.T) {
	root := dataRoot(t, true)
	remote := newRemote(t, root, "origin")
	repo := newRepo(t, root, "repo")
	runGit(t, repo, "remote", "add", "origin", remote)
	write(t, repo, "a.json", "{}")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "initial")

	// Upstream is configured but its tracking ref is gone: pushing with -u would silently rewrite
	// branch.main.merge, so the entry must be skipped instead.
	runGit(t, repo, "config", "branch.main.merge", "refs/heads/main")
	runGit(t, repo, "config", "branch.main.remote", "origin")
	runGit(t, repo, "update-ref", "-d", "refs/remotes/origin/main")
	before := runGit(t, repo, "config", "--get", "branch.main.merge")

	pushIfPending(repo, "repo")

	if after := runGit(t, repo, "config", "--get", "branch.main.merge"); after != before {
		t.Fatalf("the tracking config must not be rewritten: %q -> %q", before, after)
	}
	if out := runGitBare(t, remote, "branch"); out != "" {
		t.Fatalf("nothing should have been pushed: %q", out)
	}
}

func TestWarnSkipLogsOncePerReason(t *testing.T) {
	// warnSkip dedupes per (reason, repo) so a missing origin does not spam every write task.
	skipMu.Lock()
	skipWarned = map[string]bool{}
	skipMu.Unlock()
	t.Cleanup(func() {
		skipMu.Lock()
		skipWarned = map[string]bool{}
		skipMu.Unlock()
	})
	warnSkip("test-reason", "repo-a", "first")
	warnSkip("test-reason", "repo-a", "duplicate")
	skipMu.Lock()
	seen := skipWarned["test-reason:repo-a"]
	skipMu.Unlock()
	if !seen {
		t.Fatal("the skip must be recorded so it is reported only once")
	}
}
