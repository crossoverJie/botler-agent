package version

import (
	"regexp"
	"testing"
)

func TestStringWithoutCommit(t *testing.T) {
	savedVersion, savedCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = savedVersion, savedCommit })

	Version = "1.2.3"
	Commit = ""
	if got := String(); got != "1.2.3" {
		t.Fatalf("String() = %q, want the bare version", got)
	}

	Commit = "abc1234"
	if got := String(); got != "1.2.3 (abc1234)" {
		t.Fatalf("String() = %q, want the version with the commit", got)
	}
}

func TestNewIDIsAV4UUID(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if !pattern.MatchString(id) {
			t.Fatalf("not a UUID v4: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = true
	}
}
