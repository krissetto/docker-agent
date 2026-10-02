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

// This checks command construction only, not registry publication or SBX validity.
func TestKitTaskPublicationCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX build task")
	}
	raw, err := os.ReadFile(repositoryPath(t, "Taskfile.yml"))
	require.NoError(t, err)
	var taskfile struct {
		Tasks struct {
			Kit struct {
				Env  map[string]string `yaml:"env"`
				Cmds []string          `yaml:"cmds"`
			} `yaml:"kit"`
		} `yaml:"tasks"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &taskfile))
	kit := taskfile.Tasks.Kit
	require.Len(t, kit.Cmds, 1)
	assert.Equal(t, "{{.CLI_ARGS}}", kit.Env["KIT_REPOSITORY"])
	assert.Equal(t, "1", kit.Env["BUILDX_GIT_CHECK_DIRTY"])
	assert.Contains(t, kit.Cmds[0], "top-level Kit metadata promotion after push")

	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, repository, commit string
		invalid                  bool
	}{
		{name: "default", commit: sha},
		{name: "repository override", repository: "registry.example:5000/team/kagent", commit: sha},
		{name: "shell text is not evaluated", repository: "team/$(touch INJECTED); kagent", commit: sha},
		{name: "missing SHA", invalid: true},
		{name: "short SHA", commit: "0123456", invalid: true},
		{name: "nonhex SHA", commit: strings.Repeat("z", 40), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A PATH-local Docker stub captures argv; these tests never contact Docker.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0o700))
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", kit.Cmds[0])
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "KIT_REPOSITORY="+tc.repository, "GIT_COMMIT="+tc.commit)
			out, err := cmd.CombinedOutput()
			if tc.invalid {
				require.Error(t, err)
				assert.Equal(t, "kit requires a full Git commit SHA\n", string(out))
				return
			}
			require.NoError(t, err, string(out))
			repository := tc.repository
			if repository == "" {
				repository = "docker.io/christopherpetito053/kagent"
			}
			assert.Equal(t, []string{"buildx", "build", ".", "-f", "kit/async-agent.yaml", "--platform", "linux/amd64,linux/arm64", "--tag", repository + ":latest", "--tag", repository + ":" + sha, "--push"}, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
			assert.NoFileExists(t, filepath.Join(dir, "INJECTED"))
		})
	}
}

func TestKitTaskPackaging(t *testing.T) {
	recipe, err := os.ReadFile(kitPath(t, "async-agent.dockerfile"))
	require.NoError(t, err)
	_, stage, found := strings.Cut(string(recipe), "FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS task\n")
	require.True(t, found, "download tools run on the builder architecture")
	stage, runtime, found := strings.Cut(stage, "\nFROM docker/sandbox-templates:")
	require.True(t, found)
	assert.Contains(t, stage, "ARG TARGETOS TARGETARCH\n")
	assert.Contains(t, stage, `test "$TARGETOS" = linux`)
	assert.Contains(t, stage, `case "$TARGETARCH" in`)
	for _, pin := range []string{
		"amd64) checksum=a54a408f6861ff921f6e87774180db31bacd8c1e7c944ca696db9fea49a82fc7 ;;",
		"arm64) checksum=e3ad19101493a0112e1f22ae8ccc54bf03e533b1076a0ca1e6c782a09ad2e588 ;;",
	} {
		assert.Contains(t, stage, pin)
	}
	assert.Contains(t, stage, `*) echo "Unsupported Task architecture: $TARGETARCH" >&2; exit 1 ;;`)
	assert.Contains(t, stage, `curl --fail --silent --show-error --location "https://github.com/go-task/task/releases/download/v3.53.1/task_linux_${TARGETARCH}.tar.gz" -o task.tar.gz`)
	assert.Contains(t, stage, "set -eux\n")
	assert.Contains(t, stage, "printf '%s  task.tar.gz\\n' \"$checksum\" | sha256sum -c -\ntar -xzf task.tar.gz task LICENSE\n", "verify before extracting")
	assert.Contains(t, runtime, "COPY --from=task --chown=root:root --chmod=0755 /task/task /usr/local/bin/task\n")
	assert.Contains(t, runtime, "RUN install -d -m 0755 /usr/local/share/licenses /usr/local/share/licenses/task\n")
	assert.Contains(t, runtime, "COPY --from=task --chown=root:root --chmod=0644 /task/LICENSE /usr/local/share/licenses/task/LICENSE\n")
	assert.Contains(t, runtime, "USER agent\n")
	assert.NotContains(t, runtime, "curl ")
	assert.NotContains(t, runtime, "wget ")

	setup, err := os.ReadFile(repositoryPath(t, ".github", "actions", "setup-go", "action.yml"))
	require.NoError(t, err)
	assert.Contains(t, string(setup), "version: 3.53.1\n", "kit and CI use the same Task release")
}
