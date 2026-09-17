package modelsdev

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedSnapshotNeverLoads(t *testing.T) {
	t.Parallel()
	poison := func(context.Context, string) (*Database, string, error) {
		panic("cached snapshot fetched catalog")
	}
	cached := &Database{Providers: map[string]Provider{"cached": {}}}
	authoritative := &Database{Providers: map[string]Provider{"authoritative": {}}}
	store := &Store{cacheFile: "must-not-read/models.json", fetch: poison}
	assert.Same(t, EmbeddedSnapshot(), store.CachedSnapshot())
	assert.Nil(t, store.db)
	assert.Nil(t, store.cacheDB)
	store.cacheDB = cached
	assert.Same(t, cached, store.CachedSnapshot())
	store.db = authoritative
	assert.Same(t, authoritative, store.CachedSnapshot())

	// The normal load holds mu across I/O. A display lookup must not wait.
	store.mu.Lock()
	got := store.CachedSnapshot()
	store.mu.Unlock()
	assert.Same(t, EmbeddedSnapshot(), got)
	assert.Same(t, EmbeddedSnapshot(), (*Store)(nil).CachedSnapshot())
}

func TestLookupModelUsesCanonicalProviderAndBedrockPrefixes(t *testing.T) {
	t.Parallel()
	db := &Database{Providers: map[string]Provider{
		"amazon-bedrock": {Models: map[string]Model{
			"anthropic.claude":    {Name: "Friendly Claude"},
			"us.anthropic.claude": {Name: "Exact US Claude"},
		}},
		"other": {Models: map[string]Model{"anthropic.claude": {Name: "Other Claude"}}},
	}}
	for _, tc := range []struct {
		provider string
		model    string
		name     string
	}{
		{"amazon-bedrock", "anthropic.claude", "Friendly Claude"},
		{"amazon-bedrock", "us.anthropic.claude", "Exact US Claude"},
		{"amazon-bedrock", "eu.anthropic.claude", "Friendly Claude"},
		{"amazon-bedrock", "apac.anthropic.claude", "Friendly Claude"},
		{"amazon-bedrock", "global.anthropic.claude", "Friendly Claude"},
		{"other", "anthropic.claude", "Other Claude"},
		{"other", "eu.anthropic.claude", ""},
		{"unknown", "anthropic.claude", ""},
		{"amazon-bedrock", "unrecognized.anthropic.claude", ""},
	} {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			id := NewID(tc.provider, tc.model)
			got, ok := db.LookupModel(id)
			assert.Equal(t, tc.name != "", ok)
			loaded, err := NewDatabaseStore(db).GetModel(t.Context(), id)
			if tc.name == "" {
				assert.Nil(t, got)
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.name, got.Name)
			assert.Equal(t, loaded, got)
		})
	}
	_, ok := (*Database)(nil).LookupModel(NewID("openai", "gpt-4o"))
	assert.False(t, ok)
	_, ok = db.LookupModel(ID{})
	assert.False(t, ok)
	_, ok = EmbeddedSnapshot().LookupModel(NewID("openai", "gpt-4o"))
	assert.True(t, ok)
}
