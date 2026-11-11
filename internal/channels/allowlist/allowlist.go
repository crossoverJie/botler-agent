// Package allowlist implements the shared channel sender allowlist.
//
// An empty/unset allowlist means "allow everyone", preserving the previous personal-deploy
// behaviour. Once configured, at least one candidate identity must match exactly.
package allowlist

import "strings"

// Parse splits a raw comma-separated allowlist into trimmed, non-empty entries.
func Parse(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Allowed reports whether any candidate identity is in the allowlist. An empty allowlist allows
// everyone; empty candidate identities never match.
func Allowed(raw string, identities []string) bool {
	allowed := Parse(raw)
	if len(allowed) == 0 {
		return true
	}
	for _, identity := range identities {
		cleaned := strings.TrimSpace(identity)
		if cleaned == "" {
			continue
		}
		for _, a := range allowed {
			if a == cleaned {
				return true
			}
		}
	}
	return false
}
