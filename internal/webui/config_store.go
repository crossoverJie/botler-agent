package webui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// ConfigFileNames are the config files the WebUI may read or edit.
var ConfigFileNames = []string{".env", "system-prompt.md", "schedules.json"}

// BackupEntry is one restorable backup.
type BackupEntry struct {
	Name  string `json:"name"`
	File  string `json:"file"`
	TS    int64  `json:"ts"`
	Bytes int64  `json:"bytes"`
}

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

// WriteConfigFile backs up the current version, then atomically replaces the file
// (write tmp → rename).
func WriteConfigFile(name, content string) error {
	if !IsConfigFileName(name) {
		return fmt.Errorf("invalid config file name: %s", name)
	}
	target := configFilePath(name)
	if _, err := config.BackupConfigFile(name, target); err != nil {
		return err
	}
	return config.WriteFileAtomic(target, []byte(content), 0o600)
}

// ListBackups returns the available config backups, newest first.
func ListBackups() []BackupEntry {
	out := []BackupEntry{}
	entries, err := os.ReadDir(config.ConfigBackupDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		name, file := e.Name(), e.Name()
		if !config.ConfigBackupNameRe.MatchString(file) {
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
	if !config.ConfigBackupNameRe.MatchString(file) {
		return fmt.Errorf("invalid backup file name: %s", file)
	}
	if len(file) <= len(name) || file[:len(name)+1] != name+"." {
		return fmt.Errorf("backup %s does not belong to %s", file, name)
	}
	src := filepath.Join(config.ConfigBackupDir(), file)
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("backup not found")
		}
		return err
	}
	// Compare canonical directories: on macOS /tmp is a symlink to /private/tmp, so a literal
	// comparison would spuriously reject legitimate backups.
	canonDir := config.ConfigBackupDir()
	if real, err := filepath.EvalSymlinks(config.ConfigBackupDir()); err == nil {
		canonDir = real
	}
	if filepath.Dir(resolved) != canonDir {
		return errors.New("backup path traversal rejected")
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return errors.New("backup not found")
	}
	target := configFilePath(name)
	if _, err := config.BackupConfigFile(name, target); err != nil {
		return err
	}
	return config.WriteFileAtomic(target, body, 0o600)
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
