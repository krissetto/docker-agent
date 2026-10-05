package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

// SessionTitleGenerator owns auxiliary title work independently of turn execution.
type SessionTitleGenerator interface {
	GenerateSessionTitle(ctx context.Context, generator *sessiontitle.Generator, messages []string, replace bool) error
}

func (h *sessionHandle) GenerateSessionTitle(ctx context.Context, generator *sessiontitle.Generator, messages []string, replace bool) error {
	d := h.driver
	var operation uint64
	var workerCtx context.Context
	var cancel context.CancelFunc
	var snapshot *session.Session
	var models []provider.Provider
	err := d.ownerCall(ctx, func() error {
		if d.stopped || d.sess == nil {
			return ErrSessionStopped
		}
		if d.titleStatus == "started" {
			return ErrSessionCapacity
		}
		if !replace && (d.sess.TitleSnapshot() != "" || d.titleStatus != "") {
			return nil
		}
		snapshot = d.sess.Clone()
		models = append([]provider.Provider(nil), d.modelProviders...)
		d.titleGeneration++
		operation = d.titleGeneration
		workerCtx, cancel = context.WithTimeout(d.r.lifetime(), 30*time.Second) //nolint:fatcontext // owner reserves independently rooted auxiliary work; joined worker owns cancellation
		d.titleCancel = cancel
		d.titleStatus = "started"
		d.wg.Add(1)
		d.publishTitleStatusLocked("started", "")
		return nil
	})
	if err != nil || operation == 0 {
		return err
	}
	go func() {
		defer d.wg.Done()
		defer cancel()
		a, agentErr := h.runtime.team.Agent(snapshot.AgentName)
		var title string
		err := agentErr
		if err == nil {
			scoped := agent.WithContextModels(workerCtx, snapshot.AgentName, models)
			if generator == nil {
				generator = sessiontitle.New(a.TitleModels(scoped)...)
			}
			if generator == nil {
				err = ErrUnsupported
			} else {
				origin := h.runtime.messageOrigin(snapshot, a, "title")
				generator = generator.WithPreparation(func(ctx context.Context, model provider.Provider, messages []chat.Message) ([]chat.Message, error) {
					return h.runtime.prepareOutboundMessages(ctx, origin, model.ID().String(), nil, messages)
				})
				title, err = generator.Generate(scoped, h.sessionID, messages)
			}
		}
		if err == nil && workerCtx.Err() != nil {
			err = workerCtx.Err()
		}
		if err == nil && title != "" {
			writeCtx, stopWrite := context.WithTimeout(d.r.durabilityContext(), 5*time.Second)
			defer stopWrite()
			err = d.durableIOContext(writeCtx, func() (sessionIOReservation, error) {
				if workerCtx.Err() != nil || d.stopped || operation != d.titleGeneration || d.titleStatus != "started" {
					return sessionIOReservation{}, ErrSessionStopped
				}
				d.titleWriteReserved = true
				return sessionIOReservation{write: func(ctx context.Context) error {
					if d.r.sessionStore != nil {
						return d.r.sessionStore.UpdateSessionTitle(ctx, d.identityID, title)
					}
					return nil
				}, commit: func(err error) error {
					d.titleWriteReserved = false
					if err != nil {
						return err
					}
					if operation != d.titleGeneration {
						return ErrSessionStopped
					}
					d.sess.SetTitle(title)
					d.titleStatus = "completed"
					d.titleCancel = nil
					d.publishTitleStatusLocked("completed", title)
					return nil
				}}, nil
			}, true)
		}
		_ = d.ownerCall(context.WithoutCancel(workerCtx), func() error {
			if operation != d.titleGeneration || d.titleStatus != "started" {
				return nil
			}
			status := "failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || d.stopped {
				status = "canceled"
			}
			d.titleStatus = status
			d.titleCancel = nil
			d.publishTitleStatusLocked(status, "")
			return nil
		})
	}()
	return nil
}

func (d *sessionDriver) publishTitleStatusLocked(status, title string) {
	event := SessionTitle(d.identityID, title).(*SessionTitleEvent)
	event.Status = status
	d.events.Publish(d.identityID, event)
}

func (d *sessionDriver) invalidateTitleLocked() {
	// The durable reservation is the commit point; cancellation cannot retract an accepted write.
	if d.titleWriteReserved {
		return
	}
	d.titleGeneration++
	if d.titleCancel != nil {
		d.titleCancel()
		d.titleCancel = nil
	}
	if d.titleStatus == "started" {
		d.titleStatus = "canceled"
		d.publishTitleStatusLocked("canceled", "")
	}
}
