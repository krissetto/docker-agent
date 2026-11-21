// Package teststate isolates process-wide state defaults for test binaries.
package teststate

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/paths"
)

// Run gives a test binary private state defaults without replacing per-test overrides.
func Run(run func() int) int {
	root, err := os.MkdirTemp("", "docker-agent-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	// Isolate defaults as well as overrides: fixture cleanups clear paths.Set*.
	for name, dir := range map[string]string{
		"HOME": root, "USERPROFILE": root,
		"XDG_CONFIG_HOME":         filepath.Join(root, ".config"),
		"XDG_DATA_HOME":           filepath.Join(root, ".local", "share"),
		"XDG_STATE_HOME":          filepath.Join(root, ".local", "state"),
		"XDG_CACHE_HOME":          filepath.Join(root, ".cache"),
		"XDG_RUNTIME_DIR":         filepath.Join(root, "run"),
		"APPDATA":                 filepath.Join(root, "AppData", "Roaming"),
		"LOCALAPPDATA":            filepath.Join(root, "AppData", "Local"),
		"DOCKER_CONFIG":           filepath.Join(root, ".docker"),
		"DOCKER_CERT_PATH":        filepath.Join(root, ".docker", "certs"),
		"DOCKER_AGENT_CONFIG_DIR": filepath.Join(root, ".config", "cagent"),
		"CAGENT_CONFIG_DIR":       filepath.Join(root, ".config", "cagent"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	paths.SetRoot("")

	return run()
}
