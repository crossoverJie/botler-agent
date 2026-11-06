//! Shared sender allowlist helper.
//!
//! Empty/unset means allow all (personal deployments); once configured, at
//! least one candidate identity must match exactly. Ported verbatim from v1
//! `src/channels/allowlist.ts`.

pub fn is_allowed_sender(raw_allowlist: Option<&str>, identities: &[Option<&str>]) -> bool {
    let allowed: Vec<&str> = raw_allowlist
        .unwrap_or("")
        .split(',')
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .collect();
    if allowed.is_empty() {
        return true;
    }
    identities.iter().flatten().any(|identity| {
        let cleaned = identity.trim();
        !cleaned.is_empty() && allowed.contains(&cleaned)
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_allowlist_allows_everyone() {
        assert!(is_allowed_sender(None, &[Some("u1")]));
        assert!(is_allowed_sender(Some("  "), &[Some("u1")]));
    }

    #[test]
    fn configured_allowlist_requires_an_exact_match() {
        assert!(!is_allowed_sender(Some("u1,u2"), &[Some("u3"), None]));
        assert!(is_allowed_sender(Some("u1, u2"), &[Some("u2")]));
        // A near miss must not pass.
        assert!(!is_allowed_sender(Some("u1"), &[Some("u1x")]));
    }
}
