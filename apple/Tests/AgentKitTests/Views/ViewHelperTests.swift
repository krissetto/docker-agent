import Foundation
import Testing
import AgentKit
@testable import DockerAgentApple

private func line(_ k: ReviewLineKind, _ o: Int, _ n: Int, _ t: String) -> ReviewDocumentLine { .init(kind: k, oldLine: o, newLine: n, text: t) }

@Test func splitPairsRemovesWithAdds() {
    let lines = [line(.hunk, 0, 0, "@@"), line(.context, 1, 1, "a"), line(.remove, 2, 0, "b"), line(.remove, 3, 0, "c"), line(.add, 0, 2, "B"), line(.context, 4, 3, "d"), line(.add, 0, 4, "e")]
    let rows = SplitRow.build(lines)
    #expect(rows.count == 6)
    guard case .full(0) = rows[0].content else { Issue.record("hunk"); return }
    guard case .pair(2, 4) = rows[2].content, case .pair(3, nil) = rows[3].content, case .pair(nil, 6) = rows[5].content else { Issue.record("pairs \(rows.map(\.content))"); return }
}

@Test func anchorUsesRealCoordinates() {
    let doc = ReviewDocument(key: .init(mode: .changes, worktreeID: "w", path: "f.go", group: .unstaged), lines: [line(.hunk, 0, 0, "@@"), line(.context, 9, 9, "x"), line(.remove, 10, 0, "y"), line(.add, 0, 10, "Y")])
    let file = ReviewFileChange(path: "f.go", group: .unstaged)
    let anchor = AnchorBuilder.anchor(range: 1...3, document: doc, file: file, generation: 7)
    #expect(anchor?.oldStart == 9 && anchor?.oldEnd == 10)
    #expect(anchor?.newStart == 9 && anchor?.newEnd == 10)
    #expect(anchor?.context == ["x", "y", "Y"])
    #expect(anchor?.contextLines.map(\.kind) == [.context, .remove, .add])
    #expect(anchor?.contextLines.map(\.oldLine) == [9, 10, 0])
    #expect(anchor?.contextLines.map(\.newLine) == [9, 0, 10])
    #expect(anchor?.documentFingerprint == reviewDocumentFingerprint(doc))
    #expect(AnchorBuilder.anchor(range: 0...0, document: doc, file: file, generation: 7) == nil)
    #expect(AnchorBuilder.row(for: anchor!, in: doc.lines) == 3)
}

@Test func anchorRejectsOversizedSelection() {
    let lines = (1...401).map { line(.context, $0, $0, "l") }
    #expect(AnchorBuilder.validate(0...400, in: lines) == .tooManyLines)
}

@Test func markdownSplitsFences() {
    let segments = MarkdownSegment.split("hi\n```swift\nlet a = 1\n```\nbye")
    #expect(segments == [.prose("hi"), .code(language: "swift", code: "let a = 1"), .prose("bye")])
    #expect(MarkdownSegment.split("x\n```\nopen") == [.prose("x"), .code(language: "", code: "open")])
}

@Test func alwaysAllowIsScopedLikeTUI() {
    #expect(PermissionPattern.build(toolName: "shell", arguments: #"{"cmd":"rm -rf ./tmp"}"#) == "shell:cmd=rm*")
    #expect(PermissionPattern.build(toolName: "read_file", arguments: #"{"path":"a"}"#) == "read_file")
    #expect(PermissionPattern.build(toolName: "shell", arguments: #"{"cmd":"rm -rf ./tm"#) == nil)
    #expect(PermissionPattern.label("shell:cmd=rm*") == "rm*")
}

@MainActor @Test func composerHandoffIsOneShot() {
    let handoff = ComposerDraftHandoff()
    let ref = SessionRef(serverID: UUID(), sessionID: "s")
    let other = SessionRef(serverID: ref.serverID, sessionID: "t")
    handoff.put("Fix the tests", for: ref)
    #expect(handoff.take(for: other) == nil)
    #expect(handoff.take(for: ref) == "Fix the tests")
    #expect(handoff.take(for: ref) == nil)
}

@MainActor @Test func composerDraftSurvivesSessionSwitch() throws {
    let handoff = ComposerDraftHandoff()
    let source = SessionRef(serverID: UUID(), sessionID: "source")
    let branch = SessionRef(serverID: source.serverID, sessionID: "branch")
    let file = try SessionDocumentAttachment.fromText("notes", name: "notes.txt")
    handoff.save(text: "half-written", attachments: [file], for: source)
    #expect(handoff.restore(for: branch) == nil)
    let restored = handoff.restore(for: source)
    #expect(restored?.text == "half-written")
    #expect(restored?.attachments.map(\.id) == [file.id])
    #expect(handoff.restore(for: source) == nil)
    handoff.save(text: "", attachments: [], for: source)
    #expect(handoff.restore(for: source) == nil)
}

@Test func teamTreeMapLayoutIsLinearBoundedAndZoomHitTestingUsesCanvasCoordinates() {
    let server = UUID()
    let leaf = AgentNodeProjection(id: "leaf", session: SessionRef(serverID: server, sessionID: "leaf"), agentName: "leaf", state: .waiting, needsAttention: true)
    let root = AgentNodeProjection(id: "root", session: SessionRef(serverID: server, sessionID: "root"), agentName: "root", state: .running,
                                   children: (0..<80).map { index in AgentNodeProjection(id: "branch-\(index)", agentName: "branch", children: index == 0 ? [leaf] : []) })
    let layout = TeamTreeLayout.build(roots: [root], cardSize: CGSize(width: 184, height: 72), columnSpacing: 212, rowSpacing: 122, inset: 32)
    #expect(layout.nodes.count == 82)
    #expect(layout.size.width.isFinite && layout.size.height.isFinite)
    #expect(layout.size.width < 20_000 && layout.size.height < 2_000)
    let leafIndex = layout.nodes.firstIndex(where: { $0.node.id == "leaf" })!
    let center = layout.points[leafIndex]
    #expect(layout.hitTest(CGPoint(x: center.x * 0.5, y: center.y * 0.5), zoom: 0.5)?.id == "leaf")
    #expect(layout.hitTest(CGPoint(x: CGFloat.infinity, y: 0), zoom: 1) == nil)
    #expect(layout.hitTest(CGPoint.zero, zoom: 0) == nil)
}

@Test @MainActor func teamWidgetSummaryAggregatesLiveSessionsDeduplicatesRootAndMarksOffline() {
    let server = UUID()
    let rootRef = SessionRef(serverID: server, sessionID: "root")
    let childRef = SessionRef(serverID: server, sessionID: "child")
    let updatedAt = Date(timeIntervalSince1970: 1_800_000_000)
    let rootNode = AgentNodeProjection(id: "root-node", session: rootRef, agentName: "root", state: .running)
    let child = AgentNodeProjection(id: "child", session: childRef, agentName: "worker", state: .running, needsAttention: true)
    let root = OpenSessionProjection(ref: rootRef, title: "private title", agentName: "root", state: .running,
                                     connection: .live, refreshedAt: updatedAt, transcript: [.init(id: "private", kind: .message, text: "private transcript")],
                                     interactions: [.maxIterations(id: "secret", owner: "root", limit: 10)], agentTree: [rootNode, child])
    let childSession = OpenSessionProjection(ref: childRef, parentSession: rootRef, title: "child", agentName: "worker", state: .running,
                                              connection: .live, refreshedAt: updatedAt, agentTree: [child])
    let summary = TeamWidgetPublisher.summary(sessions: [root, childSession])
    #expect(summary.agentCount == 2)
    #expect(summary.activeCount == 2)
    #expect(summary.attentionCount == 2)
    #expect(summary.liveSessions == 2)
    #expect(summary.serverCount == 1)
    #expect(summary == TeamWidgetPublisher.summary(sessions: [childSession, root]))
    let offline = OpenSessionProjection(ref: rootRef, title: "root", agentName: "root", state: .running, connection: .offline, agentTree: [child])
    let unavailable = TeamWidgetPublisher.summary(sessions: [offline])
    #expect(unavailable.liveSessions == 0)
    #expect(unavailable.updatedAt == nil)
    #expect(unavailable.agentCount == 0)
    #expect(unavailable.serverCount == 0)
}

@Test func markdownSplitsBlocks() {
    let text = "# Plan\nIntro line\n- first\n- [x] done\n  continued\n1. one\n> quoted\n---\n| a | b |\n|---|---|\n| 1 | 2 |\ntail"
    let segments = MarkdownSegment.split(text)
    #expect(segments[0] == .heading(level: 1, "Plan"))
    #expect(segments[1] == .prose("Intro line"))
    #expect(segments.count == 7)
    #expect(segments[2] == .list([.init(marker: "•", depth: 0, text: "first"), .init(marker: "☑", depth: 0, text: "done continued"), .init(marker: "1.", depth: 0, text: "one")]))
    #expect(segments[3] == .quote("quoted"))
    #expect(segments[4] == .rule)
    #expect(segments[5] == .table([["a", "b"], ["1", "2"]]))
    #expect(segments[6] == .prose("tail"))
    #expect(MarkdownSegment.split("#hashtag not heading") == [.prose("#hashtag not heading")])
    #expect(MarkdownSegment.split("| not | a table") == [.prose("| not | a table")])
}

@Test func ageFormatsDensely() {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    #expect(Format.age(now.addingTimeInterval(-10), now: now) == "now")
    #expect(Format.age(now.addingTimeInterval(-300), now: now) == "5m")
    #expect(Format.age(now.addingTimeInterval(-7_200), now: now) == "2h")
    #expect(Format.age(now.addingTimeInterval(-3 * 86_400), now: now) == "3d")
    #expect(Format.age(nil, now: now) == nil)
}

@Test func transcriptBottomDetectionTracksVisibleRect() {
    let content = CGSize(width: 375, height: 4_000)
    let visible = CGSize(width: 375, height: 600)
    // At rest at the end the inset-adjusted visible rect meets the content height.
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: 3_400), size: visible), contentSize: content))
    // Within the trailing padding slack still counts; past it the control shows.
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: 3_400 - TranscriptScroll.tolerance + 1), size: visible), contentSize: content))
    #expect(!TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: 3_300), size: visible), contentSize: content))
    #expect(!TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: .zero, size: visible), contentSize: content))
}

@Test func transcriptFollowTracksContentGrowthButNotScrollOffset() {
    let content = CGSize(width: 375, height: 4_000)
    let visible = CGRect(origin: CGPoint(x: 0, y: 3_400), size: CGSize(width: 375, height: 600))
    let settled = TranscriptScroll.Geometry(visibleRect: visible, contentSize: content)
    let scrolled = TranscriptScroll.Geometry(visibleRect: visible.offsetBy(dx: 0, dy: -120), contentSize: content)
    let scrolledFurther = TranscriptScroll.Geometry(visibleRect: visible.offsetBy(dx: 0, dy: -240), contentSize: content)
    let appended = TranscriptScroll.Geometry(visibleRect: visible, contentSize: CGSize(width: 375, height: 4_100))
    let rotated = TranscriptScroll.Geometry(visibleRect: visible, contentSize: CGSize(width: 500, height: 4_000))

    #expect(!TranscriptScroll.shouldFollowLatest(from: settled, to: scrolled))
    #expect(scrolled == scrolledFurther)
    #expect(TranscriptScroll.shouldFollowLatest(from: settled, to: appended))
    #expect(TranscriptScroll.shouldFollowLatest(from: settled, to: rotated))
}

@Test func transcriptLatestTargetUsesTheLastTopLevelTurn() {
    let turns = [["u1", "tool1", "tool2"], ["u2", "assistant1", "tool3", "assistant2"]]
    #expect(TranscriptScroll.latestTargetID(turns: turns) == "u2")
    #expect(TranscriptScroll.latestTargetID(turns: []) == nil)
}

@Test func transcriptJumpControlFootprintDoesNotStickOrOverlap() {
    let content = CGSize(width: 375, height: 4_000)
    let visible = CGSize(width: 375, height: 600)
    let control = Metrics.target
    // Scrolled up: the 44pt control appears as a bottom inset; state stays "not at bottom".
    let scrolledUp = CGRect(origin: CGPoint(x: 0, y: 3_000), size: CGSize(width: 375, height: visible.height - control))
    #expect(!TranscriptScroll.isAtBottom(visibleRect: scrolledUp, contentSize: content))
    // Jump aligns the last row's bottom with the visible bottom above the control: at bottom, control hides.
    let afterJump = CGRect(origin: CGPoint(x: 0, y: content.height - Metrics.l - (visible.height - control)), size: CGSize(width: 375, height: visible.height - control))
    #expect(TranscriptScroll.isAtBottom(visibleRect: afterJump, contentSize: content))
    // Removing the inset only grows the visible rect downward, so the state cannot flip back.
    let settled = CGRect(origin: afterJump.origin, size: visible)
    #expect(TranscriptScroll.isAtBottom(visibleRect: settled, contentSize: content))
    // Streaming growth while following: new content extends past the visible rect until the follow scroll lands.
    let grown = CGSize(width: 375, height: content.height + 400)
    #expect(!TranscriptScroll.isAtBottom(visibleRect: settled, contentSize: grown))
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: grown.height - visible.height), size: visible), contentSize: grown))
}

@Test func transcriptShortContentNeverShowsJumpControl() {
    let visible = CGSize(width: 375, height: 600)
    let short = CGSize(width: 375, height: 200)
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: .zero, size: visible), contentSize: short))
    // Rubber-band bounce produces negative offsets; still no control.
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: -80), size: visible), contentSize: short))
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: CGPoint(x: 0, y: 120), size: visible), contentSize: short))
    #expect(TranscriptScroll.isAtBottom(visibleRect: .zero, contentSize: .zero))
    // Content exactly filling the view is at the bottom at rest.
    #expect(TranscriptScroll.isAtBottom(visibleRect: CGRect(origin: .zero, size: visible), contentSize: visible))
}

@Test func streamingRevealStartsWithExistingHistoryAndRevealsOnlyAppendedText() {
    var reveal = StreamingTextReveal()
    let now = ContinuousClock.now
    #expect(reveal.update("already here", isStreaming: true, reduceMotion: false, now: now) == "already here")
    #expect(reveal.update("already here and new", isStreaming: true, reduceMotion: false, now: now) == "already here")
    #expect(reveal.advance(now: now) == "already here and new")
    #expect(!reveal.hasPendingText)
}

@Test func streamingRevealKeepsGraphemeTextValidAndIdleFlushesStalledSuffix() {
    var reveal = StreamingTextReveal()
    let now = ContinuousClock.now
    let start = "Working 👩‍"
    let withFamily = start + "👩‍👧‍👦e"
    let full = withFamily + "́👍🏽"
    #expect(reveal.update(start, isStreaming: true, reduceMotion: false, now: now) == start)
    #expect(reveal.update(withFamily, isStreaming: true, reduceMotion: false, now: now) == start)
    let firstFrame = reveal.advance(now: now.advanced(by: .milliseconds(34)))
    #expect(firstFrame == withFamily)
    #expect(reveal.update(full, isStreaming: true, reduceMotion: false, now: now.advanced(by: .milliseconds(40))) == firstFrame)
    #expect(reveal.hasPendingText)
    #expect(reveal.advance(now: now.advanced(by: .milliseconds(230))) == full)
    #expect(!reveal.hasPendingText)
}

@Test func streamingRevealFinalizesExactlyAndResetsOnReplacementOrReduceMotion() {
    var reveal = StreamingTextReveal()
    let now = ContinuousClock.now
    #expect(reveal.update("draft", isStreaming: true, reduceMotion: false, now: now) == "draft")
    #expect(reveal.update("draft update", isStreaming: true, reduceMotion: false, now: now) == "draft")
    #expect(reveal.update("corrected", isStreaming: true, reduceMotion: false, now: now) == "corrected")
    #expect(reveal.update("reduced motion", isStreaming: true, reduceMotion: true, now: now) == "reduced motion")
    #expect(reveal.update("final markdown text", isStreaming: false, reduceMotion: false, now: now) == "final markdown text")
    #expect(!reveal.hasPendingText)
}

@Test func streamingRevealBoundsCatchupDebtAndPreservesAllAuthoritativeBytes() {
    var reveal = StreamingTextReveal()
    let now = ContinuousClock.now
    let initial = "start"
    let huge = initial + String(repeating: "a", count: StreamingTextReveal.maximumPendingBytes + 1)
    #expect(reveal.update(initial, isStreaming: true, reduceMotion: false, now: now) == initial)
    #expect(reveal.update(huge, isStreaming: true, reduceMotion: false, now: now) == huge)
    #expect(!reveal.hasPendingText)
}

@Test func streamingRevealReleasesBoundedFramesThenCatchesUpExactly() {
    var reveal = StreamingTextReveal()
    let now = ContinuousClock.now
    let initial = "start"
    let burst = initial + String(repeating: "a", count: 700)
    #expect(reveal.update(initial, isStreaming: true, reduceMotion: false, now: now) == initial)
    #expect(reveal.update(burst, isStreaming: true, reduceMotion: false, now: now) == initial)
    let frame = reveal.advance(now: now.advanced(by: .milliseconds(34)))
    #expect(frame.count - initial.count == 48)
    #expect(reveal.update(burst, isStreaming: false, reduceMotion: false, now: now) == burst)
    #expect(!reveal.hasPendingText)
}

@Test func currentNodeMarkerPrefersSessionMatchAndRefusesAmbiguousRoots() {
    let server = UUID()
    let ref = SessionRef(serverID: server, sessionID: "root")
    let child = AgentNodeProjection(id: "child", session: SessionRef(serverID: server, sessionID: "child"), agentName: "engineer", state: .running)
    let unnamedRoot = AgentNodeProjection(id: "r1", session: nil, agentName: "root", state: .running, children: [child])
    #expect([unnamedRoot].currentNodeID(for: ref, agentName: "root") == "r1")
    #expect([unnamedRoot].currentNodeID(for: ref, agentName: "other") == nil)
    let twin = AgentNodeProjection(id: "r2", session: nil, agentName: "root", state: .idle)
    #expect([unnamedRoot, twin].currentNodeID(for: ref, agentName: "root") == nil)
    let withSession = AgentNodeProjection(id: "r3", session: ref, agentName: "root", state: .running)
    #expect([unnamedRoot, twin, withSession].currentNodeID(for: ref, agentName: "root") == "r3")
    let serverRoot = AgentNodeProjection(id: "root:root", session: SessionRef(serverID: server, sessionID: "other-form"), agentName: "root", state: .settled)
    #expect([serverRoot, twin].currentNodeID(for: ref, agentName: "root") == "root:root")
    #expect([serverRoot].currentNodeID(for: SessionRef(serverID: server, sessionID: "elsewhere"), agentName: "nobody") == nil)
    let tally = TeamTally([unnamedRoot])
    #expect(tally.total == 2 && tally.subagents == 1 && tally.runningSubagents == 1 && tally.subagentLine == "1 of 1 sub-agent running")
}

@Test func teamTreeMapAccessibilityFramesFollowZoomAndMatchHitTesting() {
    let server = UUID()
    let children = (0..<3).map { AgentNodeProjection(id: "c\($0)", session: SessionRef(serverID: server, sessionID: "c\($0)"), agentName: "agent\($0)") }
    let root = AgentNodeProjection(id: "root", agentName: "root", children: children)
    let layout = TeamTreeLayout.build(roots: [root], cardSize: CGSize(width: 164, height: 58), columnSpacing: 184, rowSpacing: 100, inset: 20)
    for zoom in [0.55, 1.0, 1.6] as [CGFloat] {
        var frames: [CGRect] = []
        for index in layout.nodes.indices {
            let frame = layout.accessibilityFrame(at: index, zoom: zoom)
            #expect(frame.width >= 44 && frame.height >= 44)
            // The frame centre is the scaled card centre, so a tap there resolves to the same node.
            #expect(layout.hitTest(CGPoint(x: frame.midX, y: frame.midY), zoom: zoom)?.id == layout.nodes[index].node.id)
            #expect(abs(frame.midX - layout.points[index].x * zoom) < 0.001 && abs(frame.midY - layout.points[index].y * zoom) < 0.001)
            frames.append(frame)
        }
        for (i, a) in frames.enumerated() {
            for b in frames[(i + 1)...] { #expect(!a.intersects(b), "grown frames must not cover neighbours at zoom \(zoom)") }
        }
    }
}
