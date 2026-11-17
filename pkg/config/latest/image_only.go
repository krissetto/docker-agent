package latest

import (
	"cmp"
	"errors"
	"fmt"
)

// IsImageOnly reports whether a resolved model uses the Images API, which
// cannot perform auxiliary text tasks. Named providers must be resolved first.
func (f *ModelConfig) IsImageOnly() bool {
	if f == nil {
		return false
	}
	api, _ := f.ProviderOpts["api_type"].(string)
	return api == "openai_images"
}

// ExplicitImageOutputEnabled requires an explicit opt-in for native OpenAI images.
// Other providers may retain their catalogue-based capability resolution.
func (f *ModelConfig) ExplicitImageOutputEnabled() bool {
	return f != nil && f.OutputCapabilities != nil && f.OutputCapabilities.Image != nil && *f.OutputCapabilities.Image
}

// ValidateImageGeneration checks public declarations before provider defaults.
func (f *ModelConfig) ValidateImageGeneration(providers map[string]ProviderConfig) error {
	if f == nil {
		return nil
	}
	if f.Provider == "openai_images" || f.ProviderOpts["api_type"] == "openai_images" {
		return errors.New("configure api_type: openai_images on a named provider under providers, then reference that provider from the model")
	}
	if _, ok := f.ProviderOpts["image_generation"]; ok {
		return errors.New("provider_opts.image_generation is unsupported; move settings to model image_generation and enable output_capabilities.image: true")
	}
	resolved := *f
	resolved.ProviderOpts = map[string]any{}
	if p, ok := providers[f.Provider]; ok {
		resolved.Provider = cmp.Or(p.Provider, "openai")
		for k, v := range p.ProviderOpts {
			resolved.ProviderOpts[k] = v
		}
		if p.APIType != "" {
			resolved.ProviderOpts["api_type"] = p.APIType
		}
	}
	for k, v := range f.ProviderOpts {
		resolved.ProviderOpts[k] = v
	}
	return resolved.ValidateResolvedImageGeneration()
}

// ValidateResolvedImageGeneration checks runtime settings after protocol resolution.
func (f *ModelConfig) ValidateResolvedImageGeneration() error {
	if f == nil {
		return nil
	}
	if _, ok := f.ProviderOpts["image_generation"]; ok {
		return errors.New("provider_opts.image_generation is unsupported; move settings to model image_generation")
	}
	api, _ := f.ProviderOpts["api_type"].(string)
	if api == "openai_images" {
		if f.Provider == "chatgpt" {
			return errors.New("chatgpt only supports the Responses API, not openai_images")
		}
		if !f.ExplicitImageOutputEnabled() {
			return errors.New("openai_images requires output_capabilities.image: true")
		}
	}
	if f.ImageGeneration == nil {
		return nil
	}
	if !f.ExplicitImageOutputEnabled() {
		return errors.New("image_generation requires output_capabilities.image: true")
	}
	if api != "openai_images" && api != "openai_responses" && !(api == "" && (f.Provider == "openai" || f.Provider == "chatgpt")) {
		return errors.New("image_generation requires openai_images or openai_responses; select api_type on a named provider")
	}
	for _, key := range []string{"type", "prompt", "stream"} {
		if _, ok := f.ImageGeneration[key]; ok {
			return fmt.Errorf("image_generation cannot override %s", key)
		}
	}
	if api == "openai_images" {
		if _, ok := f.ImageGeneration["model"]; ok {
			return errors.New("image_generation cannot override model for openai_images; use the model field")
		}
	}
	return nil
}
