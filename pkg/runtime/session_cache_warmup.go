package runtime

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// SessionCacheWarmer starts best-effort managed API prefix warming without
// admitting a turn, executing tools, or retaining a conversation.
type SessionCacheWarmer interface{ WarmSessionCache() }

func (h *sessionHandle) WarmSessionCache() {
	// Even reservation runs off the HTTP/attachment path. Never hold the owner
	// while loading tools, running hooks, or waiting for the model.
	go h.warmSessionCache()
}

func (h *sessionHandle) warmSessionCache() {
	d, r := h.driver, h.runtime
	ctx, cancel := context.WithTimeout(r.lifetime(), 30*time.Second)
	defer cancel()
	var snapshot *session.Session
	var generation uint64
	var scoped context.Context
	reserved := false
	var bindingVersion uint64
	err := d.ownerCall(ctx, func() error {
		if d.stopped || d.reclaiming || d.running() || d.starting() || d.settling() || d.sess == nil {
			return nil
		}
		scoped = agent.WithContextModels(ctx, d.AgentNameLocked(), slices.Clone(d.modelProviders))
		if d.teamEnvironment != nil {
			scoped = context.WithValue(scoped, teamEnvironmentContextKey{}, d.teamEnvironment.Environment())
		}
		snapshot, generation, bindingVersion = d.sess.OwnSnapshot(), d.generation, d.bindingVersion
		return nil
	})
	if err != nil || snapshot == nil {
		return
	}
	a := r.resolveSessionAgentContext(scoped, snapshot)
	if a == nil || a.HasHarness() {
		return
	}
	model := a.Model(scoped)
	if model == nil {
		return
	}
	config := model.BaseConfig().ModelConfig
	if config.IsImageOnly() {
		return
	}
	key := a.Name() + "\x00" + model.ID().String()
	err = d.ownerCall(ctx, func() error {
		if d.stopped || d.reclaiming || d.running() || d.starting() || d.settling() || d.generation != generation || d.bindingVersion != bindingVersion || d.cacheWarmups[key] {
			return nil
		}
		if d.cacheWarmups == nil {
			d.cacheWarmups = make(map[string]bool)
		}
		d.cacheWarmups[key] = true
		d.wg.Add(1)
		reserved = true
		return nil
	})
	if err != nil || !reserved {
		return
	}
	defer d.wg.Done()
	ctx, release, err := r.captureEnvironmentForOperation(scoped, snapshot)
	if err != nil {
		return
	}
	defer release()
	a = r.resolveSessionAgentContext(ctx, snapshot)
	if a == nil || a.HasHarness() {
		return
	}
	discard := EventSinkFunc(func(Event) {})
	agentTools, err := r.getTools(ctx, snapshot, a, trace.SpanFromContext(ctx), discard, false)
	if err != nil {
		return
	}
	agentTools = r.prepareTurnTools(ctx, snapshot, a, agentTools)
	start := r.executeSessionStartHooks(ctx, snapshot, a, discard)
	turn := r.executeTurnStartHooks(ctx, snapshot, a, discard)
	sources := instructionSources(start.messages, nil, turn, start.sources...)
	messages, state, _ := r.messagesWithDynamicContext(ctx, snapshot, a, sources, slices.Concat(start.legacyMessages(), turn.legacyMessages()))
	// Freeze the same initial dynamic prefix the real turn will reuse. A real
	// turn that has already begun wins; warmup never overwrites its state.
	if state != nil {
		accepted := false
		err = d.ownerCall(ctx, func() error {
			if d.stopped || d.generation != generation || d.bindingVersion != bindingVersion || d.running() || d.starting() {
				return nil
			}
			if d.sess.InstructionContext == nil {
				d.sess.InstructionContext = state
			}
			snapshot.InstructionContext = d.sess.OwnSnapshot().InstructionContext
			accepted = true
			return nil
		})
		if err != nil || !accepted {
			return
		}
		messages, _, _ = r.messagesWithDynamicContext(ctx, snapshot, a, sources, nil)
	}
	// Only the invariant prefix is useful; do not send or retain the session's
	// existing conversation for an auxiliary greeting.
	prefix := make([]chat.Message, 0, len(messages)+1)
	for _, message := range messages {
		if message.Role == chat.MessageRoleSystem {
			prefix = append(prefix, message)
		}
	}
	prefix = append(prefix, chat.Message{Role: chat.MessageRoleUser, Content: "hi"})
	stop, _, rewritten := r.executeBeforeLLMCallHooks(ctx, snapshot, a, model.ID().String(), 1, prefix)
	if stop {
		return
	}
	if rewritten != nil {
		prefix = rewritten
	}
	prefix, err = r.prepareMessagesForModel(ctx, snapshot, a, model, prefix)
	if err != nil {
		return
	}
	agentTools = r.toolDeferrals.MarkAt(snapshot.ID, "", agentTools)
	agentTools = toolsForProvider(ctx, model, agentTools)
	stream, err := model.CreateChatCompletionStream(ctx, prefix, agentTools)
	if err != nil {
		return
	}
	m, _ := r.modelsStore.GetModel(ctx, model.ID())
	res, streamErr := handleStream(ctx, nil, stream, a, agentTools, snapshot, m, r.telemetry, discard, streamIdleTimeoutFor(model))
	// Usage, when available, is billed even if a response ends in an error.
	if res.Usage == nil {
		return
	}
	id, cfg := model.ID(), model.BaseConfig()
	if res.Serving != nil {
		id, cfg = res.Serving.ID, res.Serving.Config
		m, _ = r.modelsStore.GetModel(ctx, id)
	}
	cost := computeMessageCost(res.Usage, applyModelCost(m, id, res.Usage, cfg))
	if len(res.UsageAttempts) > 0 {
		cost = attemptsCost(ctx, res.UsageAttempts, dispatchedConfig(res, model.BaseConfig()), r.modelsStore)
	}
	var amount float64
	if cost != nil {
		amount = *cost
	}
	r.telemetry.RecordTokenUsage(ctx, id.String(), res.Usage.PromptTokens(), res.Usage.OutputTokens, amount)
	r.recordBudgetForContext(ctx, snapshot, a, res.Usage, cost, 0, discard)
	record := session.AuxiliaryUsage{Agent: a.Name(), Model: id.String(), Cost: amount, Usage: *res.Usage}
	writeCtx, stopWrite := context.WithTimeout(r.durabilityContext(), defaultSubagentPersistenceTimeout)
	defer stopWrite()
	err = d.recordCacheWarmupUsage(writeCtx, record, r.resolveContextLimit(ctx, model, id), a.CompactionThreshold())
	if err != nil {
		slog.WarnContext(ctx, "Failed to save cache warmup accounting", "session_id", h.ID(), "error", err)
	}
	if streamErr != nil {
		slog.DebugContext(ctx, "Cache warmup stream ended early", "error", streamErr)
	}
}

func (r *LocalRuntime) prepareTurnTools(ctx context.Context, sess *session.Session, a *agent.Agent, agentTools []tools.Tool) []tools.Tool {
	agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)
	agentTools = r.skillSubSessionTools(ctx, sess, a, agentTools)
	agentTools = enrichSpawnToolDescription(a, agentTools, r.allowedFromAgentForTeam(r.teamForContext(ctx, sess), a))
	if model := a.Model(ctx); model != nil {
		config := model.BaseConfig().ModelConfig
		if config.IsImageOnly() {
			// Image-only children cannot use automatic coordination tools. Keep
			// explicit tools so the provider can reject incompatible config.
			return r.filterSessionDelegationToolsContext(ctx, sess, a.FilterTools(agentTools))
		}
	}
	return r.filterSessionDelegationToolsContext(ctx, sess, a.FilterTools(addConfiguredSubagentTools(sess, a, r.teamForContext(ctx, sess), addListAgentsTool(sess, a, addAsyncChildTools(sess, agentTools)))))
}

func (d *sessionDriver) recordCacheWarmupUsage(ctx context.Context, record session.AuxiliaryUsage, contextLimit int64, threshold float64) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		next := d.sess.OwnSnapshot()
		next.AddAuxiliaryUsage(record)
		input, output := next.Usage()
		next.SetTokensAndCost(input, output, next.TotalCost())
		return sessionIOReservation{write: func(ctx context.Context) error {
			if d.r.sessionStore != nil {
				return d.r.sessionStore.UpdateSession(ctx, next)
			}
			return nil
		}, commit: func(err error) error {
			if err != nil {
				return err
			}
			d.sess.AddAuxiliaryUsage(record)
			usage := SessionUsage(d.sess, contextLimit, threshold)
			usage.Auxiliary = d.sess.AuxiliaryUsageSnapshot()
			d.events.Publish(d.identityID, NewTokenUsageEvent(d.identityID, record.Agent, usage))
			return nil
		}}, nil
	})
}
