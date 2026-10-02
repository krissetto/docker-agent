//go:build darwin || linux

package root

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
)

func managedFixture(t *testing.T) (string, *snapshot.Manifest) {
	t.Helper()
	workspace, err := managedWorkspace(t.TempDir())
	require.NoError(t, err)
	dir, err := managedStateDir(filepath.Join(t.TempDir(), "private"), workspace)
	require.NoError(t, err)
	m, err := snapshot.Snapshot(t.Context(), config.NewBytesSource("team.yaml", []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: test\n")), snapshot.Options{Startup: snapshot.Startup{Workspace: workspace, SourceKey: "exact/team.yaml"}})
	require.NoError(t, err)
	return dir, m
}

// The helper starts the real API and SQLite lifecycle, but no session, provider,
// tool, or model. It is a tiny detached subprocess, never a live inference test.
func TestManagedAPIChild(t *testing.T) {
	if os.Getenv("DOCKER_AGENT_MANAGED_TEST_CHILD") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	f := apiFlags{managedState: os.Getenv("MANAGED_TEST_STATE"), managedManifest: os.Getenv("MANAGED_TEST_MANIFEST"), maxRequestSize: 1 << 20}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := f.runAPICommand(cmd, []string{"unused"}); err != nil {
		t.Fatal(err)
	}
}

type managedTestChildren struct {
	mu       sync.Mutex
	children []*exec.Cmd
	starts   atomic.Int32
}

func (c *managedTestChildren) start(dir, candidate, _ string) error {
	c.starts.Add(1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedAPIChild$")
	cmd.Env = append(os.Environ(), "DOCKER_AGENT_MANAGED_TEST_CHILD=1", "MANAGED_TEST_STATE="+dir, "MANAGED_TEST_MANIFEST="+candidate, "TELEMETRY_ENABLED=false")
	managedDetach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.children = append(c.children, cmd)
	c.mu.Unlock()
	return nil
}
func (c *managedTestChildren) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cmd := range c.children {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	c.children = nil
}
func managedTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), 10*time.Second)
}

func TestManagedAPIConcurrentReuseAndRestart(t *testing.T) {
	dir, m := managedFixture(t)
	children := &managedTestChildren{}
	t.Cleanup(children.stop)
	ctx, cancel := managedTestContext(t)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan managedDescriptor, 4)
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() { d, err := ensureManagedAPIWithStart(ctx, dir, m, children.start); results <- d; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var first managedDescriptor
	for d := range results {
		if first.InstanceID == "" {
			first = d
		}
		assert.Equal(t, first, d)
	}
	assert.EqualValues(t, 1, children.starts.Load())
	token, err := managedRead(filepath.Join(dir, "token"))
	require.NoError(t, err)
	client, err := newRemoteClient(first.Address, filepath.Join(dir, "token"))
	require.NoError(t, err)
	cfg, err := client.GetAgent(ctx, first.Source)
	require.NoError(t, err, "exact source keys may contain path separators")
	require.Len(t, cfg.Agents, 1)

	ids, err := remoteSessionIDs(ctx, first.Address, filepath.Join(dir, "token"), first.Source, first.Workspace)
	require.NoError(t, err)
	assert.Empty(t, ids)
	ids, err = managedSessionIDs(ctx, filepath.Dir(dir), first.Workspace)
	require.NoError(t, err, "managed list uses committed manifest, not default team")
	assert.Empty(t, ids)
	// Authenticated readiness/catalog are provider-free and create no sessions.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, first.Address+"/api/v2/server", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	stale := first
	stale.InstanceID = "stale-instance"
	require.ErrorContains(t, managedProbe(ctx, dir, stale, m), "identity mismatch")
	incompatible, err := snapshot.Snapshot(ctx, config.NewBytesSource("team.yaml", []byte("agents:\n  root:\n    model: openai/changed\n")), snapshot.Options{Startup: m.Startup()})
	require.NoError(t, err)
	_, err = ensureManagedAPIWithStart(ctx, dir, incompatible, children.start)
	require.ErrorContains(t, err, "configuration differs")
	require.NoError(t, managedProbe(ctx, dir, first, m), "incompatible request must not kill owner")
	children.stop()
	restarted, err := ensureManagedAPIWithStart(ctx, dir, nil, children.start)
	require.NoError(t, err)
	assert.Equal(t, first.Address, restarted.Address, "restart preserves reconnect endpoint")
	assert.NotEqual(t, first.InstanceID, restarted.InstanceID)
	nextToken, err := managedRead(filepath.Join(dir, "token"))
	require.NoError(t, err)
	assert.Equal(t, token, nextToken)
	ids, err = remoteSessionIDs(ctx, restarted.Address, filepath.Join(dir, "token"), restarted.Source, restarted.Workspace)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestManagedAPICancelDoesNotKillChild(t *testing.T) {
	dir, m := managedFixture(t)
	children := &managedTestChildren{}
	t.Cleanup(children.stop)
	ctx, cancel := context.WithCancel(t.Context())
	start := func(dir, candidate, source string) error {
		err := children.start(dir, candidate, source)
		cancel()
		return err
	}
	_, err := ensureManagedAPIWithStart(ctx, dir, m, start)
	require.ErrorIs(t, err, context.Canceled)
	ctx2, stop := managedTestContext(t)
	defer stop()
	d, err := managedWaitReady(ctx2, dir, m)
	require.NoError(t, err)
	require.NoError(t, managedProbe(ctx2, dir, d, m))
	assert.EqualValues(t, 1, children.starts.Load())
}

func TestManagedAPIBusyOwnerNeverReplaced(t *testing.T) {
	dir, m := managedFixture(t)
	owner, ok, err := managedTryLock(filepath.Join(dir, "owner.lock"))
	require.NoError(t, err)
	require.True(t, ok)
	defer owner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	started := false
	_, err = ensureManagedAPIWithStart(ctx, dir, m, func(string, string, string) error { started = true; return nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, started)
	_, err = os.Stat(filepath.Join(dir, "token"))
	require.True(t, errors.Is(err, os.ErrNotExist), "active owner token cannot rotate")
}

func TestManagedAPIPrivatePaths(t *testing.T) {
	dir, m := managedFixture(t)
	assert.NoError(t, managedPrivateDir(dir), "normal macOS /var parent alias is supported")
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0600))
	link := filepath.Join(dir, "token")
	require.NoError(t, os.Symlink(target, link))
	_, err := managedRead(link)
	require.Error(t, err)
	_, err = readAPIAuthToken(link)
	require.Error(t, err)
	_, err = newRemoteClient("http://127.0.0.1:1", link)
	require.Error(t, err, "remote clients must reject credential symlinks")
	digest, err := m.Digest()
	require.NoError(t, err)
	d := managedDescriptor{Address: "http://127.0.0.1:1", InstanceID: "test", Fingerprint: digest, Workspace: m.Startup().Workspace, Source: m.Startup().SourceKey}
	err = managedProbe(t.Context(), dir, d, m)
	require.ErrorContains(t, err, "private API auth token file", "probe must reject the token before networking")

	_, _, err = managedTryLock(link)
	require.Error(t, err)
	require.Error(t, managedAtomicWrite(link, []byte("replacement")))
	require.Error(t, managedPrivateDir(link))
	require.NoError(t, os.Chmod(target, 0644))
	_, err = managedRead(target)
	require.Error(t, err)
	require.NoError(t, os.Chmod(dir, 0755))
	require.Error(t, managedPrivateDir(dir))
	require.NoError(t, os.Chmod(dir, 0700))
	// A second daemon cannot commit a manifest or touch the database.
	owner, ok, err := managedTryLock(filepath.Join(dir, "owner.lock"))
	require.NoError(t, err)
	require.True(t, ok)
	defer owner.Close()
	candidate := filepath.Join(dir, "candidate-test.json")
	data, err := m.Marshal()
	require.NoError(t, err)
	require.NoError(t, managedAtomicWrite(candidate, data))
	f := apiFlags{managedState: dir, managedManifest: candidate}
	_, err = f.prepareManagedDaemon()
	require.ErrorContains(t, err, "already has an owner")
	_, err = os.Stat(filepath.Join(dir, "session.db"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestManagedAPIStickyOccupiedPortFails(t *testing.T) {
	dir, m := managedFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	digest, err := m.Digest()
	require.NoError(t, err)
	descriptor := managedDescriptor{Address: "http://" + ln.Addr().String(), InstanceID: "previous", Fingerprint: digest, Workspace: m.Startup().Workspace, Source: m.Startup().SourceKey}
	data, err := json.Marshal(descriptor)
	require.NoError(t, err)
	require.NoError(t, managedAtomicWrite(filepath.Join(dir, "server.json"), data))
	data, err = m.Marshal()
	require.NoError(t, err)
	require.NoError(t, managedAtomicWrite(filepath.Join(dir, "manifest.json"), data))
	children := &managedTestChildren{}
	t.Cleanup(children.stop)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err = ensureManagedAPIWithStart(ctx, dir, m, children.start)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	saved, err := managedReadDescriptor(dir)
	require.NoError(t, err)
	assert.Equal(t, descriptor, saved)
}

func TestManagedAPIUnsupportedModesAndEmptyList(t *testing.T) {
	_, err := (&runExecFlags{sessionDB: "explicit.db"}).bootstrapManagedAPI(t.Context(), "default")
	require.ErrorContains(t, err, "cannot use --session-db")

	for _, f := range []*runExecFlags{{fakeResponses: "test"}, {recordPath: "test"}, {fakeStreamDelay: 1}} {
		_, err := f.bootstrapManagedAPI(t.Context(), "default")
		require.ErrorContains(t, err, "does not support")
	}
	ids, err := managedSessionIDs(t.Context(), filepath.Join(t.TempDir(), "private"), t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestAPIAuthTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(" token\n"), 0600))
	token, err := readAPIAuthToken(path)
	require.NoError(t, err)
	assert.Equal(t, "token", token)
	require.NoError(t, os.Chmod(path, 0644))
	_, err = readAPIAuthToken(path)
	require.Error(t, err)
	require.NoError(t, os.Chmod(path, 0600))
	require.NoError(t, os.WriteFile(path, []byte("a\nb"), 0600))
	_, err = readAPIAuthToken(path)
	require.Error(t, err)
}
