package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
)

func withProject(t *testing.T, name, agents string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Set(&config.Config{DataRoot: root})
	return root
}

// v1 strips only the FIRST heading line when building a routing summary.
func TestProjectSummaryStripsOnlyFirstHeading(t *testing.T) {
	withProject(t, "cook", "# cook\n\n## 数据\n记账在 cook/data.json\n\n路由提示：「记账」\n")
	got := BuildRoutePrompt("记一笔", RouteOptions{IncludeScheduler: true})
	if strings.Contains(got, "# cook") {
		t.Fatal("the first heading must be stripped")
	}
	if !strings.Contains(got, "## 数据") {
		t.Fatal("later headings must be preserved in the summary")
	}
}

func TestLoadSystemPromptInjectsPlaceholders(t *testing.T) {
	root := withProject(t, "cook", "# cook\n记账\n")
	got := LoadSystemPrompt([]string{"cook"})
	if strings.Contains(got, "__DATA_ROOT__") || strings.Contains(got, "__PROJECT_CONTEXT__") || strings.Contains(got, "__TODAY__") {
		t.Fatal("placeholders must be substituted")
	}
	if !strings.Contains(got, root) {
		t.Fatal("DATA_ROOT must be injected")
	}
	if !strings.Contains(got, "记账") {
		t.Fatal("the selected project's convention doc must be included")
	}
}

func TestLoadSystemPromptUsesSchedulerContext(t *testing.T) {
	withProject(t, "cook", "# cook\n记账\n")
	got := LoadSystemPrompt([]string{SchedulerVirtualProject})
	if !strings.Contains(got, "定时任务管理") {
		t.Fatal("the scheduler virtual project must render the scheduling section")
	}
	if strings.Contains(got, "记账数据") {
		t.Fatal("scheduler context must not include data-project rules")
	}
}
