//! DATA_ROOT path allowlist — the framework's primary security boundary.
//!
//! The allowlist is the set of first-level, non-hidden subdirectories of
//! `DATA_ROOT`, computed once at startup. Rules preserved from v1:
//! - relative paths resolve against `DATA_ROOT`; absolute paths stay absolute
//!   but must still fall inside the allowlist;
//! - prefix matching uses `root + separator`, so `/agent2` never matches
//!   `/agent`;
//! - the deepest *existing* ancestor is canonicalized and the remaining path is
//!   appended back, which blocks symlink escapes without failing on new files;
//! - an optional per-task project subset further narrows the allowlist.

use std::collections::HashMap;
use std::fs;
use std::path::{Component, Path, PathBuf};

use anyhow::{bail, Result};

#[derive(Debug, Clone)]
pub struct PathGuard {
    data_root: PathBuf,
    /// Canonicalized first-level project roots.
    roots: Vec<PathBuf>,
    /// Project name -> canonical root.
    by_name: HashMap<String, PathBuf>,
    follow_symlinks: bool,
}

impl PathGuard {
    pub fn new(data_root: impl AsRef<Path>) -> Result<Self> {
        let data_root = data_root.as_ref();
        if data_root.as_os_str().is_empty() {
            bail!(
                "DATA_ROOT is not set. Configure DATA_ROOT (the data root operated by the agent) in ~/.botler-agent/.env or the source .env."
            );
        }
        let data_root = lexical_absolute(data_root);
        if !data_root.exists() {
            bail!("DATA_ROOT does not exist: {}", data_root.display());
        }
        // Canonicalize the root itself: on macOS `/var` is a symlink to
        // `/private/var`, and project roots below are canonicalized too.
        // Without this every containment check would compare a lexical path
        // against canonical roots and reject valid files.
        let data_root = fs::canonicalize(&data_root).unwrap_or(data_root);

        let mut roots = Vec::new();
        let mut by_name = HashMap::new();
        for entry in fs::read_dir(&data_root)? {
            let entry = entry?;
            if !entry.file_type()?.is_dir() {
                continue;
            }
            let name = entry.file_name();
            let name = name.to_string_lossy().to_string();
            if name.starts_with('.') {
                continue;
            }
            let full = entry.path();
            let canonical = fs::canonicalize(&full).unwrap_or(full);
            roots.push(canonical.clone());
            by_name.insert(name, canonical);
        }
        if roots.is_empty() {
            bail!(
                "DATA_ROOT has no subproject directories: {}",
                data_root.display()
            );
        }
        roots.sort();
        Ok(Self {
            data_root,
            roots,
            by_name,
            follow_symlinks: true,
        })
    }

    pub fn data_root(&self) -> &Path {
        &self.data_root
    }

    /// First-level project names, sorted.
    pub fn project_names(&self) -> Vec<String> {
        let mut names: Vec<String> = self.by_name.keys().cloned().collect();
        names.sort();
        names
    }

    pub fn project_root(&self, name: &str) -> Option<&Path> {
        self.by_name.get(name).map(PathBuf::as_path)
    }

    pub fn with_follow_symlinks(mut self, follow: bool) -> Self {
        self.follow_symlinks = follow;
        self
    }

    /// Resolve and authorize a path. `projects` is an optional task-local
    /// subset (additive on top of the global allowlist).
    pub fn safe_path(&self, p: impl AsRef<Path>, projects: Option<&[String]>) -> Result<PathBuf> {
        let p = p.as_ref();
        let abs = lexical_absolute(self.data_root.join(p));
        if !self.is_allowed(&abs) {
            bail!(
                "Path out of bounds (not within the allowed directory): {}",
                p.display()
            );
        }
        self.assert_selected(&abs, projects)?;

        if self.follow_symlinks {
            let mut anc = abs.clone();
            let mut tail: Vec<std::ffi::OsString> = Vec::new();
            while !anc.exists() {
                let Some(name) = anc.file_name().map(|n| n.to_os_string()) else {
                    bail!("Path out of bounds: {}", p.display());
                };
                tail.push(name);
                if !anc.pop() {
                    bail!("Path out of bounds: {}", p.display());
                }
            }
            let mut real = fs::canonicalize(&anc)?;
            for part in tail.into_iter().rev() {
                real.push(part);
                // `exists()` follows links, so a symlink whose target does not
                // exist yet is not part of the canonicalized ancestor. Reject
                // it explicitly: `create_dir_all` would otherwise follow it
                // outside the allowlist.
                match fs::symlink_metadata(&real) {
                    Ok(meta) if meta.file_type().is_symlink() => {
                        bail!("Path out of bounds (symlink escape): {}", p.display());
                    }
                    Ok(_) => {}
                    Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
                    Err(err) => return Err(err.into()),
                }
            }
            if !self.is_allowed(&real) {
                bail!("Path out of bounds (symlink escape): {}", p.display());
            }
            self.assert_selected(&real, projects)?;
            return Ok(real);
        }
        Ok(abs)
    }

    /// Which project a (resolved) path belongs to, if any.
    pub fn project_of(&self, abs: &Path) -> Option<String> {
        for (name, root) in &self.by_name {
            if is_within(abs, root) {
                return Some(name.clone());
            }
        }
        None
    }

    fn is_allowed(&self, abs: &Path) -> bool {
        self.roots.iter().any(|root| is_within(abs, root))
    }

    fn assert_selected(&self, abs: &Path, projects: Option<&[String]>) -> Result<()> {
        let Some(projects) = projects else {
            return Ok(());
        };
        let matched = self.project_of(abs);
        match matched {
            Some(name) if projects.iter().any(|p| p == &name) => Ok(()),
            _ => bail!("Path not selected for this task: {}", abs.display()),
        }
    }
}

/// `root == p` or `p` starts with `root + separator`.
pub fn is_within(p: &Path, root: &Path) -> bool {
    if p == root {
        return true;
    }
    match p.strip_prefix(root) {
        Ok(rest) => !rest.as_os_str().is_empty(),
        Err(_) => false,
    }
}

/// Lexically normalize an absolute path (mirrors Node's `path.resolve`).
pub fn lexical_absolute(p: impl AsRef<Path>) -> PathBuf {
    let p = p.as_ref();
    let mut out = PathBuf::new();
    for comp in p.components() {
        match comp {
            Component::Prefix(_) | Component::RootDir => out.push(comp.as_os_str()),
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    // Already at root; `..` is a no-op.
                }
            }
            Component::Normal(part) => out.push(part),
        }
    }
    if out.as_os_str().is_empty() {
        PathBuf::from(std::path::MAIN_SEPARATOR.to_string())
    } else {
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use proptest::prelude::*;

    fn guard_with_projects() -> (tempfile::TempDir, PathGuard) {
        let tmp = tempfile::tempdir().unwrap();
        let root = fs::canonicalize(tmp.path()).unwrap();
        fs::create_dir_all(root.join("notes")).unwrap();
        fs::create_dir_all(root.join("vocab")).unwrap();
        fs::create_dir_all(root.join(".hidden")).unwrap();
        fs::write(root.join("notes/data.json"), "{}").unwrap();
        let guard = PathGuard::new(&root).unwrap();
        (tmp, guard)
    }

    fn safe_component() -> impl Strategy<Value = String> {
        "[A-Za-z0-9_ -]{1,12}".prop_filter("components must not be traversal", |s| {
            s != "." && s != ".."
        })
    }

    fn config() -> ProptestConfig {
        ProptestConfig::with_cases(48)
    }

    #[test]
    fn allows_relative_paths_inside_projects() {
        let (_tmp, guard) = guard_with_projects();
        let p = guard.safe_path("notes/data.json", None).unwrap();
        assert!(p.ends_with("notes/data.json"));
        assert_eq!(guard.project_of(&p).as_deref(), Some("notes"));
    }

    #[test]
    fn rejects_prefix_trick() {
        let (_tmp, guard) = guard_with_projects();
        let sibling = guard.data_root().parent().unwrap().join("notes2");
        fs::create_dir_all(&sibling).unwrap();
        let err = guard
            .safe_path(sibling.join("data.json"), None)
            .unwrap_err()
            .to_string();
        assert!(err.contains("out of bounds"), "{err}");
    }

    #[test]
    fn rejects_parent_traversal() {
        let (_tmp, guard) = guard_with_projects();
        assert!(guard.safe_path("notes/../../etc/passwd", None).is_err());
        assert!(guard.safe_path("../outside.json", None).is_err());
    }

    #[test]
    fn allows_new_file_under_project() {
        let (_tmp, guard) = guard_with_projects();
        let p = guard.safe_path("notes/new/dir/file.json", None).unwrap();
        assert!(p.ends_with("notes/new/dir/file.json"));
    }

    #[test]
    fn rejects_hidden_project_dirs() {
        let (_tmp, guard) = guard_with_projects();
        assert!(!guard.project_names().contains(&".hidden".to_string()));
        assert!(guard.safe_path(".hidden/x.json", None).is_err());
    }

    #[test]
    fn selected_project_subset_is_enforced() {
        let (_tmp, guard) = guard_with_projects();
        let only_notes = vec!["notes".to_string()];
        assert!(guard
            .safe_path("notes/data.json", Some(&only_notes))
            .is_ok());
        assert!(guard.safe_path("vocab/x.json", Some(&only_notes)).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn rejects_symlink_escape_via_existing_ancestor() {
        use std::os::unix::fs::symlink;
        let tmp = tempfile::tempdir().unwrap();
        let root = fs::canonicalize(tmp.path()).unwrap();
        let outside = root.join("outside");
        fs::create_dir_all(&outside).unwrap();
        fs::write(outside.join("secret.json"), "{}").unwrap();
        fs::create_dir_all(root.join("data/notes")).unwrap();
        symlink(&outside, root.join("data/notes/link")).unwrap();
        let guard = PathGuard::new(root.join("data")).unwrap();
        // The file exists only via the symlink.
        assert!(guard.safe_path("notes/link/secret.json", None).is_err());
        // A new file through the symlinked dir must be rejected too.
        assert!(guard.safe_path("notes/link/new.json", None).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn rejects_dangling_symlink_to_a_missing_outside_target() {
        use std::os::unix::fs::symlink;
        let tmp = tempfile::tempdir().unwrap();
        let root = fs::canonicalize(tmp.path()).unwrap();
        let outside = root.join("outside");
        fs::create_dir_all(&outside).unwrap();
        fs::create_dir_all(root.join("data/notes")).unwrap();
        // The target intentionally does not exist, so `Path::exists()` is
        // false and the old deepest-ancestor walk would skip this link.
        symlink(
            outside.join("not-created-yet"),
            root.join("data/notes/link"),
        )
        .unwrap();
        let guard = PathGuard::new(root.join("data")).unwrap();
        assert!(guard.safe_path("notes/link/file.json", None).is_err());
        assert!(guard.safe_path("notes/link/sub/file.json", None).is_err());
    }

    proptest! {
        #![proptest_config(config())]

        #[test]
        fn relative_paths_resolve_under_the_selected_project(
            parts in prop::collection::vec(safe_component(), 1..4)
        ) {
            let (_tmp, guard) = guard_with_projects();
            let rel = format!("notes/{}", parts.join("/"));
            let resolved = guard.safe_path(&rel, None).unwrap();
            let project = guard.project_root("notes").unwrap();
            let resolved_project = guard.project_of(&resolved);
            prop_assert!(is_within(&resolved, guard.data_root()));
            prop_assert!(is_within(&resolved, project));
            prop_assert_eq!(resolved_project.as_deref(), Some("notes"));
        }

        #[test]
        fn accepted_input_never_escapes_the_boundary(raw in any::<String>()) {
            let (_tmp, guard) = guard_with_projects();
            if let Ok(resolved) = guard.safe_path(&raw, None) {
                prop_assert!(is_within(&resolved, guard.data_root()));
            }
        }

        #[test]
        fn first_level_directories_outside_the_allowlist_are_rejected(
            name in "\\.[A-Za-z0-9_-]{0,10}"
        ) {
            let (_tmp, guard) = guard_with_projects();
            let candidate = format!("{name}/x.json");
            prop_assert!(guard.safe_path(&candidate, None).is_err());
        }

        #[test]
        fn sibling_names_with_the_project_as_a_prefix_are_rejected(
            suffix in "[a-z]{1,8}"
        ) {
            let tmp = tempfile::tempdir().unwrap();
            let data = tmp.path().join("data");
            fs::create_dir_all(data.join("notes")).unwrap();
            fs::create_dir_all(tmp.path().join(format!("notes{suffix}"))).unwrap();
            let guard = PathGuard::new(&data).unwrap();

            let sibling = tmp.path().join(format!("notes{suffix}/data.json"));
            let relative = format!("../notes{suffix}/data.json");
            prop_assert!(guard.safe_path(&sibling, None).is_err());
            prop_assert!(guard.safe_path(&relative, None).is_err());
        }

        #[test]
        fn traversal_sequences_never_leave_the_boundary(
            parts in prop::collection::vec(
                prop::sample::select(vec!["..", "notes", "vocab", "x", "data.json"]),
                1..8,
            )
        ) {
            let (_tmp, guard) = guard_with_projects();
            let raw = parts.join("/");
            if let Ok(resolved) = guard.safe_path(&raw, None) {
                prop_assert!(is_within(&resolved, guard.data_root()));
            }
        }

        #[cfg(unix)]
        #[test]
        fn symlink_escapes_are_rejected_for_any_new_file(name in "[A-Za-z0-9_-]{1,10}") {
            use std::os::unix::fs::symlink;
            let tmp = tempfile::tempdir().unwrap();
            let root = fs::canonicalize(tmp.path()).unwrap();
            let outside = root.join("outside");
            fs::create_dir_all(&outside).unwrap();
            fs::write(outside.join("secret.json"), "{}").unwrap();
            fs::create_dir_all(root.join("data/notes")).unwrap();
            symlink(&outside, root.join("data/notes/link")).unwrap();
            let guard = PathGuard::new(root.join("data")).unwrap();

            prop_assert!(guard.safe_path("notes/link/secret.json", None).is_err());
            let candidate = format!("notes/link/{name}.json");
            prop_assert!(guard.safe_path(&candidate, None).is_err());
        }
    }
}
