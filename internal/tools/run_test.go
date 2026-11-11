package tools

import (
	"strings"
	"testing"
)

// v1's execFileSync throws once a script exceeds the 1MB maxBuffer, so the run is a runtime failure
// rather than a silent success on truncated output.
func TestRunScriptExceedingMaxBufferFails(t *testing.T) {
	dir := withRoot(t, "cook")
	script := dir + "/cook/loud.py"
	if err := writeFile(script, "print('x' * (1024 * 1024 + 64))\n"); err != nil {
		t.Fatal(err)
	}
	res := RunScript("cook/loud.py", nil)
	if res.OK {
		t.Fatal("an oversized script must not be reported as success")
	}
	if res.ErrorKind != ErrRuntime {
		t.Fatalf("unexpected error kind %q", res.ErrorKind)
	}
	if !strings.Contains(res.Text, "maxBuffer exceeded") {
		t.Fatalf("unexpected text: %q", res.Text)
	}
}

// v1 returns stdout untrimmed (only the emptiness test trims).
func TestRunScriptKeepsTrailingNewline(t *testing.T) {
	dir := withRoot(t, "cook")
	if err := writeFile(dir+"/cook/echo.py", "print('hello')\n"); err != nil {
		t.Fatal(err)
	}
	res := RunScript("cook/echo.py", nil)
	if !res.OK {
		t.Fatalf("unexpected failure: %+v", res)
	}
	if res.Text != "hello\n" {
		t.Fatalf("stdout must be returned untrimmed, got %q", res.Text)
	}
}
