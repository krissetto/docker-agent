package tui

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil/teststate"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
)

func TestMain(m *testing.M) {
	os.Exit(teststate.Run(func() int {
		// Keep layouts independent of the checkout's working directory and branch.
		restore := sidebar.SetWorkingDirectoryForTesting("/work/app", "")
		defer restore()
		return m.Run()
	}))
}
