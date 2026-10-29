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
	parsed, ok := ParseReferences(serialized)
	require.True(t, ok)
	require.Len(t, parsed.Comments, 1)
	require.Equal(t, ReferenceGitChange, parsed.Comments[0].Kind)
	require.Equal(t, "docker-agent", parsed.Comments[0].Repository)
	require.Equal(t, "worktree-x", parsed.Comments[0].Checkout)
	require.Equal(t, "feature/x", parsed.Comments[0].Branch)
	require.Equal(t, "pkg/app/app.go", parsed.Comments[0].Path)
	require.Equal(t, []string{" line", "-old", "+new"}, parsed.Comments[0].Snippet)
	require.Equal(t, "Why was this changed?", parsed.Comments[0].Comment)
	require.NotContains(t, serialized, c.ID)
	require.NotContains(t, serialized, c.Anchor.Version)
	require.NoError(t, d.Edit(c.ID, "changed body"))
	parsed, ok = ParseReferences(Serialize(d.Snapshot().Comments))
	require.True(t, ok)
	require.Equal(t, "changed body", parsed.Comments[0].Comment)
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

func TestGeneralOnlySendAndVersionedAcceptance(t *testing.T) {
	d := New("workspace")
	require.NoError(t, d.SetGeneral("context"))
	attempt, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	parsed, ok := ParseReferences(attempt.Payload)
	require.True(t, ok)
	require.Equal(t, "context", parsed.General)
	require.NoError(t, d.SetGeneral("newer context"))
	require.NoError(t, d.MarkAccepted(Receipt{RequestID: attempt.RequestID, SessionID: attempt.Target.SessionID, Accepted: true}))
	require.Equal(t, "newer context", d.Snapshot().General, "an edit after the captured send survives acceptance")
}

func TestRetryResetsFailedAttemptToSending(t *testing.T) {
	d := New("workspace")
	_, err := d.Add(sampleAnchor(), "comment")
	require.NoError(t, err)
	first, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.NoError(t, d.Fail(first.RequestID, context.DeadlineExceeded))
	require.Equal(t, StatusFailed, d.Snapshot().Status)
	retry, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.Equal(t, first, retry)
	snap := d.Snapshot()
	require.Equal(t, StatusSending, snap.Status)
	require.Empty(t, snap.Err)
}

func TestCharacterRangeAllowsEarlierEndColumnOnLaterLine(t *testing.T) {
	d := New("workspace")
	a := sampleAnchor()
	a.CharacterRange = true
	a.StartColumn, a.EndColumn = 10, 2
	a.Context = []string{"+first selected line", "+second selected line"}
	_, err := d.Add(a, "multiline snippet")
	require.NoError(t, err)
	a.Context = []string{"+one line"}
	_, err = d.Add(a, "invalid same-line columns")
	require.ErrorIs(t, err, ErrInvalidAnchor)
}

func TestSerializeTypedReferencesMixedAndGeneralOnly(t *testing.T) {
	git := sampleAnchor()
	git.CharacterRange = true
	git.StartColumn, git.EndColumn = 2, 5
	files := sampleAnchor()
	files.Mode, files.Group, files.Path, files.Context = "files", "", "README.md", []string{"# title\r", "@line"}
	comments := []Comment{
		{ID: "secret-id", Anchor: git, Body: "Review this\nplease"},
		{ID: "other-id", Anchor: files, Body: "Clarify it"},
	}
	payload := SerializeWithGeneral(comments, "General / note")
	parsed, ok := ParseReferences(payload)
	require.True(t, ok)
	require.Equal(t, ReferenceGitChange, parsed.Comments[0].Kind)
	require.Equal(t, ReferenceFile, parsed.Comments[1].Kind)
	require.Equal(t, git.Context, parsed.Comments[0].Snippet)
	require.Equal(t, git.StartColumn, parsed.Comments[0].CharacterRange.Start)
	require.Equal(t, git.EndColumn, parsed.Comments[0].CharacterRange.End)
	require.Equal(t, files.Context, parsed.Comments[1].Snippet, "CR and leading @ survive JSON encoding")
	require.Equal(t, comments[0].Body, parsed.Comments[0].Comment)
	require.Equal(t, "General / note", parsed.General)
	require.NotContains(t, payload, "secret-id")
	require.NotContains(t, payload, "hash-v1")
	require.NotContains(t, payload, "External")

	readable, ok := RenderReferences(payload)
	require.True(t, ok)
	require.Contains(t, readable, "Git · pkg/app/app.go:10-11→10-12 · unstaged")
	require.Contains(t, readable, "README.md:10-11→10-12")
	require.Contains(t, readable, "Review this\nplease")
	require.Contains(t, readable, "General / note")
	require.NotContains(t, readable, "secret-id")

	fileOnly := Serialize([]Comment{{Anchor: files, Body: "file only"}})
	fileReadable, ok := RenderReferences(fileOnly)
	require.True(t, ok)
	require.True(t, strings.HasPrefix(fileReadable, "README.md:10-11→10-12\n"))
	require.NotContains(t, fileReadable, "review")
	require.NotContains(t, fileReadable, "Files ·")

	generalOnly := SerializeWithGeneral(nil, "only context")
	generalReadable, ok := RenderReferences(generalOnly)
	require.True(t, ok)
	require.Equal(t, "only context", generalReadable)
	generalParsed, ok := ParseReferences(generalOnly)
	require.True(t, ok)
	require.Empty(t, generalParsed.Comments)
	require.Equal(t, "only context", generalParsed.General)
}

func TestParseReferencesStrictFallbackAndQueuedTrailingNewline(t *testing.T) {
	body := SerializeWithGeneral([]Comment{{Anchor: sampleAnchor(), Body: "body"}}, "")
	readable, ok := RenderReferences(body + "\n")
	require.True(t, ok, "runtime appends a newline to some queued user inputs")
	require.Contains(t, readable, "body")
	for _, malformed := range []string{
		"prefix " + body,
		body + " suffix",
		strings.Replace(body, `"repository":`, `"repo":`, 1),
		strings.Replace(body, `"kind":"git-change"`, `"kind":"unknown"`, 1),
		"<user-references version=\"2\">{}\n</user-references>",
	} {
		_, recognized := RenderReferences(malformed)
		require.False(t, recognized, "must not partially recognize %q", malformed)
	}
	for _, malformed := range []string{
		"<user-references version=\"1\">",
		"<user-references version=\"1\">\n{}\n</user-references>",
		SerializeWithGeneral([]Comment{{Anchor: sampleAnchor(), Body: "body"}}, "") + strings.Repeat("\n", 5),
	} {
		_, recognized := ParseReferences(malformed)
		require.False(t, recognized)
	}
}

func TestSerializeOmitsFenceSensitiveFramingAndPreservesControlBytes(t *testing.T) {
	a := sampleAnchor()
	a.Context = []string{"```", "```diff", "@heading", "/skill", "\rline\t"}
	a.External = map[string]string{"internal-id": "must not serialize"}
	body := Serialize([]Comment{{ID: "id", Anchor: a, Body: "line one\r\nline two\t"}})
	parsed, ok := ParseReferences(body)
	require.True(t, ok)
	require.Equal(t, a.Context, parsed.Comments[0].Snippet)
	require.Equal(t, "line one\r\nline two\t", parsed.Comments[0].Comment)
	require.NotContains(t, body, "internal-id")
	readable, ok := RenderReferences(body)
	require.True(t, ok)
	require.Contains(t, readable, "␍line\t")
	require.Contains(t, readable, "@heading")
	require.Contains(t, readable, "/skill")
	require.Contains(t, readable, "```diff")
}

func TestSerializeAndRetryKeepExactPayloadAndReferenceBounds(t *testing.T) {
	d := New("workspace")
	a := sampleAnchor()
	a.Mode = "files"
	a.Context = []string{"@intro", "/skill", "```"}
	a.External = map[string]string{"private": "not visible"}
	_, err := d.Add(a, "  exact\r\ncomment  ")
	require.NoError(t, err)
	attempt, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	parsed, ok := ParseReferences(attempt.Payload)
	require.True(t, ok)
	require.Equal(t, []string{"@intro", "/skill", "```"}, parsed.Comments[0].Snippet)
	require.Equal(t, "exact\r\ncomment", parsed.Comments[0].Comment)
	require.NotContains(t, attempt.Payload, "private")
	require.NoError(t, d.Fail(attempt.RequestID, context.DeadlineExceeded))
	retry, err := d.BeginSend(sampleTarget())
	require.NoError(t, err)
	require.Equal(t, attempt.Payload, retry.Payload)
	require.Equal(t, attempt.RequestID, retry.RequestID)

	tooManyLines := make([]string, MaxContextLines+1)
	for i := range tooManyLines {
		tooManyLines[i] = "x"
	}
	invalid := ReferencePayload{Comments: []Reference{{Kind: ReferenceFile, Repository: "repo", Checkout: "/work", Path: "a.txt", Snippet: tooManyLines, Comment: "x"}}}
	_, ok = ParseReferences(serializeReferences(invalid))
	require.False(t, ok)
}

func TestRenderReferencesUsesCompactDistinctGitAndFilePresentations(t *testing.T) {
	git := sampleAnchor()
	git.OldPath = "pkg/app/old.go"
	git.OldStart, git.OldEnd, git.NewStart, git.NewEnd = 3, 5, 7, 8
	file := sampleAnchor()
	file.Mode, file.Group, file.Path, file.OldStart, file.OldEnd, file.NewStart, file.NewEnd = "files", "", "README.md", 0, 0, 2, 2
	payload := Serialize([]Comment{
		{Anchor: git, Body: "Git note"},
		{Anchor: file, Body: "File note"},
	})
	readable, ok := RenderReferences(payload)
	require.True(t, ok)
	require.Contains(t, readable, "Git · pkg/app/old.go→pkg/app/app.go:3-5→7-8 · unstaged")
	require.Contains(t, readable, "README.md:2")
	require.Equal(t, "Git · pkg/app/old.go→pkg/app/app.go:3-5→7-8 · unstaged\n line\n-old\n+new\n\nGit note\n\nREADME.md:2\n line\n-old\n+new\n\nFile note", readable)
	require.Contains(t, readable, "Git note")
	require.Contains(t, readable, "File note")
	require.NotContains(t, readable, "Repository:")
	require.NotContains(t, readable, "checkout")
	require.NotContains(t, readable, "Branch:")
	require.NotContains(t, readable, "Snippet:")
	require.NotContains(t, readable, "Comment:")

	file.OldStart, file.OldEnd, file.NewStart, file.NewEnd = 2, 2, 0, 0
	deleted, ok := RenderReferences(Serialize([]Comment{{Anchor: file, Body: "removed"}}))
	require.True(t, ok)
	require.Contains(t, deleted, "README.md:2→∅")
}

func TestOversizeCanonicalPayloadFallsBackWithoutTruncation(t *testing.T) {
	comments := make([]Comment, 6)
	for i := range comments {
		a := sampleAnchor()
		a.Context = []string{strings.Repeat("x", 220<<10)}
		comments[i] = Comment{Anchor: a, Body: "comment"}
	}
	body := Serialize(comments)
	require.Greater(t, len(body), 1<<20)
	_, ok := RenderReferences(body)
	require.False(t, ok, "oversize content is not decoded for presentation")
	require.Contains(t, body, strings.Repeat("x", 100), "canonical content is retained")
}

func TestRenderReferencesDisambiguatesIdenticalCheckoutBasenames(t *testing.T) {
	first, second := sampleAnchor(), sampleAnchor()
	first.Path, second.Path = "README.md", "README.md"
	first.Mode, second.Mode = "files", "files"
	first.WorktreePath, second.WorktreePath = "/alpha/repo", "/beta/repo"
	first.OldStart, first.OldEnd, first.NewStart, first.NewEnd = 0, 0, 1, 1
	second.OldStart, second.OldEnd, second.NewStart, second.NewEnd = 0, 0, 2, 2
	readable, ok := RenderReferences(Serialize([]Comment{{Anchor: first, Body: "one"}, {Anchor: second, Body: "two"}}))
	require.True(t, ok)
	require.Contains(t, readable, "README.md:1 · docker-agent@/alpha/repo")
	require.Contains(t, readable, "README.md:2 · docker-agent@/beta/repo")
}

func TestSerializeRejectsUnpresentableLocationBounds(t *testing.T) {
	d := New("workspace")
	a := sampleAnchor()
	a.RepositoryName = strings.Repeat("r", maxReferenceLabelBytes+1)
	_, err := d.Add(a, "body")
	require.ErrorIs(t, err, ErrInvalidAnchor)
}
