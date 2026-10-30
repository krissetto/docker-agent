// Package facadetest is the conformance suite for facade adapters, plus the
// scripted fixtures it runs against. It is test support, not production code.
package facadetest

import (
	"context"
	"io"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// Script is one model call. Hold, when set, blocks the call before it produces
// anything until the channel closes or the call's context ends.
type Script struct {
	Text     string
	ToolCall string
	// ToolArgs are the JSON arguments of ToolCall; empty means "{}".
	ToolArgs string
	// Hold blocks the stream before it produces anything until the channel closes or the call's context ends.
	Hold <-chan struct{}
	// Entered, when set, is closed as soon as the call begins. A scenario waits on it to
	// order its own operations against the call deterministically.
	Entered chan struct{}
	// Block holds the call inside its creation, before the stream is returned, until the
	// channel closes or the call's context ends. Work admitted while blocked is therefore
	// ordered before the runtime's post-open boundary, so it stays pending until release.
	Block <-chan struct{}
}

// Model is a scripted provider. Each model call consumes one Script; when the
// queue is empty the call answers "ok".
type Model struct {
	mu      sync.Mutex
	scripts []Script
	calls   int
}

var _ provider.Provider = (*Model)(nil)

// NewModel returns an empty scripted model.
func NewModel() *Model { return &Model{} }

// Push appends scripts to be consumed by subsequent model calls.
func (m *Model) Push(scripts ...Script) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = append(m.scripts, scripts...)
}

// Calls reports how many model calls have been made.
func (m *Model) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *Model) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("scripted/model") }

func (m *Model) BaseConfig() base.Config { return base.Config{} }

func (m *Model) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	m.mu.Lock()
	m.calls++
	script := Script{Text: "ok"}
	if len(m.scripts) > 0 {
		script = m.scripts[0]
		m.scripts = m.scripts[1:]
	}
	m.mu.Unlock()
	if script.Entered != nil {
		close(script.Entered)
	}
	if script.Block != nil {
		select {
		case <-script.Block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &stream{done: ctx.Done(), ctxErr: ctx.Err, script: script}, nil
}

type stream struct {
	done   <-chan struct{}
	ctxErr func() error
	script Script
	step   int
}

func (s *stream) Recv() (chat.MessageStreamResponse, error) {
	if s.script.Hold != nil && s.step == 0 {
		select {
		case <-s.script.Hold:
		case <-s.done:
			return chat.MessageStreamResponse{}, s.ctxErr()
		}
	}
	s.step++
	if s.script.ToolCall == "" {
		switch s.step {
		case 1:
			return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: s.script.Text}}}}, nil
		case 2:
			return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
		}
		return chat.MessageStreamResponse{}, io.EOF
	}
	switch s.step {
	case 1:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: "call-" + s.script.ToolCall, Type: "function", Function: tools.FunctionCall{Name: s.script.ToolCall, Arguments: toolArgs(s.script.ToolArgs)},
		}}}}}}, nil
	case 2:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}
	return chat.MessageStreamResponse{}, io.EOF
}

func (s *stream) Close() {}

func toolArgs(args string) string {
	if args == "" {
		return "{}"
	}
	return args
}
