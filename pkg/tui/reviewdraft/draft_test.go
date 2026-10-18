package reviewdraft

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sampleAnchor() Anchor {
	return Anchor{
		RepositoryID: "repo-id", RepositoryName: "docker-agent", WorktreeID: "wt-id",
		WorktreePath: "worktree-x", Branch: "feature/x", Path: "pkg/app/app.go",
		Mode: "changes", Group: "unstaged", OldStart: 10, OldEnd: 11,
		NewStart: 10, NewEnd: 12, Context: []string{" line", "-old", "+new"}, Version: "hash-v1",
	}
}

func sampleTarget() Target {
	return Target{Kind: TargetSession, SessionID: "session-1", AgentName: "root", Source: "team.yaml", WorkingDir: "/workspace", RouteID: "tab-1", RouteGeneration: 5}
}

func TestDraftAddEditDeleteAndLiteralSerialize(t *testing.T) {
	d := New("workspace-anchor")
	c, err := d.Add(sampleAnchor(), "  Why was this changed?  ")
	require.NoError(t, err)
	require.Equal(t, "Why was this changed?", c.Body)
	serialized := Serialize(d.Snapshot().Comments)
	for _, want := range []string{
		`Repository: "docker-agent" ("repo-id")`,
		`Checkout: "worktree-x" ("wt-id")`,
		`Branch: "feature/x"`,
		`File: "pkg/app/app.go"`,
		"Old lines: 10-11", "New lines: 10-12", "-old",
		"Comment (literal user text):", "Why was this changed?",
	} {
		require.Contains(t, serialized, want)
	}
	require.NoError(t, d.Edit(c.ID, "changed body"))
	require.NotContains(t, Serialize(d.Snapshot().Comments), "Why was this changed?")
	require.NoError(t, d.Delete(c.ID))
	require.Empty(t, d.Snapshot().Comments)
}

func TestBeginSendRetriesExactAttemptAndAcceptedClearsOnlyCapturedVersions(t *testing.T) {
	d := New("workspace")
	first, err := d.Add(sampleAnchor(), "first")
	require.NoError(t, err)
	attempt, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	again, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.Equal(t, attempt, again)

	// Transport ambiguity preserves exact target, request id, payload and snapshot.
	require.NoError(t, d.Fail(attempt.RequestID, context.DeadlineExceeded))
	retry, ok := d.Attempt()
	require.True(t, ok)
	require.Equal(t, attempt, retry)

	// Concurrent additions/edits survive admission of the older payload.
	second, err := d.Add(sampleAnchor(), "second")
	require.NoError(t, err)
	require.NoError(t, d.Edit(first.ID, "edited after preview"))
	require.NoError(t, d.MarkAccepted(Receipt{RequestID: attempt.RequestID, SessionID: attempt.Target.SessionID, Accepted: true, Durable: false}))
	snap := d.Snapshot()
	require.Len(t, snap.Comments, 2)
	require.Equal(t, StatusIdle, snap.Status)
	require.Nil(t, snap.Attempt)
	require.NotEqual(t, first.Version, snap.Comments[0].Version)
	require.Equal(t, second.ID, snap.Comments[1].ID)
}

func TestBeginSendCapturesAttachedAndNewTargets(t *testing.T) {
	for _, target := range []Target{
		{Kind: TargetAttached, SessionID: "child", ParentSessionID: "parent", NodeID: "node", RouteID: "tab", RouteGeneration: 3},
		{Kind: TargetNewSession, SessionID: "reserved", AgentName: "reviewer", Source: "team.yaml", WorkingDir: "/work"},
	} {
		d := New("workspace")
		_, err := d.Add(sampleAnchor(), "check this")
		require.NoError(t, err)
		attempt, err := d.BeginSend(target)
		require.NoError(t, err)
		require.Equal(t, target, attempt.Target)
		require.NotEmpty(t, attempt.Fingerprint)
	}
}

func TestAnchorVersionChangesWithSelectedRawContext(t *testing.T) {
	a := sampleAnchor()
	first := AnchorVersion("scope", "fingerprint", 4, a)
	a.Context[2] = "+different"
	require.NotEqual(t, first, AnchorVersion("scope", "fingerprint", 4, a))
	require.NotEqual(t, first, AnchorVersion("scope", "fingerprint2", 4, sampleAnchor()))
}

func TestSerializeDoesNotInterpretSlashOrShellLookingComments(t *testing.T) {
	d := New("workspace")
	a := sampleAnchor()
	a.Context = []string{"+`! rm -rf /"}
	_, err := d.Add(a, "/changes ! do not execute")
	require.NoError(t, err)
	body := Serialize(d.Snapshot().Comments)
	require.Contains(t, body, "/changes ! do not execute")
	require.Contains(t, body, "`! rm -rf /")
	require.False(t, strings.HasPrefix(body, "/changes"))
}

func TestBeginSendDoesNotRetargetPendingAttempt(t *testing.T) {
	d := New("workspace")
	_, err := d.Add(sampleAnchor(), "comment")
	require.NoError(t, err)
	first, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	other := sampleTarget()
	other.SessionID = "other-session"
	_, err = d.BeginSend(other)
	require.ErrorIs(t, err, ErrAttemptMismatch)
	retry, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.Equal(t, first, retry)
}

func TestMarkAcceptedRejectsErrorAndRetainsDraft(t *testing.T) {
	d := New("workspace")
	_, err := d.Add(sampleAnchor(), "comment")
	require.NoError(t, err)
	attempt, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.ErrorIs(t, d.MarkAccepted(Receipt{RequestID: attempt.RequestID, SessionID: attempt.Target.SessionID, Accepted: true, Err: context.DeadlineExceeded}), context.DeadlineExceeded)
	require.Len(t, d.Snapshot().Comments, 1)
	pending, ok := d.Attempt()
	require.True(t, ok)
	require.Equal(t, attempt, pending)
}

func TestCancelLeavesAttemptForSameTargetRetry(t *testing.T) {
	d := New("workspace")
	_, err := d.Add(sampleAnchor(), "comment")
	require.NoError(t, err)
	attempt, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	// Closing a preview is not an admission result; without Fail or MarkAccepted,
	// the next confirmed retry uses the exact same immutable attempt.
	retry, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.Equal(t, attempt, retry)
	require.Len(t, d.Snapshot().Comments, 1)
}
