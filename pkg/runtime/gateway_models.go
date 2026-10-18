package runtime

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsgateway"
)

// gatewayModelsTTL is how long a gateway /v1/models response (or failure)
// is reused before the gateway is queried again. It keeps the model picker
// snappy on repeated opens while still picking up gateway changes
// eventually.
const gatewayModelsTTL = 5 * time.Minute

// gatewayModelsCache memoizes the result of the gateway model discovery,
// including failures, so an unsupported or slow gateway is not re-queried
// on every picker open. The mutex only guards the cached fields; the
// network fetch itself runs outside the lock, coalesced by the
// singleflight group so concurrent callers share one in-flight request
// instead of stalling on each other.
type gatewayModelsEntry struct {
	ids       []string
	err       error
	fetchedAt time.Time
}

type gatewayModelsCache struct {
	mu      sync.Mutex
	sf      singleflight.Group
	entries map[string]gatewayModelsEntry
}

// listGatewayModels returns the model IDs served by the configured models
// gateway, using the runtime's cache when fresh.
func (r *LocalRuntime) listGatewayModels(ctx context.Context) ([]string, error) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.listGatewayModelsForConfig(ctx, config)
}

func (r *LocalRuntime) listGatewayModelsForConfig(ctx context.Context, config *ModelSwitcherConfig) ([]string, error) {
	if config == nil {
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
	c := &r.gatewayModels
	readFresh := func() (ids []string, ok bool, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		entry, exists := c.entries[key]
		if exists && !entry.fetchedAt.IsZero() && now().Sub(entry.fetchedAt) < gatewayModelsTTL {
			return slices.Clone(entry.ids), true, entry.err
		}
		return nil, false, nil
	}
	if ids, ok, err := readFresh(); ok {
		return ids, err
	}
	start := time.Now()
	v, err, _ := c.sf.Do(key, func() (any, error) {
		if ids, ok, err := readFresh(); ok {
			return ids, err
		}
		ids, err := modelsgateway.ListModels(ctx, config.ModelsGateway, config.EnvProvider)
		if err != nil && ctx.Err() != nil {
			return ids, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries == nil {
			c.entries = make(map[string]gatewayModelsEntry)
		}
		c.entries[key] = gatewayModelsEntry{ids: slices.Clone(ids), err: err, fetchedAt: now()}
		return ids, err
	})
	if err != nil {
		slog.DebugContext(ctx, "Gateway model discovery fetch completed", "duration", time.Since(start), "error", err)
		return nil, err
	}
	ids := v.([]string)
	slog.DebugContext(ctx, "Gateway model discovery fetch completed", "duration", time.Since(start), "models", len(ids))
	return slices.Clone(ids), nil
}

// buildGatewayChoices builds ModelChoice entries from the models served by
// the configured gateway, deduplicated against the explicitly configured
// models. The second return value reports whether discovery succeeded;
// when false (gateway unreachable, /v1/models unsupported, or an empty
// list that gives no usable signal) callers should fall back to the
// models.dev catalog.
func (r *LocalRuntime) buildGatewayChoices(ctx context.Context) ([]ModelChoice, bool) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, false
	}
	return r.buildGatewayChoicesForConfig(ctx, config)
}

func (r *LocalRuntime) buildGatewayChoicesForConfig(ctx context.Context, config *ModelSwitcherConfig) ([]ModelChoice, bool) {
	ids, err := r.listGatewayModelsForConfig(ctx, config)
	if err != nil {
		slog.DebugContext(ctx, "Gateway model discovery failed, falling back to catalog", "error", err)
		return nil, false
	}
	if len(ids) == 0 {
		slog.DebugContext(ctx, "Gateway returned no models, falling back to catalog")
		return nil, false
	}

	existingRefs := make(map[string]bool, len(config.Models)*2)
	for name, cfg := range config.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	// The metadata callback memoizes full catalog entries so the display
	// name/pricing enrichment below reuses the lookup NormalizeIDs already
	// performed for its embedding/modality filters.
	metas := make(map[string]*modelsdev.Model)
	lookup := func(ctx context.Context, prov, model string) (modelsgateway.Metadata, bool) {
		if r.modelsStoreForConfig(config) == nil {
			return modelsgateway.Metadata{}, false
		}
		m, err := r.modelsStoreForConfig(config).GetModel(ctx, modelsdev.NewID(prov, model))
		if err != nil {
			return modelsgateway.Metadata{}, false
		}
		metas[prov+"/"+model] = m
		return modelsgateway.Metadata{Family: m.Family, OutputModalities: m.Modalities.Output}, true
	}

	refs := modelsgateway.NormalizeIDs(ctx, ids, lookup)

	choices := make([]ModelChoice, 0, len(refs))
	for _, mr := range refs {
		ref := mr.String()
		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		choice := ModelChoice{
			Name:      mr.Model,
			Ref:       ref,
			Provider:  mr.Provider,
			Model:     mr.Model,
			IsCatalog: true,
		}
		if meta := metas[ref]; meta != nil {
			if meta.Name != "" {
				choice.Name = meta.Name
			}
			applyCatalogMetadata(&choice, meta)
		}
		choices = append(choices, choice)
	}

	slog.DebugContext(ctx, "Built gateway model choices", "count", len(choices))
	return choices, true
}
