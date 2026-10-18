package root

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
)

func managedSourceDir(state string) string { return filepath.Join(state, "sources") }
func sourceIDForPath(relative string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(relative)))
	return "kit:" + hex.EncodeToString(sum[:])
}
func sourceStorageKey(sourceID string) string {
	sum := sha256.Sum256([]byte(sourceID))
	return hex.EncodeToString(sum[:])
}
func sourceRevisionPath(state, sourceID, revision string) string {
	return filepath.Join(managedSourceDir(state), sourceStorageKey(sourceID), revision+".json")
}
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func managedReadManifestAt(path string) (*snapshot.Manifest, error) {
	data, err := managedRead(path)
	if err != nil {
		return nil, err
	}
	return snapshot.Parse(data)
}

type persistedSourceIdentity struct {
	SourceID    string `json:"source_id"`
	Path        string `json:"path"`
	InputDigest string `json:"input_digest"`
}
type persistedSourceHead struct {
	SourceID    string `json:"source_id"`
	Path        string `json:"path"`
	Revision    string `json:"revision"`
	InputDigest string `json:"input_digest"`
}

func readSourceHead(path, sourceID, relative string) (persistedSourceHead, error) {
	data, err := managedRead(path)
	if err != nil {
		return persistedSourceHead{}, err
	}
	var head persistedSourceHead
	if json.Unmarshal(data, &head) == nil && head.SourceID != "" {
		if head.SourceID != sourceID || head.Path != relative || !filepath.IsLocal(head.Path) || !validSourceRevision(head.Revision) || !validSourceRevision(head.InputDigest) {
			return persistedSourceHead{}, errors.New("managed source head identity mismatch")
		}
		return head, nil
	}
	revision := strings.TrimSpace(string(data))
	if !validSourceRevision(revision) {
		return persistedSourceHead{}, errors.New("saved current team revision is invalid")
	}
	return persistedSourceHead{SourceID: sourceID, Path: relative, Revision: revision}, nil
}

func validSourceRevision(revision string) bool {
	if len(revision) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil
}

func resolveManagedSourcePath(workspace, workingDir, requested string) (string, string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace: %w", err)
	}
	base := workingDir
	if base == "" {
		base = root
	}
	if !filepath.IsAbs(base) {
		base = filepath.Join(root, base)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", "", err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", "", fmt.Errorf("resolve connection working directory: %w", err)
	}
	if err := pathWithinRoot(root, base); err != nil {
		return "", "", fmt.Errorf("connection working directory: %w", err)
	}
	path := requested
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("team file not found or inaccessible: %s: %w", requested, err)
	}
	if err := pathWithinRoot(root, path); err != nil {
		return "", "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("team file is not a regular file: %s", requested)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return "", "", errors.New("Kit connection team must be a workspace YAML file (.yaml or .yml)")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", errors.New("team file is outside the sandbox workspace")
	}
	return path, relative, nil
}
func pathWithinRoot(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return fmt.Errorf("path is outside the sandbox workspace %s", root)
	}
	return nil
}

func inputDigest(ctx context.Context, manifest *snapshot.Manifest, workspace string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	files := make(map[string]string)
	for _, descriptor := range manifest.Sources() {
		if !filepath.IsAbs(descriptor.Name) || strings.Contains(descriptor.Name, "://") {
			continue
		}
		path, err := filepath.EvalSymlinks(descriptor.Name)
		if err != nil {
			return "", fmt.Errorf("resolve workspace config dependency %s: %w", descriptor.Name, err)
		}
		rootInWorkspace := pathWithinRoot(root, path) == nil
		if !rootInWorkspace && descriptor.Key != "$root" {
			return "", fmt.Errorf("local source dependency is outside the workspace: %s", descriptor.Name)
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return "", fmt.Errorf("managed source input fingerprint does not support local %s config dependencies", ext)
		}
		var data []byte
		if rootInWorkspace {
			data, err = readManagedRootFile(root, path)
		} else {
			data, err = readManagedDirectoryFile(filepath.Dir(path), path)
		}
		if err != nil {
			return "", fmt.Errorf("read workspace config dependency %s: %w", descriptor.Name, err)
		}
		files[filepath.ToSlash(path)] = digestBytes(data)
		instructions, err := yamlInstructionFiles(data)
		if err != nil {
			return "", fmt.Errorf("inspect instruction_file dependencies in %s: %w", descriptor.Name, err)
		}
		for _, instruction := range instructions {
			if !filepath.IsLocal(instruction) {
				return "", fmt.Errorf("instruction_file %q must stay inside its config directory", instruction)
			}
			file, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(path), instruction))
			if err != nil {
				return "", fmt.Errorf("resolve instruction_file %q: %w", instruction, err)
			}
			if err := pathWithinRoot(filepath.Dir(path), file); err != nil {
				return "", fmt.Errorf("instruction_file %q escapes its config directory", instruction)
			}
			if rootInWorkspace {
				if err := pathWithinRoot(root, file); err != nil {
					return "", fmt.Errorf("instruction_file %q escapes the workspace", instruction)
				}
			}
			var content []byte
			if rootInWorkspace {
				content, err = readManagedRootFile(root, file)
			} else {
				content, err = readManagedDirectoryFile(filepath.Dir(path), file)
			}
			if err != nil {
				return "", fmt.Errorf("read instruction_file %q: %w", instruction, err)
			}
			files[filepath.ToSlash(file)] = digestBytes(content)
		}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}
func readManagedRootFile(root, path string) ([]byte, error) {
	return readManagedDirectoryFile(root, path)
}

func readManagedDirectoryFile(root, path string) ([]byte, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, errors.New("managed source input is outside its config directory")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadFile(filepath.ToSlash(relative))
}

func yamlInstructionFiles(data []byte) ([]string, error) {
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	agents, ok := document["agents"].(map[string]any)
	if !ok {
		return nil, nil
	}
	var paths []string
	for _, rawAgent := range agents {
		agent, ok := rawAgent.(map[string]any)
		if !ok {
			continue
		}
		switch value := agent["instruction_file"].(type) {
		case nil:
		case string:
			paths = append(paths, value)
		case []any:
			for _, raw := range value {
				text, ok := raw.(string)
				if !ok {
					return nil, errors.New("instruction_file list must contain strings")
				}
				paths = append(paths, text)
			}
		default:
			return nil, errors.New("instruction_file must be a string or list of strings")
		}
	}
	return paths, nil
}

type managedFrozenSource struct {
	config.Source
	manifest    *snapshot.Manifest
	revision    string
	inputDigest string
	persistence *runtime.TeamEditPersistence
}

func (s managedFrozenSource) RevisionID() string                                { return s.revision }
func (s managedFrozenSource) SourceID() string                                  { return s.manifest.Startup().SourceKey }
func (s managedFrozenSource) InputDigest() string                               { return s.inputDigest }
func (s managedFrozenSource) ManagedManifest() *snapshot.Manifest               { return s.manifest }
func (s managedFrozenSource) TeamEditPersistence() *runtime.TeamEditPersistence { return s.persistence }
func (s managedFrozenSource) EncryptedConfig() string {
	if source, ok := s.Source.(config.EncryptedConfigSource); ok {
		return source.EncryptedConfig()
	}
	return ""
}
func (s managedFrozenSource) ResolveSource(ref string, env environment.Provider) (config.Source, error) {
	return s.manifest.SourceResolver(ref, env)
}
func pinnedSourceResolver(baseline *snapshot.Manifest) func(string, environment.Provider) (config.Source, error) {
	return func(ref string, env environment.Provider) (config.Source, error) {
		if baseline != nil {
			if source, err := baseline.SourceResolver(ref, env); err == nil {
				return source, nil
			}
		}
		return nil, fmt.Errorf("configuration reference %q is absent from the pinned managed source graph", ref)
	}
}

type managedSourceCoordinator struct {
	f                *apiFlags
	state, workspace string
	manager          *server.SessionManager
	mu               sync.Mutex
}
type stagedManagedSource struct {
	managedFrozenSource
	commit func() error
}

func (s stagedManagedSource) CommitSource() error { return s.commit() }
func (s stagedManagedSource) AbortSource()        {}

func (f *apiFlags) managedSourceRegistrar(state, workspace string, manager *server.SessionManager) func(context.Context, string, string) (api.SourceRegistration, config.Source, error) {
	c := &managedSourceCoordinator{f: f, state: state, workspace: workspace, manager: manager}
	return func(ctx context.Context, requested, workingDir string) (api.SourceRegistration, config.Source, error) {
		registration, source, commit, err := c.prepare(ctx, requested, workingDir)
		if err != nil {
			return api.SourceRegistration{}, nil, err
		}
		return registration, stagedManagedSource{managedFrozenSource: source, commit: commit}, nil
	}
}
func (c *managedSourceCoordinator) prepare(ctx context.Context, requested, workingDir string) (api.SourceRegistration, managedFrozenSource, func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, relative, err := resolveManagedSourcePath(c.workspace, workingDir, requested)
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	return c.prepareResolved(ctx, path, relative)
}

func (c *managedSourceCoordinator) prepareStartup(ctx context.Context) (api.SourceRegistration, managedFrozenSource, func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.f.manifest == nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, errors.New("managed startup source is unavailable")
	}
	name := c.f.manifest.FrozenSource().Name()
	if !filepath.IsAbs(name) || strings.Contains(name, "://") {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, errors.New("managed startup source must be a local YAML file")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".yaml" && ext != ".yml" {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, fmt.Errorf("managed team edit persistence requires a workspace YAML source, got %s", ext)
	}
	path, err := filepath.EvalSymlinks(name)
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, fmt.Errorf("resolve managed startup source: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, errors.New("managed startup source is not a regular file")
	}
	relative, err := filepath.Rel(c.workspace, path)
	if err != nil || !filepath.IsLocal(relative) {
		relative = "@startup"
	}
	return c.prepareResolved(ctx, path, relative)
}

func (c *managedSourceCoordinator) prepareResolved(ctx context.Context, path, relative string) (api.SourceRegistration, managedFrozenSource, func() error, error) {
	sourceID := sourceIDForPath(relative)
	sourcePath := path
	if c.f.manifest != nil {
		name := c.f.manifest.FrozenSource().Name()
		if filepath.IsAbs(name) {
			if canonical, e := filepath.EvalSymlinks(name); e == nil && canonical == path {
				sourceID = c.f.manifest.Startup().SourceKey
				sourcePath = name
			}
		}
	}
	startup := snapshot.Startup{Workspace: c.workspace, SourceKey: sourceID, Runtime: c.f.runConfig.Config, ModelOverrides: c.f.manifest.Startup().ModelOverrides, Snapshots: c.f.manifest.Startup().Snapshots}
	startup.Runtime.WorkingDir = c.workspace
	manifest, err := snapshot.Snapshot(ctx, config.NewFileSource(sourcePath), snapshot.Options{Startup: startup, Resolve: pinnedSourceResolver(c.f.manifest)})
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, fmt.Errorf("validating workspace team %s: %w", relative, err)
	}
	inputHash, err := inputDigest(ctx, manifest, c.workspace)
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	revision, err := manifest.Digest()
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	dir := filepath.Dir(sourceRevisionPath(c.state, sourceID, revision))
	if err := managedPrivateDir(dir); err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	headPath := filepath.Join(dir, "current")
	head, headErr := readSourceHead(headPath, sourceID, relative)
	if headErr != nil && !errors.Is(headErr, os.ErrNotExist) {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, headErr
	}
	if headErr == nil && head.InputDigest == inputHash {
		persisted, err := managedReadManifestAt(sourceRevisionPath(c.state, sourceID, head.Revision))
		if err != nil {
			return api.SourceRegistration{}, managedFrozenSource{}, nil, err
		}
		persistedRevision, err := persisted.Digest()
		if err != nil || persistedRevision != head.Revision || !validSourceRevision(head.InputDigest) {
			return api.SourceRegistration{}, managedFrozenSource{}, nil, errors.New("saved managed source revision is invalid")
		}
		current := managedFrozenSource{Source: persisted.FrozenSource(), manifest: persisted, revision: head.Revision, inputDigest: head.InputDigest}
		current.persistence = c.Persistence(current)
		commit := func() error { return c.commitHead(headPath, sourceID, relative, head, false, head) }
		return api.SourceRegistration{SourceID: sourceID, Revision: head.Revision}, current, commit, nil
	}
	data, err := manifest.Marshal()
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	statePath := sourceRevisionPath(c.state, sourceID, revision)
	if err := managedWriteImmutable(statePath, data); err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	identityPath := filepath.Join(dir, "identity.json")
	identity := persistedSourceIdentity{SourceID: sourceID, Path: relative, InputDigest: inputHash}
	identityData, err := json.Marshal(identity)
	if err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	if old, err := managedRead(identityPath); err == nil {
		var prev persistedSourceIdentity
		if json.Unmarshal(old, &prev) != nil || prev.SourceID != sourceID || prev.Path != relative {
			return api.SourceRegistration{}, managedFrozenSource{}, nil, errors.New("saved source identity conflicts with workspace path")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	} else if err := managedWriteImmutable(identityPath, identityData); err != nil {
		return api.SourceRegistration{}, managedFrozenSource{}, nil, err
	}
	next := persistedSourceHead{SourceID: sourceID, Path: relative, Revision: revision, InputDigest: inputHash}
	missing := errors.Is(headErr, os.ErrNotExist)
	commit := func() error { return c.commitHead(headPath, sourceID, relative, head, missing, next) }
	current := managedFrozenSource{Source: manifest.FrozenSource(), manifest: manifest, revision: revision, inputDigest: inputHash}
	current.persistence = c.Persistence(current)
	return api.SourceRegistration{SourceID: sourceID, Revision: revision}, current, commit, nil
}

func (c *managedSourceCoordinator) Persistence(source config.Source) *runtime.TeamEditPersistence {
	managed, ok := source.(managedFrozenSource)
	if !ok {
		return nil
	}
	return &runtime.TeamEditPersistence{ExpectedSourceRevision: managed.revision, Persist: func(ctx context.Context, expected string, candidate config.Source) (string, error) {
		return c.persist(ctx, managed, expected, candidate)
	}}
}

func (c *managedSourceCoordinator) persist(ctx context.Context, original managedFrozenSource, expected string, candidate config.Source) (string, error) {
	if original.manifest == nil || candidate == nil {
		return "", errors.New("managed source persistence is unavailable")
	}
	if candidate.Name() != original.Name() || candidate.ParentDir() != original.ParentDir() {
		return "", errors.New("managed source edit changed the pinned source identity")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sourceID := original.SourceID()
	dir := filepath.Join(managedSourceDir(c.state), sourceStorageKey(sourceID))
	identityData, err := managedRead(filepath.Join(dir, "identity.json"))
	if err != nil {
		return "", err
	}
	var identity persistedSourceIdentity
	if json.Unmarshal(identityData, &identity) != nil || identity.SourceID != sourceID {
		return "", errors.New("managed source identity is invalid")
	}
	headPath := filepath.Join(dir, "current")
	head, err := readSourceHead(headPath, sourceID, identity.Path)
	if err != nil {
		return "", err
	}
	if head.Revision != expected || head.InputDigest != original.inputDigest {
		return "", errors.New("managed source revision conflict")
	}
	currentInput, err := inputDigest(ctx, original.manifest, c.workspace)
	if err != nil {
		return "", err
	}
	if currentInput != original.inputDigest {
		return "", errors.New("workspace source inputs changed since registration; reconnect before persisting")
	}
	manifest, err := snapshot.Snapshot(ctx, candidate, snapshot.Options{Startup: original.manifest.Startup(), Resolve: pinnedSourceResolver(original.manifest)})
	if err != nil {
		return "", fmt.Errorf("validating persisted team revision: %w", err)
	}
	revision, err := manifest.Digest()
	if err != nil {
		return "", err
	}
	data, err := manifest.Marshal()
	if err != nil {
		return "", err
	}
	newPath := sourceRevisionPath(c.state, sourceID, revision)
	if err := managedWriteImmutable(newPath, data); err != nil {
		return "", err
	}
	next := persistedSourceHead{SourceID: sourceID, Path: identity.Path, Revision: revision, InputDigest: original.inputDigest}
	current := managedFrozenSource{Source: manifest.FrozenSource(), manifest: manifest, revision: revision, inputDigest: original.inputDigest}
	current.persistence = c.Persistence(current)
	if err := c.manager.PublishSourceWithCommit(sourceID, current, func() error { return c.commitHead(headPath, sourceID, identity.Path, head, false, next) }); err != nil {
		return "", err
	}
	return revision, nil
}

func (c *managedSourceCoordinator) commitHead(path, sourceID, relative string, expected persistedSourceHead, missing bool, next persistedSourceHead) error {
	current, err := readSourceHead(path, sourceID, relative)
	if missing {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("managed source revision conflict")
		}
	} else if err != nil || current.Revision != expected.Revision || current.InputDigest != expected.InputDigest {
		return errors.New("managed source revision conflict")
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return managedAtomicWrite(path, data)
}

func managedWriteImmutable(path string, data []byte) error {
	if current, err := managedRead(path); err == nil {
		if string(current) != string(data) {
			return errors.New("immutable managed source revision already exists with different content")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := managedPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".immutable-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			current, readErr := managedRead(path)
			if readErr == nil && string(current) == string(data) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			return errors.New("immutable managed source revision already exists with different content")
		}
		return err
	}
	return nil
}

func (f *apiFlags) restoreManagedSources(ctx context.Context, state, workspace string, manager *server.SessionManager) error {
	dir := managedSourceDir(state)
	if err := managedPrivateDir(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		storageDir := filepath.Join(dir, entry.Name())
		if err := managedPrivateDir(storageDir); err != nil {
			return err
		}
		identityData, err := managedRead(filepath.Join(storageDir, "identity.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var identity persistedSourceIdentity
		if err := json.Unmarshal(identityData, &identity); err != nil || identity.SourceID == "" || identity.Path == "" || !filepath.IsLocal(identity.Path) || entry.Name() != sourceStorageKey(identity.SourceID) {
			return errors.New("managed source identity is invalid")
		}
		if identity.SourceID != sourceIDForPath(identity.Path) {
			if f.manifest == nil || identity.SourceID != f.manifest.Startup().SourceKey {
				return errors.New("managed source identity does not match its workspace path")
			}
			startupName := f.manifest.FrozenSource().Name()
			startupName, pathErr := filepath.EvalSymlinks(startupName)
			if pathErr != nil {
				return errors.New("managed startup source identity does not match its source")
			}
			if identity.Path == "@startup" {
				if withinErr := pathWithinRoot(workspace, startupName); withinErr == nil {
					return errors.New("managed startup source sentinel used for an in-workspace path")
				}
			} else {
				path, relative, pathErr := resolveManagedSourcePath(workspace, workspace, identity.Path)
				if pathErr != nil || relative != identity.Path || startupName != path {
					return errors.New("managed startup source identity does not match its workspace path")
				}
			}
		}
		head, err := readSourceHead(filepath.Join(storageDir, "current"), identity.SourceID, identity.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		manifest, err := managedReadManifestAt(sourceRevisionPath(state, identity.SourceID, head.Revision))
		if err != nil {
			return err
		}
		if manifest.Startup().SourceKey != identity.SourceID || manifest.Startup().Workspace != workspace {
			return errors.New("managed source revision identity mismatch")
		}
		revision, err := manifest.Digest()
		if err != nil || revision != head.Revision {
			return errors.New("managed source revision digest mismatch")
		}
		inputHash := head.InputDigest
		if inputHash == "" {
			inputHash, err = inputDigest(ctx, manifest, workspace)
			if err != nil {
				return err
			}
		}
		if !validSourceRevision(inputHash) {
			return errors.New("managed source input fingerprint is invalid")
		}
		source := managedFrozenSource{Source: manifest.FrozenSource(), manifest: manifest, revision: head.Revision, inputDigest: inputHash}
		coordinator := &managedSourceCoordinator{f: f, state: state, workspace: workspace, manager: manager}
		source.persistence = coordinator.Persistence(source)
		if err := manager.RegisterSource(identity.SourceID, source); err != nil {
			return fmt.Errorf("restoring managed source %s: %w", identity.SourceID, err)
		}
	}
	return nil
}

func (f *apiFlags) initializeManagedStartupSource(ctx context.Context, manager *server.SessionManager) error {
	if f.manifest == nil {
		return nil
	}
	name := f.manifest.FrozenSource().Name()
	if !filepath.IsAbs(name) || strings.Contains(name, "://") {
		return nil
	}
	coordinator := &managedSourceCoordinator{f: f, state: f.managedState, workspace: f.manifest.Startup().Workspace, manager: manager}
	registration, source, commit, err := coordinator.prepareStartup(ctx)
	if err != nil {
		return fmt.Errorf("preparing managed startup source: %w", err)
	}
	if err := manager.PublishSourceWithCommit(registration.SourceID, source, commit); err != nil {
		return fmt.Errorf("publishing managed startup source: %w", err)
	}
	return nil
}
