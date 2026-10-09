package root

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// Only an explicit --working-dir makes generic new-session actions in the
// TUI default to the initial session's directory; the value passed on is the
// session's effective directory (worktree/resume resolution included), not
// the raw flag (#4039).
func TestExplicitDefaultWorkingDir(t *testing.T) {
	t.Parallel()

	t.Run("no explicit flag keeps the picker", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{}
		sess := session.New(session.WithWorkingDir("/repo"))
		assert.Empty(t, f.explicitDefaultWorkingDir(sess),
			"without --working-dir no default may be configured")
	})

	t.Run("managed API without explicit flag keeps picker", func(t *testing.T) {
		f := &runExecFlags{managedAPI: true, remoteAddress: "http://127.0.0.1:1234"}
		assert.Empty(t, f.explicitDefaultWorkingDir(session.New(session.WithWorkingDir("/repo"))))
		f.workingDirChanged = true
		assert.Equal(t, "/repo", f.explicitDefaultWorkingDir(session.New(session.WithWorkingDir("/repo"))))
	})

	t.Run("explicit flag uses the session's effective directory", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{workingDirChanged: true}
		sess := session.New(session.WithWorkingDir("/repo/worktree"))
		assert.Equal(t, "/repo/worktree", f.explicitDefaultWorkingDir(sess))
	})

	t.Run("explicit flag falls back to the process CWD", func(t *testing.T) {
		t.Parallel()
		f := &runExecFlags{workingDirChanged: true}
		wd, err := os.Getwd()
		require.NoError(t, err)
		assert.Equal(t, wd, f.explicitDefaultWorkingDir(session.New()),
			"an empty session working dir must mirror runTUIWrapped's CWD fallback")
	})
}

func TestKitLauncherKeepsImplicitWorkspaceOutOfNewTabDefault(t *testing.T) {
	launcher, err := os.ReadFile("../../kit/launch.sh")
	require.NoError(t, err)
	require.Contains(t, string(launcher), `--managed-api --managed-api-attach`)
	require.False(t, strings.Contains(string(launcher), "--working-dir"), "the launcher's CWD is implicit; only user arguments may select a default")
	flags := &runExecFlags{managedAPI: true, remoteAddress: "http://127.0.0.1:1234"}
	sess := session.New(session.WithWorkingDir("/mounted/workspace"))
	require.Empty(t, flags.explicitDefaultWorkingDir(sess))
	flags.workingDirChanged = true
	require.Equal(t, sess.WorkingDir, flags.explicitDefaultWorkingDir(sess), "a user's explicit --working-dir still selects the default")
}
