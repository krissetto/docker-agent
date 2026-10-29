import Foundation
import Testing
import AgentKit
@testable import DockerAgentApple

@MainActor
struct AppLifecycleTests {
    @Test func twoForegroundScenesKeepObservationUntilLastSceneBackgroundsAndResumeAtCursor() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        let snapshot = try fixtureSnapshot(epoch: "epoch-a", cursor: 7)
        setup.model.openSessions[ref] = snapshot.project(ref: ref, connection: .live)
        setup.model.sessionLeases[ref] = 1
        setup.state.sseMode = .stalled

        let firstScene = UUID()
        let secondScene = UUID()
        setup.model.setSceneActive(true, for: firstScene)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1 }
        setup.model.setSceneActive(true, for: secondScene)
        setup.model.setSceneActive(false, for: firstScene)
        #expect(setup.model.streamTasks[ref] != nil)
        #expect(setup.state.stopLoadingCount == 0)

        setup.model.setSceneActive(false, for: secondScene)
        try await waitUntil { setup.state.stopLoadingCount > 0 }
        #expect(setup.model.openSessions[ref]?.cursor == 7)
        #expect(setup.model.sessionLeases[ref] == 1)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/cancel") || ($0.url?.path ?? "").hasSuffix("/messages") }.count == 0)

        setup.state.sseMode = .stalled
        setup.model.setSceneActive(true, for: firstScene)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 2 }
        let eventRequests = setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/events") }
        #expect(URLComponents(url: eventRequests[1].url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "since" })?.value == "7")
        #expect(URLComponents(url: eventRequests[1].url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "since_epoch" })?.value == "epoch-a")
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/open-view") || ($0.httpMethod == "PATCH" && $0.url?.path == "/api/v2/sessions/session-demo-001") }.count == 0)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/messages") || ($0.url?.path ?? "").hasSuffix("/cancel") }.count == 0)
        setup.model.setSceneActive(false, for: firstScene)
        try await waitUntil { setup.state.stopLoadingCount > 1 }
    }

    @Test func advertisedHistoryCapabilityOpensBoundedTailLoadsOlderAndUsesTailSSE() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: setup.identity.instanceID, ready: true, capabilities: ["session_history_pages"])
        setup.state.serverIdentityData = try JSONEncoder().encode(identity)
        setup.state.tailSnapshotData = Data(#"{"projection":"tail","session":{"id":"session-demo-001","title":"Bounded tail"},"status":{"session_id":"session-demo-001","agent_name":"root","state":"running"},"epoch":"tail-epoch","cursor":9,"transcript_position":3,"items":[{"position":2,"item":{"message":{"agent_name":"root","message":{"role":"assistant","content":"newest"}}}}],"history":{"start_position":2,"end_position":3,"total_items":3,"omitted_ranges":[],"next_cursor":"opaque-tail","truncated":true}}"#.utf8)
        let pageItems = (0..<2).map { position in "{\"position\":\(position),\"item\":{\"message\":{\"agent_name\":\"root\",\"message\":{\"role\":\"user\",\"content\":\"older-\(position)\"}}}}" }.joined(separator: ",")
        setup.state.tailHistoryPageData = Data("{\"projection\":\"tail\",\"session_id\":\"session-demo-001\",\"epoch\":\"tail-epoch\",\"cursor\":9,\"transcript_position\":3,\"items\":[\(pageItems)],\"history\":{\"start_position\":0,\"end_position\":3,\"total_items\":3,\"truncated\":false}}".utf8)
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]

        let lease = try await setup.model.acquireSessionView(ref)
        defer { setup.model.releaseSessionView(lease) }
        try await waitUntil("tail observation starts") { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1 }
        #expect(setup.model.openSessions[ref]?.transcriptWindow.hasOlderOnServer == true)
        #expect(setup.model.openSessions[ref]?.transcript.first?.text == "newest")
        setup.model.openSessions[ref]?.transcript.append(TranscriptItem(id: "live-append", kind: .message, owner: "root", role: "assistant", text: "arrived live", sourcePosition: 3, sourcePartIndex: 0))
        let eventRequest = try #require(setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").first)
        let eventQuery = URLComponents(url: try #require(eventRequest.url), resolvingAgainstBaseURL: false)?.queryItems ?? []
        #expect(eventQuery.first(where: { $0.name == "projection" })?.value == "tail")
        #expect(!eventQuery.contains(where: { $0.name == "since" }))

        try await setup.model.loadOlderSessionHistory(ref)
        #expect(setup.model.openSessions[ref]?.transcript.map(\.sourcePosition) == [0, 1, 2, 3])
        #expect(setup.model.openSessions[ref]?.transcript.last?.id == "live-append")
        #expect(setup.model.openSessions[ref]?.transcriptWindow.hasOlderOnServer == false)
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001/history").count == 1)
    }

    @Test func lateOlderHistoryPageCannotApplyAfterSessionGenerationChanges() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: setup.identity.instanceID, ready: true, capabilities: ["session_history_pages"])
        setup.state.serverIdentityData = try JSONEncoder().encode(identity)
        setup.state.tailSnapshotData = Data(#"{"projection":"tail","session":{"id":"session-demo-001"},"status":{"session_id":"session-demo-001"},"epoch":"tail-epoch","cursor":9,"transcript_position":2,"items":[{"position":1,"item":{"summary":"latest"}}],"history":{"start_position":1,"end_position":2,"total_items":2,"next_cursor":"opaque-tail","truncated":true}}"#.utf8)
        setup.state.tailHistoryPageData = Data(#"{"projection":"tail","session_id":"session-demo-001","epoch":"tail-epoch","cursor":9,"transcript_position":2,"items":[{"position":0,"item":{"summary":"oldest"}}],"history":{"start_position":0,"end_position":2,"total_items":2,"truncated":false}}"#.utf8)
        setup.state.delayHistoryPage = true
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]

        let first = try await setup.model.acquireSessionView(ref)
        try await waitUntil("first bounded stream") { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1 }
        let pageTask = Task { try? await setup.model.loadOlderSessionHistory(ref) }
        try await waitUntil("older page begins") { setup.state.requestsFor("/api/v2/sessions/session-demo-001/history").count == 1 }
        setup.model.releaseSessionView(first)
        let second = try await setup.model.acquireSessionView(ref)
        defer { setup.model.releaseSessionView(second) }
        try await waitUntil("new bounded stream") { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 2 }
        await pageTask.value
        #expect(setup.model.openSessions[ref]?.transcript.map(\.text) == ["latest"])
        #expect(setup.model.openSessions[ref]?.transcriptWindow.hasOlderOnServer == true)
        #expect(setup.model.historyPageErrors[ref] == nil)
    }

    @Test func staleHistoryCursorRebaselinesTailAndSurfacesRetryState() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: setup.identity.instanceID, ready: true, capabilities: ["session_history_pages"])
        setup.state.serverIdentityData = try JSONEncoder().encode(identity)
        setup.state.tailSnapshotData = Data(#"{"projection":"tail","session":{"id":"session-demo-001"},"status":{"session_id":"session-demo-001"},"pending_inputs":[{"turn_id":"turn-canceled-before-rebaseline","content":"do not resurrect"}],"epoch":"tail-epoch","cursor":9,"transcript_position":1,"items":[{"position":0,"item":{"summary":"old tail"}}],"history":{"start_position":0,"end_position":1,"total_items":1,"next_cursor":"opaque-tail","truncated":true}}"#.utf8)
        let rows = (0..<3).map { position in "{\"position\":\(position),\"item\":{\"summary\":\"rebased-\(position)\"}}" }.joined(separator: ",")
        setup.state.tailSnapshotRefreshData = Data("{\"projection\":\"tail\",\"session\":{\"id\":\"session-demo-001\"},\"status\":{\"session_id\":\"session-demo-001\"},\"epoch\":\"tail-epoch\",\"cursor\":10,\"transcript_position\":3,\"items\":[\(rows)],\"history\":{\"start_position\":0,\"end_position\":3,\"total_items\":3,\"truncated\":false}}".utf8)
        setup.state.tailHistoryPageData = Data(#"{"projection":"tail","session_id":"session-demo-001","epoch":"tail-epoch","cursor":9,"transcript_position":1,"items":[],"history":{"start_position":0,"end_position":1,"total_items":1,"truncated":false}}"#.utf8)
        setup.state.historyPageStatus = 409
        setup.state.tailSSEData = Data(("data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":\(String(decoding: setup.state.tailSnapshotRefreshData!, as: UTF8.self))}\n\ndata: {\"version\":2,\"type\":\"ready\"}\n\ndata: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"session-demo-001\",\"epoch\":\"tail-epoch\",\"sequence\":11,\"transcript_position\":3,\"event\":{\"type\":\"pending_user_message_accepted\",\"turn_id\":\"turn-new-after-baseline\",\"message\":\"accepted after baseline\",\"input_origin\":\"user\",\"session_position\":3}}}\n\n").utf8)
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        let lease = try await setup.model.acquireSessionView(ref)
        defer { setup.model.releaseSessionView(lease) }
        let streamBaseline = setup.state.sseMode
        setup.state.sseMode = .finite
        _ = streamBaseline
        let error = await #expect(throws: APIError.self) { try await setup.model.loadOlderSessionHistory(ref) }
        #expect(error?.code == "history_cursor_stale")
        try await waitUntil("fresh tail SSE baseline and post-baseline event") { setup.model.openSessions[ref]?.cursor == 11 && setup.model.historyPageErrors[ref]?.contains("ready") == true }
        try await waitUntil("post-baseline queued event") { setup.model.openSessions[ref]?.transcript.contains(where: { $0.turnID == "turn-new-after-baseline" }) == true }
        #expect(setup.model.openSessions[ref]?.transcript.map(\.text).prefix(3) == ["rebased-0", "rebased-1", "rebased-2"])
        #expect(setup.model.openSessions[ref]?.transcript.contains(where: { $0.turnID == "turn-canceled-before-rebaseline" }) == false, "stale queued inputs are not resurrected over an authoritative baseline")
        #expect(setup.model.historyPageErrors[ref]?.contains("load older again") == true)
    }

    @Test func unauthorizedStreamStopsRetriesAndOnlyExplicitReconnectResumes() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        setup.model.openSessions[ref] = try fixtureSnapshot(epoch: "epoch-a", cursor: 0).project(ref: ref, connection: .live)
        setup.model.sessionLeases[ref] = 1
        setup.state.sseMode = .unauthorized
        setup.model.setSceneActive(true, for: UUID())
        try await waitUntil { setup.model.servers.first?.state == .unauthorized }
        #expect(setup.model.openSessions[ref]?.connection == .unauthorized)
        try await Task.sleep(for: .milliseconds(50))
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1)

        setup.state.sseMode = .stalled
        setup.state.identityInstanceID = setup.identity.instanceID
        let sceneID = UUID()
        setup.model.setSceneActive(true, for: sceneID)
        try await setup.model.reconnectServer(setup.serverID)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 2 }
        #expect(setup.model.servers.first?.identity?.instanceID == setup.identity.instanceID)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/cancel") || ($0.url?.path ?? "").hasSuffix("/messages") }.count == 0)
        setup.model.setSceneActive(false, for: sceneID)
        try await waitUntil { setup.state.stopLoadingCount > 0 }
    }

    @Test func changedServerIdentityIsAnExplicitEditAndTestIssueWithoutRetargetingCredentials() async throws {
        let setup = try makeModel()
        let configBefore = try #require(setup.configStore.load().first)
        setup.state.identityInstanceID = "rebuilt-server-instance"
        setup.state.serverIdentityData = try JSONEncoder().encode(ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "rebuilt-server-instance", ready: true))

        await #expect(throws: APIError.self) { try await setup.model.reconnectServer(setup.serverID) }
        #expect(setup.model.servers.first?.state == .offline)
        #expect(setup.model.servers.first?.connectionIssue == .identityChanged)
        #expect(setup.model.servers.first?.connectionIssue?.message == "The server identity changed. Edit and test this server before reconnecting.")
        #expect(setup.configStore.load().first?.instanceID == configBefore.instanceID)
        #expect(await setup.tokenStore.value == "saved-token")
        #expect(await setup.tokenStore.mutationCount == 0)
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001").isEmpty)

        setup.state.identityStatus = 503
        await #expect(throws: APIError.self) { try await setup.model.reconnectServer(setup.serverID) }
        #expect(setup.model.servers.first?.connectionIssue == .identityChanged)

        setup.state.identityStatus = 200
        setup.state.identityInstanceID = setup.identity.instanceID
        setup.state.serverIdentityData = try JSONEncoder().encode(setup.identity)
        try await setup.model.reconnectServer(setup.serverID)
        #expect(setup.model.servers.first?.connectionIssue == nil)
    }

    @Test func savedCredentialLoadMarksPinnedIdentityMismatchWithoutChangingTrust() async throws {
        let setup = try makeModel()
        let configBefore = try #require(setup.configStore.load().first)
        setup.state.identityInstanceID = "rebuilt-server-instance"
        setup.state.serverIdentityData = try JSONEncoder().encode(ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "rebuilt-server-instance", ready: true))

        await setup.model.loadSavedCredentials()
        #expect(setup.model.servers.first?.state == .offline)
        #expect(setup.model.servers.first?.connectionIssue == .identityChanged)
        #expect(setup.configStore.load().first?.instanceID == configBefore.instanceID)
        #expect(await setup.tokenStore.value == "saved-token")
        #expect(await setup.tokenStore.mutationCount == 0)
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001").isEmpty)
    }

    @Test func failedIdentityFetchDoesNotMisreportIdentityChanged() async throws {
        let setup = try makeModel()
        setup.state.identityStatus = 503
        await setup.model.loadSavedCredentials()
        #expect(setup.model.servers.first?.state == .offline)
        #expect(setup.model.servers.first?.connectionIssue == nil)
        #expect(setup.configStore.load().first?.instanceID == setup.identity.instanceID)
        #expect(await setup.tokenStore.mutationCount == 0)
    }

    @Test func unsupportedServerVersionIsNotAnIdentityMismatch() async throws {
        let setup = try makeModel()
        setup.state.serverIdentityData = try JSONEncoder().encode(ServerIdentity(version: 3, sessionAPIVersion: 2, instanceID: "rebuilt-server-instance", ready: true))
        await setup.model.loadSavedCredentials()
        #expect(setup.model.servers.first?.state == .offline)
        #expect(setup.model.servers.first?.connectionIssue == nil)
    }

    @Test func metadataProjectionUsesValidatedCapabilityAndRequiresPrepareAndOpenMarkers() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: setup.identity.instanceID, ready: true,
                                      capabilities: ["session_v2", "session_view_metadata"])
        setup.state.serverIdentityData = try JSONEncoder().encode(identity)
        let cachedIdentity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: identity.instanceID, ready: true, capabilities: ["session_v2"])
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: cachedIdentity, state: .live)]
        setup.model.clients[setup.serverID] = setup.client
        setup.state.viewMode = .metadata
        let lease = try await setup.model.acquireSessionView(ref)
        let handshake = setup.state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" }
        #expect(setup.state.requestsFor("/api/v2/server").count == 1)
        #expect(handshake.count == 2)
        #expect(setup.model.servers.first?.identity?.supportsMetadataSessionView == true)
        #expect(handshake.allSatisfy { URLComponents(url: $0.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(URLQueryItem(name: "projection", value: "metadata")) == true })
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001/snapshot").count == 1)
        setup.model.releaseSessionView(lease)
        try await waitUntil { setup.model.streamTasks[ref] == nil }

        let prepareMismatch = try makeModel()
        prepareMismatch.state.serverIdentityData = try JSONEncoder().encode(identity)
        prepareMismatch.model.servers = [ServerProjection(id: prepareMismatch.serverID, name: "lifecycle", baseURL: "https://\(prepareMismatch.host)", identity: identity, state: .live)]
        prepareMismatch.model.clients[prepareMismatch.serverID] = prepareMismatch.client
        prepareMismatch.state.viewMode = .metadataMissingPrepare
        let prepareRef = refFor(setup: prepareMismatch)
        await #expect(throws: APIError.self) { try await prepareMismatch.model.openSession(prepareRef) }
        #expect(!prepareMismatch.state.requests.contains { $0.httpMethod == "PATCH" && $0.url?.path == "/api/v2/sessions/session-demo-001" })
        #expect(prepareMismatch.state.requestsFor("/api/v2/sessions/session-demo-001/snapshot").isEmpty)
        #expect(prepareMismatch.state.requestsFor("/api/v2/sessions/session-demo-001/events").isEmpty)

        let openMismatch = try makeModel()
        openMismatch.state.serverIdentityData = try JSONEncoder().encode(identity)
        openMismatch.model.servers = [ServerProjection(id: openMismatch.serverID, name: "lifecycle", baseURL: "https://\(openMismatch.host)", identity: identity, state: .live)]
        openMismatch.model.clients[openMismatch.serverID] = openMismatch.client
        openMismatch.state.viewMode = .metadataMissingOpen
        let openRef = refFor(setup: openMismatch)
        await #expect(throws: APIError.self) { try await openMismatch.model.openSession(openRef) }
        #expect(openMismatch.state.requests.filter { $0.httpMethod == "PATCH" && $0.url?.path == "/api/v2/sessions/session-demo-001" }.count == 1)
        #expect(openMismatch.state.requestsFor("/api/v2/sessions/session-demo-001/snapshot").isEmpty)
        #expect(openMismatch.state.requestsFor("/api/v2/sessions/session-demo-001/events").isEmpty)

        let changedInstance = try makeModel()
        let foreignIdentity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "different-instance", ready: true, capabilities: ["session_view_metadata"])
        changedInstance.state.serverIdentityData = try JSONEncoder().encode(foreignIdentity)
        changedInstance.model.servers = [ServerProjection(id: changedInstance.serverID, name: "lifecycle", baseURL: "https://\(changedInstance.host)", identity: changedInstance.identity, state: .live)]
        changedInstance.model.clients[changedInstance.serverID] = changedInstance.client
        await #expect(throws: APIError.self) { try await changedInstance.model.openSession(refFor(setup: changedInstance)) }
        #expect(changedInstance.state.requestsFor("/api/v2/server").count == 1)
        #expect(changedInstance.model.servers.first?.connectionIssue == .identityChanged)
        #expect(changedInstance.model.servers.first?.state == .offline)
        #expect(changedInstance.state.requestsFor("/api/v2/sessions/session-demo-001").isEmpty)

        let legacy = try makeModel()
        legacy.model.clients[legacy.serverID] = legacy.client
        legacy.model.servers = [ServerProjection(id: legacy.serverID, name: "legacy", baseURL: "https://\(legacy.host)", identity: legacy.identity, state: .live)]
        let legacyRef = refFor(setup: legacy)
        let legacyLease = try await legacy.model.acquireSessionView(legacyRef)
        let legacyHandshake = legacy.state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" }
        #expect(legacyHandshake.count == 2)
        #expect(legacyHandshake.allSatisfy { URLComponents(url: $0.url!, resolvingAgainstBaseURL: false)?.queryItems?.allSatisfy { $0.name != "projection" } == true })
        legacy.model.releaseSessionView(legacyLease)
        try await waitUntil { legacy.model.streamTasks[legacyRef] == nil }
    }

    private func refFor(setup: Setup) -> SessionRef { SessionRef(serverID: setup.serverID, sessionID: "session-demo-001") }

    @Test func staleOpenFailureCannotClearOrCloseReopenedSessionAttempt() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        setup.state.prepareMode = .firstRequestWaitsThenFails
        let first = Task { try await setup.model.openSession(ref) }
        try await waitUntil("first prepare request") { setup.state.requestsFor("/api/v2/sessions/session-demo-001").count == 1 }
        setup.model.closeSession(ref)
        let second = Task { try await setup.model.openSession(ref) }
        try await second.value
        await #expect(throws: Error.self) { try await first.value }
        #expect(setup.state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" && $0.httpMethod == "PATCH" }.count == 1)
        await #expect(throws: Error.self) { try await first.value }
        #expect(setup.model.openSessions[ref] != nil)
        #expect(setup.model.sessionLeases[ref] == 1)
        #expect(setup.model.streamTasks[ref] != nil)
        setup.model.closeSession(ref)
        try await waitUntil { setup.model.streamTasks[ref] == nil }
    }

    @Test func cancellingOneViewWaiterDoesNotCancelSharedOpenForAnotherOwner() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        setup.state.prepareMode = .firstRequestWaitsThenSucceeds

        let first = Task { try await setup.model.acquireSessionView(ref) }
        try await waitUntil("first view prepare request") { setup.state.requestsFor("/api/v2/sessions/session-demo-001").count == 1 }
        let second = Task { try await setup.model.acquireSessionView(ref) }
        try await waitUntil("two view lease owners") { setup.model.sessionLeases[ref] == 2 }

        first.cancel()
        try await waitUntil("cancelled view lease release") { setup.model.sessionLeases[ref] == 1 }
        let survivor = try await second.value
        await #expect(throws: CancellationError.self) { try await first.value }
        #expect(setup.model.openSessions[ref] != nil)
        #expect(setup.model.streamTasks[ref] != nil)
        #expect(setup.model.sessionLeases[ref] == 1)
        #expect(setup.state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" && $0.httpMethod == "GET" }.count == 1)
        #expect(setup.state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" && $0.httpMethod == "PATCH" }.count == 1)

        setup.model.closeSession(ref)
        #expect(setup.model.openSessions[ref] != nil, "legacy close must not consume a view-owned token")
        #expect(setup.model.sessionLeases[ref] == 1)
        setup.model.releaseSessionView(survivor)
        setup.model.releaseSessionView(survivor)
        try await waitUntil("last view lease closes stream") { setup.model.streamTasks[ref] == nil }
        #expect(setup.model.openSessions[ref] == nil)
        #expect(setup.model.sessionLeases[ref] == nil)
    }

    @Test func lateOldViewReleaseCannotCloseNewGenerationOrItsStream() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]

        let oldLease = try await setup.model.acquireSessionView(ref)
        let secondOldLease = try await setup.model.acquireSessionView(ref)
        #expect(oldLease.id != secondOldLease.id)
        setup.model.releaseSessionView(oldLease)
        setup.model.releaseSessionView(oldLease)
        #expect(setup.model.openSessions[ref] != nil)
        #expect(setup.model.sessionLeases[ref] == 1)

        setup.model.releaseSessionView(secondOldLease)
        try await waitUntil("old generation closes") { setup.model.streamTasks[ref] == nil && setup.model.openSessions[ref] == nil }
        let newLease = try await setup.model.acquireSessionView(ref)
        #expect(newLease.id != oldLease.id)
        #expect(setup.model.openSessions[ref] != nil)
        setup.model.releaseSessionView(oldLease)
        setup.model.releaseSessionView(secondOldLease)
        setup.model.closeSession(ref)
        #expect(setup.model.openSessions[ref] != nil)
        #expect(setup.model.sessionLeases[ref] == 1)
        setup.model.releaseSessionView(newLease)
        try await waitUntil("new generation closes") { setup.model.streamTasks[ref] == nil }
    }

    private func makeModel() throws -> Setup {
        let serverID = UUID()
        let host = "lifecycle-\(UUID().uuidString.lowercased()).example"
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentLifecycleTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "lifecycle", baseURL: "https://\(host)", instanceID: "lifecycle-instance")
        let state = LifecycleURLState(host: host)
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(config)
        let sessionConfiguration = URLSessionConfiguration.ephemeral
        sessionConfiguration.protocolClasses = [LifecycleURLProtocol.self]
        let urlSession = URLSession(configuration: sessionConfiguration)
        let client = try APIClient(config: config, token: "", session: urlSession)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "lifecycle-instance", ready: true)
        let tokenStore = LifecycleTokenStore()
        let model = AppModel(tokenStore: tokenStore, configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: urlSession, autoConnect: false)
        LifecycleURLProtocol.install(state)
        state.identityInstanceID = identity.instanceID
        state.serverIdentityData = try JSONEncoder().encode(identity)
        return Setup(model: model, client: client, state: state, tokenStore: tokenStore, configStore: configStore, serverID: serverID, host: host, identity: identity)
    }

    private func fixtureSnapshot(epoch: String, cursor: UInt64) throws -> SessionSnapshotWire {
        let object: [String: Any] = ["session": ["id": "session-demo-001"], "status": ["session_id": "session-demo-001", "agent_name": "root", "state": "running"], "epoch": epoch, "cursor": cursor]
        return try JSONDecoder().decode(SessionSnapshotWire.self, from: JSONSerialization.data(withJSONObject: object))
    }

    private func waitUntil(_ description: String = "lifecycle transition", timeout: Duration = .seconds(2), _ predicate: @escaping @MainActor () -> Bool) async throws {
        let start = ContinuousClock.now
        while !predicate() {
            if ContinuousClock.now - start > timeout { throw APIError("Timed out waiting for \(description).") }
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}

@MainActor
private struct Setup {
    let model: AppModel
    let client: APIClient
    let state: LifecycleURLState
    let tokenStore: LifecycleTokenStore
    let configStore: InstanceConfigStore
    let serverID: UUID
    let host: String
    let identity: ServerIdentity
}

private actor LifecycleTokenStore: TokenStoring {
    private(set) var value: String? = "saved-token"
    private(set) var mutationCount = 0
    func read(for id: UUID) async throws -> String? { value }
    func write(_ token: String, for id: UUID) async throws { value = token; mutationCount += 1 }
    func delete(for id: UUID) async throws { value = nil; mutationCount += 1 }
}

private final class LifecycleURLState: @unchecked Sendable {
    enum SSEMode { case stalled, finite, unauthorized }
    enum PrepareMode { case normal, firstRequestWaitsThenFails, firstRequestWaitsThenSucceeds }
    enum ViewMode { case legacy, metadata, metadataMissingPrepare, metadataMissingOpen, metadataWrongPrepareVersion, metadataWrongPrepareSession }
    let host: String
    private let lock = NSLock()
    private var requestStorage: [URLRequest] = []
    private var stoppedStorage = 0
    private var prepareRequestCount = 0
    var prepareMode: PrepareMode = .normal
    var viewMode: ViewMode = .legacy
    var sseMode: SSEMode = .stalled { didSet { lock.lock(); defer { lock.unlock() }; modeStorage = sseMode } }
    private var modeStorage: SSEMode = .stalled
    var identityInstanceID = "lifecycle-instance"
    var serverIdentityData = Data()
    var tailSnapshotData: Data?
    var tailSnapshotRefreshData: Data?
    private var tailSnapshotRequestCount = 0
    var tailHistoryPageData: Data?
    var tailSSEData: Data?
    var historyPageStatus = 200
    var delayHistoryPage = false
    var identityStatus = 200
    init(host: String) { self.host = host }
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return requestStorage }
    var stopLoadingCount: Int { lock.lock(); defer { lock.unlock() }; return stoppedStorage }
    func requestsFor(_ path: String) -> [URLRequest] { requests.filter { $0.url?.path == path } }
    func stop() { lock.lock(); defer { lock.unlock() }; stoppedStorage += 1 }
    func handle(_ request: URLRequest) -> (Int, Data, String, Bool, Bool) {
        lock.lock(); requestStorage.append(request); let mode = modeStorage
        let path = request.url?.path ?? ""
        if request.httpMethod == "GET", path == "/api/v2/sessions/session-demo-001" {
            prepareRequestCount += 1
            let isFirstDelayed = prepareRequestCount == 1 && prepareMode != .normal
            let firstShouldFail = prepareMode == .firstRequestWaitsThenFails
            let isMetadataRequested = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(URLQueryItem(name: "projection", value: "metadata")) == true
            let responseMode = viewMode
            lock.unlock()
            if isFirstDelayed && firstShouldFail { return (500, Data(#"{"message":"delayed stale open failed"}"#.utf8), "application/json", true, true) }
            let fixture = isMetadataRequested && responseMode != .metadataMissingPrepare ? "prepare_info_metadata.json" : "prepare_info.json"
            guard let url = fixtureURL(fixture), var prepare = try? Data(contentsOf: url) else { return (500, Data(), "application/json", true, false) }
            if responseMode == .metadataWrongPrepareVersion || responseMode == .metadataWrongPrepareSession,
               var object = try? JSONSerialization.jsonObject(with: prepare) as? [String: Any] {
                if responseMode == .metadataWrongPrepareVersion { object["version"] = 1 }
                if responseMode == .metadataWrongPrepareSession, var info = object["info"] as? [String: Any] {
                    info["session_id"] = "different-session"
                    object["info"] = info
                }
                if let changed = try? JSONSerialization.data(withJSONObject: object) { prepare = changed }
            }
            return (200, prepare, "application/json", true, isFirstDelayed)
        }
        if request.httpMethod == "PATCH", path == "/api/v2/sessions/session-demo-001" {
            let isMetadataRequested = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(URLQueryItem(name: "projection", value: "metadata")) == true
            let responseMode = viewMode
            lock.unlock()
            let fixture = isMetadataRequested && responseMode != .metadataMissingOpen ? "open_view_metadata.json" : "open_view.json"
            guard let url = fixtureURL(fixture), let opened = try? Data(contentsOf: url) else { return (500, Data(), "application/json", true, false) }
            return (200, opened, "application/json", true, false)
        }
        if request.httpMethod == "GET", path == "/api/v2/sessions/session-demo-001/snapshot" {
            tailSnapshotRequestCount += 1
            let tail = tailSnapshotRequestCount > 1 ? (tailSnapshotRefreshData ?? tailSnapshotData) : tailSnapshotData
            lock.unlock()
            if let tail { return (200, tail, "application/json", true, false) }
            guard let url = fixtureURL("snapshot.json"), let snapshot = try? Data(contentsOf: url) else { return (500, Data(), "application/json", true, false) }
            return (200, snapshot, "application/json", true, false)
        }
        if request.httpMethod == "GET", path == "/api/v2/sessions/session-demo-001/history" {
            let page = tailHistoryPageData
            let status = historyPageStatus
            let delayed = delayHistoryPage
            lock.unlock()
            if status == 409 { return (status, Data(#"{"error":"history_cursor_stale"}"#.utf8), "application/json", true, delayed) }
            return (page == nil ? 404 : status, page ?? Data(), "application/json", true, delayed)
        }
        if path == "/api/v2/server" { let status = identityStatus; let data = serverIdentityData; lock.unlock(); return (status, data, "application/json", true, false) }
        if path.hasSuffix("/events") {
            lock.unlock()
            switch mode {
            case .stalled: return (200, Data(), "text/event-stream", false, false)
            case .finite:
                let data = tailSSEData ?? Data("event: ready\ndata: {\"version\":2,\"type\":\"ready\",\"cursor\":7}\n\n".utf8)
                return (200, data, "text/event-stream", true, false)
            case .unauthorized: return (401, Data(#"{"message":"unauthorized"}"#.utf8), "application/json", true, false)
            }
        }
        lock.unlock()
        return (404, Data(#"{"message":"not found"}"#.utf8), "application/json", true, false)
    }
}

private final class LifecycleURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: LifecycleURLState] = [:]
    private var state: LifecycleURLState?
    static func install(_ state: LifecycleURLState) { lock.lock(); defer { lock.unlock() }; states[state.host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock(); self.state = state
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        let (status, data, type, finish, delayed) = state.handle(request)
        if delayed {
            DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(200)) { [weak self] in
                self?.deliver(status: status, data: data, type: type, finish: finish)
            }
            return
        }
        deliver(status: status, data: data, type: type, finish: finish)
    }
    private func deliver(status: Int, data: Data, type: String, finish: Bool) {
        guard let requestURL = request.url, let client else { return }
        let response = HTTPURLResponse(url: requestURL, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": type])!
        client.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !data.isEmpty { client.urlProtocol(self, didLoad: data) }
        if finish { client.urlProtocolDidFinishLoading(self) }
    }
    override func stopLoading() { state?.stop() }
}

private func fixtureURL(_ name: String) -> URL? {
    Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures")
}
