package scheduler

import (
	"fmt"
	"os"
)

// Package log sinks. Runtime log strings stay English (repo convention). They are plain
// variables (not synchronized) because they are only assigned at startup or by tests.
var (
	warnf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[scheduler] "+format+"\n", args...) }
	infof = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[scheduler] "+format+"\n", args...) }
)

// SetLogHandlers replaces the package log sinks (tests / embedders). nil keeps the current one.
func SetLogHandlers(warn, info func(format string, args ...any)) {
	if warn != nil {
		warnf = warn
	}
	if info != nil {
		infof = info
	}
}
