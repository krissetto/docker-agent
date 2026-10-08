package gemini

import (
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// Pre-compiled regex for extracting text from error messages (performance optimization).
var textExtractRegex = regexp.MustCompile(`"text"\s*:\s*"([^"\\]*(\\.[^"\\]*)*)"`)

const http2BodyClosedError = "http2: response body closed"

// StreamAdapter adapts the Gemini streaming iterator to chat.MessageStream
type StreamAdapter struct {
	iter       func(func(*genai.GenerateContentResponse, error) bool)
	ch         chan result
	done       chan struct{}
	startOnce  sync.Once
	closeOnce  sync.Once
	model      string
	trackUsage bool
	pending    []chat.MessageStreamResponse
}

type result struct {
	resp *genai.GenerateContentResponse
	err  error
	done bool
}

// NewStreamAdapter constructs a StreamAdapter from Gemini's iterator
func NewStreamAdapter(iter func(func(*genai.GenerateContentResponse, error) bool), model string, trackUsage bool) *StreamAdapter {
	return &StreamAdapter{
		iter:       iter,
		ch:         make(chan result),
		done:       make(chan struct{}),
		model:      model,
		trackUsage: trackUsage,
	}
}

func (g *StreamAdapter) start() {
	g.startOnce.Do(func() {
		go g.run()
	})
}

func (g *StreamAdapter) run() {
	defer close(g.ch)

	hasContent := false
	hasToolCalls := false
	var lastResponse *genai.GenerateContentResponse

	// Consume the iterator
	g.iter(func(resp *genai.GenerateContentResponse, err error) bool {
		// Skip noisy http2 errors
		if err != nil && err.Error() == http2BodyClosedError {
			return true
		}

		// Handle streaming parser errors from new Gemini 2.5 response fields
		if err != nil {
			errMsg := err.Error()
			// Check if this is a streaming chunk parsing error that contains valid response data
			if strings.Contains(errMsg, "invalid stream chunk") && strings.Contains(errMsg, `"text":`) {
				// Try to extract text content from the error message
				if textContent := extractTextFromError(errMsg); textContent != "" {
					// Create a synthetic response with the extracted text
					if !g.send(result{resp: &genai.GenerateContentResponse{
						Candidates: []*genai.Candidate{
							{
								Content: &genai.Content{
									Parts: []*genai.Part{
										{Text: textContent},
									},
								},
							},
						},
					}}) {
						return false
					}
					hasContent = true

					// Check if this appears to be a complete response (has finishReason)
					if strings.Contains(errMsg, `"finishReason"`) {
						// This is the final chunk, send done signal
						if !g.send(result{done: true}) {
							return false
						}
						return false
					}

					// Continue iteration to potentially get more chunks
					return true
				}
			}

			if !g.send(result{err: err}) {
				return false
			}
			return false
		}

		if resp != nil {
			// Check for text content and generated inline media without using
			// Text() to avoid warnings
			hasText := false
			hasMedia := false
			for _, candidate := range resp.Candidates {
				if candidate.Content != nil {
					for _, part := range candidate.Content.Parts {
						if part.Text != "" {
							hasText = true
						}
						if part.InlineData != nil && len(part.InlineData.Data) > 0 {
							hasMedia = true
						}
					}
				}
				if hasText && hasMedia {
					break
				}
			}

			// Check for function calls
			hasFuncs := len(resp.FunctionCalls()) > 0
			// Gemini 3 can emit usage metadata on chunks without text/tool
			// calls. Forward such chunks so downstream can capture token usage.
			hasUsage := resp.UsageMetadata != nil

			// Send response if it has content, generated media, function calls, or usage metadata
			if hasText || hasMedia || hasFuncs || hasUsage {
				hasContent = hasContent || hasText || hasMedia
				hasToolCalls = hasToolCalls || hasFuncs
				lastResponse = resp // Store for final message
				if !g.send(result{resp: resp}) {
					return false
				}
			}
		}

		return true
	})

	// Send final message with appropriate stop reason
	if hasContent || hasToolCalls {
		if lastResponse == nil {
			lastResponse = &genai.GenerateContentResponse{}
		}
		if !g.send(result{done: true, resp: lastResponse}) {
			return
		}
	}
}

func (g *StreamAdapter) send(res result) bool {
	select {
	case g.ch <- res:
		return true
	case <-g.done:
		return false
	}
}

// Recv gets the next Gemini content chunk
func (g *StreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	select {
	case <-g.done:
		return chat.MessageStreamResponse{}, io.EOF
	default:
	}

	if len(g.pending) > 0 {
		return g.popPending(), nil
	}

	g.start()

	var res result
	select {
	case r, ok := <-g.ch:
		if !ok {
			return chat.MessageStreamResponse{}, io.EOF
		}
		res = r
	case <-g.done:
		return chat.MessageStreamResponse{}, io.EOF
	}

	if res.err != nil {
		return chat.MessageStreamResponse{}, wrapGeminiError(res.err)
	}

	// Build response
	resp := chat.MessageStreamResponse{
		Model:   g.model,
		Choices: []chat.MessageStreamChoice{{}},
	}

	// Extract usage metadata before branching on done state so that the final
	// "done" event also surfaces usage if the last upstream chunk carried it
	// (Gemini 3 emits usage on chunks without text/tool calls).
	if res.resp != nil {
		resp.ID = res.resp.ResponseID

		if usage := res.resp.UsageMetadata; usage != nil && g.trackUsage {
			// Server-side tool results are additional, uncached input tokens.
			resp.Usage = &chat.Usage{
				InputTokens:       int64(usage.PromptTokenCount-usage.CachedContentTokenCount) + int64(usage.ToolUsePromptTokenCount),
				OutputTokens:      int64(usage.CandidatesTokenCount + usage.ThoughtsTokenCount),
				CachedInputTokens: int64(usage.CachedContentTokenCount),
				ReasoningTokens:   int64(usage.ThoughtsTokenCount),
			}
		}
	}

	if res.done {
		// Set finish reason and role
		resp.Choices[0].Delta.Role = string(chat.MessageRoleAssistant)

		// Check if we have function calls in the final response
		if res.resp != nil && len(res.resp.FunctionCalls()) > 0 {
			resp.Choices[0].FinishReason = chat.FinishReasonToolCalls
			// Don't include function calls in the final message - they were already sent
			slog.Debug("Gemini: Final message with tool calls finish reason")
		} else {
			resp.Choices[0].FinishReason = chat.FinishReasonStop
		}
	} else if res.resp != nil {
		var thoughtSignature []byte
		for candidateIndex, candidate := range res.resp.Candidates {
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if len(part.ThoughtSignature) > 0 {
					thoughtSignature = part.ThoughtSignature
				}

				delta := chat.MessageDelta{}
				if part.Thought {
					delta.ReasoningContent = part.Text
				} else {
					delta.Content = part.Text
				}
				if part.InlineData != nil && len(part.InlineData.Data) > 0 {
					delta.Media = []chat.MediaDelta{{
						Data:     part.InlineData.Data,
						MimeType: part.InlineData.MIMEType,
						Name:     part.InlineData.DisplayName,
						Size:     int64(len(part.InlineData.Data)),
					}}
				}
				// Match the SDK's FunctionCalls selection of the first candidate.
				if fc := part.FunctionCall; candidateIndex == 0 && fc != nil {
					argsJSON, _ := json.Marshal(fc.Args)
					id := "call_" + uuid.New().String()
					slog.Debug("Gemini: Function call", "name", fc.Name, "args", string(argsJSON), "id", id)
					delta.ToolCalls = []tools.ToolCall{{
						ID:   id,
						Type: "function",
						Function: tools.FunctionCall{
							Name:      fc.Name,
							Arguments: string(argsJSON),
						},
					}}
				}
				if delta.Content == "" && delta.ReasoningContent == "" && len(delta.Media) == 0 && len(delta.ToolCalls) == 0 {
					continue
				}
				g.pending = append(g.pending, chat.MessageStreamResponse{
					ID:      resp.ID,
					Model:   resp.Model,
					Choices: []chat.MessageStreamChoice{{Delta: delta}},
				})
			}
		}
		if len(g.pending) > 0 {
			// Keep the chunk's last signature available before any tool call,
			// as it was when all parts were emitted together.
			g.pending[0].Choices[0].Delta.ThoughtSignature = thoughtSignature
			g.pending[len(g.pending)-1].Usage = resp.Usage
			return g.popPending(), nil
		}
		resp.Choices[0].Delta.ThoughtSignature = thoughtSignature
	}

	return resp, nil
}

func (g *StreamAdapter) popPending() chat.MessageStreamResponse {
	resp := g.pending[0]
	g.pending[0] = chat.MessageStreamResponse{}
	g.pending = g.pending[1:]
	return resp
}

// Close closes the stream
func (g *StreamAdapter) Close() {
	g.closeOnce.Do(func() {
		close(g.done)
	})
}

// extractTextFromError attempts to extract text content from streaming parsing errors
func extractTextFromError(errMsg string) string {
	// Look for the JSON response in the error message
	// The error typically contains something like: invalid stream chunk: [{...JSON...}]

	// First try to find the complete JSON object
	startIdx := strings.Index(errMsg, "[{")
	if startIdx == -1 {
		return ""
	}

	// Find the matching closing bracket
	jsonStart := startIdx + 1 // Skip the opening [
	bracketCount := 0
	var jsonEnd int

	for i := jsonStart; i < len(errMsg); i++ {
		char := errMsg[i]
		if char == '{' {
			bracketCount++
		} else if char == '}' {
			bracketCount--
			if bracketCount == 0 {
				jsonEnd = i + 1
				break
			}
		}
	}

	if bracketCount != 0 || jsonEnd == 0 {
		// Fallback to regex approach for partial text
		return extractTextViaRegex(errMsg)
	}

	// Extract the JSON string
	jsonStr := errMsg[jsonStart:jsonEnd]

	// Try to parse the JSON to extract text
	var response struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &response); err == nil {
		if len(response.Candidates) > 0 && len(response.Candidates[0].Content.Parts) > 0 {
			return response.Candidates[0].Content.Parts[0].Text
		}
	}

	// Final fallback to regex approach
	return extractTextViaRegex(errMsg)
}

// extractTextViaRegex extracts text content using pre-compiled regex
func extractTextViaRegex(errMsg string) string {
	matches := textExtractRegex.FindStringSubmatch(errMsg)
	if len(matches) <= 1 {
		return ""
	}

	textContent := matches[1]
	textContent = strings.ReplaceAll(textContent, `\"`, `"`)
	textContent = strings.ReplaceAll(textContent, `\\`, `\`)
	textContent = strings.ReplaceAll(textContent, `\n`, "\n")
	textContent = strings.ReplaceAll(textContent, `\t`, "\t")
	return textContent
}
