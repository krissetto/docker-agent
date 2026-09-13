package dialog

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type dialogScheduler struct{ now time.Time }

func newDialogRuntime() *animation.Runtime {
	s := &dialogScheduler{now: time.Unix(1, 0)}
	return animation.NewRuntimeWithScheduler(s)
}
func (s *dialogScheduler) Now() time.Time { return s.now }
func (s *dialogScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return f(s.now) }
}

func acceptedDialogTick(r *animation.Runtime, cmd tea.Cmd) animation.TickMsg {
	msg, ok := r.Accept(cmd().(animation.TickMsg))
	if !ok {
		panic("tick rejected")
	}
	return msg
}

func advanceDialog(r *animation.Runtime, cmd tea.Cmd, target time.Duration) animation.TickMsg {
	var msg animation.TickMsg
	for r.Now() < target {
		msg = acceptedDialogTick(r, cmd)
		cmd = r.Continue()
	}
	return msg
}
