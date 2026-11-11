package tools

import "github.com/crossoverJie/botler-agent/internal/agentrt"

// DataTools are available to every execution (no framework-level conversation control).
func DataTools() []agentrt.Tool {
	return []agentrt.Tool{readTool{}, writeTool{}, editTool{}, runTool{}, newScheduleTool()}
}

// FileTools are the execution tools for IM user tasks; the extra tool only clears the active
// conversation session and takes no path parameter.
func FileTools() []agentrt.Tool {
	return append(DataTools(), clearConversationTool{})
}
