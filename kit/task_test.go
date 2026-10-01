package asyncsubagents_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitTaskPackaging(t *testing.T) {
	recipe, err := os.ReadFile("async-agent.dockerfile")
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
	assert.Contains(t, runtime, "COPY --from=task --chmod=0755 /task/task /usr/local/bin/task\n")
	assert.Contains(t, runtime, "COPY --from=task --chmod=0644 /task/LICENSE /usr/local/share/licenses/task/LICENSE\n")
	assert.Contains(t, runtime, "USER agent\n")
	assert.NotContains(t, runtime, "curl ")
	assert.NotContains(t, runtime, "wget ")

	setup, err := os.ReadFile("../.github/actions/setup-go/action.yml")
	require.NoError(t, err)
	assert.Contains(t, string(setup), "version: 3.53.1\n", "kit and CI use the same Task release")
}
