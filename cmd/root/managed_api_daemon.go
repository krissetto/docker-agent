package root

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/server"
)

// readPrivateAuthToken is shared by serve and remote clients. Unix opens use
// O_NOFOLLOW and fstat owner/mode checks on the actual descriptor, not a prior
// path lookup. Credentials never appear in errors or argv.
func readPrivateAuthToken(path string) (string, error) {
	f, err := openPrivateAuthToken(path)
	if err != nil {
		return "", fmt.Errorf("opening private API auth token file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", err
	}
	if len(data) == 64<<10 {
		return "", errors.New("API auth token file exceeds size limit")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("API auth token file must contain one nonempty token")
	}
	return token, nil
}

func readAPIAuthToken(path string) (string, error) { return readPrivateAuthToken(path) }

// prepareManagedDaemon owns the kernel lock before any state is committed or
// SQLite is opened. Its cleanup must run AFTER the session store is closed.
func (f *apiFlags) prepareManagedDaemon() (func(), error) {
	noop := func() {}
	if f.managedState == "" && f.managedManifest == "" && f.manifest == nil {
		return noop, nil
	}
	if f.managedState == "" || (f.managedManifest == "" && f.manifest == nil) {
		return nil, errors.New("managed state and manifest must be supplied together")
	}
	if err := managedPlatformSupported(); err != nil {
		return nil, err
	}
	if err := managedPrivateDir(f.managedState); err != nil {
		return nil, err
	}
	if f.manifest == nil && (filepath.Dir(f.managedManifest) != f.managedState || !strings.HasPrefix(filepath.Base(f.managedManifest), "candidate-")) {
		return nil, errors.New("managed candidate must be inside private state directory")
	}
	// Each candidate belongs to exactly this spawn attempt, not another launcher.
	if f.managedManifest != "" {
		defer os.Remove(f.managedManifest)
	}
	owner, ok, err := managedTryLock(filepath.Join(f.managedState, "owner.lock"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("managed API already has an owner; refusing to open database")
	}
	release := func() { _ = owner.Close() }
	fail := func(err error) (func(), error) { release(); return nil, err }
	m := f.manifest
	if m == nil {
		data, err := managedRead(f.managedManifest)
		if err != nil {
			return fail(err)
		}
		m, err = snapshot.Parse(data)
		if err != nil {
			return fail(err)
		}
	}
	previous, err := managedReadManifest(f.managedState)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if previous != nil {
		if err = managedCompatible(m, previous); err != nil {
			return fail(err)
		}
		m = previous
	}
	startup := m.Startup()
	f.listenAddr = "127.0.0.1:0"
	if d, err := managedReadDescriptor(f.managedState); err == nil {
		digest, _ := m.Digest()
		if !managedValidAddress(d.Address) || d.Fingerprint != digest || d.Workspace != startup.Workspace || d.Source != startup.SourceKey {
			return fail(errors.New("saved managed endpoint identity mismatch"))
		}
		f.listenAddr = strings.TrimPrefix(d.Address, "http://")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if f.fakeResponses != "" || f.recordPath != "" || f.pullIntervalMins != 0 {
		return fail(errors.New("managed API cannot use fake, record or source refresh"))
	}
	f.authTokenFile = filepath.Join(f.managedState, "token")
	if err = managedCheckFile(f.authTokenFile); err != nil {
		return fail(err)
	}
	f.sessionDB = filepath.Join(f.managedState, "session.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err = managedCheckFile(f.sessionDB + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	// Precreate SQLite's main file privately; WAL files inherit its permissions.
	db, err := managedOpenPrivate(f.sessionDB, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fail(err)
	}
	_ = db.Close()
	f.sessionWorkingDirRoot = startup.Workspace
	f.runConfig = config.RuntimeConfig{Config: startup.Runtime}
	// Flavors have already been materialized in the immutable source graph.
	f.runConfig.Flavors = nil
	f.manifest = m
	data, err := m.Marshal()
	if err != nil {
		return fail(err)
	}
	if err = managedAtomicWrite(filepath.Join(f.managedState, "manifest.json"), data); err != nil {
		return fail(err)
	}
	return release, nil
}

func (f *apiFlags) serveManaged(ctx context.Context, s *server.Server, ln net.Listener, identity api.ServerInfo) error {
	if f.manifest == nil {
		return s.Serve(ctx, ln)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Serve(serveCtx, ln) }()
	d := managedDescriptor{Address: "http://" + ln.Addr().String(), InstanceID: identity.InstanceID, Fingerprint: identity.ConfigFingerprint, Workspace: identity.WorkspaceRoot, Source: identity.Source, PID: os.Getpid()}
	readyCtx, stop := context.WithTimeout(ctx, managedStartupTimeout)
	defer stop()
	for {
		probeCtx, end := context.WithTimeout(readyCtx, time.Second)
		err := managedProbe(probeCtx, f.managedState, d, f.manifest)
		end()
		if err == nil {
			data, err := json.Marshal(d)
			if err == nil {
				err = managedAtomicWrite(filepath.Join(f.managedState, "server.json"), data)
			}
			if err != nil {
				cancel()
				<-result
				return err
			}
			return <-result
		}
		select {
		case err := <-result:
			return err
		default:
		}
		if err = managedPause(readyCtx); err != nil {
			cancel()
			<-result
			return fmt.Errorf("managed API readiness: %w", err)
		}
	}
}
func (f *apiFlags) managedIdentity() (api.ServerInfo, error) {
	if f.manifest == nil {
		return api.ServerInfo{}, nil
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return api.ServerInfo{}, err
	}
	digest, err := f.manifest.Digest()
	if err != nil {
		return api.ServerInfo{}, err
	}
	startup := f.manifest.Startup()
	return api.ServerInfo{InstanceID: hex.EncodeToString(nonce), WorkspaceRoot: startup.Workspace, Source: startup.SourceKey, ConfigFingerprint: digest}, nil
}
