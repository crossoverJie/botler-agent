package dispatch

import (
	"context"

	"github.com/crossoverJie/botler-agent/internal/scheduler"
)

// This adapter wires the scheduler engine to the dispatch pipeline.
//
// The dependency has to stay one-way: dispatch reaches the `schedule` tool through
// runner → tools, and that tool imports internal/scheduler, so the scheduler package cannot
// import dispatch without forming an import cycle. The engine therefore exposes a registration
// hook (scheduler.SetDispatch) and this package — which already imports everything it needs —
// installs it from its own init, so any binary that links both packages is wired automatically.
func init() {
	scheduler.SetDispatch(
		func(ctx context.Context, id, message, projectHint string) scheduler.FireResult {
			res := Dispatch(ctx, message, Options{
				ID:          id,
				Source:      "scheduler",
				ProjectHint: projectHint,
			})
			return scheduler.FireResult{Text: res.Text, Images: res.Images, Status: string(res.Status)}
		},
		func(ctx context.Context, id, script string, args []string, commit *bool) scheduler.FireResult {
			res := DispatchScriptedTask(ctx, ScriptTaskOptions{
				ID:         id,
				Script:     script,
				ScriptArgs: args,
				Commit:     commit,
				Source:     "scheduler",
			})
			return scheduler.FireResult{Text: res.Text, Images: res.Images, Status: string(res.Status)}
		},
	)
}
