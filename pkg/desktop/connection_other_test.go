//go:build !windows && !js

package desktop

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scratchDir returns a fresh directory under $TMPDIR. Every test here points
// TMPDIR at scratch space, so nothing is ever created in the real /tmp.
func scratchDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TMPDIR")
	if base == "" {
		t.Fatal("TMPDIR must name a scratch directory")
	}
	dir, err := os.MkdirTemp(base, "dt") //nolint:forbidigo,usetesting // t.TempDir embeds the long test name, which overflows the sockaddr_un limit these tests probe
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// socketScratch returns a root for test sockets and points TMPDIR at a short
// subdirectory of it. The fallback link is created under TMPDIR, so this skips
// when the outer scratch path leaves no room for the link in sockaddr_un.
func socketScratch(t *testing.T) (root, tmp string) {
	t.Helper()
	root = scratchDir(t)
	tmp = filepath.Join(root, "t")
	// The longest name dial's MkdirTemp produces is "dcs-" plus 10 digits.
	link := filepath.Join(tmp, "dcs-0000000000", "s")
	if len(link) > maxUnixSocketPathLen || len(filepath.Join(root, "s.sock")) > maxUnixSocketPathLen {
		t.Skipf("scratch path %q leaves no room for Unix socket paths", root)
	}
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	return root, tmp
}

// overlongDir returns a real directory chain under root whose full path is
// longer than any sockaddr_un can hold.
func overlongDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, strings.Repeat("a", 50), strings.Repeat("b", 50))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return dir
}

// listenOK accepts n connections on path, sending "ok" to each one.
func listenOK(t *testing.T, path string, n int) {
	t.Helper()
	require.LessOrEqual(t, len(path), maxUnixSocketPathLen)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for range n {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("ok"))
			_ = conn.Close()
		}
	}()
}

func readOK(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 2)
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(buf))
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "dial must not leave temporary entries behind")
}

// overlongSocket returns a path that is too long for sockaddr_un and that
// reaches a socket at sock through a symlink.
func overlongSocket(t *testing.T, root, sock string) string {
	t.Helper()
	backend := filepath.Join(overlongDir(t, root), "backend.sock")
	require.Greater(t, len(backend), maxUnixSocketPathLen)
	require.NoError(t, os.Symlink(sock, backend))
	return backend
}

func TestDialShortPathDoesNotUseTMPDIR(t *testing.T) {
	root := scratchDir(t)
	sock := filepath.Join(root, "s.sock")
	listenOK(t, sock, 1)
	t.Setenv("TMPDIR", filepath.Join(root, "missing"))

	conn, err := dial(t.Context(), sock)
	require.NoError(t, err)
	readOK(t, conn)
}

func TestDialOverlongPathReachesSocketThroughShortLink(t *testing.T) {
	root, tmp := socketScratch(t)
	sock := filepath.Join(root, "s.sock")
	listenOK(t, sock, 1)
	backend := overlongSocket(t, root, sock)

	conn, err := dial(t.Context(), backend)
	require.NoError(t, err)
	readOK(t, conn)

	assertEmptyDir(t, tmp)
}

func TestDialOverlongPathConcurrent(t *testing.T) {
	const n = 8
	root, tmp := socketScratch(t)
	sock := filepath.Join(root, "s.sock")
	listenOK(t, sock, n)
	backend := overlongSocket(t, root, sock)

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			conn, err := dial(t.Context(), backend)
			if !assert.NoError(t, err) { //nolint:testifylint // assert, not require: this runs on a goroutine, and require would call FailNow off the test goroutine
				return
			}
			defer conn.Close()
			assert.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second))) //nolint:testifylint // assert, not require: this runs on a goroutine, and require would call FailNow off the test goroutine
			buf := make([]byte, 2)
			_, err = io.ReadFull(conn, buf)
			assert.NoError(t, err) //nolint:testifylint // assert, not require: this runs on a goroutine, and require would call FailNow off the test goroutine
			assert.Equal(t, "ok", string(buf))
		})
	}
	wg.Wait()

	assertEmptyDir(t, tmp)
}

func TestDialOverlongPathSetupFailureReturnsOriginalError(t *testing.T) {
	root := scratchDir(t)
	notDir := filepath.Join(root, "not-a-dir")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))

	cases := []struct {
		name string
		tmp  string
	}{
		{"missing temp root", filepath.Join(root, "missing")},
		{"temp root is a file", notDir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", tc.tmp)
			backend := filepath.Join(overlongDir(t, root), "backend.sock")

			_, err := dial(t.Context(), backend)
			require.ErrorIs(t, err, syscall.EINVAL)
		})
	}
}

func TestDialOverlongPathLongTMPDIRReturnsOriginalError(t *testing.T) {
	root := scratchDir(t)
	longTmp := overlongDir(t, root)
	t.Setenv("TMPDIR", longTmp)
	backend := filepath.Join(longTmp, "backend.sock")

	_, err := dial(t.Context(), backend)
	require.ErrorIs(t, err, syscall.EINVAL)

	assertEmptyDir(t, longTmp)
}

func TestDialOverlongPathSecondDialFailureCleansUp(t *testing.T) {
	root, tmp := socketScratch(t)
	backend := filepath.Join(overlongDir(t, root), "missing.sock")

	_, err := dial(t.Context(), backend)
	require.ErrorIs(t, err, syscall.ENOENT)

	assertEmptyDir(t, tmp)
}

func TestDialOverlongPathCancelledContextCleansUp(t *testing.T) {
	root, tmp := socketScratch(t)
	sock := filepath.Join(root, "s.sock")
	listenOK(t, sock, 0)
	backend := overlongSocket(t, root, sock)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := dial(ctx, backend)
	require.Error(t, err)

	assertEmptyDir(t, tmp)
}

// TestShortSocketLinkCancelledDialRemovesLink covers the cleanup that dial
// defers once a link exists. A context cancelled before dial is called fails
// before the link is ever used, so this drives the link helper directly.
func TestShortSocketLinkCancelledDialRemovesLink(t *testing.T) {
	root, tmp := socketScratch(t)
	sock := filepath.Join(root, "s.sock")
	listenOK(t, sock, 0)
	backend := overlongSocket(t, root, sock)

	link, cleanup, err := shortSocketLink(backend)
	require.NoError(t, err)
	_, err = os.Lstat(link)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = (&net.Dialer{}).DialContext(ctx, "unix", link)
	require.Error(t, err)
	cleanup()

	assertEmptyDir(t, tmp)
}
