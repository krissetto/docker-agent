package kit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func optIn(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(kitPath(t, "opt-ins/"+name+"/"+name+".yaml"))
	require.NoError(t, err)
	var d map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &d))
	assert.True(t, strings.HasPrefix(string(raw), "# syntax=docker/sandbox-kit:d841dec@sha256:"))
	assert.Equal(t, "mixin", d["kind"])
	for _, rawCap := range d["capabilities"].([]any) {
		c := rawCap.(map[string]any)
		assert.NotContains(t, c, "optional", "selected opt-ins fail closed, not silently skip")
	}
	return d
}

func TestKitOptInsDefaultOff(t *testing.T) {
	for _, c := range readKitCapabilities(t, kitPath(t, "async-agent.yaml")) {
		for _, name := range []string{"open-url", "privileged", "docker-auth", "host-port", "host-mount", "volume"} {
			assert.NotEqual(t, "com.docker.sandbox/"+name+"@1", c.Type)
		}
	}
	recipe, err := os.ReadFile(kitPath(t, "async-agent.dockerfile"))
	require.NoError(t, err)
	assert.Contains(t, string(recipe), `LABEL com.docker.sandboxes.start-docker="false"`)
	browser := optIn(t, "open-url")["capabilities"].([]any)[0].(map[string]any)
	assert.Equal(t, "com.docker.sandbox/open-url@1", browser["type"])
	assert.NotContains(t, browser, "config")
}

func TestKitDockerOptionalWithRequiredOptIn(t *testing.T) {
	var docker []kitCapability
	for _, c := range readKitCapabilities(t, kitPath(t, "async-agent.yaml")) {
		if c.Type == "com.docker.sandbox/dockerd@1" {
			docker = append(docker, c)
		}
	}
	require.Len(t, docker, 1, "the base preserves Docker intent as one optional request")
	assert.True(t, docker[0].Optional, "denial or unsupported Docker must leave the workload usable")
	assert.Equal(t, map[string]any{"autoStart": true}, docker[0].Config.Fields)
	required := optIn(t, "dockerd")
	assert.Equal(t, []any{"deb/docker-ce"}, required["requires"])
	assert.Equal(t, []any{map[string]any{"type": "com.docker.sandbox/dockerd@1", "config": map[string]any{"autoStart": true}}}, required["capabilities"])
}

func TestKitHostPortBounds(t *testing.T) {
	d := optIn(t, "host-port")
	args := d["args"].(map[string]any)
	for _, name := range []string{"hostPort", "guestPort"} {
		a := args[name].(map[string]any)
		assert.Equal(t, true, a["required"])
		pattern := regexp.MustCompile(a["pattern"].(string))
		for _, port := range []string{"1", "80", "5432", "65535"} {
			assert.True(t, pattern.MatchString(port), port)
		}
		for _, port := range []string{"", "0", "-1", "65536", "99999", "080", "1.0", "80/tcp", "*", "80\n"} {
			assert.False(t, pattern.MatchString(port), port)
		}
	}
	assert.Equal(t, []any{"tcp", "udp"}, args["transport"].(map[string]any)["enum"])
	assert.Equal(t, []any{"ipv4", "ipv6", "dual"}, args["family"].(map[string]any)["enum"])
	assert.Equal(t, []any{map[string]any{"type": "com.docker.sandbox/host-port@1", "config": map[string]any{
		"host": "${{ kit.args.hostPort }}", "container": "${{ kit.args.guestPort }}", "transport": "${{ kit.args.transport }}", "family": "${{ kit.args.family }}",
	}}}, d["capabilities"])
}

func TestKitRegistryOptIn(t *testing.T) {
	d := optIn(t, "docker-auth")
	a := d["args"].(map[string]any)["registry"].(map[string]any)
	assert.True(t, a["required"].(bool))
	assert.NotContains(t, a, "default")
	pattern := regexp.MustCompile(a["pattern"].(string))
	for _, host := range []string{"ghcr.io", "registry.example.com", "docker.io"} {
		assert.True(t, pattern.MatchString(host), host)
	}
	for _, host := range []string{"*", "*.example.com", "https://ghcr.io", "ghcr.io:443", "ghcr.io/path", "GHCR.io", "-bad.example", "a..example"} {
		assert.False(t, pattern.MatchString(host), host)
	}
	assert.Equal(t, []any{
		map[string]any{"type": "com.docker.sandbox/docker-auth@1", "config": map[string]any{"registries": []any{"${{ kit.args.registry }}"}}},
		map[string]any{"type": "com.docker.sandbox/network-policy@1", "config": map[string]any{"runtime": map[string]any{"allow": []any{"${{ kit.args.registry }}:443"}}}},
	}, d["capabilities"])
}

func TestKitRetainedDefaults(t *testing.T) {
	d := optIn(t, "retained-state")
	caps := d["capabilities"].([]any)
	require.Len(t, caps, 3)
	for i, path := range []string{"/home/agent/.config/cagent", "/home/agent/.cagent"} {
		assert.Equal(t, map[string]any{"type": "com.docker.sandbox/volume@1", "config": map[string]any{"path": path, "mode": "0700"}}, caps[i])
	}
	hook := caps[2].(map[string]any)["config"].(map[string]any)["install"].([]any)[0].(map[string]any)
	assert.Equal(t, "agent", hook["user"])
	command := hook["command"].([]any)[3].(string)
	home := t.TempDir()
	config := filepath.Join(home, ".config/cagent")
	require.NoError(t, os.MkdirAll(config, 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".cagent"), 0700))
	for _, name := range []string{"user-config", "context-hooks"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, "default-"+name+".yaml"), []byte(name), 0600))
	}
	command = strings.ReplaceAll(strings.ReplaceAll(command, "/home/agent", home), "/opt/async-agent", home)
	run := func() {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "/bin/sh", "-eu", "-c", command).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run()
	raw, err := os.ReadFile(filepath.Join(config, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "user-config", string(raw))
	settings := filepath.Join(config, "config.yaml")
	require.NoError(t, os.WriteFile(settings, []byte("private customized settings"), 0600))
	run()
	raw, err = os.ReadFile(settings)
	require.NoError(t, err)
	assert.Equal(t, "private customized settings", string(raw))
}

func TestKitManagedACP(t *testing.T) {
	var acp kitCapability
	for _, c := range readKitCapabilities(t, kitPath(t, "async-agent.yaml")) {
		if c.Type == "com.docker.sandbox/acp@1" {
			acp = c
		}
	}
	require.Equal(t, "com.docker.sandbox/acp@1", acp.Type)
	assert.True(t, acp.Optional)
	assert.Equal(t, "async-agent-kit", acp.Config.Fields["agent"])
	assert.Equal(t, []any{"/opt/async-agent/acp.sh"}, acp.Config.Fields["command"])
	home, workspace := kitScriptFixture(t, "acp.sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, "hackerspace.yaml"), []byte("agents: {}"), 0600))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(home, "acp.sh"))
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "WORKSPACE_DIR="+workspace, "ASYNC_AGENT_KIT_STATE_DIR="+home+"/private", "ASYNC_AGENT_KIT_TEAM=auto")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, []string{workspace, "serve", "acp", "--managed-api", "--managed-api-attach", "--managed-api-state-dir", home + "/private", "--working-dir", workspace, home + "/hackerspace.yaml"}, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
}
