package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// TestApplySamplingHandlerOpts_RegistrationMatrix pins the registration choice
// applySamplingHandlerOpts makes for each combination of the two sampling
// handler fields. The reviewer flagged a reconnect race: if Initialize ran
// before configureToolsetHandlers had wired up the handlers, the old
// implementation registered neither CreateMessage* callback with the SDK and
// the next sampling/createMessage request from the server failed with no
// handler. The helper closes that race by registering the with-tools callback
// whenever it might ever be needed — including when both fields are still nil
// — while preserving the basic-only path for callers that explicitly chose it.
func TestApplySamplingHandlerOpts_RegistrationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		setBasic          bool
		setWithTools      bool
		wantWithTools     bool
		wantBasic         bool
		registrationNotes string
	}{
		{
			name:              "neither handler set: still register with-tools",
			wantWithTools:     true,
			registrationNotes: "reconnect race guard — late SetSamplingWithToolsHandler must take effect",
		},
		{
			name:              "only with-tools handler set",
			setWithTools:      true,
			wantWithTools:     true,
			registrationNotes: "modern path",
		},
		{
			name:              "only basic handler set",
			setBasic:          true,
			wantBasic:         true,
			registrationNotes: "legacy path — caller demonstrated they only want basic sampling",
		},
		{
			name:              "both handlers set: prefer with-tools",
			setBasic:          true,
			setWithTools:      true,
			wantWithTools:     true,
			registrationNotes: "with-tools is a superset; SDK panics if both callbacks are registered",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var c sessionClient
			if tc.setBasic {
				c.SetSamplingHandler(func(context.Context, *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
					return &gomcp.CreateMessageResult{}, nil
				})
			}
			if tc.setWithTools {
				c.SetSamplingWithToolsHandler(func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
					return &gomcp.CreateMessageWithToolsResult{}, nil
				})
			}

			opts := &gomcp.ClientOptions{}
			c.applySamplingHandlerOpts(opts)

			assert.Equalf(t, tc.wantWithTools, opts.CreateMessageWithToolsHandler != nil,
				"CreateMessageWithToolsHandler registration mismatch (%s)", tc.registrationNotes)
			assert.Equalf(t, tc.wantBasic, opts.CreateMessageHandler != nil,
				"CreateMessageHandler registration mismatch (%s)", tc.registrationNotes)
			// The SDK panics if both are populated; pin that we never end up there.
			assert.Falsef(t, opts.CreateMessageHandler != nil && opts.CreateMessageWithToolsHandler != nil,
				"both CreateMessage* handlers registered — SDK would panic")
		})
	}
}

// TestHandleSamplingWithToolsRequest_LateSetterTakesEffect proves the
// lazy-read behaviour the registration guard relies on: a
// SetSamplingWithToolsHandler call that lands AFTER applySamplingHandlerOpts
// has already wired the callback into the SDK still has its handler invoked
// on the next inbound sampling/createMessage request. This is what makes the
// race-free reconnect path safe — Initialize doesn't need to re-run for late
// handler registration to become effective.
func TestHandleSamplingWithToolsRequest_LateSetterTakesEffect(t *testing.T) {
	t.Parallel()

	var c sessionClient
	session := &gomcp.ClientSession{}
	c.setSession(session)
	token := c.registerCallContext(t.Context(), session)
	defer c.unregisterCallContext(token)
	req := &gomcp.CreateMessageWithToolsRequest{Session: session, Params: &gomcp.CreateMessageWithToolsParams{Meta: gomcp.Meta{CallbackTokenMetaKey: token}}}

	// Mimic the Initialize-then-handler-registration order: helper runs
	// first (registering the with-tools callback against a still-nil
	// handler field), then SetSamplingWithToolsHandler lands.
	opts := &gomcp.ClientOptions{}
	c.applySamplingHandlerOpts(opts)
	require.NotNil(t, opts.CreateMessageWithToolsHandler, "with-tools callback must be wired even with nil handler field")

	// Sanity-check the pre-registration behaviour: an inbound request with
	// no handler yet must surface a clean error, not a panic or silent drop.
	_, err := c.handleSamplingWithToolsRequest(t.Context(), req)
	require.Error(t, err, "handler must error when no SamplingWithToolsHandler is set yet")

	// Late registration — the simulated post-reconnect wiring step.
	called := false
	c.SetSamplingWithToolsHandler(func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
		called = true
		return &gomcp.CreateMessageWithToolsResult{Model: "late-bound"}, nil
	})

	result, err := c.handleSamplingWithToolsRequest(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, called, "late SetSamplingWithToolsHandler must take effect without re-init")
	assert.Equal(t, "late-bound", result.Model)
}

func TestCallbacksRequireExactActiveCapability(t *testing.T) {
	t.Parallel()
	var c sessionClient
	session := &gomcp.ClientSession{}
	c.setSession(session)
	var calls []string
	owner := func(name string) context.Context {
		return tools.WithHandlerScope(t.Context(), tools.HandlerScope{
			Elicitation: func(_ context.Context, params *gomcp.ElicitParams) (tools.ElicitationResult, error) {
				require.NotContains(t, params.Meta, CallbackTokenMetaKey)
				calls = append(calls, name+":elicit")
				return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
			},
			Sampling: func(_ context.Context, params *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
				require.NotContains(t, params.Meta, CallbackTokenMetaKey)
				calls = append(calls, name+":sample")
				return &gomcp.CreateMessageResult{}, nil
			},
			SamplingWithTools: func(_ context.Context, params *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
				require.NotContains(t, params.Meta, CallbackTokenMetaKey)
				calls = append(calls, name+":tools")
				return &gomcp.CreateMessageWithToolsResult{}, nil
			},
		})
	}
	c.SetElicitationHandler(tools.ScopedElicitationHandler)
	c.SetSamplingHandler(tools.SamplingScopeHandler)
	c.SetSamplingWithToolsHandler(tools.SamplingWithToolsScopeHandler)
	invoke := func(ctx context.Context, s *gomcp.ClientSession, value any, valid bool) {
		t.Helper()
		meta := gomcp.Meta{CallbackTokenMetaKey: value}
		result, err := c.handleElicitationRequest(ctx, &gomcp.ElicitRequest{Session: s, Params: &gomcp.ElicitParams{Meta: meta}})
		if valid {
			require.NoError(t, err)
			require.Equal(t, "decline", result.Action)
		} else {
			require.Error(t, err)
		}
		_, err = c.handleSamplingRequest(ctx, &gomcp.CreateMessageRequest{Session: s, Params: &gomcp.CreateMessageParams{Meta: meta}})
		require.Equal(t, valid, err == nil)
		_, err = c.handleSamplingWithToolsRequest(ctx, &gomcp.CreateMessageWithToolsRequest{Session: s, Params: &gomcp.CreateMessageWithToolsParams{Meta: meta}})
		require.Equal(t, valid, err == nil)
	}
	tokenA := c.registerCallContext(owner("a"), session)
	tokenB := c.registerCallContext(owner("b"), session)
	invoke(t.Context(), session, tokenA, true)
	require.Equal(t, []string{"a:elicit", "a:sample", "a:tools"}, calls)
	calls = nil
	c.unregisterCallContext(tokenA)
	for _, value := range []any{tokenA, nil, "forged", 42, []string{tokenB}} {
		invoke(owner("stale connection"), session, value, false)
	}
	invoke(t.Context(), &gomcp.ClientSession{}, tokenB, false)
	require.Empty(t, calls, "delayed A and unsolicited callbacks must not invoke B")
	invoke(t.Context(), session, tokenB, true)
	require.Equal(t, []string{"b:elicit", "b:sample", "b:tools"}, calls)
	calls = nil
	c.setSession(&gomcp.ClientSession{})
	invoke(t.Context(), session, tokenB, false)
	require.Empty(t, calls)
	c.setSession(session)
	cancelCtx, cancel := context.WithCancel(owner("canceled"))
	tokenCanceled := c.registerCallContext(cancelCtx, session)
	cancel()
	invoke(t.Context(), session, tokenCanceled, false)
	require.Empty(t, calls)
	c.setSession(nil)
	invoke(t.Context(), session, tokenCanceled, false)
	require.Empty(t, calls)
}

func TestCallbacksRejectUnprovenBeforeLegacyHandlers(t *testing.T) {
	t.Parallel()
	var c sessionClient
	session := &gomcp.ClientSession{}
	c.setSession(session)
	token := c.registerCallContext(t.Context(), session)
	defer c.unregisterCallContext(token)
	called := false
	c.SetElicitationHandler(func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		called = true
		return tools.ElicitationResult{}, nil
	})
	c.SetSamplingHandler(func(context.Context, *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
		called = true
		return &gomcp.CreateMessageResult{}, nil
	})
	c.SetSamplingWithToolsHandler(func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
		called = true
		return &gomcp.CreateMessageWithToolsResult{}, nil
	})
	_, err := c.handleElicitationRequest(t.Context(), &gomcp.ElicitRequest{Session: session, Params: &gomcp.ElicitParams{}})
	require.Error(t, err)
	_, err = c.handleSamplingRequest(t.Context(), &gomcp.CreateMessageRequest{Session: session, Params: &gomcp.CreateMessageParams{}})
	require.Error(t, err)
	_, err = c.handleSamplingWithToolsRequest(t.Context(), &gomcp.CreateMessageWithToolsRequest{Session: session, Params: &gomcp.CreateMessageWithToolsParams{}})
	require.Error(t, err)
	require.False(t, called)
}

func TestScopedCallbackPreservesOwnerAndCancellation(t *testing.T) {
	t.Parallel()
	var c sessionClient
	session := &gomcp.ClientSession{}
	c.setSession(session)
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "owner"))
	defer cancel()
	token := c.registerCallContext(tools.WithHandlerScope(ctx, tools.HandlerScope{}), session)
	meta := gomcp.Meta{CallbackTokenMetaKey: token}
	callbackCtx, release, err := c.callbackContext(t.Context(), session, meta)
	require.NoError(t, err)
	defer release()
	require.Equal(t, "owner", callbackCtx.Value(key{}))
	cancel()
	require.Eventually(t, func() bool { return callbackCtx.Err() == context.Canceled }, time.Second, time.Millisecond)
	_, _, err = c.callbackContext(t.Context(), session, meta)
	require.Error(t, err)
	c.unregisterCallContext(token)
	_, _, err = c.callbackContext(t.Context(), session, meta)
	require.Error(t, err)
}

func TestSDKInlineInputRequestsRetainExactCallScope(t *testing.T) {
	for _, withTools := range []bool{false, true} {
		t.Run(fmt.Sprint("with-tools=", withTools), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var c sessionClient
			c.SetElicitationHandler(tools.ScopedElicitationHandler)
			var calls []string
			scope := tools.HandlerScope{
				Elicitation: func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
					calls = append(calls, "elicit")
					return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
				},
				Sampling: func(context.Context, *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
					calls = append(calls, "sample")
					return &gomcp.CreateMessageResult{Role: "assistant", Content: &gomcp.TextContent{Text: "sampled"}, Model: "fake"}, nil
				},
				SamplingWithTools: func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
					calls = append(calls, "sample")
					return &gomcp.CreateMessageWithToolsResult{Role: "assistant", Content: []gomcp.Content{&gomcp.TextContent{Text: "sampled"}}, Model: "fake"}, nil
				},
			}
			if withTools {
				c.SetSamplingWithToolsHandler(tools.SamplingWithToolsScopeHandler)
			} else {
				c.SetSamplingHandler(tools.SamplingScopeHandler)
			}
			opts := &gomcp.ClientOptions{ElicitationHandler: c.handleElicitationRequest}
			c.applySamplingHandlerOpts(opts)
			client := gomcp.NewClient(&gomcp.Implementation{Name: "fake-client"}, opts)
			server := gomcp.NewServer(&gomcp.Implementation{Name: "fake-server"}, nil)
			gomcp.AddTool(server, &gomcp.Tool{Name: "inline"}, func(_ context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
				if len(req.Params.InputResponses) == 0 {
					return &gomcp.CallToolResult{InputRequests: gomcp.InputRequestMap{"elicit": &gomcp.ElicitParams{Message: "Confirm?"}}}, nil, nil
				}
				if _, ok := req.Params.InputResponses["sample"]; !ok {
					return &gomcp.CallToolResult{InputRequests: gomcp.InputRequestMap{"sample": &gomcp.CreateMessageParams{MaxTokens: 1}}}, nil, nil
				}
				return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "done"}}}, nil, nil
			})
			clientTransport, serverTransport := gomcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverTransport, nil)
			require.NoError(t, err)
			defer serverSession.Close()
			session, err := client.Connect(ctx, clientTransport, nil)
			require.NoError(t, err)
			c.setSession(session)
			defer c.Close(context.WithoutCancel(t.Context()))
			// An unrelated active operation must not affect inline ownership.
			other := c.registerCallContext(ctx, session)
			defer c.unregisterCallContext(other)
			meta := gomcp.Meta{"existing": "preserved"}
			request := &gomcp.CallToolParams{Name: "inline", Meta: meta}
			result, err := c.CallTool(tools.WithHandlerScope(ctx, scope), request)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, []string{"elicit", "sample"}, calls)
			require.Equal(t, gomcp.Meta{"existing": "preserved"}, meta, "outbound metadata must not mutate the caller's map")
		})
	}
}

func TestSDKReverseCallbacksCannotInheritInlineMarker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var c sessionClient
	var calls atomic.Int32
	c.SetElicitationHandler(func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		calls.Add(1)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	})
	c.SetSamplingHandler(func(context.Context, *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
		calls.Add(1)
		return &gomcp.CreateMessageResult{Role: "assistant", Content: &gomcp.TextContent{Text: "fake"}, Model: "fake"}, nil
	})
	opts := &gomcp.ClientOptions{ElicitationHandler: c.handleElicitationRequest}
	c.applySamplingHandlerOpts(opts)
	client := gomcp.NewClient(&gomcp.Implementation{Name: "fake-client"}, opts)
	server := gomcp.NewServer(&gomcp.Implementation{Name: "fake-server"}, nil)
	observations := make(chan error, 1)
	gomcp.AddTool(server, &gomcp.Tool{Name: "reverse"}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		// Even though these arrive while CallTool is active, no private client
		// marker traverses the transport into inbound request dispatch.
		if _, err := req.Session.Elicit(ctx, &gomcp.ElicitParams{Message: "Unsolicited"}); err == nil {
			observations <- errors.New("untagged reverse elicitation was accepted")
			return &gomcp.CallToolResult{}, nil, nil
		}
		if _, err := req.Session.CreateMessage(ctx, &gomcp.CreateMessageParams{MaxTokens: 1}); err == nil {
			observations <- errors.New("untagged reverse sampling was accepted")
			return &gomcp.CallToolResult{}, nil, nil
		}
		meta := gomcp.Meta{CallbackTokenMetaKey: req.Params.Meta[CallbackTokenMetaKey]}
		_, err := req.Session.Elicit(ctx, &gomcp.ElicitParams{Meta: meta, Message: "Scoped"})
		if err == nil {
			_, err = req.Session.CreateMessage(ctx, &gomcp.CreateMessageParams{Meta: meta, MaxTokens: 1})
		}
		observations <- err
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "done"}}}, nil, nil
	})
	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	session, err := client.Connect(ctx, clientTransport, &gomcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	require.NoError(t, err)
	c.setSession(session)
	defer c.Close(context.WithoutCancel(t.Context()))
	_, err = c.CallTool(ctx, &gomcp.CallToolParams{Name: "reverse"})
	require.NoError(t, err)
	require.NoError(t, <-observations)
	require.Equal(t, int32(2), calls.Load(), "only tagged reverse callbacks may invoke handlers")
}

func TestCallbackPreservesExplicitScopeOptOut(t *testing.T) {
	t.Parallel()
	var c sessionClient
	session := &gomcp.ClientSession{}
	c.setSession(session)
	scope := tools.HandlerScope{Elicitation: func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		t.Fatal("disabled scope handler must not be invoked")
		return tools.ElicitationResult{}, nil
	}}
	ctx := tools.WithoutHandlerScope(tools.WithHandlerScope(t.Context(), scope))
	token := c.registerCallContext(ctx, session)
	defer c.unregisterCallContext(token)
	callbackCtx, release, err := c.callbackContext(t.Context(), session, gomcp.Meta{CallbackTokenMetaKey: token})
	require.NoError(t, err)
	defer release()
	require.False(t, tools.HasHandlerScope(callbackCtx), "causal ownership must not re-enable disabled handlers")
	c.SetElicitationHandler(tools.ScopedElicitationHandler)
	_, err = c.handleElicitationRequest(t.Context(), &gomcp.ElicitRequest{Session: session, Params: &gomcp.ElicitParams{Meta: gomcp.Meta{CallbackTokenMetaKey: token}}})
	require.Error(t, err)
}
