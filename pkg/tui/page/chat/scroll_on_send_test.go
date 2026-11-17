package chat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

type sentInputSpy struct {
	list.Model
	turns []string
}

func (s *sentInputSpy) ScrollToSentInput(turn string) { s.turns = append(s.turns, turn) }

func TestScrollOnSendLocalAcceptanceExclusionsAndDedupe(t *testing.T) {
	p, _ := newSteerablePage(t)
	p.SetScrollOnSend(true)
	spy := &sentInputSpy{Model: p.messages}
	p.messages = spy
	accepted := localSendAcceptedMsg{inputResult: inputResult{p.inputScope}, submission: runtime.Submission{SessionID: p.app.Session().ID, TurnID: "local"}}
	queued := accepted
	queued.submission.TurnID, queued.submission.Disposition = "queued", runtime.SubmissionDispositionQueued
	foreign := accepted
	foreign.submission.SessionID = "other"
	empty := accepted
	empty.submission.TurnID = ""
	for _, msg := range []localSendAcceptedMsg{queued, foreign, empty} {
		p.Update(msg)
	}
	require.Empty(t, spy.turns)
	p.Update(accepted)
	p.Update(accepted)
	require.Equal(t, []string{"local"}, spy.turns)
	p.SetScrollOnSend(false)
	accepted.submission.TurnID = "disabled"
	p.Update(accepted)
	require.Len(t, spy.turns, 1)
	p.SetScrollOnSend(true)
	Cleanup(p)
	accepted.submission.TurnID = "closed"
	p.Update(accepted)
	require.Len(t, spy.turns, 1)
}

func TestScrollOnSendRuntimeReplayAndBusyAdmissionNeverArm(t *testing.T) {
	p, _ := newSteerablePage(t)
	p.SetScrollOnSend(true)
	spy := &sentInputSpy{Model: p.messages}
	p.messages = spy
	for _, event := range []runtime.Event{
		runtime.UserMessage("external or replay", p.app.Session().ID, nil),
		runtime.PendingUserMessageAccepted(p.app.Session().ID, "queued", "later", nil, 1),
		runtime.PendingUserMessagePromoted(p.app.Session().ID, "queued", "later", nil, 1),
	} {
		p.Update(msgtypes.SessionRuntimeEventMsg{OriginSessionID: p.app.Session().ID, Event: event, Seed: true})
	}
	for _, send := range []msgtypes.SendMsg{{Content: "steer", Steer: true}, {Content: "queue", Queue: true}} {
		p.working = true
		_, cmd := p.handleSendMsg(send)
		for _, result := range flattenCmdMsgs(cmd) {
			p.Update(result)
		}
	}
	require.Empty(t, spy.turns)
}

func TestScrollOnSendIdleSubmissionPublishesOnlyAcceptedLocalReceipt(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/rejected=%v", enabled, rejected), func(t *testing.T) {
				p, handle := newSteerablePage(t)
				p.working = false
				p.SetScrollOnSend(enabled)
				if rejected {
					handle.submitErr = errors.New("rejected input")
				}
				spy := &sentInputSpy{Model: p.messages}
				p.messages = spy
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ready, done := make(chan struct{}), make(chan struct{})
				events := make(chan any, 32)
				go func() {
					defer close(done)
					p.app.Subscribe(ctx, func(msg any) { events <- msg }, app.SubscribeOptions{Ready: ready})
				}()
				<-ready
				p.handleSendMsg(msgtypes.SendMsg{Content: "new question"})
				if rejected || enabled {
					select {
					case msg := <-events:
						if rejected {
							require.IsType(t, &runtime.ErrorEvent{}, msg)
							require.Equal(t, "rejected input", msg.(*runtime.ErrorEvent).Error)
						} else {
							receipt, ok := msg.(localSendAcceptedMsg)
							require.True(t, ok)
							require.Equal(t, "submit", receipt.submission.TurnID)
							p.Update(receipt)
							require.Equal(t, []string{"submit"}, spy.turns)
						}
					case <-time.After(time.Second):
						t.Fatal("no local receipt/error")
					}
				} else {
					require.Eventually(t, func() bool { return len(handle.submitted()) == 1 }, time.Second, time.Millisecond)
				}
				// A second publication would be a duplicate error/acceptance effect.
				select {
				case msg := <-events:
					t.Fatalf("unexpected additional event: %T", msg)
				case <-time.After(100 * time.Millisecond):
				}
				cancel()
				<-done
				Cleanup(p)
			})
		}
	}
}
