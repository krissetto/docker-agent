package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

func TestExec_OpenAI(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "What's 2+2?")

	require.Equal(t, "2 + 2 equals 4.", out)
}

// TestExec_OpenAI_V3Config tests that v3 configs work correctly with thinking disabled by default.
// This uses gpt-5 with a v3 config file to verify thinking is disabled for old config versions.
func TestExec_OpenAI_V3Config(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic_v3.yaml", "What's 2+2?")

	// v3 config with gpt-5 should work correctly (thinking disabled by default for old configs)
	require.Equal(t, "4", out)
}

// TestExec_OpenAI_WithThinkingBudget tests that when thinking_budget is explicitly configured
// in the YAML, thinking is enabled by default.
func TestExec_OpenAI_WithThinkingBudget(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic_with_thinking.yaml", "What's 2+2?")

	// With thinking_budget explicitly configured, response should include reasoning
	// The output format includes the reasoning summary when thinking is enabled
	require.Contains(t, out, "4")
}

func TestExec_OpenAI_ToolCall(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/fs_tools.yaml", "How many files in testdata/working_dir? Only output the number.")

	require.Equal(t, "\nCalling list_directory(path: \"testdata/working_dir\")\n\nlist_directory response → \"FILE README.me\\n\"\n1", out)
}

func TestExec_OpenAI_HideToolCalls(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/fs_tools.yaml", "--hide-tool-calls", "How many files in testdata/working_dir? Only output the number.")

	require.Equal(t, "1", out)
}

func TestExec_OpenAI_gpt5(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=openai/gpt-5", "What's 2+2?")

	// With thinking enabled by default, response may include reasoning summary
	require.Contains(t, out, "4")
}

func TestExec_OpenAI_gpt5_1(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=openai/gpt-5.1", "What's 2+2?")

	require.Equal(t, "2 + 2 = 4.", out)
}

func TestExec_OpenAI_gpt5_codex(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=openai/gpt-5-codex", "What's 2+2?")

	// Model reasoning summary varies, just check for the core response
	require.Contains(t, out, "4")
}

func TestExec_Anthropic(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=anthropic/claude-sonnet-4-0", "What's 2+2?")

	// With interleaved thinking enabled by default, Anthropic responses include thinking content
	require.Contains(t, out, "2 + 2 = 4")
}

func TestExec_Anthropic_ToolCall(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/fs_tools.yaml", "--model=anthropic/claude-sonnet-4-0", "How many files in testdata/working_dir? Only output the number.")

	// With interleaved thinking enabled by default, Anthropic responses include thinking content
	require.Contains(t, out, `Calling list_directory(path: "testdata/working_dir")`)
	require.Contains(t, out, `list_directory response → "FILE README.me\n"`)
	// The response should end with "1" (the count)
	require.True(t, out != "" && out[len(out)-1] == '1', "response should end with '1'")
}

func TestExec_Anthropic_AgentsMd(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/agents-md.yaml", "--model=anthropic/claude-sonnet-4-0", "What's 2+2?")

	// With interleaved thinking enabled by default, Anthropic responses include thinking content
	require.Contains(t, out, "2 + 2 = 4")
}

func TestExec_Gemini(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=google/gemini-2.5-flash", "What's 2+2?")

	// With thinking enabled by default (dynamic thinking for Gemini 2.5), responses may include thinking content
	// The response should contain the answer "4" somewhere
	require.Contains(t, out, "4")
}

func TestExec_Gemini_ToolCall(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/fs_tools.yaml", "--model=google/gemini-2.5-flash", "How many files in testdata/working_dir? Only output the number.")

	// With thinking enabled by default (dynamic thinking for Gemini 2.5), responses include thinking content
	require.Contains(t, out, `Calling list_directory(path: "testdata/working_dir")`)
	require.Contains(t, out, `list_directory response → "FILE README.me\n"`)
	// The response should end with "1" (the count)
	require.True(t, out != "" && out[len(out)-1] == '1', "response should end with '1'")
}

func TestExec_Mistral(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/basic.yaml", "--model=mistral/mistral-small", "What's 2+2?")

	require.Equal(t, "The sum of 2 + 2 is 4.", out)
}

func TestExec_Mistral_ToolCall(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "testdata/fs_tools.yaml", "--model=mistral/mistral-small", "How many files in testdata/working_dir? Only output the number.")

	require.Equal(t, "\nCalling list_directory(path: \"testdata/working_dir\")\n\nlist_directory response → \"FILE README.me\\n\"\n1", out)
}

func TestExec_ToolCallsNeedAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	agentSource, err := sources.Resolve("testdata/file_writer.yaml", nil)
	require.NoError(t, err)
	_, runConfig := startRecordingAIProxy(t)
	workingDir := t.TempDir()
	runConfig.WorkingDir = workingDir
	loadedTeam, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	require.NoError(t, err)

	rt, err := runtime.New(ctx, loadedTeam, runtime.WithWorkingDir(workingDir))
	require.NoError(t, err)
	supervisor := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })

	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(workingDir))
	handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()

	submission, err := handle.Submit(ctx, runtime.TurnInput{Content: `Create a hello.txt file with "Hello, World!" content. Try only once. On error, exit without further message.`})
	require.NoError(t, err)

	var confirmation *runtime.ToolCallConfirmationEvent
	for envelope := range observation.Events {
		if envelope.TurnID != submission.TurnID {
			continue
		}
		switch event := envelope.Event.(type) {
		case *runtime.ToolCallConfirmationEvent:
			confirmation = event
			require.Equal(t, sess.ID, event.SessionID)
			require.NotEmpty(t, event.RequestID)
			require.NoError(t, handle.Respond(ctx, runtime.InteractionResponse{
				InteractionID: event.RequestID,
				Kind:          runtime.InteractionConfirmation,
				Resume:        runtime.ResumeApprove(),
			}))
		case *runtime.StreamStoppedEvent:
			require.NotNil(t, confirmation, "run stopped before requesting tool confirmation")
			content, readErr := os.ReadFile(filepath.Join(workingDir, "hello.txt"))
			require.NoError(t, readErr, "approved tool call must write the file")
			require.Equal(t, "Hello, World!", string(content))
			return
		}
	}
	t.Fatal("session observation closed before the submitted run stopped")
}
