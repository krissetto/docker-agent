package modelpicker

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestDisplayNameCachedCatalogAndFallbacks(t *testing.T) {
	database := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"provider": {Models: map[string]modelsdev.Model{
			"raw-model": {Name: "Friendly model"},
			"latest":    {Name: "Friendly model (latest)"},
			"blank":     {Name: " "},
		}},
		"other":          {Models: map[string]modelsdev.Model{"raw-model": {Name: "Other friendly model"}}},
		"amazon-bedrock": {Models: map[string]modelsdev.Model{"canonical": {Name: "Bedrock friendly"}}},
	}}
	for _, tc := range []struct {
		provider, model, fallback, want string
	}{
		{"provider", "raw-model", "configured", "Friendly model"},
		{"other", "raw-model", "configured", "Other friendly model"},
		{"provider", "latest", "configured", "Friendly model (latest)"},
		{"custom", "raw-model", "configured", "configured"},
		{"provider", "missing", "", "missing"},
		{"provider", "blank", "configured", "configured"},
		{"amazon-bedrock", "us.canonical", "configured", "Bedrock friendly"},
		{"amazon-bedrock", "custom.canonical", "configured", "configured"},
	} {
		require.Equal(t, tc.want, DisplayName(database, tc.provider, tc.model, tc.fallback))
	}
	require.Equal(t, "Friendly model", database.Providers["provider"].Models["raw-model"].Name)
}

func TestDisplayNameEmbeddedFallbackUsesCachedSnapshot(t *testing.T) {
	database := modelsdev.EmbeddedSnapshot()
	for provider, entry := range database.Providers {
		for model, metadata := range entry.Models {
			if metadata.Name != "" {
				require.Equal(t, metadata.Name, DisplayName(nil, provider, model, "configured"))
				return
			}
		}
	}
	t.Fatal("embedded catalog contains no friendly model metadata")
}
