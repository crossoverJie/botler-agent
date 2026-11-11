// Package safety is the last line of defense around data writes: post-write JSON validation and
// the automatic git commit / push of changed data subprojects.
package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

// ValidationResult reports whether all data JSON is valid.
type ValidationResult struct {
	OK bool
	// Fix is a self-contained correction instruction (file + location + how to change it).
	Fix string
}

// skipDirs are build artifact / dependency directory names, not data dirs.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"public":       true,
	".pi":          true,
	".codebuddy":   true,
}

func walkJSON(dir string, out *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if skipDirs[e.Name()] {
				continue
			}
			walkJSON(full, out)
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			*out = append(*out, full)
		}
	}
}

// collectJSONFiles returns JSON data files in the selected projects, relative to DATA_ROOT.
func collectJSONFiles(projects []string) []string {
	root := config.C().DataRoot
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var rel []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if projects != nil && !contains(projects, e.Name()) {
			continue
		}
		var found []string
		walkJSON(filepath.Join(root, e.Name()), &found)
		for _, f := range found {
			if r, err := filepath.Rel(root, f); err == nil {
				rel = append(rel, r)
			}
		}
	}
	return rel
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ValidateState checks that every JSON file of the selected projects parses.
// Business semantics (field values, aggregation consistency) are guaranteed by each project's
// AGENTS.md and its own scripts; the framework only guarantees "all data JSON is valid".
// A nil `projects` validates every project; names are verified against listProjectDirs so ".."
// or an unknown name can never escape the DATA_ROOT boundary.
func ValidateState(projects []string) ValidationResult {
	var scoped []string
	if projects != nil {
		known := paths.ListProjects()
		scoped = []string{}
		for _, p := range known {
			if contains(projects, p) {
				scoped = append(scoped, p)
			}
		}
	}
	root := config.C().DataRoot
	for _, rel := range collectJSONFiles(scoped) {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return ValidationResult{
				OK:  false,
				Fix: rel + " is not valid JSON (parse failed: " + err.Error() + "). Read the file, fix the syntax error, and write it back. Do not create a new file.",
			}
		}
	}
	return ValidationResult{OK: true}
}
