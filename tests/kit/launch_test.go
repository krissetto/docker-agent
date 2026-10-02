package kit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLauncherTeamSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runtime launcher")
	}
	script, err := os.ReadFile(kitPath(t, "launch.sh"))
	require.NoError(t, err)
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace := filepath.Join(home, "workspace with spaces")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	// Substitute only the installation prefix; parse and forward real shell argv.
	script = []byte(strings.ReplaceAll(string(script), "/opt/async-agent", home))
	launcher := filepath.Join(home, "launch.sh")
	require.NoError(t, os.WriteFile(launcher, script, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0o700))
	for _, name := range []string{"hackerspace.yaml", "team.yaml", "team with spaces.yaml", "team\n$(touch INJECTED);*.yaml", "-team.yaml"} {
		require.NoError(t, os.WriteFile(filepath.Join(workspace, name), []byte("agents: {}"), 0o600))
	}
	absoluteTeam := filepath.Join(home, "mounted team.yaml")
	require.NoError(t, os.WriteFile(absoluteTeam, []byte("agents: {}"), 0o600))
	unreadable := filepath.Join(workspace, "unreadable.yaml")
	require.NoError(t, os.WriteFile(unreadable, []byte("agents: {}"), 0o000))

	for _, tc := range []struct {
		name      string
		args      []string
		team      string
		forwarded []string
		wantError string
	}{
		{name: "no arguments"},
		{name: "relative", args: []string{"--team", "./team.yaml"}, team: workspace + "/./team.yaml"},
		{name: "absolute", args: []string{"--team", absoluteTeam}, team: absoluteTeam},
		{name: "equals", args: []string{"--team=team.yaml"}, team: workspace + "/team.yaml"},
		{name: "spaces", args: []string{"--team", "team with spaces.yaml"}, team: workspace + "/team with spaces.yaml"},
		{name: "equals spaces", args: []string{"--team=" + absoluteTeam}, team: absoluteTeam},
		{name: "special path", args: []string{"--team", "team\n$(touch INJECTED);*.yaml"}, team: workspace + "/team\n$(touch INJECTED);*.yaml"},
		{name: "dash path", args: []string{"--team", "-team.yaml"}, team: workspace + "/-team.yaml"},
		{name: "agent args", args: []string{"--team", "team.yaml", "--model", "openai/example", "--exec", "--session", "session id", "--", "--team", "line one\n$(touch INJECTED); *", ""}, team: workspace + "/team.yaml", forwarded: []string{"--model", "openai/example", "--exec", "--session", "session id", "--", "--team", "line one\n$(touch INJECTED); *", ""}},
		{name: "sentinel", args: []string{"--", "--team", "missing.yaml"}, forwarded: []string{"--", "--team", "missing.yaml"}},
		{name: "headless prompt", args: []string{"--exec", "--", "--team"}, forwarded: []string{"--exec", "--", "--team"}},
		{name: "resume", args: []string{"--session", "--team"}, forwarded: []string{"--session", "--team"}},
		{name: "continue", args: []string{"--session=-1"}, forwarded: []string{"--session=-1"}},
		{name: "managed state", args: []string{"--managed-api-state-dir", "private state\n$(touch INJECTED)"}, forwarded: []string{"--managed-api-state-dir", "private state\n$(touch INJECTED)"}},
		{name: "model value", args: []string{"--model", "--team", "--dry-run"}, forwarded: []string{"--model", "--team", "--dry-run"}},
		{name: "prompt string", args: []string{"discuss --team team.yaml"}, forwarded: []string{"discuss --team team.yaml"}},
		{name: "empty argument", args: []string{"", "--team", "team.yaml"}, forwarded: []string{"", "--team", "team.yaml"}},
		{name: "missing value", args: []string{"--team"}, wantError: "--team requires a file path"},
		{name: "empty value", args: []string{"--team", ""}, wantError: "--team requires a file path"},
		{name: "empty equals value", args: []string{"--team="}, wantError: "--team requires a file path"},
		{name: "missing file", args: []string{"--team", "missing.yaml"}, wantError: "team file is not a readable regular file: " + workspace + "/missing.yaml"},
		{name: "directory", args: []string{"--team", workspace}, wantError: "team file is not a readable regular file: " + workspace},
		{name: "unreadable", args: []string{"--team", unreadable}, wantError: "team file is not a readable regular file: " + unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root can read files regardless of mode bits")
			}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{launcher}, tc.args...)...)
			cmd.Dir = workspace
			out, err := cmd.CombinedOutput()
			if tc.wantError != "" {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, string(out))
				assert.Equal(t, 2, exitErr.ExitCode())
				assert.Equal(t, "launch.sh: "+tc.wantError+"\n", string(out))
				return
			}
			require.NoError(t, err, string(out))
			team := tc.team
			if team == "" {
				team = filepath.Join(workspace, "hackerspace.yaml")
			}
			expected := append([]string{"run", team, "--managed-api", "--working-dir", workspace}, tc.forwarded...)
			assert.Equal(t, expected, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
			assert.NoFileExists(t, filepath.Join(workspace, "INJECTED"))
		})
	}
}

func TestLauncherManagedEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runtime launcher")
	}
	script, err := os.ReadFile(kitPath(t, "launch.sh"))
	require.NoError(t, err)
	home := t.TempDir()
	launcher := filepath.Join(home, "launch.sh")
	script = []byte(strings.ReplaceAll(string(script), "/opt/async-agent", home))
	require.NoError(t, os.WriteFile(launcher, script, 0o700))
	stub := "#!/bin/sh\nprintf '%s\\0' \"$DOCKER_AGENT_AUTO_UPDATE\" \"$DOCKER_AGENT_NO_TOUR\" \"$DOCKER_AGENT_HIDE_TELEMETRY_BANNER\" \"$TELEMETRY_ENABLED\"\nexit 23\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte(stub), 0o700))
	t.Setenv("DOCKER_AGENT_AUTO_UPDATE", "1")
	t.Setenv("DOCKER_AGENT_NO_TOUR", "0")
	t.Setenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER", "0")
	t.Setenv("TELEMETRY_ENABLED", "true")
	cmd := exec.CommandContext(t.Context(), "/bin/sh", launcher)
	cmd.Dir = home
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, string(out))
	assert.Equal(t, 23, exitErr.ExitCode(), "foreground client exit status is preserved")
	assert.Equal(t, "0\x001\x001\x00false\x00", string(out))
}
