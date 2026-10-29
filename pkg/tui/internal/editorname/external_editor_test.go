package editorname

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareCommandFromEnvParsesQuotedExecutableAndKeepsFilenameAsArgument(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "my editor")
	require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	path := filepath.Join(t.TempDir(), "résumé file.md")

	cmd, err := PrepareCommandFromEnv(`"`+program+`" --wait -c 'set ft=markdown'`, "unused", path)
	require.NoError(t, err)
	require.Equal(t, []string{program, "--wait", "-c", "set ft=markdown", path}, cmd.Args)
	require.Same(t, os.Stdin, cmd.Stdin)
	require.Same(t, os.Stdout, cmd.Stdout)
	require.Same(t, os.Stderr, cmd.Stderr)
}

func TestPrepareCommandFromEnvDoesNotEvaluateShellSyntax(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "editor")
	require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", dir)
	path := ";$(touch should-not-exist) file.md"

	cmd, err := PrepareCommandFromEnv(`editor "$HOME"`, "fallback", path)
	require.NoError(t, err)
	require.Equal(t, []string{"editor", "$HOME", path}, cmd.Args)
	_, err = os.Stat(filepath.Join(dir, "should-not-exist"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPrepareCommandFromEnvVisualFallbackAndUnavailableErrors(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "available")
	require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", dir)

	cmd, err := PrepareCommandFromEnv("  ", "available --wait", "file with spaces")
	require.NoError(t, err)
	require.Equal(t, []string{"available", "--wait", "file with spaces"}, cmd.Args)
	_, err = PrepareCommandFromEnv("missing-editor", "available", "file")
	require.ErrorContains(t, err, "unavailable")
	if runtime.GOOS != "windows" {
		_, err = PrepareCommandFromEnv(`"unterminated`, "available", "file")
		require.ErrorContains(t, err, "parse external editor")
	}
}

func TestCommandFromEnvMaintainsCommandArgsAndStdio(t *testing.T) {
	cmd := CommandFromEnv("", "editor --wait", "/tmp/file.md")
	require.Equal(t, []string{"editor", "--wait", "/tmp/file.md"}, cmd.Args)
	require.Same(t, os.Stdin, cmd.Stdin)
}

func TestSplitCommandPreservesWindowsBackslashesAndEscapedSpaces(t *testing.T) {
	got, err := splitCommand(`"C:\Program Files\My Editor\editor.exe" editor\ config`)
	require.NoError(t, err)
	require.Equal(t, []string{`C:\Program Files\My Editor\editor.exe`, "editor config"}, got)
}
