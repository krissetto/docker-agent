package environment

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sync/singleflight"
)

// onePasswordPrefix marks an environment value as a 1Password secret reference
// (e.g. "op://vault/item/field") that must be resolved with the `op` CLI.
const onePasswordPrefix = "op://"

// OnePasswordProvider decorates another Provider and resolves 1Password secret
// references. When the wrapped provider returns a value starting with "op://",
// the value is treated as a secret reference and resolved using the `op read`
// CLI command. All other values are passed through unchanged.
type OnePasswordProvider struct {
	provider Provider
	// resolve turns a "op://..." reference into its secret value. It is a field
	// so tests can inject a fake resolver without relying on the `op` binary.
	resolve func(ctx context.Context, reference string) (string, bool)

	// group coalesces concurrent resolutions of the same reference so the `op`
	// CLI is spawned at most once per reference, while still allowing distinct
	// references to resolve in parallel.
	group singleflight.Group

	// cache memoizes resolved references. Successful results and deterministic
	// failures (op missing, bad reference) are cached so we don't keep spawning
	// the CLI; transient failures (e.g. context cancellation) are not cached so
	// a later call can retry.
	mu    sync.Mutex
	cache map[string]string
}

type OnePasswordNotAvailableError struct{}

func (OnePasswordNotAvailableError) Error() string {
	return "op (1Password CLI) is not installed"
}

// NewOnePasswordProvider wraps provider so that "op://" references are resolved
// with the `op` CLI. The `op` binary is looked up lazily, only when a reference
// is actually encountered, so an "op://" value is never silently passed through
// as if it were a real secret.
func NewOnePasswordProvider(provider Provider) Provider {
	return &OnePasswordProvider{
		provider: provider,
		resolve:  resolveOnePasswordReference,
		cache:    make(map[string]string),
	}
}

func resolveOnePasswordReference(ctx context.Context, reference string) (string, bool) {
	// Tests must never unlock or read the developer's real vault.
	if testing.Testing() {
		return "", false
	}

	path, err := lookupBinary("op", OnePasswordNotAvailableError{})
	if err != nil {
		slog.WarnContext(ctx, "Cannot resolve 1Password secret reference: op (1Password CLI) is not installed")
		return "", false
	}

	return readOnePasswordReference(ctx, path, reference)
}

func readOnePasswordReference(ctx context.Context, path, reference string) (string, bool) {
	return runCommand(ctx, "1password", path, "read", reference)
}

func (p *OnePasswordProvider) Get(ctx context.Context, name string) (string, bool) {
	value, found := p.provider.Get(ctx, name)
	if !found || !strings.HasPrefix(value, onePasswordPrefix) {
		return value, found
	}

	// Always report the variable as found: returning the raw "op://" reference
	// would leak it to downstream providers, so an unresolved reference becomes
	// an empty value instead.
	return p.resolveCached(ctx, value), true
}

func (p *OnePasswordProvider) resolveCached(ctx context.Context, reference string) string {
	if cached, ok := p.cachedValue(reference); ok {
		return cached
	}

	resolved, _, _ := p.group.Do(reference, func() (any, error) {
		if cached, ok := p.cachedValue(reference); ok {
			return cached, nil
		}

		value, ok := p.resolve(ctx, reference)
		if !ok {
			slog.WarnContext(ctx, "Failed to resolve 1Password secret reference; using empty value")
			value = ""
			// Don't cache failures caused by a cancelled/expired context: those
			// are transient and a later call should be allowed to retry.
			if ctx.Err() != nil {
				return value, nil
			}
		}

		p.storeValue(reference, value)
		return value, nil
	})

	return resolved.(string)
}

func (p *OnePasswordProvider) cachedValue(reference string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value, ok := p.cache[reference]
	return value, ok
}

func (p *OnePasswordProvider) storeValue(reference, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[string]string)
	}
	p.cache[reference] = value
}
