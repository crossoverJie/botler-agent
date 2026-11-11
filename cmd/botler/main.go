// Command botler is the single entry point: subcommands (init / wechat-login / wechat-logout /
// --version), CLI one-shot mode, or the persistent channel service.
//
// Startup-order contract (design doc §2.1.1): `init` and `--version` return before
// paths.Validate(), every other entry validates DATA_ROOT explicitly in main(), and the service
// order is health server → "no channel => exit 1" → WebUI → scheduler.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/crossoverJie/botler-agent/internal/channels/telegram"
	"github.com/crossoverJie/botler-agent/internal/channels/wechat"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/dispatch"
	"github.com/crossoverJie/botler-agent/internal/initcmd"
	"github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/scheduler"
	"github.com/crossoverJie/botler-agent/internal/version"
	"github.com/crossoverJie/botler-agent/internal/webui"
)

func main() {
	args := os.Args[1:]
	// v1's rule: the first argument that is not a long flag is the positional. A short flag such as
	// "-v" is a message, not a mode switch.
	positional := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			positional = a
			break
		}
	}

	// --version: a new entry point that must work without a data directory. It is only recognised as
	// the first argument so a message that happens to contain the word "version" is still a message.
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println(version.String())
		return
	}

	// init: never touches DATA_ROOT (v1 parity).
	if positional == "init" {
		if err := initcmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[init] failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Every other entry validates the allowlist up front, preserving v1's "die at startup".
	if err := paths.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch positional {
	case "wechat-login":
		if err := wechat.Login(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[wechat] login failed: %v\n", err)
			os.Exit(1)
		}
		return
	case "wechat-logout":
		wechat.Logout()
		fmt.Println("✅ WeChat session logged out")
		return
	}

	cfg := config.C()
	if positional == "" {
		fmt.Printf("[main] model: %s/%s (config: %s)\n", cfg.Provider, cfg.Model, config.UserConfigDir())
	}

	// CLI one-shot mode: run a single message and exit (local debugging / end-to-end testing).
	if positional != "" {
		reply := dispatch.Dispatch(ctx, positional, dispatch.Options{Source: "cli"})
		fmt.Println(reply.Text)
		if len(reply.Images) > 0 {
			fmt.Printf("[images] %s\n", strings.Join(reply.Images, ", "))
		}
		return
	}

	// Persistent channels, started per .env config. The health server comes first (independent of
	// the WebUI; an ops requirement).
	if cfg.MonitorEnabled {
		monitor.StartHealthServer()
	}

	started := false
	if cfg.TelegramToken != "" {
		if err := telegram.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[main] Failed to start Telegram: %v\n", err)
		} else {
			started = true
		}
	}
	if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
		// v3.0 does not implement Feishu; the config keys stay parseable so existing .env files
		// never error out.
		fmt.Println("[feishu] not implemented in this build; skipped")
	}
	if cfg.WeChatEnabled {
		if err := wechat.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[main] Failed to start WeChat: %v\n", err)
		} else {
			started = true
		}
	}

	if !started {
		fmt.Fprintln(os.Stderr,
			"No channel configured (TELEGRAM_BOT_TOKEN / WECHAT_ENABLED).\n"+
				"Run in CLI mode: botler \"your message\"\n"+
				"Or run `botler init` and fill in the channel credentials.")
		os.Exit(1)
	}

	if cfg.WebUIEnabled {
		webui.Start(ctx)
	}
	if cfg.SchedulerEnabled {
		if err := scheduler.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[main] Failed to start scheduler: %v\n", err)
		}
	}

	<-ctx.Done()
	fmt.Println("[main] shutting down")
}
