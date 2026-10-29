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
	"strconv"
	"strings"
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

func TestManagedListenHostAndDiscoveryAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		want string
		err  bool
	}{
		{name: "default", want: "127.0.0.1"},
		{name: "loopback", host: "127.0.0.1", want: "127.0.0.1"},
		{name: "wildcard", host: "0.0.0.0", want: "0.0.0.0"},
		{name: "other host rejected", host: "192.0.2.1", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := managedListenHost(tc.host)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, tc := range []struct {
		host string
		port string
		want string
		err  bool
	}{
		{port: "0", want: "127.0.0.1:0"},
		{host: "0.0.0.0", port: "0", want: "0.0.0.0:0"},
		{host: "0.0.0.0", port: "44453", want: "0.0.0.0:44453"},
		{host: "127.0.0.1", port: "44453", want: "127.0.0.1:44453"},
		{host: "0.0.0.0", port: "-1", err: true},
	} {
		got, err := managedListenerAddress(tc.host, tc.port)
		if tc.err {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	for _, addr := range []net.Addr{
		&net.TCPAddr{IP: net.IPv4zero, Port: 44453},
		&net.TCPAddr{IP: net.IPv6zero, Port: 44453},
	} {
		discovered, err := managedDiscoveryAddress(addr)
		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:44453", discovered)
	}
	_, err := managedDiscoveryAddress(&net.UnixAddr{Name: "socket", Net: "unix"})
	require.Error(t, err)
}

func TestPrepareManagedDaemonSelectsKitBindingAndPreservesPort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       string
		listenPort int
		first      string
		port       string
		want       string
	}{
		{name: "default loopback", first: "127.0.0.1:0", port: "46321", want: "127.0.0.1:46321"},
		{name: "Kit wildcard ephemeral", host: "0.0.0.0", first: "0.0.0.0:0", port: "46322", want: "0.0.0.0:46322"},
		{name: "Kit fixed port overrides old endpoint", host: "0.0.0.0", listenPort: 9182, first: "0.0.0.0:9182", port: "46323", want: "0.0.0.0:9182"},
		{name: "maximum valid port", listenPort: 65535, first: "127.0.0.1:65535", port: "46324", want: "127.0.0.1:65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, manifest := managedFixture(t)
			token := filepath.Join(dir, "token")
			require.NoError(t, managedAtomicWrite(token, []byte("fixture-token")))
			manifestPath := filepath.Join(dir, "candidate-test.json")
			data, err := manifest.Marshal()
			require.NoError(t, err)
			require.NoError(t, managedAtomicWrite(manifestPath, data))
			flags := &apiFlags{managedState: dir, managedManifest: manifestPath, managedAPIListenHost: tc.host, managedAPIListenPort: tc.listenPort}
			release, err := flags.prepareManagedDaemon()
			require.NoError(t, err)
			assert.Equal(t, tc.first, flags.listenAddr)
			release()

			digest, err := manifest.Digest()
			require.NoError(t, err)
			startup := manifest.Startup()
			descriptor := managedDescriptor{Address: "http://127.0.0.1:" + tc.port, InstanceID: "prior-instance", Fingerprint: digest, Workspace: startup.Workspace, Source: startup.SourceKey}
			descriptorData, err := json.Marshal(descriptor)
			require.NoError(t, err)
			require.NoError(t, managedAtomicWrite(filepath.Join(dir, "server.json"), descriptorData))
			require.NoError(t, managedAtomicWrite(manifestPath, data))
			flags = &apiFlags{managedState: dir, managedManifest: manifestPath, managedAPIListenHost: tc.host, managedAPIListenPort: tc.listenPort}
			release, err = flags.prepareManagedDaemon()
			require.NoError(t, err)
			assert.Equal(t, tc.want, flags.listenAddr, "restart binds selected host to the saved port")
			release()
		})
	}
}

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
	listenPort, _ := strconv.Atoi(os.Getenv("MANAGED_TEST_LISTEN_PORT"))
	f := apiFlags{managedState: os.Getenv("MANAGED_TEST_STATE"), managedManifest: os.Getenv("MANAGED_TEST_MANIFEST"), managedAPIListenHost: os.Getenv("MANAGED_TEST_LISTEN_HOST"), managedAPIListenPort: listenPort, maxRequestSize: 1 << 20}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := f.runAPICommand(cmd, []string{"unused"}); err != nil {
		t.Fatal(err)
	}
}

type managedTestChildren struct {
	listenHost string
	listenPort int
	mu         sync.Mutex
	children   []*exec.Cmd
	starts     atomic.Int32
}

func (c *managedTestChildren) start(dir, candidate, _ string) error {
	c.starts.Add(1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedAPIChild$")
	cmd.Env = append(os.Environ(), "DOCKER_AGENT_MANAGED_TEST_CHILD=1", "MANAGED_TEST_STATE="+dir, "MANAGED_TEST_MANIFEST="+candidate, "MANAGED_TEST_LISTEN_HOST="+c.listenHost, "MANAGED_TEST_LISTEN_PORT="+strconv.Itoa(c.listenPort), "TELEMETRY_ENABLED=false")
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
	assert.True(t, strings.HasPrefix(first.Address, "http://127.0.0.1:"), "managed discovery remains loopback")
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

func TestManagedAPIBroadBindUsesLocalDiscoveryAndAuthentication(t *testing.T) {
	dir, m := managedFixture(t)
	children := &managedTestChildren{listenHost: "0.0.0.0"}
	t.Cleanup(children.stop)
	ctx, cancel := managedTestContext(t)
	defer cancel()
	d, err := ensureManagedAPIWithStart(ctx, dir, m, children.start)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(d.Address, "http://127.0.0.1:"), "discovered URL must remain locally usable")
	assertManagedLocalAuth(t, dir, d)

	children.stop()
	restarted, err := ensureManagedAPIWithStart(ctx, dir, nil, children.start)
	require.NoError(t, err)
	assert.Equal(t, d.Address, restarted.Address, "restart preserves the selected ephemeral port")
	assertManagedLocalAuth(t, dir, restarted)

	guestIP := managedTestNonLoopbackIPv4(t)
	t.Run("non-loopback guest peer", func(t *testing.T) {
		if guestIP == nil {
			t.Skip("host has no non-loopback IPv4 address for guest peer reachability check")
		}
		if !managedWildcardPeerControl(t, guestIP) {
			t.Skip("independent wildcard listener control cannot self-connect via the host's non-loopback address")
		}
		_, port, err := net.SplitHostPort(strings.TrimPrefix(restarted.Address, "http://"))
		require.NoError(t, err)
		externalURL := "http://" + net.JoinHostPort(guestIP.String(), port) + "/api/v2/server"
		client := managedTestHTTPClient(t)
		assert.Equal(t, http.StatusUnauthorized, managedGuestServerStatus(t, client, externalURL, ""))
		token, err := managedRead(filepath.Join(dir, "token"))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, managedGuestServerStatus(t, client, externalURL, string(token)))
	})
}

func TestManagedAPIFixedPortLifecycle(t *testing.T) {
	port := managedAvailableTCPPort(t)
	dir, manifest := managedFixture(t)
	children := &managedTestChildren{listenHost: "0.0.0.0", listenPort: port}
	t.Cleanup(children.stop)
	ctx, cancel := managedTestContext(t)
	defer cancel()
	d, err := ensureManagedAPIWithStart(ctx, dir, manifest, children.start)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:"+strconv.Itoa(port), d.Address)
	assertManagedLocalAuth(t, dir, d)

	children.stop()
	restarted, err := ensureManagedAPIWithStart(ctx, dir, nil, children.start)
	require.NoError(t, err)
	assert.Equal(t, d.Address, restarted.Address)
	assertManagedLocalAuth(t, dir, restarted)
}

func managedAvailableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func assertManagedLocalAuth(t *testing.T, dir string, d managedDescriptor) {
	t.Helper()
	client := managedTestHTTPClient(t)
	serverURL := d.Address + "/api/v2/server"
	assert.Equal(t, http.StatusUnauthorized, managedGuestServerStatus(t, client, serverURL, ""))
	token, err := managedRead(filepath.Join(dir, "token"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, managedGuestServerStatus(t, client, serverURL, string(token)))
	assert.NoError(t, managedCheckFile(filepath.Join(dir, "token")), "token remains private")
	remote, err := newRemoteClient(d.Address, filepath.Join(dir, "token"))
	require.NoError(t, err)
	_, err = remote.ServerInfo(t.Context())
	require.NoError(t, err, "managed local client uses loopback discovery and private token")
}

func managedTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func managedGuestServerStatus(t *testing.T, client *http.Client, address, token string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	require.NoError(t, err)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	require.NoError(t, err, "managed listener should accept the requested peer")
	defer response.Body.Close()
	return response.StatusCode
}

func managedWildcardPeerControl(t *testing.T, address net.IP) bool {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	client := managedTestHTTPClient(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+net.JoinHostPort(address.String(), strconv.Itoa(port)), nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	if errors.Is(err, syscall.ECONNRESET) {
		return false
	}
	require.NoError(t, err, "independent wildcard listener should accept the host's non-loopback address")
	defer response.Body.Close()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	return true
}

func managedTestNonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ipv4 := ip.To4(); ipv4 != nil && !ipv4.IsLoopback() && !ipv4.IsUnspecified() {
				return ipv4
			}
		}
	}
	return nil
}

func TestManagedAPIPortCollisionIsActionableAndPreservesListener(t *testing.T) {
	occupied, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	dir, manifest := managedFixture(t)
	require.NoError(t, managedAtomicWrite(filepath.Join(dir, "token"), []byte("fixture-token")))
	flags := &apiFlags{managedState: dir, manifest: manifest, managedAPIListenHost: "0.0.0.0", managedAPIListenPort: port, maxRequestSize: 1 << 20}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = flags.runAPICommand(cmd, []string{"unused"})
	require.ErrorContains(t, err, "configured port may be occupied")
	require.ErrorContains(t, err, "no existing listener was stopped")
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	require.NoError(t, err, "the process that owned the colliding port remains reachable")
	_ = connection.Close()
	owner, acquired, err := managedTryLock(filepath.Join(dir, "owner.lock"))
	require.NoError(t, err)
	require.True(t, acquired, "failed managed startup releases its own owner lock")
	_ = owner.Close()
}

func TestPrepareManagedDaemonRejectsInvalidPortsAndReleasesOwner(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			dir, manifest := managedFixture(t)
			require.NoError(t, managedAtomicWrite(filepath.Join(dir, "token"), []byte("fixture-token")))
			flags := &apiFlags{managedState: dir, manifest: manifest, managedAPIListenPort: port}
			_, err := flags.prepareManagedDaemon()
			require.ErrorContains(t, err, "between 0 and 65535")
			owner, acquired, err := managedTryLock(filepath.Join(dir, "owner.lock"))
			require.NoError(t, err)
			require.True(t, acquired, "invalid port startup releases the owner lock")
			_ = owner.Close()
		})
	}
}

func TestExplicitPortDoesNotBypassExistingManagedOwner(t *testing.T) {
	dir, manifest := managedFixture(t)
	require.NoError(t, managedAtomicWrite(filepath.Join(dir, "token"), []byte("fixture-token")))
	digest, err := manifest.Digest()
	require.NoError(t, err)
	startup := manifest.Startup()
	descriptor := managedDescriptor{Address: "http://127.0.0.1:43123", InstanceID: "owner", Fingerprint: digest, Workspace: startup.Workspace, Source: startup.SourceKey}
	descriptorData, err := json.Marshal(descriptor)
	require.NoError(t, err)
	require.NoError(t, managedAtomicWrite(filepath.Join(dir, "server.json"), descriptorData))
	owner, acquired, err := managedTryLock(filepath.Join(dir, "owner.lock"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer owner.Close()
	flags := &apiFlags{managedState: dir, manifest: manifest, managedAPIListenHost: "0.0.0.0", managedAPIListenPort: 9182}
	_, err = flags.prepareManagedDaemon()
	require.ErrorContains(t, err, "already has an owner")
	after, err := managedReadDescriptor(dir)
	require.NoError(t, err)
	assert.Equal(t, descriptor, after, "configured port cannot mutate an active owner's descriptor")
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
