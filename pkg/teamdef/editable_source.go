package teamdef

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// BuildValidator validates a candidate source using the caller's real build
// policy. It is intentionally injected so teamdef does not depend on teamloader.
type BuildValidator func(context.Context, config.Source) error

// EditAuthorityMode selects a constructor-pinned editor policy.
type EditAuthorityMode string

const (
	EditAuthorityRestricted EditAuthorityMode = "restricted"
	EditAuthorityFull       EditAuthorityMode = "full"
	EditAuthorityOff        EditAuthorityMode = "off"
)

// EditableSourceOptions carries trusted caller capabilities. Never populate
// authority, allowlists, or paths from tool arguments or editable YAML.
type EditableSourceOptions struct {
	Source         config.Source
	BuildValidator BuildValidator
	ExportRoot     string
	// WritablePath is an explicit caller-trusted capability. It must be the
	// exact existing regular local file represented by Source.
	WritablePath string
	Authority    EditAuthorityMode
	// AllowedAgentIDs restricts full-mode callers when non-nil. Nil means the
	// trusted full-edit grant applies source-wide; an empty slice grants none.
	AllowedAgentIDs []string
}

// Revision identifies an immutable authored-source revision. Its ID is an
// opaque CAS token and is safe to expose; it contains no source text.
type Revision struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

// EditableSource owns immutable YAML revision bytes and their original source
// identity. Raw source is only made available through SourceForLoad, for trusted
// config loaders; it is never included in inspect results or revision tokens.
const maxEditableRevisions = 32

type EditableSource struct {
	mu sync.RWMutex

	name            string
	parentDir       string
	writable        string
	mode            os.FileMode
	exportRoot      string
	encryptedConfig string

	current       Revision
	revisions     map[string][]byte
	prepared      map[string]Revision
	persistedHash [32]byte
}

// NewEditableSource creates an editable snapshot only for a direct root
// latest-version YAML source. Older configs, source graphs, flavors, YAML
// aliases, and ambiguous source structures are rejected rather than rewritten.
func NewEditableSource(ctx context.Context, source config.Source) (*EditableSource, error) {
	return newEditableSource(ctx, source, "", "", EditAuthorityRestricted)
}

// NewEditableSourceWithExportRoot captures a source with explicit authority to
// export revisions beneath exportRoot. The root is never inferred from CWD.
func NewEditableSourceWithExportRoot(ctx context.Context, source config.Source, exportRoot string) (*EditableSource, error) {
	if strings.TrimSpace(exportRoot) == "" {
		return nil, errors.New("explicit export root is required")
	}
	return newEditableSource(ctx, source, exportRoot, "", EditAuthorityRestricted)
}

// NewEditableSourceWithOptions adds explicit persistence/export capabilities to
// an in-memory source. Callers must never pass these options from tool input.
func NewEditableSourceWithOptions(ctx context.Context, source config.Source, opts EditableSourceOptions) (*EditableSource, error) {
	mode := opts.Authority
	if mode == "" {
		mode = EditAuthorityRestricted
	}
	return newEditableSource(ctx, source, opts.ExportRoot, opts.WritablePath, mode)
}

func newEditableSource(ctx context.Context, source config.Source, exportRoot, writablePath string, mode EditAuthorityMode) (*EditableSource, error) {
	if source == nil {
		return nil, errors.New("editable source is required")
	}
	name := source.Name()
	remote := strings.Contains(name, "://")
	extensionPath := name
	if remote {
		parsed, parseErr := url.Parse(name)
		if parseErr != nil {
			return nil, errors.New("controlled editing requires a direct YAML source")
		}
		extensionPath = parsed.Path
	}
	if filepath.Ext(extensionPath) == "" {
		return nil, errors.New("controlled editing requires a direct YAML source")
	}
	if filepath.IsAbs(name) && source.ParentDir() == "" && !remote {
		return nil, errors.New("controlled editing requires a direct YAML source with explicit local directory provenance")
	}
	if source.ParentDir() != "" {
		if _, err := editableLocalPath(source); err != nil {
			return nil, errors.New("controlled editing requires a direct source path within its declared directory")
		}
	}
	ext := strings.ToLower(filepath.Ext(extensionPath))
	if ext != ".yaml" && ext != ".yml" {
		return nil, errors.New("controlled editing requires a direct YAML source")
	}
	var canonicalSourcePath string
	if writablePath != "" {
		if remote {
			return nil, errors.New("remote editable sources cannot be writable")
		}
		var pathErr error
		canonicalSourcePath, pathErr = editableLocalPath(source)
		if pathErr != nil {
			return nil, errors.New("writable capability requires a direct local source path")
		}
		requested, absErr := filepath.Abs(writablePath)
		if absErr != nil {
			return nil, errors.New("writable path must exactly match the direct source file")
		}
		resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(requested))
		if parentErr != nil || filepath.Join(resolvedParent, filepath.Base(requested)) != canonicalSourcePath {
			return nil, errors.New("writable path must exactly match the direct source file")
		}
		info, statErr := os.Lstat(canonicalSourcePath)
		if statErr != nil || !info.Mode().IsRegular() {
			return nil, errors.New("writable source must be an existing regular file, not a symlink or special file")
		}
	}
	data, err := source.Read(ctx)
	if err != nil {
		return nil, errors.New("reading editable source failed")
	}
	var documentErr error
	switch mode {
	case EditAuthorityFull:
		documentErr = validateFullEditableDocument(data)
	case EditAuthorityRestricted:
		documentErr = validateEditableDocument(data)
	default:
		return nil, errors.New("editor authority is disabled")
	}
	if documentErr != nil {
		return nil, documentErr
	}

	editable := &EditableSource{name: name, parentDir: source.ParentDir(), revisions: make(map[string][]byte), prepared: make(map[string]Revision), exportRoot: exportRoot}
	if encrypted, ok := source.(config.EncryptedConfigSource); ok {
		editable.encryptedConfig = encrypted.EncryptedConfig()
	}
	if writablePath != "" {
		info, err := os.Lstat(canonicalSourcePath)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("writable source is no longer a regular file")
		}
		current, err := os.ReadFile(canonicalSourcePath)
		if err != nil || !bytes.Equal(current, data) {
			return nil, errors.New("writable source does not match the captured source bytes")
		}
		editable.writable, editable.mode, editable.persistedHash = canonicalSourcePath, info.Mode().Perm(), sha256.Sum256(data)
	}
	editable.current, err = revisionFor(1)
	if err != nil {
		return nil, err
	}
	editable.revisions[editable.current.ID] = bytes.Clone(data)
	return editable, nil
}

func editableLocalPath(source config.Source) (string, error) {
	name := source.Name()
	if strings.Contains(name, "://") || source.ParentDir() == "" {
		return "", errors.New("source is not a writable local file")
	}
	path, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.Abs(source.ParentDir())
	if err != nil {
		return "", err
	}
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(parent) {
		return "", errors.New("source path escapes its declared directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolving editable source directory: %w", err)
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

func revisionFor(generation uint64) (Revision, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return Revision{}, fmt.Errorf("creating editable revision token: %w", err)
	}
	return Revision{ID: fmt.Sprintf("%d:%s", generation, hex.EncodeToString(token)), Generation: generation}, nil
}

func (s *EditableSource) prepare(base Revision, data []byte) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current.ID != base.ID || s.current.Generation != base.Generation {
		return Revision{}, errors.New("editable source revision conflict")
	}
	revision, err := revisionFor(base.Generation + 1)
	if err != nil {
		return Revision{}, err
	}
	for s.revisions[revision.ID] != nil {
		revision, err = revisionFor(base.Generation + 1)
		if err != nil {
			return Revision{}, err
		}
	}
	s.revisions[revision.ID] = bytes.Clone(data)
	s.prepared[revision.ID] = revision
	return revision, nil
}

func (s *EditableSource) discardPrepared(revision Revision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepared, ok := s.prepared[revision.ID]; ok && prepared == revision {
		delete(s.prepared, revision.ID)
		delete(s.revisions, revision.ID)
	}
}

func (s *EditableSource) publishPrepared(base, revision Revision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared, ok := s.prepared[revision.ID]
	if !ok || prepared != revision || s.current != base || revision.Generation != base.Generation+1 {
		return errors.New("prepared source publication conflict")
	}
	delete(s.prepared, revision.ID)
	s.current = revision
	if len(s.revisions) > maxEditableRevisions {
		evictOldEditableRevisions(s.revisions, s.current.ID)
	}
	return nil
}

func (s *EditableSource) CurrentRevision() Revision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// SourceForLoad returns an immutable in-memory config.Source for trusted
// validation/build code. Each Read returns independent bytes. Never expose its
// contents in tool responses or logs.
func (s *EditableSource) SourceForLoad(revision Revision) (config.Source, error) {
	s.mu.RLock()
	data, ok := s.revisions[revision.ID]
	if ok {
		ok = revision.Generation != 0 && generationFromID(revision.ID) == revision.Generation
	}
	name, parent := s.name, s.parentDir
	copyData := bytes.Clone(data)
	encrypted := s.encryptedConfig
	s.mu.RUnlock()
	if !ok {
		return nil, errors.New("unknown editable source revision")
	}
	return inMemorySource{name: name, parentDir: parent, data: copyData, encryptedConfig: encrypted}, nil
}

func generationFromID(id string) uint64 {
	generation, _, _ := strings.Cut(id, ":")
	var n uint64
	_, _ = fmt.Sscan(generation, &n)
	return n
}

type inMemorySource struct {
	name, parentDir string
	data            []byte
	encryptedConfig string
}

func (s inMemorySource) Name() string                         { return s.name }
func (s inMemorySource) ParentDir() string                    { return s.parentDir }
func (s inMemorySource) Read(context.Context) ([]byte, error) { return bytes.Clone(s.data), nil }
func (s inMemorySource) EncryptedConfig() string              { return s.encryptedConfig }

// Persist replaces the original writable file only if it still matches
// the bytes captured at editor creation. The final hash recheck narrows but
// cannot eliminate races with non-cooperating writers between check and rename.
func (s *EditableSource) Persist(revision Revision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writable == "" {
		return errors.New("editable source has no original writable local file")
	}
	if revision.ID != s.current.ID || revision.Generation != s.current.Generation {
		return errors.New("only the current editable source revision can be persisted")
	}
	info, err := os.Lstat(s.writable)
	if err != nil {
		return fmt.Errorf("checking original editable source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("original editable source is no longer a regular file")
	}
	current, err := os.ReadFile(s.writable)
	if err != nil {
		return fmt.Errorf("reading original editable source: %w", err)
	}
	if sha256.Sum256(current) != s.persistedHash {
		return errors.New("original editable source changed externally; refusing to overwrite")
	}
	data, exists := s.revisions[revision.ID]
	if !exists || revision.Generation == 0 || generationFromID(revision.ID) != revision.Generation {
		return errors.New("unknown editable source revision")
	}
	if err := replaceIfUnchanged(s.writable, data, s.mode, s.persistedHash); err != nil {
		return err
	}
	s.persistedHash = sha256.Sum256(data)
	return nil
}

// ExportNew writes the requested revision to a new path without clobbering an
// existing entry. The resulting file is created with 0600 permissions.
func (s *EditableSource) ExportNew(revision Revision, path string) error {
	if path == "" {
		return errors.New("export path is required")
	}
	if s.exportRoot == "" {
		return errors.New("export requires an explicit trusted export root")
	}
	root, err := filepath.Abs(s.exportRoot)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("export path must be inside the configured export root")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("checking export root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("export root must be a real directory")
	}
	for current := filepath.Dir(target); current != root; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return fmt.Errorf("checking export directory: %w", statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("export directory path must contain only real non-symlink directories")
		}
		if filepath.Dir(current) == current {
			return errors.New("export path is outside configured root")
		}
	}
	s.mu.RLock()
	data, ok := s.revisions[revision.ID]
	copyData := bytes.Clone(data)
	s.mu.RUnlock()
	if !ok || revision.Generation == 0 || generationFromID(revision.ID) != revision.Generation {
		return errors.New("unknown editable source revision")
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("opening export root: %w", err)
	}
	defer rootFS.Close()
	file, err := rootFS.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating exported editable source: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = rootFS.Remove(rel)
		return fmt.Errorf("setting exported editable source permissions: %w", err)
	}
	if _, err := io.Copy(file, bytes.NewReader(copyData)); err != nil {
		file.Close()
		_ = rootFS.Remove(rel)
		return fmt.Errorf("writing exported editable source: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = rootFS.Remove(rel)
		return fmt.Errorf("syncing exported editable source: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = rootFS.Remove(rel)
		return fmt.Errorf("closing exported editable source: %w", err)
	}
	return nil
}

func validateEditableDocument(data []byte) error {
	var doc yamlDocument
	if err := decodeYAMLNode(data, &doc); err != nil {
		return fmt.Errorf("parsing editable YAML source: %w", err)
	}
	if doc.Root == nil || doc.Root.Kind != yamlMappingNode {
		return errors.New("controlled editing requires a single root YAML mapping")
	}
	if err := rejectAmbiguousYAML(doc.Root); err != nil {
		return err
	}
	version := ""
	if versionNode, ok := mappingValue(doc.Root, "version"); ok {
		if versionNode.Kind != yamlScalarNode || versionNode.ShortTag() != "String" {
			return errors.New("version must be a string scalar")
		}
		version = versionNode.Value
	}
	if version != "" && version != latest.Version {
		return fmt.Errorf("controlled editing supports latest config version %s only (got %q)", latest.Version, version)
	}
	for _, unsupported := range []string{"flavors", "include"} {
		if _, exists := mappingValue(doc.Root, unsupported); exists {
			return fmt.Errorf("controlled editing does not support included source graphs or %s", unsupported)
		}
	}
	agents, ok := mappingValue(doc.Root, "agents")
	if !ok || agents.Kind != yamlMappingNode || len(agents.Content) < 2 {
		return errors.New("controlled editing requires a non-empty direct agents mapping")
	}
	for i := 1; i < len(agents.Content); i += 2 {
		if agents.Content[i].Kind != yamlMappingNode {
			return fmt.Errorf("controlled editing requires each direct agent %q to be a mapping", agents.Content[i-1].Value)
		}
		for _, unsupported := range []string{"instruction_file", "use_toolsets", "use_commands", "use_skills"} {
			if _, exists := mappingValue(agents.Content[i], unsupported); exists {
				return fmt.Errorf("controlled editing does not support agent %q with %s provenance", agents.Content[i-1].Value, unsupported)
			}
		}
	}
	return nil
}

// replaceIfUnchanged writes a sibling temp file, rechecks the original hash
// immediately before rename, and atomically publishes it. The hash check is
// not a kernel CAS: an uncoordinated writer may still win after the recheck.
func replaceIfUnchanged(path string, data []byte, mode os.FileMode, expected [32]byte) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening editable source directory: %w", err)
	}
	defer root.Close()
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("creating editable temp name: %w", err)
	}
	tmp := fmt.Sprintf(".team-edit-%x", token)
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating editable temp file: %w", err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing editable temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("syncing editable temp file: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("setting editable source mode: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing editable temp file: %w", err)
	}
	current, err := root.ReadFile(base)
	if err != nil {
		return fmt.Errorf("rechecking editable source before replace: %w", err)
	}
	if sha256.Sum256(current) != expected {
		return errors.New("original editable source changed externally; refusing to overwrite")
	}
	if err := root.Rename(tmp, base); err != nil {
		return fmt.Errorf("atomically replacing editable source: %w", err)
	}
	removeTemp = false
	return nil
}

func evictOldEditableRevisions(revisions map[string][]byte, currentID string) {
	for len(revisions) > maxEditableRevisions {
		var oldestID string
		oldestGeneration := ^uint64(0)
		for id := range revisions {
			if id == currentID {
				continue
			}
			generation := generationFromID(id)
			if generation < oldestGeneration {
				oldestID, oldestGeneration = id, generation
			}
		}
		if oldestID == "" {
			return
		}
		delete(revisions, oldestID)
	}
}
