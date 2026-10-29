import Foundation
import Testing
@testable import AgentKit

@MainActor
struct SecurityStorageTests {
    @Test func urlPolicyRequiresHTTPSExceptExplicitLoopbackAndRejectsURLSecrets() throws {
        #expect(try URLPolicy.validate("https://agent.example/api", allowsLocalHTTP: false).host == "agent.example")
        #expect(throws: APIError.self) { try URLPolicy.validate("http://agent.example", allowsLocalHTTP: true) }
        #expect(throws: APIError.self) { try URLPolicy.validate("http://localhost:8080", allowsLocalHTTP: false) }
        #expect(try URLPolicy.validate("http://localhost:8080/api", allowsLocalHTTP: true).host == "localhost")
        #expect(try URLPolicy.validate("http://127.0.0.1:8080", allowsLocalHTTP: true).host == "127.0.0.1")
        #expect(try URLPolicy.validate("http://[::1]:8080", allowsLocalHTTP: true).host == "::1")
        #expect(throws: APIError.self) { try URLPolicy.validate("https://user:secret@agent.example", allowsLocalHTTP: false) }
        #expect(throws: APIError.self) { try URLPolicy.validate("https://agent.example/path?token=secret", allowsLocalHTTP: false) }
        #expect(throws: APIError.self) { try URLPolicy.validate("https://agent.example/path#fragment", allowsLocalHTTP: false) }
        #expect(throws: APIError.self) { try URLPolicy.validate("file:///tmp/agent", allowsLocalHTTP: false) }
    }

    @Test func originComparisonNormalizesSchemeHostAndDefaultPortsWithoutCrossHostMixup() {
        #expect(URLPolicy.sameOrigin("https://EXAMPLE.com", "https://example.com:443/other/path"))
        #expect(URLPolicy.sameOrigin("http://localhost", "http://LOCALHOST:80/api"))
        #expect(!URLPolicy.sameOrigin("https://agent.example", "http://agent.example"))
        #expect(!URLPolicy.sameOrigin("https://agent.example", "https://other.example"))
        #expect(!URLPolicy.sameOrigin("https://agent.example:8443", "https://agent.example"))
    }

    @Test func instanceConfigStorePersistsOnlyConfigurationAndNeverCredentials() throws {
        let url = temporaryURL("instances.json")
        defer { removeStore(at: url) }
        let store = InstanceConfigStore(fileURL: url)
        let config = InstanceConfig(id: UUID(), name: "Dev", baseURL: "https://agent.example/api", allowsLocalHTTP: false, instanceID: "instance-1", capabilities: ["session_v2"])
        try store.upsert(config)
        let bytes = try Data(contentsOf: url)
        let serialized = String(decoding: bytes, as: UTF8.self)
        #expect(serialized.contains("agent.example"))
        #expect(serialized.contains("instance-1"))
        #expect(!serialized.lowercased().contains("token"))
        #expect(!serialized.contains("secret-value-never-stored"))
        #expect(store.load() == [config])
        try store.remove(config.id)
        #expect(store.load().isEmpty)
    }

    @Test func instanceConfigPersistsAnExplicitDefaultAgentSourceWithoutBreakingOldConfigs() throws {
        let url = temporaryURL("preferred-source.json")
        defer { removeStore(at: url) }
        let store = InstanceConfigStore(fileURL: url)
        let config = InstanceConfig(id: UUID(), name: "Kit", baseURL: "https://kit.example", instanceID: "kit-instance", preferredSource: "team.yaml")
        try store.upsert(config)
        #expect(store.load().first?.preferredSource == "team.yaml")

        let legacy = Data(#"[{"id":"00000000-0000-0000-0000-000000000001","name":"Old","baseURL":"https://old.example","allowsLocalHTTP":false,"capabilities":[]}]"#.utf8)
        try legacy.write(to: url, options: .atomic)
        #expect(store.load().first?.preferredSource == nil)
    }

    @Test func reviewDraftEditsAndStaleStatePersistByFullScope() throws {
        let url = temporaryURL("review-drafts.json")
        defer { removeStore(at: url) }
        let scope = makeScope(sessionID: "session-a", worktreeID: "worktree-a")
        let other = makeScope(sessionID: "session-b", worktreeID: "worktree-b")
        let anchor = makeAnchor(contextLines: [.init(kind: .context, oldLine: 1, newLine: 1, text: "let x = 1")])
        let store = ReviewDraftStore(fileURL: url)
        let comment = try store.add(scope: scope, anchor: anchor, body: "Original note")
        try store.edit(scope: scope, commentID: comment.id, body: "Edited note")
        store.markCommentStale(scope: scope, commentID: comment.id)

        let reloaded = ReviewDraftStore(fileURL: url)
        #expect(reloaded.draft(for: scope).comments.first?.body == "Edited note")
        #expect(reloaded.draft(for: scope).comments.first?.stale == true)
        #expect(reloaded.draft(for: other).comments.isEmpty)
    }

    @Test func pendingReviewAttemptFreezesIdentityAndBodyUntilExplicitPreviewUpdate() throws {
        let url = temporaryURL("review-attempt.json")
        defer { removeStore(at: url) }
        let scope = makeScope()
        let session = SessionRef(serverID: scope.serverID, sessionID: scope.sessionID)
        let store = ReviewDraftStore(fileURL: url)
        let first = try store.add(scope: scope, anchor: makeAnchor(), body: "body at preview")
        let initialAttempt = try beginAttempt(store: store, scope: scope, session: session, commentIDs: [first.id])
        try store.edit(scope: scope, commentID: first.id, body: "edited before send")
        let preview = try store.updatePreview(scope: scope, requestID: initialAttempt.requestID, expectedAgent: "agent-b", payload: "updated preview body", comments: store.draft(for: scope).comments)
        #expect(preview.requestID == initialAttempt.requestID)
        #expect(preview.payload == "updated preview body")
        #expect(preview.expectedAgent == "agent-b")
        #expect(preview.state == .previewed)

        try store.markSending(scope: scope, requestID: preview.requestID)
        #expect(store.persistenceError == nil)
        try store.edit(scope: scope, commentID: first.id, body: "edited after send began")
        let reloaded = ReviewDraftStore(fileURL: url)
        let savedAttempt = try #require(reloaded.attempts[scope])
        #expect(savedAttempt.requestID == preview.requestID)
        #expect(savedAttempt.payload == preview.payload)
        #expect(savedAttempt.state == .sending)
        #expect(savedAttempt.comments.first?.body == "edited before send")
        #expect(reloaded.draft(for: scope).comments.first?.body == "edited after send began")
        #expect(throws: ReviewDraftError.changed) {
            try reloaded.updatePreview(scope: scope, requestID: preview.requestID, expectedAgent: "agent-c", payload: "unsafe rewrite", comments: reloaded.draft(for: scope).comments)
        }
    }

    @Test func acceptingReviewSendClearsOnlyCommentsMatchingCapturedVersion() throws {
        let url = temporaryURL("review-ack.json")
        defer { removeStore(at: url) }
        let scope = makeScope()
        let session = SessionRef(serverID: scope.serverID, sessionID: scope.sessionID)
        let store = ReviewDraftStore(fileURL: url)
        let unchanged = try store.add(scope: scope, anchor: makeAnchor(path: "unchanged.swift"), body: "sent body")
        let changed = try store.add(scope: scope, anchor: makeAnchor(path: "changed.swift"), body: "captured body")
        let attempt = try beginAttempt(store: store, scope: scope, session: session, commentIDs: [unchanged.id, changed.id])
        try store.markSending(scope: scope, requestID: attempt.requestID)
        try store.edit(scope: scope, commentID: changed.id, body: "new unsent edit")

        try store.acceptSend(scope: scope, requestID: attempt.requestID, session: session)
        let reloaded = ReviewDraftStore(fileURL: url)
        let remaining = reloaded.draft(for: scope).comments
        #expect(remaining.count == 1)
        #expect(remaining.first?.id == changed.id)
        #expect(remaining.first?.body == "new unsent edit")
        #expect(reloaded.attempts[scope] == nil)
    }

    @Test func serializedReviewContextIsBoundedToFourHundredLines() {
        let store = ReviewDraftStore(fileURL: nil)
        let lines = (0..<450).map { ReviewAnchorLine(kind: .context, oldLine: $0 + 1, newLine: $0 + 1, text: "line-\($0)") }
        let comment = ReviewComment(anchor: makeAnchor(contextLines: lines), body: "check bounded context")
        let body = store.serializeForSend([comment], workspaceLabel: "workspace")
        #expect(body.contains("line-399"))
        #expect(!body.contains("line-400"))
        #expect(!body.contains("line-449"))
    }

    private func beginAttempt(store: ReviewDraftStore, scope: ReviewDraftScope, session: SessionRef, commentIDs: [UUID]) throws -> ReviewSendAttempt {
        let draft = store.draft(for: scope)
        let anchors = Dictionary(uniqueKeysWithValues: draft.comments.filter { commentIDs.contains($0.id) }.map { comment in
            var anchor = comment.anchor
            anchor.generation = 8
            return (comment.id, anchor)
        })
        return try store.beginSend(scope: scope, focusedSession: session, expectedAgent: "agent-a", currentGeneration: 8, freshlyChecked: anchors)
    }

    private func makeScope(sessionID: String = "session-a", worktreeID: String = "worktree-a") -> ReviewDraftScope {
        ReviewDraftScope(serverID: UUID(uuidString: "C589EA21-E6F2-452F-9B45-5A67648CFD42")!, instanceID: "instance-a", sessionID: sessionID, repositoryID: "repo-a", worktreeID: worktreeID, workspaceLabel: "workspace")
    }

    private func makeAnchor(path: String = "file.swift", contextLines: [ReviewAnchorLine] = []) -> ReviewAnchor {
        ReviewAnchor(path: path, mode: .changes, group: .unstaged, generation: 8, documentFingerprint: "fingerprint", oldStart: 1, oldEnd: 1, newStart: 1, newEnd: 1, contextLines: contextLines)
    }

    private func temporaryURL(_ name: String) -> URL {
        FileManager.default.temporaryDirectory.appending(path: "DockerAgentSecurityStorageTests/\(UUID().uuidString)/\(name)")
    }

    private func removeStore(at url: URL) { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
}
