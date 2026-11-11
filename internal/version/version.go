// Package version holds the build version injected at link time and small identity helpers.
package version

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Version is the release version; set via
// -ldflags "-X github.com/crossoverJie/botler-agent/internal/version.Version=<tag>".
var Version = "dev"

// Commit is the git commit the binary was built from (optional, same mechanism).
var Commit = ""

// String renders the user-facing version line.
func String() string {
	if Commit == "" {
		return Version
	}
	return Version + " (" + Commit + ")"
}

// NewID returns a random UUID v4 string, used for task-log ids.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}
