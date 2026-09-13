package app

import "context"

func (a *App) initBus(ctx context.Context) {
	a.busMu.Lock()
	defer a.busMu.Unlock()
	if a.busContext == nil {
		a.busContext, a.busCancel = context.WithCancel(ctx)
		a.busDone = make(chan struct{})
	}
}

func (a *App) busLifetime() context.Context {
	a.busMu.Lock()
	ctx := a.busContext
	a.busMu.Unlock()
	if ctx != nil {
		return ctx
	}
	a.initBus(a.ctx())
	a.busMu.Lock()
	defer a.busMu.Unlock()
	return a.busContext
}

func (a *App) stopBus() {
	a.busMu.Lock()
	cancel := a.busCancel
	a.busMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close detaches presentation workers. It never cancels or releases execution.
func (a *App) Close() {
	a.stopBus()
	a.fenceSessionBridge()
}
