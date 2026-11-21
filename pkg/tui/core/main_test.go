package core

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil/teststate"
)

func TestMain(m *testing.M) {
	os.Exit(teststate.Run(m.Run))
}
