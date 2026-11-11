// Package paths implements the security boundary: every file the agent may touch must live
// inside one of the first-level (non-hidden) subdirectories of DATA_ROOT.
//
// Rules carried over from v1 (src/tools/paths.ts):
//   - the allowlist is computed once, lazily (Go has no lazy imports, so callers that must not
//     touch DATA_ROOT — init / --version — simply never call Validate);
//   - relative paths resolve against DATA_ROOT, absolute paths are kept as-is but still checked;
//   - prefix matching uses root + separator so /agent2 or /agent-bak cannot slip in;
//   - the target file is never realpath'd directly (a new file would ENOENT); instead the deepest
//     existing ancestor is realpath'd and the remaining path concatenated back, blocking symlink
//     escapes;
//   - an optional task-local project subset is enforced on top of the global allowlist.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/crossoverJie/botler-agent/internal/config"
)

var (
	once    sync.Once
	allowed []string // absolute project root dirs, sorted
	initErr error
)

// resetForTest clears the cached allowlist (tests only).
func resetForTest() {
	once = sync.Once{}
	allowed = nil
	initErr = nil
}

func computeAllowed() ([]string, error) {
	root := config.C().DataRoot
	if root == "" {
		return nil, errors.New("DATA_ROOT is not set. Configure DATA_ROOT (the data root operated by the Agent) in ~/.botler-agent/.env")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve DATA_ROOT: %w", err)
	}
	st, err := os.Stat(absRoot)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("DATA_ROOT does not exist: %s", absRoot)
	}
	// Canonicalize the root once. macOS temp dirs live under /var, a symlink to /private/var, and
	// the escape check below realpaths the target's deepest existing ancestor — comparing the two
	// only works when both sides are canonical.
	if real, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = real
	}
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return nil, fmt.Errorf("read DATA_ROOT: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, filepath.Join(absRoot, e.Name()))
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("DATA_ROOT has no subproject directories: %s", absRoot)
	}
	sort.Strings(dirs)
	// v1 keeps the resolved (not realpath'd) roots: the symlink-escape check realpaths the target's
	// deepest existing ancestor instead, so a symlinked DATA_ROOT still behaves the same way.
	return dirs, nil
}

// dataRoot returns the canonical DATA_ROOT used for relative resolution.
func dataRoot() string {
	root := config.C().DataRoot
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func ensure() error {
	once.Do(func() {
		allowed, initErr = computeAllowed()
	})
	return initErr
}

// Validate forces the allowlist computation. Entries that must fail fast (the persistent service,
// CLI one-shot mode, wechat-login / wechat-logout) call it explicitly in main, preserving v1's
// "die at startup" behaviour.
func Validate() error { return ensure() }

// Allowed returns the absolute allowlisted project roots, sorted by name.
func Allowed() ([]string, error) {
	if err := ensure(); err != nil {
		return nil, err
	}
	out := make([]string, len(allowed))
	copy(out, allowed)
	return out, nil
}

// Options are the per-call knobs of SafePath.
//
// Projects follows v1's truthiness rule: nil means "no task-local subset" (framework calls),
// while a non-nil slice — including an empty one — restricts the call to exactly those projects,
// so an empty slice rejects every path.
type Options struct {
	// FollowSymlinks realpaths the deepest existing ancestor to prevent symlink escapes. Default on.
	FollowSymlinks *bool
	// Projects is an optional task-local subset: when set, the path must fall inside one of them.
	Projects []string
}

func within(root, abs string) bool {
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+string(filepath.Separator))
}

func assertSelectedProject(abs string, projects []string) error {
	if projects == nil {
		return nil
	}
	selected := make(map[string]struct{}, len(projects))
	for _, p := range projects {
		selected[p] = struct{}{}
	}
	for _, root := range allowed {
		if within(root, abs) {
			if _, ok := selected[filepath.Base(root)]; ok {
				return nil
			}
			return fmt.Errorf("path not selected for this task: %s", abs)
		}
	}
	return fmt.Errorf("path not selected for this task: %s", abs)
}

// SafePath resolves p (relative to DATA_ROOT, or absolute) and verifies it stays inside the
// allowlist. Returns an error when out of bounds, escaping through a symlink, or outside the
// task-local project subset.
func SafePath(p string, opts Options) (string, error) {
	if err := ensure(); err != nil {
		return "", err
	}
	var abs string
	root := dataRoot()
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(root, p)
	}
	ok := false
	for _, root := range allowed {
		if within(root, abs) {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("path out of bounds (not within the allowed directory): %s", p)
	}
	if err := assertSelectedProject(abs, opts.Projects); err != nil {
		return "", err
	}

	follow := true
	if opts.FollowSymlinks != nil {
		follow = *opts.FollowSymlinks
	}
	if !follow {
		return abs, nil
	}

	anc := abs
	var tail []string
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		tail = append([]string{filepath.Base(anc)}, tail...)
		anc = parent
	}
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		// v1 lets realpathSync throw here, i.e. the path is denied. Falling through would turn a
		// denial into an IO error later (and skip the escape check).
		return "", fmt.Errorf("path out of bounds (cannot resolve %s): %s", anc, p)
	}
	real := filepath.Join(append([]string{realAnc}, tail...)...)
	ok = false
	for _, root := range allowed {
		if within(root, real) {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("path out of bounds (symlink escape): %s", p)
	}
	if err := assertSelectedProject(real, opts.Projects); err != nil {
		return "", err
	}
	return real, nil
}

// ProjectOf reports which data subproject a resolved absolute path belongs to.
func ProjectOf(absPath string) (string, bool) {
	if err := ensure(); err != nil {
		return "", false
	}
	for _, root := range allowed {
		if within(root, absPath) {
			return filepath.Base(root), true
		}
	}
	return "", false
}

// ListProjects returns the first-level non-hidden subdir names under DATA_ROOT, sorted.
// Unlike Allowed it never validates (it degrades to an empty list), matching v1's
// listProjectDirs used by routing / validation.
func ListProjects() []string {
	root := config.C().DataRoot
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ResetCache clears the lazily computed allowlist. It exists for tests that need to point
// DATA_ROOT somewhere else in the same process; production code never calls it.
func ResetCache() { resetForTest() }
