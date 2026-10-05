package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

// MessageTransform is the in-process-only handler signature for a
// before_llm_call transform that rewrites the chat messages about to
// be sent to the model. It receives the full message slice in chain
// order and returns the (possibly-rewritten) replacement.
//
// Transforms are intentionally a runtime-private contract: the cost of
// JSON-roundtripping a full conversation through the cross-process
// hook protocol would be prohibitive, so command and model hooks
// cannot rewrite messages. Embedders register transforms via
// [WithMessageTransform]; the runtime ships
// [BuiltinStripUnsupportedModalities] out of the box.
//
// Transforms run AFTER the standard before_llm_call gate (see
// [LocalRuntime.executeBeforeLLMCallHooks]) — a hook that wants to
// abort the call should target the gate, not a transform.
//
// Returning a non-nil error logs a warning and falls through to the
// previous message slice; a transform failure must never break the
// run loop.
type MessageTransform func(ctx context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error)

// registeredTransform pairs a [MessageTransform] with the name it was
// registered under. The name is purely diagnostic — it shows up in
// slog records when a transform errors out — so re-registering the
// same name simply appends another entry without any de-duplication.
type registeredTransform struct {
	name   string
	fn     MessageTransform
	policy bool
}

// WithMessageTransform registers a [MessageTransform] under name so
// it is applied to every LLM call, in registration order, after the
// before_llm_call gate. Transforms are runtime-global: per-agent
// scoping (if needed) lives in the transform body, where
// [hooks.Input.AgentName] is available — the runtime-shipped strip
// transform is an example.
//
// Empty name or nil fn are silently ignored, matching the no-error
// shape of the other [Opt] helpers.
func WithMessageTransform(name string, fn MessageTransform) Opt {
	return func(r *LocalRuntime) {
		if name == "" || fn == nil {
			slog.Warn("Ignoring message transform with empty name or nil fn", "name", name)
			return
		}
		r.transforms = append(r.transforms, registeredTransform{name: name, fn: fn})
	}
}

// WithMessagePolicy registers an authoritative outbound rewrite/check. Policies
// run after projections on every provider attempt (including fallbacks), auxiliary
// completions, and harness input. An error or panic prevents delivery; use
// WithMessageTransform only for best-effort projections, never for redaction or authorization.
func WithMessagePolicy(name string, fn MessageTransform) Opt {
	return func(r *LocalRuntime) {
		if name == "" || fn == nil {
			fn = func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) {
				return nil, errors.New("message policy requires a non-empty name and handler")
			}
		}
		r.transforms = append(r.transforms, registeredTransform{name: name, fn: fn, policy: true})
	}
}

func copyTransformInput(in *hooks.Input, msgs []chat.Message) (*hooks.Input, []chat.Message, error) {
	data, err := json.Marshal(msgs)
	if err != nil {
		return nil, nil, err
	}
	var snapshot []chat.Message
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, nil, err
	}
	copy := *in
	if in.ModelCapabilities != nil {
		caps := *in.ModelCapabilities
		copy.ModelCapabilities = &caps
	}
	return &copy, snapshot, nil
}

func invokeMessageTransform(ctx context.Context, fn MessageTransform, in *hooks.Input, msgs []chat.Message) (out []chat.Message, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("message handler panicked: %v", p)
		}
	}()
	// Execution already owns this worker: cancellation must not abandon a callback.
	out, err = fn(ctx, in, msgs)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

type outboundOrigin struct {
	sessionID, rootSessionID, agentName, cwd, purpose string
}

type outboundOriginKey struct{}

func (r *LocalRuntime) messageOrigin(sess *session.Session, a *agent.Agent, purpose string) outboundOrigin {
	if sess == nil || a == nil {
		return outboundOrigin{purpose: purpose}
	}
	cwd := sess.WorkingDir
	if cwd == "" {
		cwd = r.workingDir
	}
	return outboundOrigin{sess.ID, r.todoRootSessionID(sess.ID), a.Name(), cwd, purpose}
}

func (r *LocalRuntime) contextMessageOrigin(ctx context.Context, purpose string) outboundOrigin {
	if origin, ok := ctx.Value(outboundOriginKey{}).(outboundOrigin); ok {
		origin.purpose = purpose
		return origin
	}
	if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && identity.driver != nil {
		sess := identity.driver.session()
		return r.messageOrigin(sess, r.resolveSessionAgent(sess), purpose)
	}
	if id := genai.ConversationIDFromContext(ctx); id != "" {
		if driver, ok := r.sessionDrivers.Lookup(id); ok {
			sess := driver.session()
			return r.messageOrigin(sess, r.resolveSessionAgent(sess), purpose)
		}
	}
	return outboundOrigin{purpose: purpose}
}

func (r *LocalRuntime) mandatoryMessagePolicies() []registeredTransform {
	if r.messagePolicies != nil {
		return r.messagePolicies
	}
	return r.transforms
}

// prepareOutboundMessages is the mandatory boundary, without optional hook dispatch.
func (r *LocalRuntime) prepareOutboundMessages(ctx context.Context, origin outboundOrigin, modelID string, caps *modelinfo.ModelCapabilities, msgs []chat.Message) ([]chat.Message, error) {
	in := &hooks.Input{SessionID: origin.sessionID, RootSessionID: origin.rootSessionID, AgentName: origin.agentName, ModelID: modelID, ModelCapabilities: caps, HookEventName: hooks.EventBeforeLLMCall, Cwd: origin.cwd, CallPurpose: origin.purpose}
	for _, t := range r.mandatoryMessagePolicies() {
		if !t.policy {
			continue
		}
		if origin.sessionID == "" || origin.agentName == "" {
			return nil, errors.New("message policy requires an originating session and agent")
		}
		input, snapshot, err := copyTransformInput(in, msgs)
		if err == nil {
			msgs, err = invokeMessageTransform(ctx, t.fn, input, snapshot)
		}
		if err != nil {
			return nil, fmt.Errorf("message policy %q for session %q: %w", t.name, origin.sessionID, err)
		}
	}
	return msgs, nil
}

func (r *LocalRuntime) applyMessagePolicies(ctx context.Context, sess *session.Session, a *agent.Agent, modelID string, caps *modelinfo.ModelCapabilities, msgs []chat.Message) ([]chat.Message, error) {
	origin, ok := ctx.Value(outboundOriginKey{}).(outboundOrigin)
	if !ok {
		origin = r.messageOrigin(sess, a, "ordinary")
	}
	return r.prepareOutboundMessages(ctx, origin, modelID, caps, msgs)
}

// applyBeforeLLMCallTransforms runs every registered
// [MessageTransform] in chain order, just before the model call and
// AFTER [LocalRuntime.executeBeforeLLMCallHooks] has approved it.
// Errors from individual transforms are logged at warn level and the
// chain continues with the previous slice — a transform failure must
// never break the run loop.
//
// modelID is the canonical model identifier the loop has just
// resolved (after per-tool overrides and alloy-mode selection);
// transforms read it via [hooks.Input.ModelID]. Calling
// agent.Model() from a transform would re-randomize the alloy pick
// and miss the per-tool override.
//
// caps is the model's already-resolved attachment capability set
// (explicit `capabilities:` config override applied — see
// [modelinfo.ResolveCapsFromModel]); transforms read it via
// [hooks.Input.ModelCapabilities]. nil means the caller has no
// capability information (e.g. the coding-harness path) and
// capability-gated transforms must not act.
func (r *LocalRuntime) prepareMessagesForModel(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	model provider.Provider,
	msgs []chat.Message,
) ([]chat.Message, error) {
	modelID := model.ID()
	catalogModel, err := r.modelsStore.GetModel(ctx, modelID)
	if err != nil {
		slog.DebugContext(ctx, "Failed to resolve model capabilities for message transforms", "model", modelID.String(), "error", err)
	}
	cfg := model.BaseConfig()
	caps := modelinfo.ResolveCapsFromModel(catalogModel, cfg.CapsOverride())
	if catalogModel == nil && cfg.CapsOverride() == nil {
		caps = providerFallbackCaps(ctx, cfg.ModelConfig, modelID)
	}
	projected := r.filterDelegationMessages(a, sess, r.applyBeforeLLMCallTransforms(ctx, sess, a, modelID.String(), &caps, msgs))
	return r.applyMessagePolicies(ctx, sess, a, modelID.String(), &caps, projected)
}

func providerFallbackCaps(ctx context.Context, cfg latest.ModelConfig, id modelsdev.ID) modelinfo.ModelCapabilities {
	if cfg.Provider == "dmr" {
		return modelinfo.CapsWith(providerOptBool(cfg.ProviderOpts, "supports_images"), providerOptBool(cfg.ProviderOpts, "supports_pdf"), false, false)
	}
	if cfg.Provider == "anthropic" || modelinfo.IsClaude(ctx, nil, id) {
		return modelinfo.CapsWith(true, true, false, false)
	}
	return modelinfo.ModelCapabilities{}
}

func providerOptBool(opts map[string]any, key string) bool {
	switch value := opts[key].(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(value)
		return err == nil && parsed
	default:
		return false
	}
}

func (r *LocalRuntime) applyBeforeLLMCallTransforms(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	modelID string,
	caps *modelinfo.ModelCapabilities,
	msgs []chat.Message,
) []chat.Message {
	if len(r.transforms) == 0 {
		return msgs
	}
	in := &hooks.Input{
		SessionID:         sess.ID,
		RootSessionID:     r.todoRootSessionID(sess.ID),
		AgentName:         a.Name(),
		ModelID:           modelID,
		ModelCapabilities: caps,
		HookEventName:     hooks.EventBeforeLLMCall,
		Cwd:               r.workingDir,
	}
	for _, t := range r.transforms {
		if t.policy {
			continue
		}
		input, snapshot, err := copyTransformInput(in, msgs)
		var out []chat.Message
		if err == nil {
			out, err = invokeMessageTransform(ctx, t.fn, input, snapshot)
		}
		if err != nil {
			slog.WarnContext(ctx, "Message transform failed; continuing with previous messages",
				"transform", t.name, "agent", a.Name(), "error", err)
			continue
		}
		msgs = out
	}
	return msgs
}
