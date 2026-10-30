package client

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// apiClientProxyChildEnv marks the re-executed test binary so it runs the assertions
// instead of spawning another child.
const apiClientProxyChildEnv = "DOCKER_AGENT_API_CLIENT_PROXY_ISOLATED_CHILD"

// proxyEnvNames are the variables Go's proxy resolution and the SSRF dial
// allowlist read.
var proxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// runIsolatedFromProxyEnv re-runs testName in a child test binary whose
// environment has no proxy variables. The package-level SSRF transport reads
// proxy env once at package init, so t.Setenv in the test cannot change it.
// It returns true in the parent once the child passed; the child returns false
// and runs the original assertions.
func runIsolatedFromProxyEnv(t *testing.T, testName string) bool {
	t.Helper()
	if os.Getenv(apiClientProxyChildEnv) != "" {
		return false
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^"+testName+"$", "-test.v", "-test.count=1")
	cmd.Env = append(withoutProxyEnv(os.Environ()), apiClientProxyChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "isolated run of %s failed:\n%s", testName, out)
	// Guard against a filter that matched nothing, which would otherwise pass.
	require.Contains(t, string(out), "--- PASS: "+testName+" (", "isolated child did not run %s:\n%s", testName, out)
	return true
}

func withoutProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(proxyEnvNames, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
