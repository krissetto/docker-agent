//go:build !darwin && !linux

package root

import (
	"errors"
	"os"
	"os/exec"
)

func managedPlatformSupported() error {
	return errors.New("managed API is supported only on Linux and macOS; use serve api and run --remote on this platform")
}
func managedOwned(os.FileInfo) bool                 { return false }
func managedTryLock(string) (*os.File, bool, error) { return nil, false, managedPlatformSupported() }
func managedDetach(*exec.Cmd)                       {}

func managedOpenPrivate(string, int) (*os.File, error) { return nil, managedPlatformSupported() }

// Platforms without O_NOFOLLOW still reject symlink leaves and verify that
// opening did not change the file identity. Managed daemon mode is unsupported.
func openPrivateAuthToken(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("API auth token file must be a private regular file (0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, errors.New("API auth token file identity changed while opening")
	}
	return f, nil
}
