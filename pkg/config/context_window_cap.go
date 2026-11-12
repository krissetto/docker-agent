package config

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ValidateContextWindowCaps rejects a model-level context_window above the context
// window the models.dev catalogue declares for the same model. It runs after
// alias resolution, so DisplayOrModel is the name the runtime resolves
// (ModelConfig.ID), and it compares the raw catalogue limit rather than the
// resolved window, which already includes the override. A model the catalogue
// has no limit for accepts any positive value; nothing is capped by guess.
func ValidateContextWindowCaps(ctx context.Context, cfg *latest.Config, store *modelsdev.Store) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		m := cfg.Models[name]
		if m.ContextWindow == nil || m.IsFirstAvailable() {
			continue
		}
		if store == nil {
			return fmt.Errorf("models.%s: cannot check context_window against the models.dev catalogue: the catalogue is unavailable", name)
		}
		id := modelsdev.NewID(catalogueProvider(&m, cfg.Providers), m.DisplayOrModel())
		entry, err := store.GetModel(ctx, id)
		if err != nil || entry == nil || entry.Limit.Context <= 0 {
			continue
		}
		if window := int64(entry.Limit.Context); *m.ContextWindow > window {
			return fmt.Errorf("models.%s: context_window %d exceeds the context window of %s, which is %d tokens in the models.dev catalogue; set models.%s.context_window to at most %d, or remove it to use the catalogue window",
				name, *m.ContextWindow, id, window, name, window)
		}
	}
	return nil
}

// catalogueProvider returns the canonical provider the runtime uses to look a
// model up in the catalogue: a custom provider's underlying provider type when it
// declares one, otherwise the model's own provider name.
func catalogueProvider(m *latest.ModelConfig, providers map[string]latest.ProviderConfig) string {
	name := m.Provider
	if p, ok := providers[m.Provider]; ok {
		name = cmp.Or(p.Provider, m.Provider)
	}
	return modelsdev.CanonicalProviderID(name)
}
