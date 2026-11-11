package scheduler

import (
	"fmt"
	"os"
)

// Runtime log strings stay English (repo convention).
func warnf(format string, args ...any) { fmt.Fprintf(os.Stderr, "[scheduler] "+format+"\n", args...) }
func infof(format string, args ...any) { fmt.Fprintf(os.Stderr, "[scheduler] "+format+"\n", args...) }
