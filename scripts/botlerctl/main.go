// Command botlerctl is the local process-control helper for botler-agent (the Go replacement
// for scripts/botlerctl.sh).
//
// Usage:
//
//	botlerctl restart [main|stable]   update the code, then (re)start in the background
//	botlerctl start                   start in the background (no code update)
//	botlerctl stop                    stop the running process
//	botlerctl status                  report whether it is running
//	botlerctl logs                    tail the log file
//
// It only touches this repository: never DATA_ROOT, never ~/.botler-agent/.
//
// Environment:
//
//	BOTLER_GITHUB_REPO      owner/name (default crossoverJie/botler-agent)
//	BOTLER_GITHUB_API       base URL   (default https://api.github.com)
//	BOTLER_DIR              repository root (default: discovered from the executable/cwd)
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	mainBranch = "main"
	remote     = "origin"
)

type ctl struct {
	repoDir string
	binary  string
	pidfile string
	logfile string
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	c, err := newCtl()
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "restart":
		mode := ""
		if len(os.Args) > 2 {
			mode = os.Args[2]
		}
		c.restart(mode)
	case "start":
		c.start()
	case "stop":
		c.stop()
	case "status":
		c.status()
	case "logs":
		c.logs()
	case "help", "-h", "--help":
		usage()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: botlerctl {restart [main|stable]|start|stop|status|logs}

  restart [branch|stable]  update code, then restart (default: the branch that is checked out)
  start                  start in the background without updating
  stop                   stop the running process
  status                 show whether it is running
  logs                   tail the log file
`)
	os.Exit(1)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "botlerctl: %v\n", err)
	os.Exit(1)
}

func newCtl() (*ctl, error) {
	dir := os.Getenv("BOTLER_DIR")
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		found, err := findRepoRoot(wd)
		if err != nil {
			return nil, err
		}
		dir = found
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &ctl{
		repoDir: abs,
		binary:  filepath.Join(abs, "botler"),
		pidfile: filepath.Join(abs, ".botler.pid"),
		logfile: filepath.Join(abs, "botler.log"),
	}, nil
}

// findRepoRoot walks up from start until it finds the module root.
func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside the botler-agent repository (no go.mod found)")
		}
		dir = parent
	}
}

func (c *ctl) readPID() (int, bool) {
	raw, err := os.ReadFile(c.pidfile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (c *ctl) isRunning() bool {
	pid, ok := c.readPID()
	if !ok {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func (c *ctl) stop() {
	if c.isRunning() {
		pid, _ := c.readPID()
		fmt.Printf("stopping (pid %d)...\n", pid)
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		for i := 0; i < 30 && c.isRunning(); i++ {
			time.Sleep(time.Second)
		}
		if c.isRunning() {
			fmt.Println("did not exit gracefully, sending SIGKILL")
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
		_ = os.Remove(c.pidfile)
	}
	fmt.Println("stopped")
}

func (c *ctl) start() {
	if c.isRunning() {
		pid, _ := c.readPID()
		fmt.Printf("already running (pid %d)\n", pid)
		return
	}
	binary := c.binary
	// Always rebuild: a stale binary silently serving an old build is far worse than a 2s compile.
	if err := c.build(); err != nil {
		fail(err)
	}
	logf, err := os.OpenFile(c.logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fail(err)
	}
	defer logf.Close()
	cmd := exec.Command(binary)
	cmd.Dir = c.repoDir
	cmd.Env = cleanEnv()
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Detach from this process group so the daemon survives the controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fail(err)
	}
	if err := os.WriteFile(c.pidfile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("started (pid %d), log: %s\n", cmd.Process.Pid, c.logfile)
}

// cleanEnv drops PI_PROVIDER / PI_MODEL inherited from the shell: process env beats .env by
// design, and a stray export silently makes the daemon use a model that ~/.botler-agent/.env does
// not mention. Set BOTLER_KEEP_ENV=1 to keep the inherited values.
func cleanEnv() []string {
	env := os.Environ()
	if os.Getenv("BOTLER_KEEP_ENV") == "1" {
		return env
	}
	kept := make([]string, 0, len(env))
	var dropped []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PI_PROVIDER=") || strings.HasPrefix(kv, "PI_MODEL=") {
			dropped = append(dropped, strings.SplitN(kv, "=", 2)[0])
			continue
		}
		kept = append(kept, kv)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "warn: ignoring inherited %s (set BOTLER_KEEP_ENV=1 to keep it); the daemon uses ~/.botler-agent/.env\n",
			strings.Join(dropped, ", "))
	}
	return kept
}

func (c *ctl) build() error {
	if _, err := os.Stat(filepath.Join(c.repoDir, "cmd", "botler")); err != nil {
		return fmt.Errorf("this checkout has no cmd/botler (the TypeScript implementation lives on main); pass a Go branch, e.g. botlerctl restart v2-golang")
	}
	fmt.Println("building botler...")
	version := strings.TrimSpace(runOutput("git", c.repoDir, "describe", "--tags", "--always", "--dirty"))
	if version == "" {
		version = "dev"
	}
	commit := strings.TrimSpace(runOutput("git", c.repoDir, "rev-parse", "--short", "HEAD"))
	ldflags := fmt.Sprintf("-s -w -X github.com/crossoverJie/botler-agent/internal/version.Version=%s -X github.com/crossoverJie/botler-agent/internal/version.Commit=%s", version, commit)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", c.binary, "./cmd/botler")
	cmd.Dir = c.repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// updateBranch fetches and hard-updates the named branch ("" = the currently checked-out branch).
// A dirty working tree is refused: `git checkout -f` would silently discard uncommitted work.
func (c *ctl) updateBranch(branch string) error {
	if branch == "" {
		current, err := c.currentBranch()
		if err != nil {
			return err
		}
		branch = current
	}
	if dirty, err := c.isDirty(); err != nil {
		return err
	} else if dirty && os.Getenv("BOTLER_FORCE") != "1" {
		return fmt.Errorf("working tree has uncommitted changes; commit/stash them or set BOTLER_FORCE=1 to discard")
	}
	fmt.Printf("updating %s...\n", branch)
	if out, err := run("git", c.repoDir, "fetch", remote, branch); err != nil {
		// A local branch that was never pushed has no remote ref yet (e.g. a feature branch under
		// development): build what is checked out instead of leaving the service stopped.
		fmt.Fprintf(os.Stderr, "warn: cannot fetch %s/%s (%s); building the local checkout\n",
			remote, branch, strings.TrimSpace(out))
		return c.build()
	}
	if out, err := run("git", c.repoDir, "checkout", "-f", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("git checkout: %v: %s", err, out)
	}
	// Branch-local anchoring: keep the checkout attached to a local branch of the same name so a
	// later `git status`/pull behaves predictably.
	if out, err := run("git", c.repoDir, "checkout", "-B", branch, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("git checkout -B %s: %v: %s", branch, err, out)
	}
	return c.build()
}

// currentBranch returns the checked-out branch name (detached HEAD falls back to "main").
func (c *ctl) currentBranch() (string, error) {
	out, err := run("git", c.repoDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return mainBranch, nil
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return mainBranch, nil
	}
	return name, nil
}

func (c *ctl) isDirty() (bool, error) {
	out, err := run("git", c.repoDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %v: %s", err, out)
	}
	return strings.TrimSpace(out) != "", nil
}

func (c *ctl) updateStable() error {
	tag := latestReleaseTag()
	if tag == "" {
		fmt.Fprintln(os.Stderr, "warn: no stable release found, falling back to the current branch")
		return c.updateBranch("")
	}
	fmt.Printf("stable: checking out %s\n", tag)
	if out, err := run("git", c.repoDir, "fetch", "--tags", remote); err != nil {
		return fmt.Errorf("git fetch --tags: %v: %s", err, out)
	}
	if out, err := run("git", c.repoDir, "checkout", "-f", tag); err != nil {
		return fmt.Errorf("git checkout %s: %v: %s", tag, err, out)
	}
	return c.build()
}

// latestReleaseTag resolves the newest GitHub release tag; empty when none exists.
func latestReleaseTag() string {
	repo := envOr("BOTLER_GITHUB_REPO", "crossoverJie/botler-agent")
	api := envOr("BOTLER_GITHUB_API", "https://api.github.com")
	req, err := http.NewRequest(http.MethodGet, api+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	// Avoid a JSON dependency here: one field is all we need.
	idx := strings.Index(string(body), `"tag_name"`)
	if idx == -1 {
		return ""
	}
	rest := string(body)[idx+len(`"tag_name"`):]
	start := strings.Index(rest, `"`)
	if start == -1 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}

func (c *ctl) restart(mode string) {
	// Check the preconditions BEFORE stopping: a refused update must not leave the service down.
	if dirty, err := c.isDirty(); err != nil {
		fail(err)
	} else if dirty && os.Getenv("BOTLER_FORCE") != "1" {
		fail(fmt.Errorf("working tree has uncommitted changes; commit/stash them or set BOTLER_FORCE=1 to discard"))
	}
	c.stop()
	var err error
	if mode == "stable" {
		err = c.updateStable()
	} else {
		err = c.updateBranch(mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "botlerctl: %v\n", err)
		// The checkout may have moved or failed; do not start a stale binary silently.
		os.Exit(1)
	}
	c.start()
}

func (c *ctl) status() {
	if c.isRunning() {
		pid, _ := c.readPID()
		fmt.Printf("running (pid %d)\n", pid)
		return
	}
	fmt.Println("not running")
}

func (c *ctl) logs() {
	f, err := os.Open(c.logfile)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	// Print the tail, then follow like `tail -f`.
	const tailLines = 40
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > tailLines {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if err != nil {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func run(name, dir string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runOutput(name, dir string, args ...string) string {
	out, _ := run(name, dir, args...)
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
