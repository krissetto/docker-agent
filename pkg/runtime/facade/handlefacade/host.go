// Package handlefacade adapts the branch session runtime (runtime.SessionRuntime
// and runtime.SessionHandle) to the facade core. Borrowed runtimes are never shut
// down here; the adapter releases only the observations it opened.
package handlefacade

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

const defaultPageSize = 50

// ErrNoSessionCatalog reports a registry that cannot list sessions. List is a
// mandatory facade operation, so such a registry is refused rather than given a
// List that cannot be truthful.
var ErrNoSessionCatalog = errors.New("session registry offers neither a summary catalog nor a session catalog")

// NewHost wraps a borrowed session registry as an facade.Host. The registry must
// implement runtime.SessionSummaryCatalog or runtime.SessionCatalog; the summary
// catalog is preferred when both exist.
func NewHost(rt runtime.SessionRuntime) (facade.Host, error) {
	if _, summary := rt.(runtime.SessionSummaryCatalog); !summary {
		if _, catalog := rt.(runtime.SessionCatalog); !catalog {
			return nil, ErrNoSessionCatalog
		}
	}
	return &host{rt: rt, sessions: map[*sessionImpl]struct{}{}}, nil
}

type host struct {
	rt runtime.SessionRuntime

	mu       sync.Mutex
	closed   bool
	sessions map[*sessionImpl]struct{}
}

func (h *host) Create(ctx context.Context, options facade.CreateOptions) (facade.Session, error) {
	if err := h.check(ctx); err != nil {
		return nil, err
	}
	opts := []session.Opt{}
	if options.WorkingDir != "" {
		opts = append(opts, session.WithWorkingDir(options.WorkingDir))
	}
	sess := session.New(opts...)
	sess.AgentName = options.AgentName
	if options.AgentName != "" {
		sess.SetAttribute(runtime.SessionAgentAttribute, options.AgentName)
	}
	handle, err := h.rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: options.AgentName, Model: options.Model})
	if err != nil {
		return nil, err
	}
	return h.adopt(handle)
}

func (h *host) Open(ctx context.Context, sessionID string) (facade.Session, error) {
	if err := h.check(ctx); err != nil {
		return nil, err
	}
	var handle runtime.SessionHandle
	var err error
	if loader, ok := h.rt.(runtime.SessionLoader); ok {
		handle, _, err = loader.LoadSession(ctx, sessionID)
	} else {
		handle, err = h.rt.SessionByID(sessionID)
	}
	if err != nil {
		return nil, err
	}
	if hydrator, ok := handle.(runtime.Hydrator); ok {
		if err := hydrator.Hydrate(ctx); err != nil {
			return nil, err
		}
	}
	return h.adopt(handle)
}

func (h *host) List(ctx context.Context, options facade.ListOptions) (facade.Page, error) {
	if err := ctx.Err(); err != nil {
		return facade.Page{}, err
	}
	summaries, err := h.summaries(ctx)
	if err != nil {
		return facade.Page{}, err
	}
	limit := options.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	page := facade.Page{}
	started := options.Cursor == ""
	for _, summary := range summaries {
		if !started {
			started = summary.SessionID == options.Cursor
			continue
		}
		if options.WorkingDir != "" && summary.WorkingDir != options.WorkingDir {
			continue
		}
		if len(page.Sessions) == limit {
			page.Next = page.Sessions[len(page.Sessions)-1].SessionID
			break
		}
		page.Sessions = append(page.Sessions, summary)
	}
	return page, nil
}

// summaries reads the registry's root-session rows, in the registry's own order.
// The summary catalog is preferred; the session catalog is the fallback.
func (h *host) summaries(ctx context.Context) ([]facade.Summary, error) {
	if catalog, ok := h.rt.(runtime.SessionSummaryCatalog); ok {
		rows, err := catalog.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{})
		if err != nil {
			return nil, err
		}
		out := make([]facade.Summary, 0, len(rows))
		for _, row := range rows {
			out = append(out, facade.Summary{
				SessionID:   row.SessionID,
				AgentName:   row.AgentName,
				Title:       row.Title,
				WorkingDir:  row.WorkingDir,
				CreatedAt:   row.CreatedAt,
				UpdatedAt:   parseUpdated(row.UpdatedAt),
				NumMessages: row.NumMessages,
				Cost:        row.Cost,
				Starred:     row.Starred,
				Loadable:    row.Loadable,
			})
		}
		return out, nil
	}
	catalog, ok := h.rt.(runtime.SessionCatalog)
	if !ok {
		return nil, ErrNoSessionCatalog
	}
	rows, err := catalog.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]facade.Summary, 0, len(rows))
	for _, row := range rows {
		out = append(out, facade.Summary{
			SessionID:   row.SessionID,
			AgentName:   row.AgentName,
			Title:       row.Title,
			WorkingDir:  row.WorkingDir,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   parseUpdated(row.UpdatedAt),
			NumMessages: row.NumMessages,
			Cost:        row.Cost,
			Starred:     row.Starred,
			Loadable:    row.Loadable,
		})
	}
	return out, nil
}

func (h *host) Delete(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.rt.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	h.mu.Lock()
	var released []*sessionImpl
	for sess := range h.sessions {
		if sess.handle.ID() == sessionID {
			released = append(released, sess)
		}
	}
	h.mu.Unlock()
	for _, sess := range released {
		_ = sess.Close(ctx)
	}
	return nil
}

func (h *host) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	list := make([]*sessionImpl, 0, len(h.sessions))
	for sess := range h.sessions {
		list = append(list, sess)
	}
	h.mu.Unlock()
	var errs []error
	for _, sess := range list {
		errs = append(errs, sess.Close(ctx))
	}
	return errors.Join(errs...)
}

func (h *host) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return facade.ErrSessionClosed
	}
	return nil
}

func (h *host) adopt(handle runtime.SessionHandle) (facade.Session, error) {
	sess := newSession(h, handle)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, facade.ErrSessionClosed
	}
	h.sessions[sess] = struct{}{}
	return sess, nil
}

func (h *host) forget(sess *sessionImpl) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, sess)
}

// titleGenerator exposes the runtime's generator when the registry provides one.
func (h *host) titleGenerator(ctx context.Context) *sessiontitle.Generator {
	if provider, ok := h.rt.(interface {
		TitleGenerator(ctx context.Context) *sessiontitle.Generator
	}); ok {
		return provider.TitleGenerator(ctx)
	}
	return nil
}

// deriveTurnID mirrors the runtime's request-to-turn identity so a subscriber
// can be registered before admission. Submit verifies the match.
func deriveTurnID(sessionID, requestID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + requestID))
	return hex.EncodeToString(sum[:])
}

func newRequestID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create request id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}
