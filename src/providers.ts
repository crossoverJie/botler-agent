import { createProvider, type Model, type Provider } from "@earendil-works/pi-ai";
import { stream as completionsStream, streamSimple as completionsStreamSimple } from "@earendil-works/pi-ai/api/openai-completions";
import { stream as anthropicStream, streamSimple as anthropicStreamSimple } from "@earendil-works/pi-ai/api/anthropic-messages";
import type { CustomProviderConfig, CustomProviderApi, ModelMeta } from "./config.ts";

type AnyApi = "openai-completions" | "anthropic-messages";
type AnyModel = Model<AnyApi>;

/** Build a pi-ai Model object from a model metadata entry, using the provider's wire protocol. */
function buildModel(providerId: string, baseUrl: string, api: CustomProviderApi, m: ModelMeta): AnyModel {
	return {
		id: m.id,
		name: m.name,
		api,
		provider: providerId,
		baseUrl,
		reasoning: m.reasoning,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: m.contextWindow,
		maxTokens: m.maxTokens,
		// openai-completions: this gateway does not support the developer role, so the system prompt is sent as system.
		// anthropic-messages: native `system` field, defaults are fine.
		compat: api === "openai-completions" ? { supportsDeveloperRole: false } : {},
	} as AnyModel;
}

/**
 * Build an OpenAI-completions / Anthropic-messages compatible Provider from a custom provider config.
 * The wire protocol is selected per provider via `cfg.api` (see providers.json).
 */
export function buildCustomProvider(cfg: CustomProviderConfig): Provider<AnyApi> {
	const envKey = cfg.id.toUpperCase().replace(/-/g, "_") + "_API_KEY";
	const api = cfg.api === "anthropic-messages"
		? { stream: anthropicStream, streamSimple: anthropicStreamSimple }
		: { stream: completionsStream, streamSimple: completionsStreamSimple };
	return createProvider({
		id: cfg.id,
		name: cfg.id,
		baseUrl: cfg.baseUrl,
		auth: {
			apiKey: {
				name: `${cfg.id} API key`,
				resolve: async ({ ctx, credential }) => {
					// providers.json apiKey takes priority; the per-provider env var (e.g. ARK_API_KEY) is the fallback / override
					const key = credential?.key ?? cfg.apiKey ?? (await ctx.env(envKey));
					if (key) return { auth: { apiKey: key }, source: cfg.apiKey ? "providers.json" : envKey };
					return undefined;
				},
			},
		},
		models: cfg.models.map((m) => buildModel(cfg.id, cfg.baseUrl, cfg.api, m)),
		api,
	} as Parameters<typeof createProvider>[0]) as Provider<AnyApi>;
}
