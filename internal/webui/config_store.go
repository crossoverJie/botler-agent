package webui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// ConfigFileNames are the config files the WebUI may read or edit.
var ConfigFileNames = []string{".env", "system-prompt.md", "schedules.json"}

// maxBackups is the per-file backup retention (v1 parity).
const maxBackups = 10

// backupRe matches config-class backup names only. Holiday backups
// (`holidays.json.<epochMs>`, owned by the scheduler) deliberately do not match, so this layer
// can never prune or restore them.
var backupRe = regexp.MustCompile(`^(\.env|system-prompt\.md|schedules\.json)\.\d{8}T\d{6}$`)

// BackupEntry is one restorable backup.
type BackupEntry struct {
	Name  string `json:"name"`
	File  string `json:"file"`
	TS    int64  `json:"ts"`
	Bytes int64  `json:"bytes"`
}

func backupDir() string { return filepath.Join(config.UserConfigDir(), ".backups") }

// configFilePath is the live location of a config file. `.env` and `system-prompt.md` always live
// in the user config dir; `schedules.json` honors BOTLER_SCHEDULES_FILE so a WebUI save lands in
// the same file the scheduler reads.
func configFilePath(name string) string {
	if name == "schedules.json" {
		return config.C().SchedulesFile
	}
	return filepath.Join(config.UserConfigDir(), name)
}

// IsConfigFileName reports whether name is one of the three editable config files.
func IsConfigFileName(name string) bool {
	for _, n := range ConfigFileNames {
		if n == name {
			return true
		}
	}
	return false
}

// ReadConfigFile returns a config file's raw text ("" when missing/unreadable).
func ReadConfigFile(name string) string {
	raw, err := os.ReadFile(configFilePath(name))
	if err != nil {
		return ""
	}
	return string(raw)
}

func backupTimestamp() string { return time.Now().Format("20060102T150405") }

// BackupConfigFile snapshots the current file into .backups/ and prunes that file's old backups.
// It returns the backup file name, or "" when there was nothing to back up.
func BackupConfigFile(name string) (string, error) {
	src := configFilePath(name)
	if _, err := os.Stat(src); err != nil {
		return "", nil
	}
	if err := os.MkdirAll(backupDir(), 0o700); err != nil {
		return "", err
	}
	body, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	file := name + "." + backupTimestamp()
	if err := os.WriteFile(filepath.Join(backupDir(), file), body, 0o600); err != nil {
		return "", err
	}
	pruneBackups(name)
	return file, nil
}

// pruneBackups keeps only the newest maxBackups backups of one config file. Holiday backups are
// not matched by backupRe, so they are never candidates.
func pruneBackups(name string) {
	entries, err := os.ReadDir(backupDir())
	if err != nil {
		return
	}
	var matched []string
	for _, e := range entries {
		if backupRe.MatchString(e.Name()) && len(e.Name()) > len(name)+1 && e.Name()[:len(name)+1] == name+"." {
			matched = append(matched, e.Name())
		}
	}
	sort.Strings(matched) // ascending → oldest first
	for len(matched) > maxBackups {
		_ = os.Remove(filepath.Join(backupDir(), matched[0]))
		matched = matched[1:]
	}
}

// WriteConfigFile backs up the current version, then atomically replaces the file
// (write tmp → rename).
func WriteConfigFile(name, content string) error {
	if !IsConfigFileName(name) {
		return fmt.Errorf("invalid config file name: %s", name)
	}
	if _, err := BackupConfigFile(name); err != nil {
		return err
	}
	target := configFilePath(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", target, time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ListBackups returns the available config backups, newest first.
func ListBackups() []BackupEntry {
	out := []BackupEntry{}
	entries, err := os.ReadDir(backupDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		name, file := e.Name(), e.Name()
		if !backupRe.MatchString(file) {
			continue
		}
		tsPart := file[lastDot(file)+1:]
		cfgName := file[:lastDot(file)]
		ts, err := time.ParseInLocation("20060102T150405", tsPart, time.Local)
		var tsMs int64
		if err == nil {
			tsMs = ts.UnixMilli()
		}
		var bytes int64
		if info, err := e.Info(); err == nil {
			bytes = info.Size()
		}
		out = append(out, BackupEntry{Name: cfgName, File: name, TS: tsMs, Bytes: bytes})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// RestoreBackup copies a listed backup over the live config file. The name must be a known config
// file and the backup name must match the strict regex; the resolved path's directory must be the
// canonical .backups dir, so a "../../.env" style name can never escape. The live file is
// snapshotted first, making the restore itself reversible.
func RestoreBackup(name, file string) error {
	if !IsConfigFileName(name) {
		return fmt.Errorf("invalid config file name: %s", name)
	}
	if !backupRe.MatchString(file) {
		return fmt.Errorf("invalid backup file name: %s", file)
	}
	if len(file) <= len(name) || file[:len(name)+1] != name+"." {
		return fmt.Errorf("backup %s does not belong to %s", file, name)
	}
	src := filepath.Join(backupDir(), file)
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("backup not found")
		}
		return err
	}
	// Compare canonical directories: on macOS /tmp is a symlink to /private/tmp, so a literal
	// comparison would spuriously reject legitimate backups.
	canonDir := backupDir()
	if real, err := filepath.EvalSymlinks(backupDir()); err == nil {
		canonDir = real
	}
	if filepath.Dir(resolved) != canonDir {
		return errors.New("backup path traversal rejected")
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return errors.New("backup not found")
	}
	if _, err := BackupConfigFile(name); err != nil {
		return err
	}
	target := configFilePath(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, body, 0o600)
}

// schedulesBackend is the process-wide scheduler seam (see SchedulesBackend).
var (
	backendMu sync.RWMutex
	backend   *SchedulesBackend
)

// SetSchedulesBackend wires the scheduler into the WebUI (called once by cmd/botler).
func SetSchedulesBackend(b SchedulesBackend) {
	backendMu.Lock()
	backend = &b
	backendMu.Unlock()
}

func schedulesBackend() (*SchedulesBackend, bool) {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return backend, backend != nil
}
