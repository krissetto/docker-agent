package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/version"
)

// ManagedAgent borrows the authenticated API's canonical sessions, execution
// and persistence. This connection owns only its submitted turns, never the API.
type ManagedAgent struct {
	*Agent
	client     *runtime.Client
	transport  *runtime.SessionTransport
	source     string
	workingDir string
	agentName  string
}

var _ acpsdk.Agent = (*ManagedAgent)(nil)

func NewManagedAgent(client *runtime.Client, source, workingDir string) (*ManagedAgent, error) {
	if source == "" || !filepath.IsAbs(workingDir) {
		return nil, errors.New("managed ACP requires a source and absolute working directory")
	}
	canonical, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return nil, err
	}
	workingDir = canonical
	transport, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
	if err != nil {
		return nil, err
	}
	core := NewAgent(nil, nil, nil)
	core.managed = true
	return &ManagedAgent{Agent: core, client: client, transport: transport, source: source, workingDir: workingDir}, nil
}

func RunManaged(ctx context.Context, client *runtime.Client, source, workingDir string, stdin io.Reader, stdout io.Writer) error {
	agent, err := NewManagedAgent(client, source, workingDir)
	if err != nil {
		return err
	}
	conn := acpsdk.NewAgentSideConnection(agent, stdout, stdin)
	conn.SetLogger(slog.Default())
	agent.SetAgentConnection(conn)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		agent.Stop(cleanup)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

func (a *ManagedAgent) Initialize(ctx context.Context, _ acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	// Resolve metadata from the authenticated authoritative API, never load a team
	// or honor client filesystem/terminal capabilities in this process.
	cfg, err := a.client.GetAgent(ctx, a.source)
	if err != nil {
		return acpsdk.InitializeResponse{}, err
	}
	if len(cfg.Agents) == 0 {
		return acpsdk.InitializeResponse{}, errors.New("managed ACP source has no agents")
	}
	a.mu.Lock()
	a.agentName = cfg.Agents[0].Name
	a.mu.Unlock()
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo:       &acpsdk.Implementation{Name: "docker agent", Version: version.Version},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession:         true,
			SessionCapabilities: acpsdk.SessionCapabilities{Close: &acpsdk.SessionCloseCapabilities{}, List: &acpsdk.SessionListCapabilities{}, Resume: &acpsdk.SessionResumeCapabilities{}},
			PromptCapabilities:  acpsdk.PromptCapabilities{EmbeddedContext: true, Image: true},
		},
	}, nil
}

func (a *ManagedAgent) NewSession(ctx context.Context, p acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	if len(p.McpServers) != 0 || len(p.AdditionalDirectories) != 0 {
		return acpsdk.NewSessionResponse{}, errors.New("managed ACP does not support client MCP servers or additional directories")
	}
	cwd, err := a.workspace(p.Cwd)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	a.mu.Lock()
	err = a.admissionErrorLocked()
	name := a.agentName
	a.mu.Unlock()
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	if name == "" {
		return acpsdk.NewSessionResponse{}, errors.New("managed ACP not initialized")
	}
	// Strict is explicit: an author default must never silently turn ACP tool
	// permission requests into blanket approval.
	template := session.New(session.WithWorkingDir(cwd), session.WithSafetyPolicy(session.SafetyPolicyStrict))
	handle, err := a.transport.CreateSession(ctx, template, runtime.SessionBinding{AgentName: name})
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	if snapshot.ID != handle.ID() || snapshot.WorkingDir != cwd || snapshot.ParentID != "" || snapshot.GetSafetyPolicy() != session.SafetyPolicyStrict || snapshot.AttributesSnapshot()["docker-agent.actor.source"] != a.source {
		return acpsdk.NewSessionResponse{}, errors.New("managed ACP created session authority mismatch")
	}
	s := &Session{id: handle.ID(), sess: snapshot, session: handle, rt: a.transport, workingDir: snapshot.WorkingDir, drainTurn: true, rejectConcurrent: true}
	if a.registerSessionIfAbsent(s) != registrationStored {
		return acpsdk.NewSessionResponse{}, errors.New("managed ACP session registration rejected")
	}
	return acpsdk.NewSessionResponse{SessionId: acpsdk.SessionId(s.id)}, nil
}

// workspace checks the real path, not a lexical prefix (including symlink escapes).
func (a *ManagedAgent) workspace(requested string) (string, error) {
	if requested == "" {
		return a.workingDir, nil
	}
	if !filepath.IsAbs(requested) {
		return "", errors.New("managed ACP requires an absolute working directory")
	}
	cwd, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(a.workingDir, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("managed ACP working directory is outside the managed workspace")
	}
	return cwd, nil
}

func (a *ManagedAgent) validateView(sess *session.Session) error {
	if sess == nil || sess.ParentID != "" || sess.AttributesSnapshot()["docker-agent.actor.source"] != a.source {
		return errors.New("managed ACP session is not a root conversation from the configured team")
	}
	if sess.WorkingDir == "" {
		return errors.New("managed ACP session has no confirmed workspace")
	}
	cwd, err := a.workspace(sess.WorkingDir)
	if err != nil {
		return err
	}
	if cwd != sess.WorkingDir {
		return errors.New("managed ACP persisted workspace is not canonical")
	}
	return nil
}

func (a *ManagedAgent) LoadSession(ctx context.Context, p acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	if _, err := a.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: p.SessionId, Cwd: p.Cwd, McpServers: p.McpServers, AdditionalDirectories: p.AdditionalDirectories}); err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	return a.replaySession(ctx, string(p.SessionId))
}

func (a *ManagedAgent) ResumeSession(ctx context.Context, p acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	if len(p.McpServers) != 0 || len(p.AdditionalDirectories) != 0 {
		return acpsdk.ResumeSessionResponse{}, errors.New("managed ACP does not support client MCP servers or additional directories")
	}
	if p.Cwd != "" {
		if _, err := a.workspace(p.Cwd); err != nil {
			return acpsdk.ResumeSessionResponse{}, err
		}
	}
	a.mu.Lock()
	err := a.admissionErrorLocked()
	initialized := a.agentName != ""
	a.mu.Unlock()
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	if !initialized {
		return acpsdk.ResumeSessionResponse{}, errors.New("managed ACP not initialized")
	}
	prepared, err := a.transport.PrepareSessionView(ctx, string(p.SessionId))
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	defer prepared.Abort()
	info := prepared.Info()
	if err := a.validateView(info.Session); err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	if p.Cwd != "" {
		cwd, err := a.workspace(p.Cwd)
		if err != nil || cwd != info.WorkingDir {
			return acpsdk.ResumeSessionResponse{}, errors.New("managed ACP requested cwd differs from the persisted session workspace")
		}
	}
	committed, err := prepared.Commit(ctx)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	// Open-view is deliberately non-executing: pending UI work and approvals
	// remain owned by their original client. Prompt has atomic idle admission.
	candidate := &Session{id: string(p.SessionId), sess: committed.Info.Session, session: committed.SessionHandle, rt: a.transport, workingDir: info.WorkingDir, drainTurn: true, rejectConcurrent: true}
	switch outcome := a.registerSessionIfAbsent(candidate); outcome {
	case registrationStored, registrationDuplicate:
		return acpsdk.ResumeSessionResponse{}, nil
	default:
		return acpsdk.ResumeSessionResponse{}, fmt.Errorf("managed ACP session registration rejected: %v", outcome)
	}
}

func (a *ManagedAgent) ListSessions(ctx context.Context, p acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	if p.Cursor != nil && *p.Cursor != "" {
		return acpsdk.ListSessionsResponse{}, errors.New("managed ACP list does not issue cursors")
	}
	cwd := ""
	if p.Cwd != nil {
		var err error
		cwd, err = a.workspace(*p.Cwd)
		if err != nil {
			return acpsdk.ListSessionsResponse{}, err
		}
	}
	rows, err := a.transport.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{})
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}
	out := acpsdk.ListSessionsResponse{Sessions: []acpsdk.SessionInfo{}}
	for _, row := range rows {
		if row.Source != a.source || row.ParentID != "" || row.WorkingDir == "" {
			continue
		}
		if resolved, err := a.workspace(row.WorkingDir); err != nil || resolved != row.WorkingDir {
			continue
		}
		if cwd != "" && cwd != row.WorkingDir {
			continue
		}
		info := acpsdk.SessionInfo{SessionId: acpsdk.SessionId(row.SessionID), Title: &row.Title, Cwd: row.WorkingDir}
		if row.UpdatedAt != "" {
			info.UpdatedAt = &row.UpdatedAt
		}
		out.Sessions = append(out.Sessions, info)
	}
	return out, nil
}
func (a *ManagedAgent) Prompt(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	for _, block := range p.Prompt {
		if block.Text == nil && block.Image == nil && (block.Resource == nil || block.Resource.Resource.TextResourceContents == nil) {
			return acpsdk.PromptResponse{}, errors.New("managed ACP supports only text, images and embedded text context")
		}
	}
	return a.Agent.Prompt(ctx, p)
}
