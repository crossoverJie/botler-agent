import { mkdirSync, writeFileSync, existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { APP_ROOT, USER_CONFIG_DIR } from "./config.ts";
import { DEFAULT_SYSTEM_PROMPT } from "./prompts/system-prompt.ts";

/**
 * Initialize the user-level config directory ~/.botler-agent/:
 *   - .env              copied from .env.example as a template (no real values filled in)
 *   - providers.json    custom model provider metadata template (baseUrl / apiKey / models)
 *   - system-prompt.md  default generic template (user can customize or have AI generate it)
 * Existing files are skipped and never overwritten.
 */
function main(): void {
	mkdirSync(USER_CONFIG_DIR, { recursive: true, mode: 0o700 });

	// 1. .env (migration first: if a source .env exists, migrate its real config; otherwise use the .env.example template)
	const envTarget = join(USER_CONFIG_DIR, ".env");
	if (!existsSync(envTarget)) {
		const sourceEnv = join(APP_ROOT, ".env");
		const envExample = join(APP_ROOT, ".env.example");
		const src = existsSync(sourceEnv) ? sourceEnv : envExample;
		if (existsSync(src)) {
			const fromSource = src === sourceEnv;
			writeFileSync(envTarget, readFileSync(src, "utf8"), { encoding: "utf8", mode: 0o600 });
			console.log(`Generated ${envTarget} (${fromSource ? "migrated from source .env" : "from .env.example template"}; please review before use)`);
		} else {
			console.log(`No .env or .env.example found; skipping .env generation`);
		}
	} else {
		console.log(`${envTarget} already exists; skipped (delete it manually to reset)`);
	}

	// 2. system-prompt.md template
	const promptTarget = join(USER_CONFIG_DIR, "system-prompt.md");
	if (!existsSync(promptTarget)) {
		const header = `<!-- 外置系统提示词。botler-agent 每次运行都会加载本文件作为 Agent 的 system prompt。
支持占位符（自动注入）：
  __DATA_ROOT__         数据根目录
  __PROJECT_CONTEXT__   各子项目清单 + 其 AGENTS.md/CLAUDE.md 约定全文
  __TODAY__             今天日期（YYYY-MM-DD）

定制方式：把下面的模板替换成你自己项目的约定（各数据子项目的文件结构、操作规则等），
或让 AI 助手根据你的项目背景帮你生成。安全边界（白名单目录、无 bash 等）由
botler-agent 代码强制，不因本文件内容而改变。
-->
`;
		writeFileSync(promptTarget, header + DEFAULT_SYSTEM_PROMPT, "utf8");
		console.log(`Generated ${promptTarget} (default template; customize or have AI generate it)`);
	} else {
		console.log(`${promptTarget} already exists; skipped`);
	}

	// 3. providers.json template (custom OpenAI-completions providers; the source of truth for model metadata)
	const providersTarget = join(USER_CONFIG_DIR, "providers.json");
	if (!existsSync(providersTarget)) {
		const template = JSON.stringify(
			{
				providers: {
					custom: {
						api: "openai-completions",
						baseUrl: "https://your-gateway.example.com/v1",
						apiKey: "sk-REPLACE_ME",
						models: [
							// vision defaults to true (image input enabled); set `vision: false` for text-only models.
							{ id: "your-model-pro", name: "Your Model Pro", reasoning: true, contextWindow: 1048576, maxTokens: 393216, vision: true },
							{ id: "your-model-flash", name: "Your Model Flash", reasoning: false, contextWindow: 1048576, maxTokens: 393216, vision: true },
						],
					},
				},
			},
			null,
			2,
		);
		writeFileSync(providersTarget, template + "\n", { encoding: "utf8", mode: 0o600 });
		console.log(`Generated ${providersTarget} (template; edit baseUrl / apiKey / models, then select via PI_PROVIDER / PI_MODEL)`);
	} else {
		console.log(`${providersTarget} already exists; skipped`);
	}

	// 4. schedules.json template (in-process scheduler; disabled example entry)
	const schedulesTarget = join(USER_CONFIG_DIR, "schedules.json");
	if (!existsSync(schedulesTarget)) {
		const template = JSON.stringify(
			{
				schedules: [
					{
						id: "daily-example-reminder",
						enabled: false,
						cron: "0 8 * * *",
						timezone: "Asia/Shanghai",
						message: "Example reminder: log today's activity — what you did and roughly how long",
						project: "my-project",
						silentHours: { from: "22:00", to: "07:00" },
					},
				],
			},
			null,
			2,
		);
		writeFileSync(schedulesTarget, template + "\n", { encoding: "utf8", mode: 0o600 });
		console.log(`Generated ${schedulesTarget} (template with a disabled example; set SCHEDULER_ENABLED=1 and enable entries)`);
	} else {
		console.log(`${schedulesTarget} already exists; skipped`);
	}

	console.log(`
Next steps:
  1. Edit ${join(USER_CONFIG_DIR, ".env")} and fill in DATA_ROOT, model selection (PI_PROVIDER / PI_MODEL), and channel credentials.
  2. Edit ${join(USER_CONFIG_DIR, "providers.json")} with your model providers (baseUrl / apiKey / models).
  3. Customize ${join(USER_CONFIG_DIR, "system-prompt.md")} as needed (you can have AI generate it from your project context).
  4. (Optional) Enable the scheduler: set SCHEDULER_ENABLED=1 and edit ${join(USER_CONFIG_DIR, "schedules.json")}.
  5. Run npm start.`);
}

main();
