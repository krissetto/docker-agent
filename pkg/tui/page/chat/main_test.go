package chat

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil/teststate"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
)

// The test binary runs inside the repository checkout; pin the sidebar's
// working directory and branch so layouts do not depend on the checkout's
// branch name.
func TestMain(m *testing.M) {
	os.Exit(teststate.Run(func() int {
		restore := sidebar.SetWorkingDirectoryForTesting("/work/app", "")
		defer restore()
		return m.Run()
	}))
}
