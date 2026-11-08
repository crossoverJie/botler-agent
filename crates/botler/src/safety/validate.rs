//! Post-write validation: every data JSON file must still parse.
//!
//! `write` guarantees serialization; `edit` is text replacement and can break
//! JSON syntax, so this is the safety net. Business semantics stay in each
//! subproject's AGENTS.md; the framework never hardcodes them.

use std::fs;
use std::path::{Path, PathBuf};

use crate::paths::PathGuard;

const SKIP_DIRS: &[&str] = &[
    "node_modules",
    ".git",
    "dist",
    "public",
    ".pi",
    ".codebuddy",
];

#[derive(Debug, Clone)]
pub struct ValidationResult {
    pub ok: bool,
    /// Self-contained fix instruction: file + failure + what to do.
    pub fix: Option<String>,
}

fn walk_json(dir: &Path, out: &mut Vec<PathBuf>) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        let name = entry.file_name().to_string_lossy().to_string();
        if name.starts_with('.') {
            continue;
        }
        let full = entry.path();
        if file_type.is_dir() {
            if SKIP_DIRS.contains(&name.as_str()) {
                continue;
            }
            walk_json(&full, out);
        } else if file_type.is_file() && name.to_ascii_lowercase().ends_with(".json") {
            out.push(full);
        }
    }
}

fn collect_json_files(guard: &PathGuard, projects: Option<&[String]>) -> Vec<PathBuf> {
    let names = match projects {
        Some(names) => names.to_vec(),
        None => guard.project_names(),
    };
    let mut out = Vec::new();
    for name in names {
        // Unknown or traversal names can never escape: project_root consults
        // the startup allowlist map, not a joined path.
        if let Some(root) = guard.project_root(&name) {
            walk_json(root, &mut out);
        }
    }
    out
}

/// Validate every JSON file in the selected projects (or all projects when
/// `projects` is `None`). Read failures are treated as "not our problem" only
/// when a file disappears mid-scan; otherwise they skip (v1 behaviour).
pub fn validate_state(guard: &PathGuard, projects: Option<&[String]>) -> ValidationResult {
    for path in collect_json_files(guard, projects) {
        let Ok(raw) = fs::read_to_string(&path) else {
            continue;
        };
        if let Err(err) = serde_json::from_str::<serde_json::Value>(&raw) {
            let relative = path
                .strip_prefix(guard.data_root())
                .map(|p| p.to_string_lossy().to_string())
                .unwrap_or_else(|_| path.display().to_string());
            return ValidationResult {
                ok: false,
                fix: Some(format!(
                    "{relative} is not valid JSON (parse failed: {err}). Read the file, fix the syntax error, and write it back. Do not create a new file."
                )),
            };
        }
    }
    ValidationResult {
        ok: true,
        fix: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flags_broken_json_and_points_to_the_file() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("notes/data")).unwrap();
        std::fs::write(dir.path().join("notes/data/ok.json"), r#"{"a":1}"#).unwrap();
        std::fs::write(dir.path().join("notes/data/bad.json"), r#"{"a":}"#).unwrap();
        // PathGuard canonicalizes; the temp dir is real.
        let guard = PathGuard::new(dir.path()).unwrap();
        let result = validate_state(&guard, Some(&["notes".into()]));
        assert!(!result.ok);
        let fix = result.fix.unwrap();
        assert!(fix.contains("notes/data/bad.json"));
        assert!(fix.contains("Do not create a new file"));
    }

    #[test]
    fn scoped_validation_ignores_a_broken_sibling() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("a")).unwrap();
        std::fs::create_dir_all(dir.path().join("b")).unwrap();
        std::fs::write(dir.path().join("a/ok.json"), "{}").unwrap();
        std::fs::write(dir.path().join("b/bad.json"), "{").unwrap();
        let guard = PathGuard::new(dir.path()).unwrap();
        assert!(validate_state(&guard, Some(&["a".into()])).ok);
        assert!(!validate_state(&guard, Some(&["b".into()])).ok);
        assert!(!validate_state(&guard, None).ok);
    }
}
