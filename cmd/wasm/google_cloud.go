//go:build js && wasm && docker_agent_wasm_cloud

package main

import (
	"cmp"
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

// googleFactory serves Gemini through the Gemini API with an API key as usual,
// and Vertex AI (Gemini, or Model Garden when a publisher is set) with an
// OAuth access token read from the session env on every request: the model's
// token_key, or GOOGLE_OAUTH_ACCESS_TOKEN. A missing token fails when the
// session is created; ADC and the Gemini API are never fallen back to.
func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	if !wantsVertexAI(ctx, cfg, env) {
		return gemini.NewClient(ctx, cfg, env, opts...)
	}
	key := cmp.Or(cfg.TokenKey, vertexTokenEnv)
	source := func(ctx context.Context) (string, error) {
		token, _ := env.Get(ctx, key)
		if token == "" {
			return "", fmt.Errorf("vertex AI in the browser requires an OAuth access token in the session env (%s)", key)
		}
		return token, nil
	}
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexai.NewClientWithTokenSource(ctx, cfg, env, source, opts...)
	}
	return gemini.NewClient(ctx, cfg, env, append(opts, options.WithTokenSource(source))...)
}
