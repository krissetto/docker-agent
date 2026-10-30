//go:build !windows && !js

package desktop

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// maxUnixSocketPathLen is the longest socket path we use, excluding the NUL.
// Darwin sun_path is 104 bytes including the NUL and Linux is 108, so 103 is
// the largest bound valid on both. Paths over it can fail with EINVAL on Darwin.
const maxUnixSocketPathLen = 103

func dialBackend(ctx context.Context) (net.Conn, error) {
	return dial(ctx, Paths().BackendSocket)
}

func dial(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{}

	conn, err := dialer.DialContext(ctx, "unix", path)
	if err == nil || !errors.Is(err, syscall.EINVAL) {
		return conn, err
	}

	// The socket path is too long for sockaddr_un, so connect through a short
	// symlink to it. The link sits in a per-call directory: concurrent dials
	// never share a name, and nothing is left in TMPDIR afterwards. Once
	// connect has returned the kernel holds the socket itself, so removing the
	// link does not affect the connection.
	link, cleanup, linkErr := shortSocketLink(path)
	if linkErr != nil {
		return nil, err // Keep the original error; the link could not be made.
	}
	defer cleanup()

	return dialer.DialContext(ctx, "unix", link)
}

// shortSocketLink creates a symlink to target whose path fits in sockaddr_un.
// The caller must call cleanup, which removes the link and its directory.
func shortSocketLink(target string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "dcs-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	link := filepath.Join(dir, "s")
	if len(link) > maxUnixSocketPathLen {
		cleanup()
		return "", nil, errors.New("temporary socket link path is too long")
	}
	if err := os.Symlink(target, link); err != nil {
		cleanup()
		return "", nil, err
	}
	return link, cleanup, nil
}
