package allowlist

import "testing"

func TestAllowedEmptyAllowlistAllowsEveryone(t *testing.T) {
	if !Allowed("", []string{"123"}) {
		t.Fatal("empty allowlist must allow every sender")
	}
	if !Allowed("  ,  ", nil) {
		t.Fatal("allowlist with only separators must allow every sender")
	}
}

func TestAllowedMatchesAnyCandidateExactly(t *testing.T) {
	raw := " 42, alice ,-100123 "
	cases := []struct {
		name string
		ids  []string
		want bool
	}{
		{"from id", []string{"42", "", ""}, true},
		{"username", []string{"7", "alice", "-100123"}, true},
		{"chat id", []string{"", "", "-100123"}, true},
		{"no match", []string{"43", "bob", "-100999"}, false},
		{"empty candidates never match", []string{"", "", ""}, false},
	}
	for _, tc := range cases {
		if got := Allowed(raw, tc.ids); got != tc.want {
			t.Fatalf("%s: Allowed(%q) = %v, want %v", tc.name, tc.ids, got, tc.want)
		}
	}
}

func TestParseTrimsAndDropsEmptyEntries(t *testing.T) {
	got := Parse(" a, ,b ,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Parse = %v", got)
	}
}
