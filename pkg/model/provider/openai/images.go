package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/tools"
)

// imageOutputEnabled keeps auxiliary requests text-only even on multimodal models.
func (c *Client) imageOutputEnabled() bool {
	return c.ModelConfig.ExplicitImageOutputEnabled() && !c.ModelOptions.GeneratingTitle() && !c.ModelOptions.Compacting()
}

func (c *Client) addImageGenerationTool(ctx context.Context, params *responses.ResponseNewParams) error {
	if !c.imageOutputEnabled() {
		return nil
	}
	if err := c.ModelConfig.ValidateResolvedImageGeneration(); err != nil {
		return err
	}
	tool := &responses.ToolImageGenerationParam{}
	tool.SetExtraFields(c.ModelConfig.ImageGeneration)
	params.Tools = append(params.Tools, responses.ToolUnionParam{OfImageGeneration: tool})
	return nil
}

func (c *Client) createImageStream(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	if len(requestTools) > 0 {
		return nil, errors.New("openai_images does not support function tools; configure a dedicated image agent without tools")
	}
	if c.ModelOptions.StructuredOutput() != nil {
		return nil, errors.New("openai_images does not support structured output")
	}
	if !c.imageOutputEnabled() {
		return nil, errors.New("openai_images requires output_capabilities.image: true and cannot perform auxiliary text tasks")
	}
	var prompt string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != chat.MessageRoleUser {
			continue
		}
		parts := []string{messages[i].Content}
		for _, part := range messages[i].MultiContent {
			if part.Type != chat.MessagePartTypeText {
				return nil, errors.New("openai_images supports text-to-image generation only; image edits are unsupported")
			}
			parts = append(parts, part.Text)
		}
		prompt = strings.TrimSpace(strings.Join(parts, "\n"))
		break
	}
	if prompt == "" {
		return nil, errors.New("openai_images requires a nonempty user text prompt")
	}
	var instructions []string
	for _, message := range messages {
		if message.Role != chat.MessageRoleSystem {
			continue
		}
		if message.Content != "" {
			instructions = append(instructions, message.Content)
		}
		for _, part := range message.MultiContent {
			if part.Type == chat.MessagePartTypeText && part.Text != "" {
				instructions = append(instructions, part.Text)
			}
		}
	}
	if len(instructions) > 0 {
		prompt = strings.Join(instructions, "\n\n") + "\n\n" + prompt
	}
	if err := c.ModelConfig.ValidateResolvedImageGeneration(); err != nil {
		return nil, err
	}
	fields := c.ModelConfig.ImageGeneration
	params := oai.ImageGenerateParams{Model: c.ModelConfig.Model, Prompt: prompt}
	// DALL-E defaults to remote URLs. GPT Image models return base64 natively
	// and reject response_format, so only set the option for other models.
	if !strings.HasPrefix(c.ModelConfig.Model, "gpt-image-") && c.ModelConfig.Model != "chatgpt-image-latest" {
		params.ResponseFormat = oai.ImageGenerateParamsResponseFormatB64JSON
	}
	params.SetExtraFields(fields)
	client, err := c.clientFn(ctx)
	if err != nil {
		return nil, err
	}
	result, err := client.Images.Generate(ctx, params)
	if err != nil {
		return nil, oaistream.WrapOpenAIError(err)
	}
	media := make([]chat.MediaDelta, 0, len(result.Data))
	for _, image := range result.Data {
		encoded := image.B64JSON
		if encoded == "" {
			encoded = image.URL
		}
		decoded, err := oaistream.DecodeImage(encoded)
		if err != nil {
			return nil, err
		}
		media = append(media, decoded)
	}
	if len(media) == 0 {
		return nil, errors.New("image generation returned no images")
	}
	return &imageStream{response: chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Role: "assistant", Media: media}, FinishReason: chat.FinishReasonStop}}}}, nil
}

type imageStream struct {
	response chat.MessageStreamResponse
	done     bool
}

func (s *imageStream) Recv() (chat.MessageStreamResponse, error) {
	if s.done {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.done = true
	return s.response, nil
}
func (s *imageStream) Close() { s.done = true }

// responseImages handles complete outputs only. Partial previews are not complete
// artifacts; done and terminal snapshots can contain the same image.
func (a *ResponseStreamAdapter) responseImages(event responses.ResponseStreamEventUnion) ([]chat.MediaDelta, error) {
	if !a.imageOutputEnabled {
		return nil, nil
	}
	var items []responses.ResponseOutputItemUnion
	switch event.Type {
	case "response.output_item.done":
		items = []responses.ResponseOutputItemUnion{event.Item}
	case "response.completed", "response.done", "response.incomplete":
		items = event.Response.Output
	default:
		return nil, nil
	}
	var media []chat.MediaDelta
	for i, item := range items {
		if item.Type != "image_generation_call" {
			continue
		}
		var image struct {
			ID     string `json:"id"`
			Result string `json:"result"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(item.RawJSON()), &image); err != nil {
			return nil, errors.New("invalid image generation output")
		}
		if image.Status == "failed" {
			return nil, errors.New("native image generation failed; verify model and image_generation tool compatibility")
		}
		if image.Result == "" {
			continue
		}
		key := image.ID
		if key == "" {
			index := int64(i)
			if event.Type == "response.output_item.done" {
				index = event.OutputIndex
			}
			key = fmt.Sprintf("output:%d", index)
		}
		if a.imageItems[key] {
			continue
		}
		decoded, err := oaistream.DecodeImage(image.Result)
		if err != nil {
			return nil, err
		}
		if a.imageItems == nil {
			a.imageItems = make(map[string]bool)
		}
		a.imageItems[key] = true
		media = append(media, decoded)
	}
	return media, nil
}

// Preserve legacy unauthenticated custom endpoints, while native image requests
// on the built-in provider retain normal OpenAI credential resolution.
func usesBuiltinImageAuth(cfg *latest.ModelConfig) bool {
	if cfg.Provider != "openai" {
		return false
	}
	return cfg.IsImageOnly() || cfg.ExplicitImageOutputEnabled()
}
