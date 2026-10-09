package root

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// This prepares the existing daemon path in-process; it never starts a child.
func (f *apiFlags) prepareForegroundManagedAPI(ctx context.Context, source string) error {
	if f.managedState != "" || f.managedManifest != "" || f.fakeResponses != "" || f.recordPath != "" || f.pullIntervalMins != 0 || f.authToken != "" || f.authTokenFile != "" {
		return errors.New("foreground managed API cannot combine explicit daemon state, fake/record, source refresh or authentication options")
	}
	if f.managedAPIStateDir != "" && !filepath.IsAbs(f.managedAPIStateDir) {
		return errors.New("foreground managed state base must be an absolute private guest path")
	}
	workspace, err := managedWorkspace(f.runConfig.WorkingDir)
	if err != nil {
		return err
	}
	base := f.managedAPIStateDir
	if base == "" {
		base = filepath.Join(paths.GetDataDir(), "managed-api")
	}
	absWorkspace, err := filepath.Abs(f.runConfig.WorkingDir)
	if f.runConfig.WorkingDir == "" {
		absWorkspace, err = os.Getwd()
	}
	if err != nil {
		return err
	}
	if base == absWorkspace || strings.HasPrefix(filepath.Clean(base), absWorkspace+string(filepath.Separator)) || base == workspace || strings.HasPrefix(filepath.Clean(base), workspace+string(filepath.Separator)) {
		return errors.New("foreground managed state must be outside the mounted workspace")
	}
	dir, err := managedStateDir(base, workspace)
	if err != nil {
		return err
	}
	if dir == workspace || strings.HasPrefix(dir, workspace+string(filepath.Separator)) {
		return errors.New("foreground managed state must be outside the mounted workspace")
	}
	lock, err := managedLaunchLock(ctx, dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	userCfg, err := userconfig.Load()
	if err != nil {
		return err
	}
	f.runConfig.GlobalHooks = config.MergeHooks(userCfg.GetSettings().GlobalHooks(), config.LoadHookDropIns())
	candidate, err := (&runExecFlags{runConfig: f.runConfig, modelOverrides: f.modelOverrides, snapshotsEnabled: userCfg.GetSettings().SnapshotsEnabled()}).managedSnapshot(ctx, source, workspace)
	if err != nil {
		return err
	}
	committed, err := managedReadManifest(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if committed != nil {
		if err := managedCompatible(candidate, committed); err != nil {
			return err
		}
		candidate = committed
	}
	tokenPath := filepath.Join(dir, "token")
	if _, err = managedRead(tokenPath); errors.Is(err, os.ErrNotExist) {
		token := make([]byte, 32)
		if _, err = rand.Read(token); err != nil {
			return err
		}
		err = managedAtomicWrite(tokenPath, []byte(hex.EncodeToString(token)))
	}
	if err != nil {
		return err
	}
	f.managedState, f.manifest = dir, candidate
	return nil
}

func validateManagedAttachFlags(cmd *cobra.Command) error {
	for _, name := range []string{"working-dir", "agent-picker", "model", "env-from-file", "flavor", "models-gateway", "code-mode-tools", "hook-pre-tool-use", "hook-post-tool-use", "hook-session-start", "hook-session-end", "hook-on-user-input", "hook-stop", "mcp-oauth-redirect-uri", "remote-auth-token-file", "remote-working-dir", "listen", "session-workingdir-root"} {
		if cmd.Flags().Changed(name) {
			return errors.New("--" + name + " cannot change a lifecycle-owned API; bind server configuration when creating the sandbox")
		}
	}
	return nil
}

func attachedManagedSessionIDs(ctx context.Context, base, root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, managedStartupTimeout)
	defer cancel()
	workspace, err := managedWorkspace(root)
	if err != nil {
		return nil, err
	}
	dir, err := managedStateDir(base, workspace)
	if err != nil {
		return nil, err
	}
	d, err := attachManagedAPI(ctx, dir)
	if err != nil {
		return nil, err
	}
	if d.Workspace != workspace {
		return nil, errors.New("managed API workspace identity mismatch")
	}
	return remoteSessionIDs(ctx, d.Address, filepath.Join(dir, "token"), d.Source, workspace)
}
