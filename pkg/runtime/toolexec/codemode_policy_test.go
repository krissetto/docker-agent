package toolexec_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type codeModeProbeTools struct{ tool tools.Tool }

func (s codeModeProbeTools) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{s.tool}, nil
}

type codeModePolicy struct {
	guard     func(*hooks.Input) *hooks.Result
	post      func(*hooks.Input) *hooks.Result
	seen      []string
	approvals []string
}

func (h *codeModePolicy) Dispatch(_ context.Context, _ *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	if event == hooks.EventToolGuard {
		h.seen = append(h.seen, in.ToolName)
		if h.guard != nil {
			return h.guard(in)
		}
	}
	if event == hooks.EventPostToolUse && h.post != nil {
		return h.post(in)
	}
	if event == hooks.EventToolResponseTransform && in.ToolName == "effect" {
		output := "transformed inner"
		return &hooks.Result{UpdatedToolResponse: &output}
	}
	return nil
}
func (*codeModePolicy) NotifyUserInput(context.Context, *agent.Agent, string, string) {}
func (h *codeModePolicy) NotifyApprovalDecision(_ context.Context, _ *session.Session, _ *agent.Agent, tc tools.ToolCall, decision, _, _ string) {
	h.approvals = append(h.approvals, tc.Function.Name+":"+decision)
}

func configuredCodeMode(t *testing.T, inner tools.Tool) (*agent.Agent, *permissions.Checker) {
	t.Helper()
	source := config.NewBytesSource("probe.yaml", []byte(`agents:
  root:
    harness:
      type: codex
    code_mode_tools: true
    toolsets:
      - type: probe
permissions:
  deny: [effect:value=blocked]
`))
	tm, err := teamloader.Load(t.Context(), source, &config.RuntimeConfig{}, teamloader.WithCodeMode(codemode.Wrap), teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"probe": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		return codeModeProbeTools{inner}, nil
	}})))
	require.NoError(t, err)
	a, err := tm.DefaultAgent()
	require.NoError(t, err)
	return a, tm.Permissions()
}

func TestConfiguredCodeModeNestedPolicy(t *testing.T) {
	for _, lane := range []string{"name-deny", "argument-deny", "dynamic-guard"} {
		t.Run(lane, func(t *testing.T) {
			var effects atomic.Int32
			inner := tools.Tool{Name: "effect", Parameters: tools.MustSchemaFor[map[string]any](), Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				effects.Add(1)
				return tools.ResultSuccess("effect"), nil
			}}
			a, configuredPermissions := configuredCodeMode(t, inner)
			policy := &codeModePolicy{}
			sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
			checker := newDenyChecker("effect")
			if lane == "argument-deny" {
				checker = configuredPermissions
			}
			if lane == "dynamic-guard" {
				policy.guard = func(in *hooks.Input) *hooks.Result {
					if in.ToolName == "effect" && in.ToolInput["value"] == "blocked" {
						return &hooks.Result{Allowed: false, Message: "guard denied"}
					}
					return nil
				}
			}
			d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: policy}
			if lane != "dynamic-guard" {
				d.Permissions = func(*session.Session) []toolexec.NamedChecker {
					return []toolexec.NamedChecker{{Checker: checker, Tier: toolexec.TierTeam, Source: "config"}}
				}
			}
			em := &captureEmitter{}
			direct := tools.ToolCall{ID: "direct", Function: tools.FunctionCall{Name: "effect", Arguments: `{"value":"blocked"}`}}
			d.Process(t.Context(), sess, []tools.ToolCall{direct}, []tools.Tool{inner}, em)
			require.Zero(t, effects.Load(), "direct control")
			aggregate, err := a.Tools(t.Context())
			require.NoError(t, err)
			d.Process(t.Context(), sess, []tools.ToolCall{{ID: "outer", Function: tools.FunctionCall{Name: "run_tools_with_javascript", Arguments: `{"script":"const name='effect'; return globalThis[name]({value:'blocked'});"}`}}}, aggregate, em)
			require.Zero(t, effects.Load(), "outer approval must not approve inner effect")
			require.Equal(t, []string{"effect", "run_tools_with_javascript", "effect"}, policy.seen)
			require.Contains(t, policy.approvals, "effect:deny")
		})
	}
}

func TestConfiguredCodeModeInnerConfirmationContextAndAdmission(t *testing.T) {
	owner := tools.NewResourceOwner()
	type key struct{}
	ctx := tools.WithResourceOwner(context.WithValue(t.Context(), key{}, "scope"), owner)
	var effects atomic.Int32
	inner := tools.Tool{Name: "effect", Category: "shell", Parameters: tools.MustSchemaFor[map[string]any](), Handler: func(ctx context.Context, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
		require.Same(t, owner, tools.ResourceOwnerFromContext(ctx))
		require.Equal(t, "scope", ctx.Value(key{}))
		require.Equal(t, "effect", tc.Function.Name)
		rt.EmitOutput(ctx, "stream")
		effects.Add(1)
		return tools.ResultSuccess("raw inner"), nil
	}}
	a, _ := configuredCodeMode(t, inner)
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	policy := &codeModePolicy{guard: func(in *hooks.Input) *hooks.Result {
		if in.ToolName == "effect" {
			return &hooks.Result{Allowed: true, Decision: hooks.DecisionAsk}
		}
		return nil
	}}
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
	permit := make(chan struct{}, 1)
	var admissions []string
	d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: policy, Resume: resume, AcquireTool: func(ctx context.Context, _, name string) (func(), error) {
		select {
		case permit <- struct{}{}:
			admissions = append(admissions, name)
			return func() { <-permit }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	aggregate, err := a.Tools(ctx)
	require.NoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	em := &captureEmitter{}
	d.Process(bounded, sess, []tools.ToolCall{{ID: "outer", Function: tools.FunctionCall{Name: "run_tools_with_javascript", Arguments: `{"script":"return effect({value:'allowed'});"}`}}}, aggregate, em)
	require.NoError(t, bounded.Err())
	require.EqualValues(t, 1, effects.Load())
	require.Equal(t, []string{"run_tools_with_javascript", "effect", "run_tools_with_javascript"}, admissions)
	require.Contains(t, policy.approvals, "effect:allow")
	require.Empty(t, permit)
	require.Len(t, em.confirmations, 1)
	require.Equal(t, "effect", em.confirmations[0].Function.Name)
	require.NotEmpty(t, em.confirmationMeta[0]["safety_label"])
	require.Len(t, em.outputs, 1)
	require.NotEqual(t, "outer", em.outputs[0].ToolCallID)
	require.Equal(t, "transformed inner", em.outputs[0].Output)
	require.Len(t, em.responses, 2)
	require.Equal(t, "transformed inner", em.responses[0].Output)
	require.Contains(t, em.responses[1].Output, "transformed inner")
	require.Len(t, em.messages, 1, "only aggregate owns a model conversation slot")
}

func TestConfiguredCodeModeNestedStopCannotBeCaught(t *testing.T) {
	var effects atomic.Int32
	inner := tools.Tool{Name: "effect", Parameters: tools.MustSchemaFor[map[string]any](), Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		effects.Add(1)
		return tools.ResultSuccess("done"), nil
	}}
	a, _ := configuredCodeMode(t, inner)
	policy := &codeModePolicy{post: func(in *hooks.Input) *hooks.Result {
		if in.ToolName == "effect" {
			return &hooks.Result{Allowed: false, Message: "nested stop"}
		}
		return nil
	}}
	d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: policy}
	aggregate, err := a.Tools(t.Context())
	require.NoError(t, err)
	stop, message := d.Process(t.Context(), session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), []tools.ToolCall{{ID: "outer", Function: tools.FunctionCall{Name: "run_tools_with_javascript", Arguments: `{"script":"try { effect({}); } catch (_) {} try { effect({}); } catch (_) {} return \"caught\";"}`}}}, aggregate, &captureEmitter{})
	require.True(t, stop)
	require.Equal(t, "nested stop", message)
	require.EqualValues(t, 1, effects.Load())
}
