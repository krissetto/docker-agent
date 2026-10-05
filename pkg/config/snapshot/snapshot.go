// Package snapshot freezes the startup configuration of a managed server without
// starting providers, tools, catalogs, or a runtime. Sources must already perform
// any format conversion (for example hcl.NewSource). Remote source reads are the
// only network operations the builder permits through its resolver.
//
// This is a configuration identity, not an execution-environment snapshot:
// add_prompt_files, local/remote skills, workspace context, tool data, environment
// values and credential rotation remain live. Their configured paths/options are
// retained, but their contents are neither read nor fingerprinted here.
//
// Manifests contain full configuration, which can itself contain secrets. They
// must be stored privately by the caller and must never be logged. No environment
// credentials are copied into a manifest.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

const (
	manifestVersion   = 1
	rootKey           = "$root"
	defaultMaxSources = 128
	// Match the team loader's external-agent nesting limit.
	maxExternalDepth = 10
)

// Startup is the server's execution-startup input, not per-session or UI state.
// ModelOverrides and Runtime.Flavors retain their order. Model overrides must be
// reapplied by the real team loader: baking them into YAML loses the loader's
// private per-agent override-policy receipts.
//
// Runtime is the plain config.Config, NOT RuntimeConfig; copying it does not
// initialize environment providers, models.dev stores, or model providers.
// EnvFiles identify ordered, absolute paths only; their values remain live.
// Runtime.EncryptedConfig is preserved but excluded from configuration identity.
type Startup struct {
	Snapshots      bool          `json:"snapshots,omitempty"`
	Workspace      string        `json:"workspace"`
	SourceKey      string        `json:"source_key"`
	ModelOverrides []string      `json:"model_overrides,omitempty"`
	Runtime        config.Config `json:"runtime"`
}

// Options controls provider-free source discovery. Resolve has the same signature
// as teamloader.SourceResolver. It is required only for external references.
// Environment is used only by that resolver for source-fetch authentication.
type Options struct {
	Startup     Startup
	Resolve     func(string, environment.Provider) (config.Source, error)
	Environment environment.Provider
	MaxSources  int
}

type frozenNode struct {
	Name       string   `json:"name"`
	ParentDir  string   `json:"parent_dir,omitempty"`
	YAML       string   `json:"yaml"`
	Encrypted  string   `json:"encrypted_config,omitempty"`
	References []string `json:"references,omitempty"`
}

type document struct {
	Version int                   `json:"version"`
	Startup json.RawMessage       `json:"startup"`
	Sources map[string]frozenNode `json:"sources"`
}

// Manifest is immutable after construction. Its sources never fall back to live
// resolution, even if an external reference is absent from the manifest.
type Manifest struct{ doc document }

// Snapshot parses, migrates, validates and freezes every reachable source before
// any provider/tool initialization. instruction_file and HCL file() dependencies
// are inlined by the ordinary config/source loaders. External SubAgents,
// Handoffs and ForceHandoff references are recursively captured.
func Snapshot(ctx context.Context, root config.Source, opts Options) (*Manifest, error) {
	if root == nil {
		return nil, errors.New("snapshot requires a root source")
	}
	startup, err := normalizeStartup(opts.Startup)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(startup)
	if err != nil {
		return nil, fmt.Errorf("encoding startup configuration: %w", err)
	}
	m := &Manifest{doc: document{Version: manifestVersion, Startup: data, Sources: make(map[string]frozenNode)}}
	limit := opts.MaxSources
	if limit <= 0 {
		limit = defaultMaxSources
	}
	active := make(map[string]bool)
	var visit func(string, config.Source, int) error
	visit = func(key string, source config.Source, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := source.Name() + "\x00" + source.ParentDir()
		if active[identity] {
			return errors.New("configuration source graph contains a cycle")
		}
		if _, exists := m.doc.Sources[key]; exists {
			return nil
		}
		if depth > maxExternalDepth {
			return fmt.Errorf("configuration source graph exceeds maximum external depth (%d)", maxExternalDepth)
		}
		if len(m.doc.Sources) >= limit {
			return fmt.Errorf("configuration source graph exceeds source limit (%d)", limit)
		}
		active[identity] = true
		defer delete(active, identity)
		cfg, err := config.Load(ctx, source, config.WithFlavors(startup.Runtime.Flavors...))
		if err != nil {
			return fmt.Errorf("loading configuration snapshot: %w", err)
		}
		refs := externalReferences(cfg)
		// Load has migrated and resolved these fields. Keeping them would
		// reapply flavors (including append patches) and duplicate toolsets.
		cfg.Version = latest.Version
		cfg.Flavors = nil
		for i := range cfg.Agents {
			cfg.Agents[i].UseToolsets = nil
			cfg.Agents[i].UseCommands = nil
			cfg.Agents[i].UseSkills = nil
		}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("encoding frozen configuration: %w", err)
		}
		node := frozenNode{Name: source.Name(), ParentDir: source.ParentDir(), YAML: string(data), References: refs}
		if encrypted, ok := source.(config.EncryptedConfigSource); ok {
			node.Encrypted = encrypted.EncryptedConfig()
		}
		// Fail explicitly rather than persisting a config that cannot be
		// reloaded identically (including exotic custom YAML field types).
		if err := validateNode(ctx, node); err != nil {
			return err
		}
		m.doc.Sources[key] = node
		for _, ref := range refs {
			if opts.Resolve == nil {
				return errors.New("external configuration references require a snapshot resolver")
			}
			// Check active edges even when the node has already been inserted.
			if existing, ok := m.doc.Sources[ref]; ok {
				if active[existing.Name+"\x00"+existing.ParentDir] {
					return errors.New("configuration source graph contains a cycle")
				}
				continue
			}
			external, err := opts.Resolve(ref, opts.Environment)
			if err != nil {
				return fmt.Errorf("resolving external snapshot source: %w", err)
			}
			if external == nil {
				return errors.New("external snapshot resolver returned a nil source")
			}
			if err := visit(ref, external, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(rootKey, root, 0); err != nil {
		return nil, err
	}
	if err := validateGraphDepth(m.doc.Sources); err != nil {
		return nil, err
	}
	return m, nil
}

func normalizeStartup(startup Startup) (Startup, error) {
	// JSON round-trip also isolates nested hook/model maps from the caller.
	data, err := json.Marshal(startup)
	if err != nil {
		return Startup{}, fmt.Errorf("encoding startup configuration: %w", err)
	}
	if err := json.Unmarshal(data, &startup); err != nil {
		return Startup{}, err
	}
	if startup.Workspace == "" {
		startup.Workspace = startup.Runtime.WorkingDir
	}
	if startup.Workspace == "" {
		return Startup{}, errors.New("snapshot requires a workspace")
	}
	startup.Workspace, err = filepath.Abs(startup.Workspace)
	if err != nil {
		return Startup{}, fmt.Errorf("resolving snapshot workspace: %w", err)
	}
	if startup.Runtime.WorkingDir != "" {
		dir, err := filepath.Abs(startup.Runtime.WorkingDir)
		if err != nil {
			return Startup{}, err
		}
		if dir != startup.Workspace {
			return Startup{}, errors.New("snapshot workspace and runtime working directory differ")
		}
	}
	startup.Runtime.WorkingDir = startup.Workspace
	startup.Runtime.EnvFiles, err = environment.AbsolutePaths(startup.Workspace, startup.Runtime.EnvFiles)
	if err != nil {
		return Startup{}, fmt.Errorf("resolving snapshot env-file paths: %w", err)
	}
	return startup, nil
}

func externalReferences(cfg *latest.Config) []string {
	var refs []string
	for _, agent := range cfg.Agents {
		all := slices.Concat(agent.SubAgents, agent.Handoffs, []string{agent.ForceHandoff})
		for _, ref := range all {
			if config.IsExternalReference(ref) {
				_, external := config.ParseExternalAgentRef(ref)
				if !slices.Contains(refs, external) {
					refs = append(refs, external)
				}
			}
		}
	}
	return refs
}

// FrozenSource returns canonical YAML with the original name/directory and
// encryption capability. Pass it directly to config.Load/teamloader.Load: do
// NOT wrap it in hcl.NewSource merely because its original name ends in .hcl.
func (m *Manifest) FrozenSource() config.Source { return frozenSource{m.doc.Sources[rootKey]} }

// SourceResolver is suitable for teamloader.WithSourceResolver. Its environment
// argument is intentionally ignored; no files or remote sources are read.
func (m *Manifest) SourceResolver(ref string, _ environment.Provider) (config.Source, error) {
	node, ok := m.doc.Sources[ref]
	if !ok || ref == rootKey {
		return nil, errors.New("external source is absent from the frozen configuration graph")
	}
	return frozenSource{node}, nil
}

type frozenSource struct{ node frozenNode }

func (s frozenSource) Name() string            { return s.node.Name }
func (s frozenSource) ParentDir() string       { return s.node.ParentDir }
func (s frozenSource) EncryptedConfig() string { return s.node.Encrypted }
func (s frozenSource) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(s.node.YAML), nil
}

// Startup returns an independent copy of the frozen startup inputs.
func (m *Manifest) Startup() Startup {
	var startup Startup
	// Construction and Parse validate this immutable JSON.
	_ = json.Unmarshal(m.doc.Startup, &startup)
	return startup
}

// Marshal encodes a manifest for private, immutable storage by the caller.
func (m *Manifest) Marshal() ([]byte, error) { return json.Marshal(m.doc) }

// Digest hashes canonical startup inputs and source graph hashes. Credentials
// from environment providers/env files are never read. Opaque encrypted config
// transport metadata is retained in the manifest but is not identity input.
func (m *Manifest) Digest() (string, error) {
	startup := m.Startup()
	startup.Runtime.EncryptedConfig = ""
	type sourceIdentity struct {
		Name       string   `json:"name"`
		ParentDir  string   `json:"parent_dir,omitempty"`
		Hash       string   `json:"sha256"`
		References []string `json:"references,omitempty"`
	}
	identity := struct {
		Version int                       `json:"version"`
		Startup Startup                   `json:"startup"`
		Sources map[string]sourceIdentity `json:"sources"`
	}{manifestVersion, startup, make(map[string]sourceIdentity, len(m.doc.Sources))}
	for key, node := range m.doc.Sources {
		identity.Sources[key] = sourceIdentity{node.Name, node.ParentDir, hash([]byte(node.YAML)), node.References}
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return hash(data), nil
}

func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Parse restores and validates a stored manifest without consulting the original
// source graph or workspace. Private storage/integrity remains caller-owned.
func Parse(data []byte) (*Manifest, error) {
	var doc document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding configuration manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing data in configuration manifest")
	}
	if doc.Version != manifestVersion {
		return nil, fmt.Errorf("unsupported configuration manifest version %d", doc.Version)
	}
	if _, ok := doc.Sources[rootKey]; !ok {
		return nil, errors.New("configuration manifest has no root source")
	}
	var startup Startup
	if err := json.Unmarshal(doc.Startup, &startup); err != nil {
		return nil, errors.New("invalid manifest startup configuration")
	}
	if startup.Workspace == "" || !filepath.IsAbs(startup.Workspace) || startup.Runtime.WorkingDir != startup.Workspace {
		return nil, errors.New("invalid manifest workspace")
	}
	// Re-encode startup so formatting of input JSON cannot change identity.
	doc.Startup, _ = json.Marshal(startup)
	active, visited := map[string]bool{}, map[string]bool{}
	var visit func(string, int) error
	visit = func(key string, depth int) error {
		if active[key] {
			return errors.New("configuration manifest contains a cycle")
		}
		if depth > maxExternalDepth {
			return errors.New("configuration manifest exceeds external depth limit")
		}
		if visited[key] {
			return nil
		}
		node, ok := doc.Sources[key]
		if !ok {
			return errors.New("configuration manifest has an unresolved external reference")
		}
		if err := validateNode(context.Background(), node); err != nil {
			return err
		}
		active[key], visited[key] = true, true
		for _, ref := range node.References {
			if err := visit(ref, depth+1); err != nil {
				return err
			}
		}
		delete(active, key)
		return nil
	}
	if err := visit(rootKey, 0); err != nil {
		return nil, err
	}
	if len(visited) != len(doc.Sources) {
		return nil, errors.New("configuration manifest contains unreachable sources")
	}
	if err := validateGraphDepth(doc.Sources); err != nil {
		return nil, err
	}
	return &Manifest{doc: doc}, nil
}

// Shared subgraphs may first be discovered by a short path and subsequently by
// a longer one. Memoize subtree heights, not just visited nodes, to enforce the
// runtime depth limit on every possible path without expanding the whole DAG.
func validateGraphDepth(nodes map[string]frozenNode) error {
	heights := make(map[string]int)
	var height func(string) int
	height = func(key string) int {
		if h, ok := heights[key]; ok {
			return h
		}
		h := 0
		for _, ref := range nodes[key].References {
			h = max(h, 1+height(ref))
		}
		heights[key] = h
		return h
	}
	// Both callers have already rejected cycles and missing nodes.
	if height(rootKey) > maxExternalDepth {
		return fmt.Errorf("configuration source graph exceeds maximum external depth (%d)", maxExternalDepth)
	}
	return nil
}

func validateNode(ctx context.Context, node frozenNode) error {
	// Remove the directory while checking, so a malformed manifest cannot
	// smuggle an instruction_file read into offline restore/validation.
	check := node
	check.ParentDir = ""
	cfg, err := config.Load(ctx, frozenSource{check})
	if err != nil {
		return fmt.Errorf("validating frozen configuration: %w", err)
	}
	if cfg.Version != latest.Version || len(cfg.Flavors) != 0 {
		return errors.New("frozen configuration is not canonical")
	}
	if !slices.Equal(externalReferences(cfg), node.References) {
		return errors.New("frozen configuration references do not match its graph")
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-encoding frozen configuration: %w", err)
	}
	if string(data) != node.YAML {
		return errors.New("configuration cannot be frozen without changing reload semantics")
	}
	return nil
}
