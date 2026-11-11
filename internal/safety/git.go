package safety

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/crossoverJie/botler-agent/internal/config"
)

type gitResult struct {
	ok     bool
	stdout string
	stderr string
}

// firstLine picks git's useful error signal (Permission denied / Host key verification failed /
// no upstream); everything after is noise.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

func git(dir string, args ...string) gitResult {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		msg := firstLine(errBuf.String())
		if msg == "" {
			msg = firstLine(err.Error())
		}
		return gitResult{ok: false, stdout: out.String(), stderr: msg}
	}
	return gitResult{ok: true, stdout: out.String()}
}

var (
	skipMu     sync.Mutex
	skipWarned = map[string]bool{}
)

// warnSkip reports each (reason, repo) skip once per process, so a repo without an origin does
// not warn on every write task.
func warnSkip(reason, name, message string) {
	key := reason + ":" + name
	skipMu.Lock()
	seen := skipWarned[key]
	skipWarned[key] = true
	skipMu.Unlock()
	if !seen {
		logErr(message)
	}
}

func reportPush(name string, out gitResult) {
	if out.ok {
		logInfo("[git] pushed " + name)
		return
	}
	logErr("[git] push failed for " + name + ": " + out.stderr)
}

func logInfo(s string) { printlnStd("out", s) }
func logErr(s string)  { printlnStd("err", s) }

func printlnStd(kind, s string) {
	if kind == "err" {
		os.Stderr.WriteString(s + "\n")
		return
	}
	os.Stdout.WriteString(s + "\n")
}

// pushIfPending pushes only when there is something to push. `rev-list --count @{u}..HEAD` walks
// local objects only (no network round trip) and fails when `@{u}` cannot be resolved.
func pushIfPending(dir, name string) {
	ahead := git(dir, "rev-list", "--count", "@{u}..HEAD")
	if ahead.ok {
		n, _ := strconv.Atoi(strings.TrimSpace(ahead.stdout))
		if n > 0 {
			reportPush(name, git(dir, "push"))
		}
		return
	}

	// rev-list failed: distinguish "no upstream configured yet" from "upstream configured but its
	// tracking ref is gone" — `push -u` would silently rewrite branch.<name>.merge in the latter case.
	branch := git(dir, "symbolic-ref", "--short", "HEAD")
	if !branch.ok {
		warnSkip("detached", name, "[git] skip push for "+name+": detached HEAD")
		return
	}
	if !git(dir, "remote", "get-url", "origin").ok {
		warnSkip("no-origin", name, "[git] skip push for "+name+": no origin remote")
		return
	}
	mergeRef := git(dir, "config", "--get", "branch."+strings.TrimSpace(branch.stdout)+".merge")
	if mergeRef.ok && strings.TrimSpace(mergeRef.stdout) != "" {
		warnSkip("stale-upstream", name,
			"[git] skip push for "+name+": upstream "+strings.TrimSpace(mergeRef.stdout)+" is configured but unresolvable ("+ahead.stderr+")")
		return
	}
	if !git(dir, "rev-parse", "--verify", "--quiet", "HEAD").ok {
		warnSkip("unborn", name, "[git] skip push for "+name+": branch has no commits yet")
		return
	}
	// The first push must be `-u`: a bare `push origin HEAD` only creates the remote-tracking ref
	// and leaves branch.<name>.merge unset, so @{u} would never resolve.
	reportPush(name, git(dir, "push", "-u", "origin", "HEAD"))
}

// CommitIfChanged commits each changed data subproject. Only first-level subdirs that are their
// own git repos and have a non-empty `git status --porcelain` are committed. When GIT_PUSH=1,
// every such repo is pushed afterwards — decoupled from this round's commit result, so a commit
// rejected by a hook still lets a previous round's failed push self-heal.
//
// `projects` narrows the loop (and therefore the push step) to the selected subprojects; a dirty
// but unselected project is never touched.
func CommitIfChanged(msg string, projects []string) {
	root := config.C().DataRoot
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range entries {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		if projects != nil && !contains(projects, d.Name()) {
			continue
		}
		dir := filepath.Join(root, d.Name())
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		if added := git(dir, "add", "-A"); !added.ok {
			logErr("[git] add failed for " + d.Name() + ": " + added.stderr)
			continue
		}
		status := git(dir, "status", "--porcelain")
		if !status.ok {
			logErr("[git] status failed for " + d.Name() + ": " + status.stderr)
			continue
		}
		if strings.TrimSpace(status.stdout) != "" {
			if commit := git(dir, "commit", "-m", msg); commit.ok {
				logInfo("[git] committed " + d.Name() + ": " + msg)
			} else {
				logErr("[git] commit failed for " + d.Name() + ": " + commit.stderr)
			}
		}
		if config.C().GitPush {
			pushIfPending(dir, d.Name())
		}
	}
}
