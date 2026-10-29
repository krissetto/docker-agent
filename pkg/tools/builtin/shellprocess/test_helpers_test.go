package shellprocess

import (
	"os"

	"github.com/docker/docker-agent/pkg/shellpath"
)

var (
	testShell, testShellArgs = shellpath.DetectShell()
	testShellEnv             = os.Environ()
)
