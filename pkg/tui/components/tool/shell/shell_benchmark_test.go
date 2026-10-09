package shell

import (
	"fmt"
	"strings"
	"testing"
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
