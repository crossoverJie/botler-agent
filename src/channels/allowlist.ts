/**
 * Shared channel sender allowlist helper.
 *
 * Empty/unset allowlist means allow all, preserving the previous personal-deploy behavior.
 * Once configured, at least one candidate identity must match exactly.
 */
export function isAllowedSender(rawAllowlist: string | undefined, identities: ReadonlyArray<string | undefined>): boolean {
	const allowed = (rawAllowlist ?? "")
		.split(",")
		.map((item) => item.trim())
		.filter(Boolean);
	if (allowed.length === 0) return true;
	return identities.some((identity) => {
		if (typeof identity !== "string") return false;
		const cleaned = identity.trim();
		return cleaned.length > 0 && allowed.includes(cleaned);
	});
}
