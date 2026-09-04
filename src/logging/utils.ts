import { SCHEDULER_VIRTUAL_PROJECT } from "../prompts/system-prompt.ts";

/**
 * Legacy singular display field derived from the selected set: non-null only when exactly one
 * real data project was selected. A multi-project log therefore has `project === null` but a
 * non-empty `projects`. The virtual `__scheduler__` project is never a data project, so it
 * also yields null.
 */
export function legacyProject(projects: readonly string[]): string | null {
	return projects.length === 1 && projects[0] !== SCHEDULER_VIRTUAL_PROJECT ? projects[0] : null;
}
