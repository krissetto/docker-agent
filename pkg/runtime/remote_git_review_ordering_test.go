package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/gitreview"
)

// orderingTransport controls when browse and directory responses return, so
// tests can interleave them with newer snapshots.
type orderingTransport struct {
	mu                   sync.Mutex
	snapshotGeneration   uint64
	snapshotStarted      chan struct{}
	snapshotGate         chan struct{}
	browseGeneration     uint64
	browseStarted        chan struct{}
	browseGate           chan struct{}
	directoryStarted     chan struct{}
	directoryGate        chan struct{}
	worktreeGenerations  []uint64
	directoryGenerations []uint64
}

func (o *orderingTransport) setSnapshotGeneration(g uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.snapshotGeneration = g
}

func (o *orderingTransport) GitReviewSnapshot(context.Context, string) (gitreview.Snapshot, error) {
	o.mu.Lock()
	generation, started, gate := o.snapshotGeneration, o.snapshotStarted, o.snapshotGate
	o.mu.Unlock()
	if started != nil {
		close(started)
	}
	if gate != nil {
		<-gate
	}
	return gitreview.Snapshot{Generation: generation}, nil
}

func (o *orderingTransport) GitReviewBrowse(context.Context, string) (gitreview.Snapshot, error) {
	if o.browseStarted != nil {
		close(o.browseStarted)
	}
	if o.browseGate != nil {
		<-o.browseGate
	}
	return gitreview.Snapshot{Generation: o.browseGeneration, Browse: true}, nil
}

func (o *orderingTransport) GitReviewWorktree(_ context.Context, _ string, generation uint64, _ string) (gitreview.Worktree, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.worktreeGenerations = append(o.worktreeGenerations, generation)
	return gitreview.Worktree{}, nil
}

func (o *orderingTransport) GitReviewDirectory(_ context.Context, _ string, generation uint64, _, _ string) (gitreview.Directory, error) {
	o.mu.Lock()
	o.directoryGenerations = append(o.directoryGenerations, generation)
	o.mu.Unlock()
	if o.directoryStarted != nil {
		close(o.directoryStarted)
	}
	if o.directoryGate != nil {
		<-o.directoryGate
	}
	return gitreview.Directory{}, nil
}

func (o *orderingTransport) GitReviewDocument(context.Context, string, uint64, gitreview.DocumentKey) (gitreview.Document, error) {
	return gitreview.Document{}, nil
}

func (o *orderingTransport) recordedDirectoryGenerations() []uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]uint64(nil), o.directoryGenerations...)
}

func (o *orderingTransport) recordedWorktreeGenerations() []uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]uint64(nil), o.worktreeGenerations...)
}

func TestGitReviewRemoteLateBrowseCannotRegressGeneration(t *testing.T) {
	transport := &orderingTransport{browseGeneration: 1, browseStarted: make(chan struct{}), browseGate: make(chan struct{})}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)

	browsed := make(chan error, 1)
	go func() {
		_, err := provider.BrowseRoot(t.Context())
		browsed <- err
	}()
	<-transport.browseStarted

	transport.setSnapshotGeneration(2)
	snapshot, err := provider.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(2), snapshot.Generation)

	close(transport.browseGate)
	require.NoError(t, <-browsed)

	_, err = provider.LoadWorktree(t.Context(), "wt")
	require.NoError(t, err)
	require.Equal(t, []uint64{2}, transport.recordedWorktreeGenerations(), "the late browse response left generation 2 in place")
}

func TestGitReviewRemoteLatestSnapshotMayLowerGenerationAfterReset(t *testing.T) {
	transport := &orderingTransport{}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)

	transport.setSnapshotGeneration(5)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	transport.setSnapshotGeneration(1)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	_, err = provider.LoadWorktree(t.Context(), "wt")
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, transport.recordedWorktreeGenerations(), "an explicit full refresh after a server reset adopts its generation")
}

func TestGitReviewRemoteDirectoryAfterNewerSnapshotIsStale(t *testing.T) {
	transport := &orderingTransport{directoryStarted: make(chan struct{}), directoryGate: make(chan struct{})}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)

	transport.setSnapshotGeneration(2)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	listed := make(chan error, 1)
	go func() {
		_, err := provider.Directory(t.Context(), "wt", "")
		listed <- err
	}()
	<-transport.directoryStarted

	transport.setSnapshotGeneration(3)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	close(transport.directoryGate)
	require.Error(t, <-listed, "a directory listed under generation 2 must not return after generation 3")
}

func TestGitReviewRemoteFullSnapshotOutranksOverlappingBrowse(t *testing.T) {
	transport := &orderingTransport{browseGeneration: 1, snapshotStarted: make(chan struct{}), snapshotGate: make(chan struct{})}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)

	transport.setSnapshotGeneration(2)
	snapshotted := make(chan error, 1)
	go func() {
		_, err := provider.Snapshot(t.Context())
		snapshotted <- err
	}()
	<-transport.snapshotStarted

	_, err = provider.BrowseRoot(t.Context())
	require.NoError(t, err)

	close(transport.snapshotGate)
	require.NoError(t, <-snapshotted)

	_, err = provider.LoadWorktree(t.Context(), "wt")
	require.NoError(t, err)
	require.Equal(t, []uint64{2}, transport.recordedWorktreeGenerations(), "a browse that completed during the full scan must not leave generation 1")
}

func TestGitReviewRemoteBrowseProvisionallyServesDuringPendingFull(t *testing.T) {
	transport := &orderingTransport{browseGeneration: 1, snapshotStarted: make(chan struct{}), snapshotGate: make(chan struct{})}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)

	transport.setSnapshotGeneration(2)
	snapshotted := make(chan error, 1)
	go func() {
		_, err := provider.Snapshot(t.Context())
		snapshotted <- err
	}()
	<-transport.snapshotStarted

	_, err = provider.BrowseRoot(t.Context())
	require.NoError(t, err)
	_, err = provider.Directory(t.Context(), "wt", "")
	require.NoError(t, err)

	close(transport.snapshotGate)
	require.NoError(t, <-snapshotted)
	_, err = provider.Directory(t.Context(), "wt", "")
	require.NoError(t, err)
	require.Equal(t, []uint64{1, 2}, transport.recordedDirectoryGenerations(), "Files serves the browse generation while the full scan is pending, then the completed scan's generation")
}
