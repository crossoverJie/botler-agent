// Package initcmd initializes the externalized config directory (~/.botler-agent/).
//
// It must never touch DATA_ROOT: v1's init.ts only imports config + the prompt module, and the
// startup-order contract (design doc §2.1.1) keeps `botler init` working on a machine without a
// data directory.
package initcmd

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/prompt"
)

//go:embed templates/env.example
var envTemplate string

const promptHeader = `<!-- 外置系统提示词。botler-agent 每次运行都会加载本文件作为 Agent 的 system prompt。
支持占位符（自动注入）：
  __DATA_ROOT__         数据根目录
  __PROJECT_CONTEXT__   各子项目清单 + 其 AGENTS.md/CLAUDE.md 约定全文
  __TODAY__             今天日期（YYYY-MM-DD）

定制方式：把下面的模板替换成你自己项目的约定（各数据子项目的文件结构、操作规则等），
或让 AI 助手根据你的项目背景帮你生成。安全边界（白名单目录、无 bash 等）由
botler-agent 代码强制，不因本文件内容而改变。
-->
`

const providersTemplate = `{
  "providers": {
    "custom": {
      "api": "openai-completions",
      "baseUrl": "https://your-gateway.example.com/v1",
      "apiKey": "sk-REPLACE_ME",
      "models": [
        {
          "id": "your-model-pro",
          "name": "Your Model Pro",
          "reasoning": true,
          "contextWindow": 1048576,
          "maxTokens": 393216,
          "vision": true
        },
        {
          "id": "your-model-flash",
          "name": "Your Model Flash",
          "reasoning": false,
          "contextWindow": 1048576,
          "maxTokens": 393216,
          "vision": true
        }
      ]
    }
  }
}
`

const schedulesTemplate = `{
  "schedules": [
    {
      "id": "daily-example-reminder",
      "enabled": false,
      "cron": "0 8 * * *",
      "timezone": "Asia/Shanghai",
      "message": "Example reminder: log today's activity — what you did and roughly how long",
      "project": "my-project",
      "silentHours": {
        "from": "22:00",
        "to": "07:00"
      }
    }
  ]
}
`

// Run creates the config directory and the four templates, never overwriting existing files.
func Run() error {
	dir := config.UserConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	writeIfAbsent(filepath.Join(dir, ".env"), envTemplate, 0o600,
		"Generated %s from the .env.example template; please review before use",
		"%s already exists; skipped (delete it manually to reset)")

	writeIfAbsent(filepath.Join(dir, "system-prompt.md"), promptHeader+prompt.DefaultSystemPrompt, 0o644,
		"Generated %s (default template; customize or have AI generate it)",
		"%s already exists; skipped")

	writeIfAbsent(filepath.Join(dir, "providers.json"), providersTemplate, 0o600,
		"Generated %s (template; edit baseUrl / apiKey / models, then select via PI_PROVIDER / PI_MODEL)",
		"%s already exists; skipped")

	writeIfAbsent(filepath.Join(dir, "schedules.json"), schedulesTemplate, 0o600,
		"Generated %s (template with a disabled example; set SCHEDULER_ENABLED=1 and enable entries)",
		"%s already exists; skipped")

	fmt.Printf(`
Next steps:
  1. Edit %s and fill in DATA_ROOT, model selection (PI_PROVIDER / PI_MODEL), and channel credentials.
  2. Edit %s with your model providers (baseUrl / apiKey / models).
  3. Customize %s as needed (you can have AI generate it from your project context).
  4. (Optional) Enable the scheduler: set SCHEDULER_ENABLED=1 and edit %s.
  5. Run botler.
`, filepath.Join(dir, ".env"), filepath.Join(dir, "providers.json"),
		filepath.Join(dir, "system-prompt.md"), filepath.Join(dir, "schedules.json"))
	return nil
}

func writeIfAbsent(path, content string, mode os.FileMode, created, existing string) {
	if _, err := os.Stat(path); err == nil {
		fmt.Printf(existing+"\n", path)
		return
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		fmt.Printf("Failed to write %s: %v\n", path, err)
		return
	}
	_ = os.Chmod(path, mode)
	fmt.Printf(created+"\n", path)
}
