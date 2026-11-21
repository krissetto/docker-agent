package app

import (
	"context"
	"slices"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/tools"
)

type backgroundOutputObservation struct {
	sessionID  string
	desired    []string
	setter     func(string, []string) error
	generation uint64
	live       bool
}

type backgroundOutputObserver struct {
	app      *App
	observer runtimeclient.Observer
	binding  *backgroundOutputObservation
}

func (a *App) clearBackgroundOutputObservation() {
	a.backgroundOutputMu.Lock()
	defer a.backgroundOutputMu.Unlock()
	if binding := a.backgroundOutput; binding != nil && binding.setter != nil {
		_ = binding.setter(binding.sessionID, nil)
	}
	a.backgroundOutput = nil
}

func (a *App) backgroundOutputObserver(observer runtimeclient.Observer, sessionID string) runtimeclient.Observer {
	a.backgroundOutputMu.Lock()
	defer a.backgroundOutputMu.Unlock()
	binding := &backgroundOutputObservation{sessionID: sessionID}
	a.backgroundOutput = binding
	return &backgroundOutputObserver{app: a, observer: observer, binding: binding}
}

func (o *backgroundOutputObserver) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	a := o.app
	a.backgroundOutputMu.Lock()
	defer a.backgroundOutputMu.Unlock()
	if a.backgroundOutput != o.binding {
		return runtime.Observation{}, runtime.ErrSessionClosed
	}
	options.BackgroundOutputInterest = map[string][]string{o.binding.sessionID: slices.Clone(o.binding.desired)}
	observation, err := o.observer.Observe(ctx, options)
	if err != nil {
		return observation, err
	}
	o.binding.setter = observation.SetBackgroundOutputInterest
	o.binding.live = true
	o.binding.generation++
	generation := o.binding.generation
	cancel := observation.Cancel
	observation.Cancel = func() {
		a.backgroundOutputMu.Lock()
		if a.backgroundOutput == o.binding && o.binding.generation == generation {
			o.binding.setter = nil
			o.binding.live = false
		}
		a.backgroundOutputMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return observation, nil
}

// A captured setter follows reconnects, but cannot target a replacement session.
func (a *App) backgroundProcessOutputInterestSetter() func([]string) error {
	a.backgroundOutputMu.Lock()
	binding := a.backgroundOutput
	a.backgroundOutputMu.Unlock()
	return func(processIDs []string) error {
		a.backgroundOutputMu.Lock()
		defer a.backgroundOutputMu.Unlock()
		if binding == nil {
			return tools.ErrBackgroundProcessOutputUnsupported
		}
		if a.backgroundOutput != binding {
			return runtime.ErrSessionClosed
		}
		binding.desired = slices.Clone(processIDs)
		if binding.setter == nil {
			if binding.live {
				return tools.ErrBackgroundProcessOutputUnsupported
			}
			return nil
		}
		return binding.setter(binding.sessionID, processIDs)
	}
}
