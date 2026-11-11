package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// ConfigBackupNameRe matches a config-class backup name ("<file>.<YYYYMMDDTHHMMSS>"). Holiday
// backups (holidays.json.<epochMs>, owned by the scheduler) deliberately do not match, so the
// config prune can never delete them and the WebUI never lists them as restorable.
var ConfigBackupNameRe = regexp.MustCompile(`^(\.env|system-prompt\.md|schedules\.json)\.\d{8}T\d{6}$`)

// MaxConfigBackups is the per-file backup retention (v1 parity).
const MaxConfigBackups = 10

// ConfigBackupDir is <config>/.backups, shared by every config-class file.
func ConfigBackupDir() string { return filepath.Join(UserConfigDir(), ".backups") }

// BackupConfigFile snapshots path as .backups/<name>.<timestamp>, prunes that file's old backups
// and returns the backup file name ("" when there is nothing to back up).
func BackupConfigFile(name, path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	dir := ConfigBackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	file := name + "." + time.Now().Format("20060102T150405")
	if err := os.WriteFile(filepath.Join(dir, file), body, 0o600); err != nil {
		return "", err
	}
	PruneConfigBackups(name)
	return file, nil
}

// PruneConfigBackups keeps only the newest MaxConfigBackups backups of one config file.
func PruneConfigBackups(name string) {
	entries, err := os.ReadDir(ConfigBackupDir())
	if err != nil {
		return
	}
	matched := []string{}
	prefix := name + "."
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > len(prefix) && e.Name()[:len(prefix)] == prefix &&
			ConfigBackupNameRe.MatchString(e.Name()) {
			matched = append(matched, e.Name())
		}
	}
	sort.Strings(matched) // ascending → oldest first
	for len(matched) > MaxConfigBackups {
		_ = os.Remove(filepath.Join(ConfigBackupDir(), matched[0]))
		matched = matched[1:]
	}
}

// WriteFileAtomic replaces path via tmp + rename, so a crash never leaves a half-written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
