package root

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// newRemoteClient never places credentials in argv, URLs, or diagnostics.
func newRemoteClient(address, tokenFile string) (*runtime.Client, error) {
	var opts []runtime.ClientOption
	if tokenFile != "" {
		value, err := readPrivateAuthToken(tokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, runtime.WithAuthToken(value))
	}
	return runtime.NewClient(address, opts...)
}

func (b *remoteBackend) openSession(ctx context.Context, client *runtime.Client, transport *runtime.SessionTransport, req runtime.CreateSessionRequest) (runtime.SessionHandle, *session.Session, error) {
	id := req.ResumeSessionID
	if session.IsRelativeSessionRef(id) {
		rows, err := transport.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{})
		if err != nil {
			return nil, nil, err
		}
		rows = slices.DeleteFunc(rows, func(row runtime.SessionSummaryEntry) bool {
			return row.ParentID != "" || row.Source != b.agentFileName || (req.WorkingDir != "" && row.WorkingDir != req.WorkingDir)
		})
		slices.SortStableFunc(rows, func(a, b runtime.SessionSummaryEntry) int {
			if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
				return n
			}
			return strings.Compare(a.SessionID, b.SessionID)
		})
		offset, _ := strconv.Atoi(id)
		if offset >= 0 || offset == -int(^uint(0)>>1)-1 || -offset > len(rows) {
			return nil, nil, fmt.Errorf("session offset %s out of range (have %d sessions for this source/workspace)", id, len(rows))
		}
		id = rows[-offset-1].SessionID
	}
	var handle runtime.SessionHandle
	var sess *session.Session
	if id != "" {
		var err error
		reader := runtime.SessionViewInfoReader(transport)
		info, readErr := reader.ConfirmedSessionViewInfo(ctx, id)
		err = readErr
		if err == nil {
			sess = info.Session
			if sess == nil || sess.ID != id {
				return nil, nil, errors.New("remote snapshot has invalid session identity")
			}
			if sess.AttributesSnapshot()[sessionActorSourceAttribute] != b.agentFileName {
				return nil, nil, errors.New("remote session belongs to a different source")
			}
			if req.WorkingDir != "" && sess.WorkingDir != req.WorkingDir {
				return nil, nil, errors.New("remote session belongs to a different workspace")
			}
			if sess.ParentID != "" {
				return nil, nil, errors.New("child sessions require confirmed session view attachment")
			}
			committed, commitErr := runtime.RestoreSessionView(ctx, transport, id)
			err = commitErr
			handle, sess = committed.SessionHandle, committed.Info.Session
		}

		if err != nil {
			var sessionErr *runtime.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound || session.IsRelativeSessionRef(req.ResumeSessionID) {
				return nil, nil, err
			}
			handle = nil
		}
	}
	agentName := req.AgentName
	if handle != nil {
		agentName = handle.AgentName()
	}
	if agentName == "" {
		cfg, err := client.GetAgent(ctx, b.agentFileName)
		if err != nil {
			return nil, nil, err
		}
		if len(cfg.Agents) == 0 {
			return nil, nil, errors.New("remote source has no agents")
		}
		agentName = cfg.Agents[0].Name
	}
	model, err := b.remoteStartupModel(ctx, client, agentName)
	if err != nil {
		return nil, nil, err
	}
	if handle == nil {
		template := session.New(session.WithAgentName(agentName), session.WithWorkingDir(req.WorkingDir), session.WithToolsApproved(req.ToolsApproved), session.WithSafetyPolicy(req.SafetyPolicy))
		if id != "" {
			template.ID = id
		}
		if p := req.GlobalPermissions; p != nil && !p.IsEmpty() {
			template.Permissions = &session.PermissionsConfig{Allow: p.AllowPatterns(), Ask: p.AskPatterns(), Deny: p.DenyPatterns()}
		}
		binding := runtime.SessionBinding{AgentName: agentName, Model: model}
		if id != "" {
			creator, ok := any(transport).(runtime.SessionIDCreator)
			if !ok {
				return nil, nil, errors.New("remote server does not support caller-supplied session IDs")
			}
			handle, err = creator.CreateSessionWithID(ctx, template, binding, id)
		} else {
			handle, err = transport.CreateSession(ctx, template, binding)
		}
		if err != nil {
			return nil, nil, err
		}
		// Do not accept a peer that silently ignored a caller-owned identity.
		if id != "" && handle.ID() != id {
			return nil, nil, errors.New("remote server did not preserve requested session ID")
		}
	} else {
		if model != "" {
			if !handle.Metadata().Capabilities.ModelSwitching {
				return nil, nil, errors.New("remote server does not support per-session --model overrides")
			}
			if err := handle.SetModel(ctx, model); err != nil {
				return nil, nil, err
			}
		}
		if req.SafetyExplicit && req.SafetyPolicy != "" {
			if _, err := handle.Edit(ctx, runtime.SessionEdit{Kind: runtime.SessionEditPolicy, SafetyPolicy: &req.SafetyPolicy}); err != nil {
				return nil, nil, err
			}
		}
	}
	sess, err = handle.Snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil || sess.ID != handle.ID() {
		return nil, nil, errors.New("remote snapshot has invalid session identity")
	}
	sess.AgentName = handle.AgentName()
	sess.HideToolResults = req.HideToolResults
	return handle, sess, nil
}

// Generic remote clients cannot change the server's shared agent configuration.
// Accept only overrides confined to this root binding; managed bootstrap applies
// the full ordered configuration overrides on the server instead.
func (b *remoteBackend) remoteStartupModel(ctx context.Context, client *runtime.Client, agentName string) (string, error) {
	if len(b.flags.modelOverrides) == 0 {
		return "", nil
	}
	cfg, err := client.GetAgent(ctx, b.agentFileName)
	if err != nil {
		return "", err
	}
	originalModels := maps.Clone(cfg.Models)
	for _, override := range b.flags.modelOverrides {
		for part := range strings.SplitSeq(override, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			target, _, targeted := strings.Cut(part, "=")
			if (targeted && strings.TrimSpace(target) != agentName) || (!targeted && len(cfg.Agents) != 1) {
				return "", errors.New("remote --model may only override the selected root agent; use managed API startup for team-wide overrides")
			}
		}
	}
	if err := config.ApplyModelOverrides(cfg, b.flags.modelOverrides); err != nil {
		return "", err
	}
	for _, agent := range cfg.Agents {
		if agent.Name == agentName {
			model := cfg.Models[agent.Model]
			effective := model.Clone()
			config.ApplyModelOverridePolicy(cfg, agentName, agent.Model, effective)
			if _, existed := originalModels[agent.Model]; !existed && effective.ParallelToolCalls != nil {
				return "", errors.New("remote --model requires server-side policy inheritance; use managed API startup to preserve parallel_tool_calls")
			}
			return agent.Model, nil
		}
	}
	return "", fmt.Errorf("remote agent %q not found", agentName)
}
