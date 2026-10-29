package teamloader

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/builtin/shellprocess"
)

func TestShellBackgroundGuidanceSurvivesRootAndChildInstructionAssembly(t *testing.T) {
	if !shellprocess.New().SupportsShellBackground() {
		t.Skip("automatic shell backgrounding is unavailable on this platform")
	}
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: root instructions
    toolsets:
      - type: shell
        background_after: 1
        instruction: |-
          {ORIGINAL_INSTRUCTIONS}
          Configured root shell note
  worker:
    model: openai/gpt-4o
    instruction: child instructions
    toolsets:
      - type: shell
        background_after: 1
        instruction: |-
          {ORIGINAL_INSTRUCTIONS}
          Configured child shell note
`)
	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("shell-background-guidance.yaml", data), &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}, withTestProviderRegistry()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loaded.Team.StopToolSets(t.Context())) })

	for _, tc := range []struct {
		name           string
		agentName      string
		asyncChild     bool
		instruction    string
		configuredNote string
	}{
		{name: "root", agentName: "root", instruction: "root instructions", configuredNote: "Configured root shell note"},
		{name: "async child", agentName: "worker", asyncChild: true, instruction: "child instructions", configuredNote: "Configured child shell note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := loaded.Team.Agent(tc.agentName)
			require.NoError(t, err)
			options := []session.Opt{}
			if tc.asyncChild {
				options = append(options, session.WithAsyncSubagent(true))
			}
			messages := session.New(options...).GetMessages(a)
			var systemText []string
			for _, message := range messages {
				if message.Role == chat.MessageRoleSystem {
					systemText = append(systemText, message.Content)
				}
			}
			assembled := strings.Join(systemText, "\n")
			require.Contains(t, assembled, tc.instruction)
			require.Contains(t, assembled, tc.configuredNote)
			for _, phrase := range []string{
				"Completion output is delivered automatically and wakes/resumes this agent",
				"do independent work",
				"end your turn without tool calls (no keep-alive needed)",
				"Never poll with inspect_running_process, list_running_processes, sleep, or dummy calls",
				"concrete diagnostic or user request",
				"intentionally cancel",
			} {
				require.Contains(t, assembled, phrase)
			}
			assertShellDiagnosticDescriptions(t, a)
		})
	}
}

func TestShellBackgroundToolDescriptionsSurviveInstructionReplacement(t *testing.T) {
	if !shellprocess.New().SupportsShellBackground() {
		t.Skip("automatic shell backgrounding is unavailable on this platform")
	}
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: root instructions
    toolsets:
      - type: shell
        background_after: 1
        instruction: Replace built-in shell prompt with this custom note.
`)
	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("shell-background-override.yaml", data), &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}, withTestProviderRegistry()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loaded.Team.StopToolSets(t.Context())) })

	root, err := loaded.Team.Agent("root")
	require.NoError(t, err)
	messages := session.New().GetMessages(root)
	var systemText []string
	for _, message := range messages {
		if message.Role == chat.MessageRoleSystem {
			systemText = append(systemText, message.Content)
		}
	}
	assembled := strings.Join(systemText, "\n")
	require.Contains(t, assembled, "Replace built-in shell prompt with this custom note.")
	require.NotContains(t, assembled, "Commands still running after")
	require.NotContains(t, assembled, "Completion output is delivered automatically and wakes/resumes this agent")
	assertShellDiagnosticDescriptions(t, root)
}

func assertShellDiagnosticDescriptions(t *testing.T, a *agent.Agent) {
	t.Helper()
	allTools, err := a.Tools(t.Context())
	require.NoError(t, err)
	var shellDescription string
	var inspectorDescriptions []string
	for _, tool := range allTools {
		switch tool.Name {
		case "shell":
			shellDescription = tool.Description
		case shellprocess.ToolNameInspectRunningProcess, shellprocess.ToolNameListRunningProcesses, shellprocess.ToolNameKillRunningProcess:
			inspectorDescriptions = append(inspectorDescriptions, tool.Description)
		}
	}
	require.NotEmpty(t, shellDescription)
	require.Len(t, inspectorDescriptions, 3)
	require.Contains(t, shellDescription, "Completion output is delivered automatically and wakes/resumes this agent")
	require.Contains(t, shellDescription, "end your turn without tool calls (no keep-alive needed)")
	require.Contains(t, shellDescription, "Never poll")
	for _, tool := range allTools {
		switch tool.Name {
		case shellprocess.ToolNameListRunningProcesses, shellprocess.ToolNameInspectRunningProcess:
			require.Contains(t, tool.Description, "wakes/resumes this agent")
			require.Contains(t, tool.Description, "never poll")
		case shellprocess.ToolNameKillRunningProcess:
			require.Contains(t, tool.Description, "Intentionally cancel")
			require.Contains(t, tool.Description, "never kill just to end your turn")
		}
	}
}
