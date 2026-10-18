package environment

import (
	"log/slog"
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// Source is a labeled secret source in the default provider chain. The name
// identifies where a value comes from (e.g. "environment", "docker-desktop") so
// diagnostic commands like `docker agent doctor` can report it.
type Source struct {
	Name     string
	Provider Provider
}

// DefaultSources returns the ordered, labeled secret sources that make up the
// default provider chain: OS env, run secrets, the docker agent env file
// (<config dir>/.env, when present), credential helper (if configured),
// and Docker Desktop. Lookup precedence is the slice order.
func DefaultSources() []Source {
	var sources []Source

	sources = append(sources,
		Source{Name: "environment", Provider: NewOsEnvProvider()},
		Source{Name: "run-secrets", Provider: NewRunSecretsProvider()},
	)

	// The docker agent env file (written by `docker agent setup`) resolves
	// without any flag. A missing file is the common case and simply skipped;
	// a malformed one is reported but never blocks the chain, unlike an
	// explicit --env-from-file, so a stray edit cannot lock every command out.
	if provider, err := newConfigEnvFileProvider(); err != nil {
		slog.Warn("Ignoring unreadable docker agent env file", "path", ConfigEnvFilePath(), "error", err)
	} else if provider != nil {
		sources = append(sources, Source{Name: "config-env-file", Provider: provider})
	}

	// The ChatGPT account login (created by the `docker agent setup` sign-in)
	// serves the virtual CHATGPT_OAUTH_TOKEN variable. It sits after the
	// explicit sources so a user-supplied value always wins.
	sources = append(sources, Source{Name: "chatgpt-login", Provider: NewChatGPTLoginProvider()})

	// Tests may construct this chain, but must not run the user's configured helper.
	if !testing.Testing() {
		if cfg, err := userconfig.Load(); err != nil {
			slog.Warn("Ignoring unreadable user config for credential helper", "path", userconfig.Path(), "error", err)
		} else if cfg.CredentialHelper != nil && cfg.CredentialHelper.Command != "" {
			sources = append(sources, Source{
				Name:     "credential-helper",
				Provider: NewCredentialHelperProvider(cfg.CredentialHelper.Command, cfg.CredentialHelper.Args...),
			})
		}
	}

	// Docker Desktop provider comes after credential helper
	sources = append(sources, Source{Name: "docker-desktop", Provider: NewDockerDesktopProvider()})

	return sources
}

// newConfigEnvFileProvider builds the provider for the docker agent env file.
// It returns (nil, nil) when the file does not exist and an error when the
// file exists but cannot be read or parsed.
func newConfigEnvFileProvider() (Provider, error) {
	path := ConfigEnvFilePath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return NewEnvFilesProvider([]string{path})
}

// NewDefaultProvider creates a provider chain from [DefaultSources].
// The whole chain is wrapped so that values shaped like "op://..." are resolved
// as 1Password secret references through the `op` CLI.
func NewDefaultProvider() Provider {
	sources := DefaultSources()

	providers := make([]Provider, 0, len(sources))
	for _, source := range sources {
		providers = append(providers, source.Provider)
	}

	// Resolve any "op://" secret references through the 1Password CLI,
	// regardless of which provider returned the value.
	return NewOnePasswordProvider(NewMultiProvider(providers...))
}
