import Foundation
import Testing
@testable import AgentKit

struct DurableOutboxTests {
    @Test func persistsExactRequestAndDistinctExplicitSends() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        let first = try outbox.prepare(identity: identity, payload: "same text", requestID: "request-1")
        let second = try outbox.prepare(identity: identity, payload: "same text", requestID: "request-2")
        #expect(first.requestID == "request-1")
        #expect(second.requestID == "request-2")
        #expect(first.state == .prepared)
        #expect(outbox.pending(for: identity).count == 2)

        let reloaded = try DurableSendOutbox(fileURL: url)
        #expect(reloaded.pending(for: identity) == [first, second])
    }

    @Test func persistedOutboxIsOwnerOnlyAndReloadsAttemptState() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        let prepared = try outbox.prepare(identity: identity, payload: "private message", requestID: "reload-permissions")
        _ = try outbox.markAttempt(requestID: prepared.requestID, identity: identity)
        let fileAttributes = try FileManager.default.attributesOfItem(atPath: url.path)
        let mode = (fileAttributes[.posixPermissions] as? NSNumber)?.intValue
        #expect(mode.map { $0 & 0o077 == 0 } == true)
        let directoryAttributes = try FileManager.default.attributesOfItem(atPath: url.deletingLastPathComponent().path)
        let directoryMode = (directoryAttributes[.posixPermissions] as? NSNumber)?.intValue
        #expect(directoryMode.map { $0 & 0o077 == 0 } == true)
        let loaded = try DurableSendOutbox(fileURL: url)
        #expect(loaded.pending(for: identity).first?.state == .deliveryUnknown)
        #expect(loaded.pending(for: identity).first?.payload == "private message")
    }

    @Test func retryReusesImmutableRequestAndMarksUnknownDeliveryAcrossRelaunch() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        var outbox = try DurableSendOutbox(fileURL: url)
        let prepared = try outbox.prepare(identity: identity, payload: "exact body", mode: "steer", requestID: "stable-id")
        let attempt = try outbox.markAttempt(requestID: prepared.requestID, identity: identity)
        #expect(attempt.state == .sending)
        #expect(attempt.attemptCount == 1)
        outbox = try DurableSendOutbox(fileURL: url)
        let recovered = try #require(outbox.pending(for: identity).first)
        #expect(recovered.state == .deliveryUnknown)
        #expect(recovered.payload == "exact body")
        #expect(recovered.mode == "steer")
        let retry = try outbox.markAttempt(requestID: recovered.requestID, identity: identity)
        #expect(retry.requestID == "stable-id")
        #expect(retry.attemptCount == 2)
        #expect(retry.payload == recovered.payload)
    }

    @Test func automaticTitleIntentPersistsAndRetriesAsExactRequestMetadata() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let original = try DurableSendOutbox(fileURL: url)
        let payload = try SessionSendPayload.text("first prompt")
        let prepared = try original.prepare(identity: identity, sendPayload: payload, requestID: "title-request", generateTitle: true)
        #expect(prepared.generateTitle)
        let originalBody = try payload.encodedRequest(requestID: prepared.requestID, generateTitle: prepared.generateTitle)

        let reloaded = try DurableSendOutbox(fileURL: url)
        let recovered = try #require(reloaded.pending(for: identity).first)
        #expect(recovered.generateTitle)
        let retryBody = try recovered.sendPayload.encodedRequest(requestID: recovered.requestID, generateTitle: recovered.generateTitle)
        #expect(try JSONDecoder().decode(JSONValue.self, from: retryBody) == JSONDecoder().decode(JSONValue.self, from: originalBody))
        #expect(throws: DurableSendOutboxError.conflictingRequestID) {
            try reloaded.prepare(identity: identity, sendPayload: payload, requestID: "title-request", generateTitle: false)
        }
    }

    @Test func acceptedTombstoneBlocksSameRequestReplayUntilExplicitHistoryClear() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        let prepared = try outbox.prepare(identity: identity, payload: "hello", requestID: "accepted-id")
        _ = try outbox.markAttempt(requestID: prepared.requestID, identity: identity)
        let accepted = try outbox.markAccepted(requestID: prepared.requestID, identity: identity)
        #expect(accepted.state == .accepted)
        #expect(outbox.pending(for: identity).isEmpty)
        let repeated = try outbox.prepare(identity: identity, payload: "hello", requestID: "accepted-id")
        #expect(repeated.state == .accepted)
        #expect(throws: DurableSendOutboxError.invalidTransition) {
            try outbox.markAttempt(requestID: "accepted-id", identity: identity)
        }
        try outbox.clearAcceptedHistory()
        let next = try outbox.prepare(identity: identity, payload: "hello", requestID: "accepted-id")
        #expect(next.state == .prepared)
    }

    @Test func requestIDPayloadAndTargetConflictsAreRejected() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let original = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        _ = try outbox.prepare(identity: original, payload: "one", requestID: "same")
        #expect(throws: DurableSendOutboxError.conflictingRequestID) {
            try outbox.prepare(identity: original, payload: "two", requestID: "same")
        }
        #expect(throws: DurableSendOutboxError.conflictingRequestID) {
            try outbox.prepare(identity: try identity(instanceID: "other-instance"), payload: "one", requestID: "same")
        }
        #expect(throws: DurableSendOutboxError.identityMismatch) {
            try outbox.markAttempt(requestID: "same", identity: try identity(serverURL: URL(string: "https://other.example/api")!))
        }
    }

    @Test func isolatesByServerInstanceSessionAndOrigin() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let outbox = try DurableSendOutbox(fileURL: url)
        let identityA = try identity()
        let identityB = try identity(sessionID: "session-b")
        let identityOtherOrigin = try identity(serverURL: URL(string: "https://other.example/api")!)
        _ = try outbox.prepare(identity: identityA, payload: "a", requestID: "a")
        #expect(outbox.pending(for: identityB).isEmpty)
        #expect(outbox.pending(for: identityOtherOrigin).isEmpty)
    }

    @Test func neverSilentlyDiscardsPossiblyDeliveredSend() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        let prepared = try outbox.prepare(identity: identity, payload: "hello", requestID: "maybe")
        _ = try outbox.markAttempt(requestID: prepared.requestID, identity: identity)
        _ = try outbox.markFailure(requestID: prepared.requestID, identity: identity, outcome: .unknown)
        #expect(throws: DurableSendOutboxError.unsafeToDiscard) {
            try outbox.discard(requestID: prepared.requestID, identity: identity)
        }
        try outbox.discard(requestID: prepared.requestID, identity: identity, confirmUnknownDelivery: true)
        #expect(outbox.pending(for: identity).isEmpty)
    }

    @Test func definitiveNoDeliveryMakesExactRequestExplicitlyRetryable() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        let prepared = try outbox.prepare(identity: identity, payload: "body", requestID: "retry")
        _ = try outbox.markAttempt(requestID: prepared.requestID, identity: identity)
        let failed = try outbox.markFailure(requestID: prepared.requestID, identity: identity, outcome: .definitelyNotDelivered)
        #expect(failed.state == .prepared)
        let retry = try outbox.markAttempt(requestID: failed.requestID, identity: identity)
        #expect(retry.requestID == prepared.requestID)
        #expect(retry.payload == prepared.payload)
    }

    @Test func corruptStoreAndStorageFailureFailClosed() throws {
        let corrupt = temporaryStoreURL()
        defer { removeStore(at: corrupt) }
        try FileManager.default.createDirectory(at: corrupt.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("not json".utf8).write(to: corrupt)
        #expect(throws: DurableSendOutboxError.corruptStore) { try DurableSendOutbox(fileURL: corrupt) }

        let blockedParent = temporaryStoreURL().deletingLastPathComponent()
        try FileManager.default.createDirectory(at: blockedParent.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("block".utf8).write(to: blockedParent)
        defer { try? FileManager.default.removeItem(at: blockedParent) }
        let outbox = try DurableSendOutbox(fileURL: blockedParent.appending(path: "outbox.json"))
        #expect(throws: DurableSendOutboxError.storageLimitReached) {
            try outbox.prepare(identity: try identity(), payload: "body", requestID: "cannot-persist")
        }
        #expect(outbox.pending(for: try identity()).isEmpty)
    }

    @Test func enforcesPayloadAndPendingRecordBoundsWithoutEviction() throws {
        let url = temporaryStoreURL()
        defer { removeStore(at: url) }
        let identity = try identity()
        let outbox = try DurableSendOutbox(fileURL: url)
        #expect(throws: DurableSendOutboxError.invalidPayload) {
            try outbox.prepare(identity: identity, payload: String(repeating: "x", count: DurableSendOutbox.maximumPayloadBytes + 1), requestID: "too-large")
        }
        for index in 0..<DurableSendOutbox.maximumPendingRecords {
            _ = try outbox.prepare(identity: identity, payload: "message \(index)", requestID: "id-\(index)")
        }
        #expect(throws: DurableSendOutboxError.outboxLimitReached) {
            try outbox.prepare(identity: identity, payload: "next", requestID: "overflow")
        }
        #expect(outbox.pending(for: identity).count == DurableSendOutbox.maximumPendingRecords)
    }

    private func identity(serverID: UUID = UUID(), instanceID: String = "instance-a",
                          sessionID: String = "session-a", serverURL: URL = URL(string: "https://agent.example/api")!) throws -> DurableSendIdentity {
        try DurableSendIdentity(serverID: serverID, serverInstanceID: instanceID,
                                session: SessionRef(serverID: serverID, sessionID: sessionID), serverURL: serverURL)
    }

    private func temporaryStoreURL() -> URL {
        FileManager.default.temporaryDirectory.appending(path: "DockerAgentOutboxTests/\(UUID().uuidString)/outbox.json")
    }

    private func removeStore(at url: URL) { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
}
