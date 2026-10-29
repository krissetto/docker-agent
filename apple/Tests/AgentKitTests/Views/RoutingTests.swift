import Foundation
import Testing
import SwiftUI
import AgentKit
@testable import DockerAgentApple

@Test func compactRoutingDoesNotRestoreOnPhone() {
    #expect(CompactRouting.restoresRememberedSession(isCompact: true) == false)
    #expect(CompactRouting.restoresRememberedSession(isCompact: false) == true)
}

@Test func compactRoutingStackIsCanonical() {
    let server = UUID()
    let a = SessionRef(serverID: server, sessionID: "a")
    let b = SessionRef(serverID: server, sessionID: "b")
    // A row tap always yields one destination, whatever was open before.
    #expect(CompactRouting.path(afterTap: a) == [a])
    // In-app opens push, so Back returns to the previous session; reopening the top is a no-op.
    #expect(CompactRouting.path(afterOpen: b, current: [a]) == [a, b])
    #expect(CompactRouting.path(afterOpen: b, current: [a, b]) == [a, b])
    #expect(CompactRouting.path(afterOpen: a, current: []) == [a])
    // Selection mirrors the top of the stack; Back to the root clears it so the same row can reopen.
    #expect(CompactRouting.selection(forPath: [a, b]) == b)
    #expect(CompactRouting.selection(forPath: []) == nil)
}

@Test func rowTapOpensOnlyOutsideEditAndMultiSelect() {
    #expect(CompactRouting.rowTapOpens(isEditing: false, selectedCount: 0))
    #expect(CompactRouting.rowTapOpens(isEditing: false, selectedCount: 1))
    #expect(CompactRouting.rowTapOpens(isEditing: true, selectedCount: 1) == false)
    #expect(CompactRouting.rowTapOpens(isEditing: false, selectedCount: 2) == false)
}

@Test func logBucketsByRecency() {
    let now = Date(timeIntervalSince1970: 1_760_000_000)
    let calendar = Calendar(identifier: .gregorian)
    #expect(LogBucket.bucket(for: now.addingTimeInterval(-60), now: now, calendar: calendar) == .today)
    #expect(LogBucket.bucket(for: calendar.date(byAdding: .day, value: -1, to: now), now: now, calendar: calendar) == .yesterday)
    #expect(LogBucket.bucket(for: now.addingTimeInterval(-3 * 86_400), now: now, calendar: calendar) == .thisWeek)
    #expect(LogBucket.bucket(for: now.addingTimeInterval(-30 * 86_400), now: now, calendar: calendar) == .earlier)
    #expect(LogBucket.bucket(for: nil, now: now, calendar: calendar) == .earlier)
}

@Test func boardEntryUsesLiveTreeWhenOpenElseCatalogChildren() {
    let server = UUID()
    let root = SessionProjection(ref: SessionRef(serverID: server, sessionID: "a"), title: "t", agentName: "root", state: .running)
    let child = SessionProjection(ref: SessionRef(serverID: server, sessionID: "b"), title: "c", agentName: "engineer", state: .waiting, parentID: "a")
    let catalog = BoardEntry.make(session: root, reason: nil, open: nil, catalogChildren: [child])
    #expect(catalog.members.map(\.name) == ["engineer"])
    #expect(catalog.membersAreLive == false)
    #expect(catalog.members[0].word == "waiting")
    #expect(catalog.activeMembers == 0)
    let tree = [AgentNodeProjection(id: "r", agentName: "root", state: .running, children: [AgentNodeProjection(id: "x", agentName: "designer", state: .running), AgentNodeProjection(id: "y", agentName: "scout", state: .idle, needsAttention: true)])]
    let open = OpenSessionProjection(ref: root.ref, title: "t", agentName: "root", state: .running, agentTree: tree)
    let live = BoardEntry.make(session: root, reason: nil, open: open, catalogChildren: [child])
    #expect(live.membersAreLive)
    #expect(live.members.map(\.word) == ["working", "needs you"])
    #expect(live.activeMembers == 1)
}

@Test func sessionAccessibilityIdentityIsStableAndContentFree() {
    let server = UUID(uuidString: "00000000-0000-0000-0000-000000000001")!
    let ref = SessionRef(serverID: server, sessionID: "abc")
    #expect(SessionAccessibility.identity(ref) == "session.detail.00000000-0000-0000-0000-000000000001.abc")
    #expect(SessionAccessibility.identity(ref) == SessionAccessibility.identity(SessionRef(serverID: server, sessionID: "abc")))
}

@Test func teamLineVisibilityIsStateDerived() {
    #expect(TeamLineVisibility.isVisible(rootState: .running, interactions: 0, runningSubagents: 0))
    #expect(TeamLineVisibility.isVisible(rootState: .cancelling, interactions: 0, runningSubagents: 0))
    #expect(TeamLineVisibility.isVisible(rootState: .idle, interactions: 0, runningSubagents: 1))
    #expect(TeamLineVisibility.isVisible(rootState: .settled, interactions: 1, runningSubagents: 0))
    #expect(TeamLineVisibility.isVisible(rootState: .queued, interactions: 0, runningSubagents: 0) == false)
    #expect(TeamLineVisibility.isVisible(rootState: .waiting, interactions: 0, runningSubagents: 0) == false)
    #expect(TeamLineVisibility.isVisible(rootState: .idle, interactions: 0, runningSubagents: 0) == false)
    // An idle root with a sub-agent flagged for attention still shows the line.
    #expect(TeamLineVisibility.isVisible(rootState: .idle, interactions: 0, runningSubagents: 0, attention: 1))
}

@Test func userTurnOriginRecognition() {
    #expect(UserTurnLayout.isHuman(origin: nil))
    #expect(UserTurnLayout.isHuman(origin: ""))
    #expect(UserTurnLayout.isHuman(origin: "user"))
    #expect(UserTurnLayout.isHuman(origin: "agent") == false)
    #expect(UserTurnLayout.isHuman(origin: "runtime") == false)
    #expect(UserTurnLayout.isHuman(origin: "something-new") == false)
}

@Test func signalTextMeetsTextContrastOnPaper() {
    #expect(Palette.signalTextLight.contrast(with: Palette.paperLight) >= 4.5)
    #expect(Palette.signalTextDark.contrast(with: Palette.paperDark) >= 4.5)
    // Grouped/bench paper on light (#F2F2F7) must hold too.
    #expect(Palette.signalTextLight.contrast(with: Palette.RGB(0xF2 / 255, 0xF2 / 255, 0xF7 / 255)) >= 4.5)
}

@Test func userTurnHugsOnlyShortSingleLines() {
    #expect(UserTurnLayout.hugsWidth("ok go"))
    #expect(UserTurnLayout.hugsWidth("send a fresh team on that at this pint") == false)
    #expect(UserTurnLayout.hugsWidth("short\nbut two lines") == false)
}

@Test func queuedTurnsMatchExactIDsOnly() {
    let items = [
        TranscriptItem(id: "a", kind: .message, role: "user", text: "same text", turnID: "t1", userInputState: .queued),
        TranscriptItem(id: "b", kind: .message, role: "user", text: "same text", turnID: "t2"),
        TranscriptItem(id: "c", kind: .message, role: "user", text: "no id", userInputState: .queued),
    ]
    #expect(QueuedTurns.shown(in: items) == ["t1"])
}

@Test func historyControlStateIsHonest() {
    // Local window only: one caption, no server control.
    let local = HistoryControlState(hiddenLocally: 200, omittedLocally: 0, history: TranscriptWindow(), loading: false, error: nil, canLoad: true, loadedOlderOnce: false)
    #expect(local.showsServerControl == false)
    #expect(local.captions == ["200 earlier rows are loaded on this device."])
    // Older on server with a known count.
    let older = HistoryControlState(hiddenLocally: 0, omittedLocally: 0, history: TranscriptWindow(isComplete: false, omittedItemCount: 1_200, olderCursor: "c1"), loading: false, error: nil, canLoad: true, loadedOlderOnce: false)
    #expect(older.showsServerControl)
    #expect(older.captions == ["\(1_200.formatted()) older items are on the server."])
    // Exhausted after loading, with oversize omissions: never claims the full history.
    let truncated = HistoryControlState(hiddenLocally: 0, omittedLocally: 0, history: TranscriptWindow(isComplete: false, olderCursor: nil, startPosition: 0, containsTruncatedItems: true, omittedRanges: [TranscriptOmittedRange(startPosition: 10, endPosition: 12, reason: "oversize")]), loading: false, error: nil, canLoad: true, loadedOlderOnce: true)
    #expect(truncated.showsServerControl == false)
    #expect(truncated.captions == ["No older history can be loaded.", "One range of items were too large for the server to send and are left out."])
    // Paging unsupported: no server control even if a cursor leaked through.
    let unsupported = HistoryControlState(hiddenLocally: 0, omittedLocally: 0, history: TranscriptWindow(olderCursor: "c"), loading: false, error: nil, canLoad: false, loadedOlderOnce: false)
    #expect(unsupported.showsServerControl == false)
    // Error keeps the control visible for Retry.
    let failed = HistoryControlState(hiddenLocally: 0, omittedLocally: 0, history: TranscriptWindow(olderCursor: "c"), loading: false, error: "Offline", canLoad: true, loadedOlderOnce: false)
    #expect(failed.showsServerControl)
}

@Test func olderLoadAnchorRules() {
    var pending = OlderLoadAnchor(anchorID: "turn-7", shownAtRequest: 150)
    // No rows arrived (nothing older / error): nothing grows, nothing restores.
    #expect(pending.windowGrowth(nowShown: 150) == 0)
    #expect(pending.shouldRestore(nowShown: 150) == false)
    // 40 older rows prepended: window grows by exactly 40 and the reader's turn is restored.
    #expect(pending.windowGrowth(nowShown: 190) == 40)
    #expect(pending.shouldRestore(nowShown: 190))
    // The reader scrolled while waiting: rows still show, but the view is not yanked.
    pending.userMoved = true
    #expect(pending.windowGrowth(nowShown: 190) == 40)
    #expect(pending.shouldRestore(nowShown: 190) == false)
    // Without a resolvable anchor there is nothing to restore.
    #expect(OlderLoadAnchor(anchorID: nil, shownAtRequest: 10).shouldRestore(nowShown: 20) == false)
}
