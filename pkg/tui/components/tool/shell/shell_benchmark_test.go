package shell

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

var shellOutputBenchmarkResult string

func BenchmarkFormatShellOutput(b *testing.B) {
	fixtures := []struct {
		name   string
		output string
		width  int
	}{
		{"ShortCommand", "total 8\n-rw-r--r-- 1 user staff 2048 Oct 9 10:00 README.md\n", 100},
		{"BuildLog5000", shellBuildLogFixture(5000, "\n", false), 100},
		{"ColorUnicodeCRLF2500", shellBuildLogFixture(2500, "\r\n", true), 80},
		{"LongJSONLine", `{"results":[` + strings.TrimSuffix(strings.Repeat(`{"path":"pkg/tui/components/tool/shell/shell.go","status":"passed"},`, 1024), ",") + `]}`, 100},
	}
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				shellOutputBenchmarkResult = formatShellOutput(fixture.output, fixture.width)
			}
		})
	}
}

func shellBuildLogFixture(count int, newline string, color bool) string {
	var output strings.Builder
	for i := range count {
		line := fmt.Sprintf("ok  github.com/docker/docker-agent/pkg/generated/package%04d  0.024s  coverage: 84.2%% of statements", i)
		if color {
			line = "\x1b[32m" + line + "  ✓ 界 e\u0301 👩🏽‍💻\x1b[0m"
		}
		output.WriteString(line)
		output.WriteString(newline)
	}
	output.WriteString("PASS" + newline)
	return output.String()
}

var expandedShellViewBenchmarkResult string

func BenchmarkExpandedShellViewCached5000OutputLines(b *testing.B) {
	ar := animation.NewRuntime()
	defer ar.Stop()
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "bench", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf build"}`}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	msg.Content = shellBuildLogFixture(5000, "\n", false)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(100, 0)
	view.SetExpanded(true)
	view.View()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		expandedShellViewBenchmarkResult = view.View()
	}
}
