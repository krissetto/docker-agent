package app

import (
	"context"
	"testing"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/stretchr/testify/require"
)

func TestBackgroundOutputInterestTracksFeedingObservation(t *testing.T) {
	a := &App{}
	var seeds []map[string][]string
	var calls [][]string
	handle := &projectionSession{id: "s", observe: func(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
		seeds = append(seeds, options.BackgroundOutputInterest)
		return runtime.Observation{SetBackgroundOutputInterest: func(id string, ids []string) error {
			require.Equal(t, "s", id)
			calls = append(calls, ids)
			return nil
		}}, nil
	}}
	observer := a.backgroundOutputObserver(handle, "s")
	observation, err := observer.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	old := a.backgroundProcessOutputInterestSetter()
	require.NoError(t, old([]string{"job"}))
	require.Equal(t, [][]string{{"job"}}, calls)
	observation.Cancel()
	require.NoError(t, old([]string{"reconnecting"}))
	require.Len(t, calls, 1)
	_, err = observer.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"reconnecting"}, seeds[1]["s"])
	require.NoError(t, old([]string{"resumed"}))
	require.Equal(t, []string{"resumed"}, calls[len(calls)-1])
	current := a.backgroundProcessOutputInterestSetter()
	require.NoError(t, current([]string{"new"}))
	a.clearBackgroundOutputObservation()
	require.Nil(t, calls[len(calls)-1])
	require.ErrorIs(t, current([]string{"late"}), runtime.ErrSessionClosed)
	replacement := a.backgroundOutputObserver(handle, "other")
	_, err = replacement.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	require.Empty(t, seeds[2]["other"])
	require.NotContains(t, seeds[2], "s")
	_, err = observer.Observe(t.Context(), runtime.ObserveOptions{})
	require.ErrorIs(t, err, runtime.ErrSessionClosed)
}
