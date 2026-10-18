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
    #expect(PermissionPattern.build(toolName: "run_background_job", arguments: #"{"command":"npm run dev"}"#) == "run_background_job:cmd=npm*")
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
