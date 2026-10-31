package provider

import (
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// Alias defines the configuration for a provider alias.
type Alias struct {
	APIType string // The actual API type to use (openai, anthropic, etc.)
	// BaseURL is the default base URL for the provider. It may contain
	// ${VAR}/${env.VAR} references (e.g. ${CLOUDFLARE_ACCOUNT_ID}) that are
	// resolved from the runtime environment when the provider is built.
	BaseURL     string
	TokenEnvVar string // Environment variable name for the API token
}

// CoreProviders lists all natively implemented provider types.
// These are the provider types that have direct implementations (not aliases).
var CoreProviders = []string{
	"openai",
	"anthropic",
	"google",
	"dmr",
	"amazon-bedrock",
}

// legacyAliasNames are built-in provider names this package keeps exposing
// under their shipped spelling. The embedded catalog uses the models.dev IDs
// for the same services (see [modelsdev.CanonicalProviderID]).
var legacyAliasNames = []string{"fireworks", "together", "moonshot", "opencode-zen"}

// aliasNameByCatalogID maps a catalog provider ID back to the name this
// package exposes it under, for the legacy spellings above.
var aliasNameByCatalogID = func() map[string]string {
	names := make(map[string]string, len(legacyAliasNames))
	for _, legacy := range legacyAliasNames {
		names[modelsdev.CanonicalProviderID(legacy)] = legacy
	}
	return names
}()

// Aliases maps provider names to their corresponding configurations. The
// table is derived from the embedded provider catalog, which is the single
// source for built-in endpoints and credential variables.
//
// Most consumers should call [LookupAlias] for a single lookup or [EachAlias]
// to iterate, both of which keep the rest of the codebase decoupled from this
// concrete map. Direct mutation of Aliases is not supported.
var Aliases = aliasesFromCatalog()

func aliasesFromCatalog() map[string]Alias {
	aliases := make(map[string]Alias)
	for _, p := range catalog.Ordered() {
		if slices.Contains(CoreProviders, p.Name) {
			continue
		}
		name := p.Name
		if legacy, ok := aliasNameByCatalogID[p.Name]; ok {
			name = legacy
		}
		alias := Alias{APIType: p.APIType, BaseURL: p.BaseURL}
		if len(p.TokenEnvVars) > 0 {
			alias.TokenEnvVar = p.TokenEnvVars[0]
		}
		aliases[name] = alias
	}
	return aliases
}

// LookupAlias returns the Alias registered for the given name (if any).
// Lookup is case-sensitive; callers that need case-insensitive matching
// should normalise the name first (e.g. [strings.ToLower]).
func LookupAlias(name string) (Alias, bool) {
	if alias, ok := Aliases[name]; ok {
		return alias, true
	}
	// Models.dev spellings of the legacy names resolve to the same alias.
	if legacy, ok := aliasNameByCatalogID[name]; ok {
		alias, found := Aliases[legacy]
		return alias, found
	}
	return Alias{}, false
}

// EachAlias returns an iterator over every registered (name, Alias) pair.
// Iteration order is not guaranteed; callers that need a deterministic order
// should sort by name.
func EachAlias() iter.Seq2[string, Alias] {
	return func(yield func(string, Alias) bool) {
		for name, alias := range Aliases {
			if !yield(name, alias) {
				return
			}
		}
	}
}

// AllProviders returns all known provider names (core providers + aliases),
// sorted for deterministic output.
func AllProviders() []string {
	providers := slices.Concat(CoreProviders, slices.Collect(maps.Keys(Aliases)))
	slices.Sort(providers)
	return providers
}

// IsKnownProvider returns true if the provider name is a core provider or an alias.
func IsKnownProvider(name string) bool {
	if slices.Contains(CoreProviders, strings.ToLower(name)) {
		return true
	}
	_, exists := LookupAlias(strings.ToLower(name))
	return exists
}

// CatalogProviders returns the list of provider names that should be shown in the model catalog.
// This includes core providers and aliases that have a defined BaseURL (self-contained endpoints).
// Aliases without a BaseURL (like azure) require user configuration and are excluded.
func CatalogProviders() []string {
	providers := make([]string, 0, len(CoreProviders)+len(Aliases))

	// Add all core providers
	providers = append(providers, CoreProviders...)

	// Add aliases that have a defined BaseURL (they work out of the box)
	for name, alias := range EachAlias() {
		if alias.BaseURL != "" {
			providers = append(providers, name)
		}
	}

	return providers
}

// IsCatalogProvider returns true if the provider name is valid for the model catalog.
func IsCatalogProvider(name string) bool {
	// Check core providers
	if slices.Contains(CoreProviders, name) {
		return true
	}
	// Check aliases with BaseURL
	if alias, exists := LookupAlias(name); exists && alias.BaseURL != "" {
		return true
	}
	return false
}
