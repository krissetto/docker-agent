package kit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kitScriptFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runtime launcher")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace := filepath.Join(home, "workspace with spaces")
	require.NoError(t, os.Mkdir(workspace, 0700))
	script, err := os.ReadFile(kitPath(t, name))
	require.NoError(t, err)
	script = []byte(strings.ReplaceAll(strings.ReplaceAll(string(script), "/opt/async-agent", home), "/workspace", workspace))
	launcher := filepath.Join(home, name)
	require.NoError(t, os.WriteFile(launcher, script, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte("#!/bin/sh\nprintf '%s\\0' \"$PWD\" \"$@\"\n"), 0700))
	return home, workspace
}

func TestLauncherAttachOnly(t *testing.T) {
	home, workspace := kitScriptFixture(t, "launch.sh")
	for _, args := range [][]string{
		{}, {"--exec", "--dry-run"}, {"--session", "session id"}, {"--session=-1"},
		{"--exec", "--safety", "strict", "--", "--team", "line one\n$(touch INJECTED); *", ""},
	} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{filepath.Join(home, "launch.sh")}, args...)...)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "ASYNC_AGENT_KIT_STATE_DIR="+home+"/private")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		expected := append([]string{workspace, "run", filepath.Join(home, "hackerspace.yaml"), "--managed-api", "--managed-api-attach", "--managed-api-state-dir", home + "/private"}, args...)
		assert.Equal(t, expected, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
		assert.NoFileExists(t, filepath.Join(workspace, "INJECTED"))
	}
	for _, args := range [][]string{{"--team", "custom.yaml"}, {"--team=custom.yaml"}} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{filepath.Join(home, "launch.sh")}, args...)...)
		out, err := cmd.CombinedOutput()
		require.Error(t, err)
		assert.Contains(t, string(out), "recreate with the team Kit argument")
	}
}

func TestStartupTeamSelection(t *testing.T) {
	home, workspace := kitScriptFixture(t, "start.sh")
	for _, name := range []string{"team.yaml", "team with spaces.yaml", "team\n$(touch INJECTED);*.yaml"} {
		require.NoError(t, os.WriteFile(filepath.Join(workspace, name), []byte("agents: {}"), 0600))
	}
	for _, tc := range []struct{ team, model, expected string }{
		{"auto", "", filepath.Join(home, "hackerspace.yaml")},
		{"team.yaml", "openai/example", filepath.Join(workspace, "team.yaml")},
		{"team with spaces.yaml", "openai/example", filepath.Join(workspace, "team with spaces.yaml")},
		{"team\n$(touch INJECTED);*.yaml", "openai/example", filepath.Join(workspace, "team\n$(touch INJECTED);*.yaml")},
		{filepath.Join(workspace, "team.yaml"), "", filepath.Join(workspace, "team.yaml")},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(home, "hackerspace.yaml"), []byte("agents: {}"), 0600))
		cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(home, "start.sh"), tc.team, home+"/private", tc.model)
		cmd.Dir = home
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		expected := []string{workspace, "serve", "api", "--managed-api", "--working-dir", workspace, "--managed-api-state-dir", home + "/private"}
		if tc.model != "" {
			expected = append(expected, "--model", tc.model)
		}
		expected = append(expected, "--", tc.expected)
		assert.Equal(t, expected, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
		assert.NoFileExists(t, filepath.Join(workspace, "INJECTED"))
	}
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hackerspace.yaml"), []byte("agents: {}"), 0600))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(home, "start.sh"), "auto", home+"/private", "")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), filepath.Join(workspace, "hackerspace.yaml"))
	cmd = exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(home, "start.sh"), "missing.yaml", home+"/private", "")
	out, err = cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "team file is not a readable regular file")
}

func TestLauncherManagedEnvironment(t *testing.T) {
	home, _ := kitScriptFixture(t, "launch.sh")
	stub := "#!/bin/sh\nprintf '%s\\0' \"$DOCKER_AGENT_AUTO_UPDATE\" \"$DOCKER_AGENT_NO_TOUR\" \"$DOCKER_AGENT_HIDE_TELEMETRY_BANNER\" \"$TELEMETRY_ENABLED\"\nexit 23\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte(stub), 0700))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(home, "launch.sh"))
	cmd.Env = append(os.Environ(), "ASYNC_AGENT_KIT_STATE_DIR="+home+"/private", "DOCKER_AGENT_AUTO_UPDATE=1", "TELEMETRY_ENABLED=true")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, string(out))
	assert.Equal(t, 23, exitErr.ExitCode())
	assert.Equal(t, "0\x001\x001\x00false\x00", string(out))
}

func TestStartupDeclaredEnvironment(t *testing.T) {
	home, workspace := kitScriptFixture(t, "start.sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, "hackerspace.yaml"), []byte("agents: {}"), 0600))
	raw, err := os.ReadFile(kitPath(t, "async-agent.yaml"))
	require.NoError(t, err)
	var descriptor struct {
		Capabilities []struct {
			Type   string `yaml:"type"`
			Config struct {
				Startup []struct {
					Command []string `yaml:"command"`
					Env     []string `yaml:"env"`
				} `yaml:"startup"`
			} `yaml:"config"`
		} `yaml:"capabilities"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &descriptor))
	for _, capability := range descriptor.Capabilities {
		if capability.Type != "com.docker.sandbox/lifecycle@1" {
			continue
		}
		hook := capability.Config.Startup[1]
		values := map[string]string{"ASYNC_AGENT_KIT_TEAM": "auto", "ASYNC_AGENT_KIT_MODEL": "openai/fixture", "ASYNC_AGENT_KIT_STATE_DIR": home + "/private", "OPENAI_API_KEY": "fixture-sentinel", "ANTHROPIC_API_KEY": "fixture-sentinel", "GOOGLE_API_KEY": "fixture-sentinel", "GH_TOKEN": "fixture-sentinel"}
		args := append([]string(nil), hook.Command...)
		for i, arg := range args {
			arg = strings.ReplaceAll(arg, "/opt/async-agent", home)
			for name, value := range values {
				arg = strings.ReplaceAll(arg, "${{ kit.env."+name+" }}", value)
			}
			args[i] = arg
		}
		stub := "#!/bin/sh\n[ \"$PWD\" = \"" + workspace + "\" ] || exit 21\n[ \"${OPENAI_API_KEY:-}\" = fixture-sentinel ] || exit 22\n[ \"${ANTHROPIC_API_KEY:-}\" = fixture-sentinel ] || exit 23\n[ \"${GOOGLE_API_KEY:-}\" = fixture-sentinel ] || exit 24\n[ \"${GH_TOKEN:-}\" = fixture-sentinel ] || exit 25\n[ -z \"${UNDECLARED_FIXTURE:-}\" ] || exit 26\n[ \"$DOCKER_AGENT_AUTO_UPDATE\" = 0 ] || exit 27\n"
		require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte(stub), 0700))
		cmd := exec.CommandContext(t.Context(), "/bin/sh", args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
		for _, name := range hook.Env {
			if value, ok := values[name]; ok {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
		}
		t.Setenv("UNDECLARED_FIXTURE", "must-not-inherit")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Empty(t, out)
		return
	}
	t.Fatal("missing lifecycle capability")
}
