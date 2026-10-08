package openai

import (
	"encoding/json"
	"slices"

	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
)

// Completed items can carry ciphertext absent from the terminal snapshot.
func (a *ResponseStreamAdapter) captureResponseState(event responses.ResponseStreamEventUnion, response *chat.MessageStreamResponse) {
	if a.responseState == nil {
		return
	}
	for _, choice := range response.Choices {
		a.responseReasoning.WriteString(choice.Delta.ReasoningContent)
	}
	if event.Type == "response.output_item.done" && event.JSON.OutputIndex.Valid() {
		if a.responseItems == nil {
			a.responseItems = make(map[int64]json.RawMessage)
		}
		a.responseItems[event.OutputIndex] = responseItemJSON(event.Item)
	}
	if event.Type != "response.completed" && event.Type != "response.done" && event.Type != "response.incomplete" {
		return
	}
	state := a.responseState.Clone()
	state.ID = event.Response.ID
	reasoning := a.responseReasoning.String()
	state.ReasoningContent = &reasoning
	if len(event.Response.Output) > 0 {
		for i, item := range event.Response.Output {
			raw := responseItemJSON(item)
			if item.Type == "reasoning" && item.EncryptedContent == "" {
				if completed := a.responseItems[int64(i)]; len(completed) > 0 {
					var previous responses.ResponseOutputItemUnion
					if json.Unmarshal(completed, &previous) == nil && previous.Type == "reasoning" && previous.ID == item.ID && previous.EncryptedContent != "" {
						raw = completed
					}
				}
			}
			state.Output = append(state.Output, slices.Clone(raw))
		}
	} else {
		indices := make([]int64, 0, len(a.responseItems))
		for index := range a.responseItems {
			indices = append(indices, index)
		}
		slices.Sort(indices)
		for _, index := range indices {
			state.Output = append(state.Output, slices.Clone(a.responseItems[index]))
		}
	}
	if len(response.Choices) == 0 {
		response.Choices = []chat.MessageStreamChoice{{}}
	}
	response.Choices[0].Delta.OpenAIResponse = state
}
