package root

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/paths"
)

const managedStartupTimeout = 30 * time.Second

var managedCapabilities = []string{"session_v2", "session_tools", "session_permissions", "mcp_prompts", "todo_editing", "session_branching", "session_explicit_ids"}

type managedDescriptor struct {
	Address     string `json:"address"`
	InstanceID  string `json:"instance_id"`
	Fingerprint string `json:"fingerprint"`
	Workspace   string `json:"workspace"`
	Source      string `json:"source"`
	PID         int    `json:"pid"` // Advisory only; never used to signal a process.
}

func managedWorkspace(root string) (string, error) {
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

func managedStateDir(base, workspace string) (string, error) {
	if err := managedPlatformSupported(); err != nil {
		return "", err
	}
	if base == "" {
		base = filepath.Join(paths.GetDataDir(), "managed-api")
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if err = managedPrivateDir(base); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workspace))
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, hex.EncodeToString(sum[:]))
	return dir, managedPrivateDir(dir)
}

func managedPrivateDir(path string) error {
	// Canonical parent aliases (notably macOS /var -> /private/var) are
	// legitimate. The final state directory itself must never be a symlink.
	parent := filepath.Dir(path)
	if parent != path {
		if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
			if err = managedPrivateDir(parent); err != nil {
				return err
			}
		}
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !managedOwned(info) {
		return fmt.Errorf("managed state directory must be owned and private (0700): %s", path)
	}
	return nil
}

func managedCheckFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !managedOwned(info) {
		return fmt.Errorf("managed state file must be owned and private (0600): %s", path)
	}
	return nil
}

func managedRead(path string) ([]byte, error) {
	f, err := managedOpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func managedAtomicWrite(path string, data []byte) error {
	if err := managedCheckFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func managedReadManifest(dir string) (*snapshot.Manifest, error) {
	data, err := managedRead(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	return snapshot.Parse(data)
}

func managedReadDescriptor(dir string) (managedDescriptor, error) {
	var d managedDescriptor
	data, err := managedRead(filepath.Join(dir, "server.json"))
	if err == nil {
		err = json.Unmarshal(data, &d)
	}
	return d, err
}

func managedPause(ctx context.Context) error {
	t := time.NewTimer(50 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func managedLaunchLock(ctx context.Context, dir string) (*os.File, error) {
	for {
		f, ok, err := managedTryLock(filepath.Join(dir, "launch.lock"))
		if err != nil || ok {
			return f, err
		}
		if err = managedPause(ctx); err != nil {
			return nil, err
		}
	}
}

func (f *runExecFlags) bootstrapManagedAPI(ctx context.Context, agentFileName string) (string, error) {
	if f.sessionDB != "" {
		return "", errors.New("--managed-api cannot use --session-db; managed sessions use the private workspace database (select --managed-api-state-dir instead)")
	}
	if f.remoteAddress != "" {
		return "", errors.New("--managed-api and --remote cannot be combined")
	}
	if f.fakeResponses != "" || f.recordPath != "" || f.fakeStreamDelay != 0 {
		return "", errors.New("managed API does not support --fake, --fake-stream or --record; run a separately configured serve api daemon instead")
	}
	ctx, cancel := context.WithTimeout(ctx, managedStartupTimeout)
	defer cancel()
	workspace, err := managedWorkspace(f.runConfig.WorkingDir)
	if err != nil {
		return "", err
	}
	source, err := sources.Resolve(agentFileName, f.runConfig.EnvProvider())
	if err != nil {
		return "", err
	}
	startup := snapshot.Startup{Snapshots: f.snapshotsEnabled, Workspace: workspace, SourceKey: agentFileName, ModelOverrides: f.modelOverrides, Runtime: f.runConfig.Config}
	startup.Runtime.WorkingDir = workspace
	manifest, err := snapshot.Snapshot(ctx, source, snapshot.Options{Startup: startup, Environment: f.runConfig.EnvProvider(), Resolve: func(ref string, env environment.Provider) (config.Source, error) { return sources.Resolve(ref, env) }})
	if err != nil {
		return "", err
	}
	dir, err := managedStateDir(f.managedAPIStateDir, workspace)
	if err != nil {
		return "", err
	}
	d, err := ensureManagedAPI(ctx, dir, manifest)
	if err != nil {
		return "", err
	}
	f.remoteAddress = d.Address
	f.remoteAuthTokenFile = filepath.Join(dir, "token")
	f.remoteWorkingDir = workspace
	// Only clear after the daemon has authenticated its exact immutable inputs.
	f.modelOverrides = nil
	return d.Source, nil
}

func managedCompatible(a, b *snapshot.Manifest) error {
	x, err := a.Digest()
	if err != nil {
		return err
	}
	y, err := b.Digest()
	if err != nil {
		return err
	}
	if x != y {
		return errors.New("managed API configuration differs from the committed workspace configuration; use the original team/models/startup options or a different --managed-api-state-dir (existing work was not stopped)")
	}
	return nil
}

// ensureManagedAPI serializes launch decisions, not daemon lifetime. The child
// separately acquires owner.lock before committing state or opening SQLite.
func ensureManagedAPI(ctx context.Context, dir string, candidate *snapshot.Manifest) (managedDescriptor, error) {
	return ensureManagedAPIWithStart(ctx, dir, candidate, managedStartChild)
}

func ensureManagedAPIWithStart(ctx context.Context, dir string, candidate *snapshot.Manifest, start func(string, string, string) error) (managedDescriptor, error) {
	var zero managedDescriptor
	lock, err := managedLaunchLock(ctx, dir)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	committed, err := managedReadManifest(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return zero, err
	}
	if committed != nil {
		if candidate != nil {
			if err = managedCompatible(candidate, committed); err != nil {
				return zero, err
			}
		}
		candidate = committed
	}
	if candidate == nil {
		return zero, os.ErrNotExist
	}
	owner, free, err := managedTryLock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return zero, err
	}
	if free {
		_ = owner.Close()
	} else {
		return managedWaitReady(ctx, dir, candidate)
	}
	// A stable token survives all daemon restarts. Never replace an existing one.
	tokenPath := filepath.Join(dir, "token")
	if _, err = managedRead(tokenPath); errors.Is(err, os.ErrNotExist) {
		token := make([]byte, 32)
		if _, err = rand.Read(token); err != nil {
			return zero, err
		}
		err = managedAtomicWrite(tokenPath, []byte(hex.EncodeToString(token)))
	}
	if err != nil {
		return zero, err
	}
	data, err := candidate.Marshal()
	if err != nil {
		return zero, err
	}
	file, err := os.CreateTemp(dir, "candidate-*.json")
	if err != nil {
		return zero, err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return zero, err
	}
	if err = file.Close(); err != nil {
		return zero, err
	}
	// Do not remove this file on caller cancellation: an already-started child
	// may still be reading it. The child removes its own per-attempt manifest.
	if err = start(dir, file.Name(), candidate.Startup().SourceKey); err != nil {
		_ = os.Remove(file.Name())
		return zero, err
	}
	return managedWaitReady(ctx, dir, candidate)
}

func managedStartChild(dir, candidate, source string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "server.log")
	if err = managedCheckFile(logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log, err := managedOpenPrivate(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	defer log.Close()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer stdin.Close()
	cmd := exec.Command(exe, "--config-dir", paths.GetConfigDir(), "--cache-dir", paths.GetCacheDir(), "--data-dir", paths.GetDataDir(), "serve", "api", "--managed-state", dir, "--managed-manifest", candidate, "--auth-token-file", filepath.Join(dir, "token"), "--", source)
	cmd.Env = managedAPIEnv()
	cmd.Stdin = stdin
	cmd.Stdout = log
	cmd.Stderr = log
	managedDetach(cmd)
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("starting managed API: %w", err)
	}
	// No CommandContext: foreground cancellation must not signal the daemon.
	go func() { _ = cmd.Wait() }()
	return nil
}

// managedAPIEnv keeps run's profiling endpoint in the foreground client;
// inheriting it would make the daemon compete for the same port.
func managedAPIEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "CAGENT_PPROF_ADDR=")
	})
}

func managedValidAddress(address string) bool {
	if !strings.HasPrefix(address, "http://") {
		return false
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(address, "http://"))
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() && port != "0" && port != ""
}

func managedProbe(ctx context.Context, dir string, d managedDescriptor, m *snapshot.Manifest) error {
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	startup := m.Startup()
	if !managedValidAddress(d.Address) || d.InstanceID == "" || d.Fingerprint != digest || d.Workspace != startup.Workspace || d.Source != startup.SourceKey {
		return errors.New("managed API descriptor identity mismatch")
	}
	client, err := newRemoteClient(d.Address, filepath.Join(dir, "token"))
	if err != nil {
		return err
	}
	info, err := client.ServerInfo(ctx)
	if err != nil {
		return err
	}
	return managedVerifyInfo(info, d)
}

func managedVerifyInfo(info api.ServerInfo, d managedDescriptor) error {
	if info.Version != 1 || info.SessionAPIVersion != api.SessionAPIVersion || !info.Ready || info.InstanceID != d.InstanceID || info.ConfigFingerprint != d.Fingerprint || info.WorkspaceRoot != d.Workspace || info.Source != d.Source {
		return errors.New("managed API authenticated readiness identity mismatch")
	}
	for _, capability := range managedCapabilities {
		if !slices.Contains(info.Capabilities, capability) {
			return fmt.Errorf("managed API lacks required capability %s", capability)
		}
	}
	return nil
}

func managedWaitReady(ctx context.Context, dir string, m *snapshot.Manifest) (managedDescriptor, error) {
	var last error
	for {
		d, err := managedReadDescriptor(dir)
		if err == nil {
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			err = managedProbe(probeCtx, dir, d, m)
			cancel()
			if err == nil {
				return d, nil
			}
		}
		last = err
		if err = managedPause(ctx); err != nil {
			return managedDescriptor{}, fmt.Errorf("managed API did not become authenticated and ready (no process was stopped); inspect %s: %w (last probe: %v)", filepath.Join(dir, "server.log"), err, last)
		}
	}
}
