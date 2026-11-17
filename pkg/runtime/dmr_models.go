package runtime

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// dmrModelsTTL is how long a Docker Model Runner /models response (or failure)
// is reused before DMR is queried again. It keeps the model picker snappy on
// repeated opens while still picking up newly-pulled models eventually.
const dmrModelsTTL = 1 * time.Minute

// dmrModelsCache memoizes the result of DMR model discovery, including
// failures, so an unreachable or absent Model Runner is not re-queried on
// every picker open. The mutex only guards the cached fields; the lookup
// itself runs outside the lock, coalesced by the singleflight group so
// concurrent callers share one in-flight request.
type dmrModelsEntry struct {
	models    []dmrmodels.Model
	err       error
	fetchedAt time.Time
}

type dmrModelsCache struct {
	mu      sync.Mutex
	sf      singleflight.Group
	entries map[string]dmrModelsEntry
}

// listDMRModels returns the models available to Docker Model Runner, using
// the runtime's cache when fresh. It returns (nil, nil) when no lister is
// configured (e.g. runtimes built directly in tests), so DMR discovery is
// opt-in via NewLocalRuntime.
func (r *LocalRuntime) listDMRModels(ctx context.Context) ([]dmrmodels.Model, error) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.listDMRModelsForConfig(ctx, config)
}

func (r *LocalRuntime) listDMRModelsForConfig(ctx context.Context, _ *ModelSwitcherConfig) ([]dmrmodels.Model, error) {
	if r.dmrModelLister == nil {
		slog.DebugContext(ctx, "DMR model discovery skipped; no lister configured")
		return nil, nil
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	key := "legacy"
	if env := teamEnvironmentFromContext(ctx); env != nil {
		key = env.SourceID() + "\x00" + env.RevisionID()
	}
	c := &r.dmrModels
	readFresh := func() (models []dmrmodels.Model, ok bool, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		entry, exists := c.entries[key]
		if exists && !entry.fetchedAt.IsZero() && now().Sub(entry.fetchedAt) < dmrModelsTTL {
			return slices.Clone(entry.models), true, entry.err
		}
		return nil, false, nil
	}
	if models, ok, err := readFresh(); ok {
		slog.DebugContext(ctx, "DMR model discovery cache hit", "models", len(models), "error", err)
		return models, err
	}
	start := time.Now()
	v, err, _ := c.sf.Do(key, func() (any, error) {
		if models, ok, err := readFresh(); ok {
			return models, err
		}
		models, err := r.dmrModelLister(ctx)
		if err != nil && ctx.Err() != nil {
			return models, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries == nil {
			c.entries = make(map[string]dmrModelsEntry)
		}
		c.entries[key] = dmrModelsEntry{models: slices.Clone(models), err: err, fetchedAt: now()}
		return models, err
	})
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery fetch completed", "duration", time.Since(start), "error", err)
		return nil, err
	}
	models := v.([]dmrmodels.Model)
	slog.DebugContext(ctx, "DMR model discovery fetch completed", "duration", time.Since(start), "models", len(models))
	return slices.Clone(models), nil
}

// buildDMRChoices builds ModelChoice entries for the models currently pulled
// in Docker Model Runner, deduplicated against the explicitly configured
// models. DMR models aren't part of the models.dev catalog, so without this
// the picker shows nothing for a working local Model Runner. When DMR is not
// installed or unreachable it returns nil.
func (r *LocalRuntime) buildDMRChoices(ctx context.Context) []ModelChoice {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil
	}
	return r.buildDMRChoicesForConfig(ctx, config)
}

func (r *LocalRuntime) buildDMRChoicesForConfig(ctx context.Context, config *ModelSwitcherConfig) []ModelChoice {
	models, err := r.listDMRModelsForConfig(ctx, config)
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery failed, skipping DMR picker entries", "error", err)
		return nil
	}
	if len(models) == 0 {
		return nil
	}

	existingRefs := make(map[string]bool, len(config.Models)*2)
	for name, cfg := range config.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	choices := make([]ModelChoice, 0, len(models))
	for _, model := range models {
		id := model.ID
		// DMR model IDs (e.g. "ai/qwen3:latest") contain slashes; the ref is
		// "dmr/<id>" and ParseModelRef cuts on the first slash, so it
		// round-trips back to provider="dmr", model="<id>".
		ref := "dmr/" + id

		// Resolve catalog metadata before the embedding filter so a model
		// whose models.dev Family is "text-embedding" is filtered even when
		// its ID doesn't contain "embed".
		var meta *modelsdev.Model
		if store := r.modelsStoreForConfig(config); store != nil {
			if m, err := r.modelsStoreForConfig(config).GetModel(ctx, modelsdev.NewID("dmr", id)); err == nil {
				meta = m
			}
		}
		family := ""
		if meta != nil {
			family = meta.Family
		}
		if isEmbeddingModel(family, id) {
			continue
		}

		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		choice := ModelChoice{
			Name:     id,
			Ref:      ref,
			Provider: "dmr",
			Model:    id,
			// Discovered (not explicitly configured), so it groups under the
			// picker's "Other models" separator alongside gateway/catalog
			// entries rather than intermixing with the configured models.
			IsCatalog: true,
		}
		if meta != nil {
			if meta.Name != "" {
				choice.Name = meta.Name
			}
			applyCatalogMetadata(&choice, meta)
		}
		applyDMRMetadata(&choice, model.Metadata)
		choices = append(choices, choice)
	}

	slog.DebugContext(ctx, "Built DMR model choices", "count", len(choices))
	return choices
}

func applyDMRMetadata(choice *ModelChoice, metadata *dmrmodels.Metadata) {
	if metadata == nil {
		return
	}
	if metadata.ContextWindow > 0 {
		choice.ContextLimit = int(metadata.ContextWindow)
	}
	choice.Architecture = metadata.Architecture
	choice.Parameters = metadata.Parameters
	choice.Quantization = metadata.Quantization
	choice.Size = metadata.Size
}

// populateDMRChoices applies DMR-reported metadata to configured choices that
// point at the local Model Runner. Configured models at explicit endpoints must
// not inherit another runner's metadata, and an explicit context_size always
// wins over the runner's reported window.
func (r *LocalRuntime) populateDMRChoices(ctx context.Context, config *ModelSwitcherConfig, choices []ModelChoice) {
	if config.ModelsGateway != "" {
		return
	}
	models, err := r.listDMRModelsForConfig(ctx, config)
	if err != nil {
		return
	}
	metadata := make(map[string]*dmrmodels.Metadata, len(models))
	for _, model := range models {
		metadata[model.ID] = model.Metadata
	}
	for i := range choices {
		choice := &choices[i]
		cfg := config.Models[choice.Ref]
		if cfg.Provider != "dmr" || cfg.BaseURL != "" {
			continue
		}
		meta := metadata[cfg.Model]
		if meta == nil {
			meta = metadata[cfg.Model+":latest"]
		}
		applyDMRMetadata(choice, meta)
		if n := configuredContextWindow(cfg, config.Providers); n > 0 {
			choice.ContextLimit = int(n)
		}
	}
}
