package modelpicker

import (
	"strings"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// DisplayName resolves a friendly model name from an already cached catalog.
// A nil database uses the embedded, process-cached catalog. This function never
// loads a store, reads the filesystem, or fetches metadata. The caller retains
// the original provider/model for routing and identity colors.
func DisplayName(database *modelsdev.Database, provider, model, fallback string) string {
	if database == nil {
		database = modelsdev.EmbeddedSnapshot()
	}
	if entry, ok := database.LookupModel(modelsdev.NewID(provider, model)); ok && strings.TrimSpace(entry.Name) != "" {
		return entry.Name
	}
	if fallback != "" {
		return fallback
	}
	return model
}
