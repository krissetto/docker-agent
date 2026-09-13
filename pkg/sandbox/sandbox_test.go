package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

func TestCheckAvailable(t *testing.T) {
	tests := []struct {
		name      string
		script    string // empty means no fake binary (docker not found)
		wantErr   string
		wantNoErr bool
	}{
		{
			name:    "no docker installed",
			wantErr: "--sandbox requires Docker Desktop",
		},
		{
			name:    "docker without sandbox support",
			script:  "#!/bin/sh\nexit 1\n",
			wantErr: "--sandbox requires Docker Desktop with sandbox support",
		},
		{
			name:      "docker with sandbox support",
			script:    "#!/bin/sh\nexit 0\n",
			wantNoErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			if tt.script != "" {
				writeMockScript(t, fakeDir, "docker", tt.script)
			}
			t.Setenv("PATH", fakeDir)

			backend := sandbox.NewBackend(false)
			err := backend.CheckAvailable(t.Context())
			if tt.wantNoErr {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestForWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wd       string
		wantName string
	}{
		{
			name:     "matching workspace",
			json:     `{"sandboxes":[{"name":"my-sandbox","workspaces":["/my/project"]}]}`,
			wd:       "/my/project",
			wantName: "my-sandbox",
		},
		{
			name: "no match",
			json: `{"sandboxes":[{"name":"other","workspaces":["/other/project"]}]}`,
			wd:   "/my/project",
		},
		{
			name: "empty list",
			json: `{"sandboxes":[]}`,
			wd:   "/my/project",
		},
		{
			name:     "multiple sandboxes",
			json:     `{"sandboxes":[{"name":"a","workspaces":["/a"]},{"name":"b","workspaces":["/b"]}]}`,
			wd:       "/b",
			wantName: "b",
		},
	}

	// Write the fake "docker" executable once and have it cat a data
	// file the subtests rewrite. Re-creating the script per subtest
	// would pay the macOS cold-exec penalty (~0.2s) every time, since
	// the OS validates each freshly written binary on first run.
	fakeDir := t.TempDir()
	dataFile := filepath.Join(fakeDir, "ls.json")
	script := fmt.Sprintf("cat %q", dataFile)
	writeMockScript(t, fakeDir, "docker", script)
	// Prepend (not replace) so the fake "docker" wins while the script
	// can still resolve "cat".
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(dataFile, []byte(tt.json), 0o600))

			backend := sandbox.NewBackend(false)
			got := backend.ForWorkspace(t.Context(), tt.wd)
			if tt.wantName == "" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantName, got.Name)
			}
		})
	}
}

func TestExisting_HasWorkspace(t *testing.T) {
	t.Parallel()

	s := &sandbox.Existing{
		Name:       "test",
		Workspaces: []string{"/workspace", "/extra:ro"},
	}

	assert.True(t, s.HasWorkspace("/workspace"))
	assert.True(t, s.HasWorkspace("/extra"), "should match ignoring :ro suffix")
	assert.False(t, s.HasWorkspace("/other"))
}

func TestNewBackend_PrefersSbx(t *testing.T) {
	fakeDir := t.TempDir()
	writeMockScript(t, fakeDir, "sbx", "exit 0")
	t.Setenv("PATH", fakeDir)

	// When sbx is available and preferred, CheckAvailable uses sbx.
	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestNewBackend_FallsBackToDocker(t *testing.T) {
	fakeDir := t.TempDir()
	// Only docker is available, no sbx.
	writeMockScript(t, fakeDir, "docker", "exit 0")
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(true)
	err := backend.CheckAvailable(t.Context())
	require.NoError(t, err)
}

func TestForWorkspace_SbxBackend(t *testing.T) {
	fakeDir := t.TempDir()
	jsonData := `{"sandboxes":[{"name":"my-sbx","workspaces":["/my/project"]}]}`
	dataFile := filepath.Join(fakeDir, "ls.json")
	require.NoError(t, os.WriteFile(dataFile, []byte(jsonData), 0o600))
	script := fmt.Sprintf("cat %q", dataFile)
	writeMockScript(t, fakeDir, "sbx", script)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := sandbox.NewBackend(true)
	got := backend.ForWorkspace(t.Context(), "/my/project")
	require.NotNil(t, got)
	assert.Equal(t, "my-sbx", got.Name)
}

func TestExtraWorkspace(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	t.Run("empty ref", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", ""))
	})

	t.Run("built-in name", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", "default"))
	})

	t.Run("OCI reference", func(t *testing.T) {
		assert.Empty(t, sandbox.ExtraWorkspace("/workspace", "docker.io/myorg/agent:latest"))
	})

	t.Run("yaml outside workspace", func(t *testing.T) {
		agentDir := t.TempDir()
		agent := filepath.Join(agentDir, "agent.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		got := sandbox.ExtraWorkspace(t.TempDir(), agent)
		assert.Equal(t, agentDir, got)
	})

	t.Run("yaml inside workspace", func(t *testing.T) {
		wd := t.TempDir()
		sub := filepath.Join(wd, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))
		agent := filepath.Join(sub, "agent.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		assert.Empty(t, sandbox.ExtraWorkspace(wd, agent))
	})

	t.Run("alias points to file outside workspace", func(t *testing.T) {
		// Regression: ExtraWorkspace used to call filepath.Abs("gopher")
		// directly and miss the alias hop, returning "". The sandbox
		// would then launch without the alias's target YAML mounted
		// and the in-sandbox docker-agent could not read it.
		agentDir := t.TempDir()
		agent := filepath.Join(agentDir, "gopher.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

		writeAlias(t, "gopher", agent)

		got := sandbox.ExtraWorkspace(t.TempDir(), "gopher")
		assert.Equal(t, agentDir, got)
	})

	t.Run("default alias points to file outside workspace", func(t *testing.T) {
		agentDir := t.TempDir()
		agent := filepath.Join(agentDir, "default.yaml")
		require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))
		writeAlias(t, "default", agent)

		wd := t.TempDir()
		assert.Equal(t, agentDir, sandbox.ExtraWorkspace(wd, "default"))
		assert.Equal(t, agentDir, sandbox.ExtraWorkspace(wd, ""))
	})

	t.Run("alias points to OCI reference", func(t *testing.T) {
		// OCI-backed aliases have nothing on the host filesystem to
		// mount; ExtraWorkspace returns "".
		writeAlias(t, "remote", "docker.io/myorg/agent:latest")

		assert.Empty(t, sandbox.ExtraWorkspace(t.TempDir(), "remote"))
	})
}

// writeAlias points the docker-agent config dir at a fresh tempdir
// and writes a single-alias config.yaml inside it. The override is
// reverted via t.Cleanup.
func writeAlias(t *testing.T, name, path string) {
	t.Helper()

	dir := t.TempDir()
	previous := paths.GetConfigDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir(previous) })

	content := "aliases:\n  " + name + ":\n    path: " + path + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600))
}

func TestAllowHosts_RejectsCommaOrWhitespaceEntries(t *testing.T) {
	t.Parallel()

	// Smuggling additional rules through a single argument by
	// embedding a comma (or whitespace) in a hostname must fail
	// loudly: the backend joins the list with commas before
	// forwarding it to the policy engine, and the inner CLI
	// otherwise has no way to distinguish a typo from an attack.
	backend := sandbox.NewBackend(false) // docker backend; sbx behaves the same
	cases := []string{
		"good.example.com,evil.example.com",
		"good.example.com evil.example.com",
		"good.example.com\tother",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			err := backend.AllowHosts(t.Context(), "sandbox-x", []string{host})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "comma or whitespace")
		})
	}
}

// Both backends share the modern CLI surface: per-sandbox network
// rules are spelled `[docker sandbox|sbx] policy allow network
// --sandbox NAME host1,host2` (the old positional-sandbox and
// `network proxy --allow-host` forms were removed from recent CLIs).
func TestAllowHosts_ArgvSpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("argv-capture script is POSIX-only")
	}

	fakeDir := t.TempDir()
	argsFile := filepath.Join(fakeDir, "args.txt")
	writeMockScript(t, fakeDir, "docker", fmt.Sprintf("echo \"$@\" > %q", argsFile))
	writeMockScript(t, fakeDir, "sbx", fmt.Sprintf("echo \"$@\" > %q", argsFile))
	t.Setenv("PATH", fakeDir)

	tests := []struct {
		name     string
		backend  *sandbox.Backend
		wantArgs string
	}{
		{
			name:     "docker",
			backend:  sandbox.NewBackend(false),
			wantArgs: "sandbox policy allow network --sandbox my-sbx a.example.com,b.example.com",
		},
		{
			name:     "sbx",
			backend:  sandbox.NewBackend(true),
			wantArgs: "policy allow network --sandbox my-sbx a.example.com,b.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, tt.backend.AllowHosts(t.Context(), "my-sbx", []string{"a.example.com", "b.example.com"}))

			got, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			assert.Equal(t, tt.wantArgs, strings.TrimSpace(string(got)))
		})
	}
}

func TestAllowHosts_SkipsEmptyEntries(t *testing.T) {
	// Empty / whitespace-only entries are silently dropped; if every
	// requested host is empty we must end up calling no command at
	// all — turn that into an observable check by pointing PATH at
	// a fake "docker" that fails loudly when invoked.
	fakeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"),
		[]byte("#!/bin/sh\necho 'AllowHosts must not call docker for empty inputs' >&2\nexit 99\n"),
		0o755))
	t.Setenv("PATH", fakeDir)

	backend := sandbox.NewBackend(false)
	require.NoError(t, backend.AllowHosts(t.Context(), "sandbox-x", []string{"", "   ", "\t"}))
}

// BuildExecCmd must inject LANG=C.UTF-8 — the only UTF-8 locale shipped
// by the sandbox template image; a locale the image lacks (en_US.UTF-8)
// silently degrades vim to latin1 and mangles non-ASCII input (#3874).
// It must also hand the wrapper the real host stdio for the interactive
// TUI session.
func TestBuildExecCmd(t *testing.T) {
	fakeDir := t.TempDir()
	writeMockScript(t, fakeDir, "sbx", "exit 0")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name       string
		backend    *sandbox.Backend
		wantPrefix []string
	}{
		{
			name:       "docker",
			backend:    sandbox.NewBackend(false),
			wantPrefix: []string{"docker", "sandbox", "exec"},
		},
		{
			name:       "sbx",
			backend:    sandbox.NewBackend(true),
			wantPrefix: []string{"sbx", "exec"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.backend.BuildExecCmd(t.Context(), "my-sbx", "/my/project",
				[]string{"agent.yaml", "--yolo"}, []string{"-e", "FOO"}, []string{"FOO=bar"})

			want := append(tt.wantPrefix,
				"-it", "-w", "/my/project",
				"-e", "FOO",
				"-e", "TERM=xterm-256color",
				"-e", "COLORTERM=truecolor",
				"-e", "LANG=C.UTF-8",
				"my-sbx", "docker-agent", "run",
				"agent.yaml", "--yolo",
			)
			assert.Equal(t, want, cmd.Args)
			assert.Same(t, os.Stdin, cmd.Stdin, "Stdin must be os.Stdin")
			assert.Same(t, os.Stdout, cmd.Stdout, "Stdout must be os.Stdout")
			assert.Same(t, os.Stderr, cmd.Stderr, "Stderr must be os.Stderr")
			assert.Contains(t, cmd.Env, "FOO=bar")
		})
	}
}

// writeMockScript writes a mock executable script to the given directory.
// On Windows, it converts the POSIX shell script to a basic .bat script.
//
// Limitations:
//   - String-replace of "cat " -> "type " could mis-translate if a future script
//     embeds "cat" in a different context (e.g. a filename).
//   - The "#!/bin/sh\n" prefix strip is vestigial as some callers omit it.
//   - Replaces "exit 0" with "exit /b 0" but leaves plain "exit" untouched.
//
// This helper covers exactly the mock scripts currently used by the sandbox tests.
func writeMockScript(t *testing.T, dir, name, script string) {
	t.Helper()
	script = strings.TrimPrefix(script, "#!/bin/sh\n")

	if runtime.GOOS == "windows" {
		name += ".bat"
		script = strings.ReplaceAll(script, "cat ", "type ")
		script = strings.ReplaceAll(script, "exit 0", "exit /b 0")
		script = "@echo off\n" + script
	} else {
		script = "#!/bin/sh\n" + script
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
}
