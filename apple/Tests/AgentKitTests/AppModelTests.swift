import Foundation
import Testing
import AgentKit
@testable import DockerAgentApple

@MainActor
struct AppModelTests {
    @Test func appModelConsumesGoStreamWithoutLosingTranscriptBoundaryState() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let model = makeModel()
        let bytes = try fixture("event_sequence.sse")
        var parser = SSEParser()
        var assembler = SnapshotAssembler()
        var cursor: SessionEventCursor?
        var projector: SessionEventProjector?
        for byte in bytes {
            for frame in try parser.append(Data([byte])) {
                guard let data = frame.data.data(using: .utf8) else { continue }
                let message = try JSONDecoder().decode(StreamMessage.self, from: data)
                if let snapshot = try assembler.accept(message) {
                    if cursor == nil {
                        cursor = SessionEventCursor(expectedSessionID: ref.sessionID)
                        projector = SessionEventProjector(projection: snapshot.project(ref: ref, connection: .live))
                        model.openSessions[ref] = snapshot.project(ref: ref, connection: .live)
                    }
                    try model.consumeStreamSnapshot(snapshot, ref: ref, cursor: &cursor, projector: &projector)
                    continue
                }
                try model.consumeStreamEvent(message.type, envelope: message.envelope, ref: ref, cursor: &cursor, projector: &projector)
            }
        }
        let transcript = try #require(model.openSessions[ref]?.transcript)
        #expect(transcript.contains { $0.kind == .message && $0.text.contains("I will inspect README.md.") })
        #expect(transcript.contains { $0.kind == .message && $0.text.contains("README.md documents the demo.") && !$0.isStreaming })
        #expect(model.openSessions[ref]?.cursor == 16)
        #expect(model.openSessions[ref]?.state == .settled)
    }

    @Test func appModelSeparatesAssistantMessagesAfterCommittedBoundary() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "s")
        let model = makeModel()
        let initial = try snapshot(sessionID: "s", epoch: "epoch-a", cursor: 0)
        model.openSessions[ref] = initial.project(ref: ref, connection: .live)
        var projector: SessionEventProjector? = SessionEventProjector(projection: initial.project(ref: ref, connection: .live))
        var cursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: "s", epoch: "epoch-a", sequence: 0, isReady: true)
        try model.consumeStreamEvent("event", envelope: event(ref, sequence: 1, type: "agent_choice", content: "first"), ref: ref, cursor: &cursor, projector: &projector)
        try model.consumeStreamEvent("event", envelope: event(ref, sequence: 2, type: "message_added", role: "assistant"), ref: ref, cursor: &cursor, projector: &projector)
        try model.consumeStreamEvent("event", envelope: event(ref, sequence: 3, type: "agent_choice", content: "second"), ref: ref, cursor: &cursor, projector: &projector)
        let messages = model.openSessions[ref]!.transcript.filter { $0.kind == .message }
        #expect(messages.map(\.text) == ["first", "second"])
        #expect(messages.map(\.id).count == Set(messages.map(\.id)).count)
    }

    private func makeModel() -> AppModel {
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let outbox = try! DurableSendOutbox(fileURL: root.appending(path: "outbox.json"))
        let reviews = ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json"))
        return AppModel(tokenStore: TestTokenStore(), configStore: config, outbox: outbox, reviewDrafts: reviews, autoConnect: false)
    }

    private func snapshot(sessionID: String, epoch: String, cursor: UInt64) throws -> SessionSnapshotWire {
        let object: [String: Any] = ["session": ["id": sessionID], "status": ["session_id": sessionID, "state": "running"], "epoch": epoch, "cursor": cursor]
        let data = try JSONSerialization.data(withJSONObject: object)
        return try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
    }

    private func event(_ ref: SessionRef, sequence: UInt64, type: String, content: String? = nil, role: String? = nil) -> StreamEnvelope {
        var payload: [String: JSONValue] = ["type": .string(type)]
        if let content { payload["content"] = .string(content) }
        if let role { payload["message_role"] = .string(role) }
        if type == "message_added" { payload["session_position"] = .number(Double(sequence)) }
        return StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: sequence, transcriptPosition: type == "message_added" ? Int(sequence) : -1, event: payload)
    }

    private func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else { throw APIError("Missing fixture") }
        return try Data(contentsOf: url)
    }
}

private actor TestTokenStore: TokenStoring {
    func read(for id: UUID) async throws -> String? { nil }
    func write(_ token: String, for id: UUID) async throws {}
    func delete(for id: UUID) async throws {}
}

extension AppModelTests {
    @Test func changingServerOriginWithBlankTokenDeletesPreviousCredential() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(InstanceConfig(id: id, name: "Old", baseURL: "https://old.example", instanceID: "old-instance"))
        let tokenStore = MemoryTokenStore()
        await tokenStore.write("old-secret", for: id)
        let session = stubSession(host: "new.example") { request in
            #expect(request.value(forHTTPHeaderField: "Authorization") == nil)
            return (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"new-instance","ready":true,"capabilities":[]}"#.utf8))
        }
        let model = AppModel(tokenStore: tokenStore, configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        let draft = ServerDraft(id: id, name: "New", baseURL: "https://new.example")
        let identity = try await model.testServer(draft)
        try await model.addOrUpdateServer(draft, identity: identity)
        #expect(await tokenStore.read(for: id) == nil)
        #expect(model.clients[id] != nil)
    }

    @Test func savingSameOriginWithBlankTokenRetainsSavedCredential() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(InstanceConfig(id: id, name: "Server", baseURL: "https://same.example", instanceID: "same-instance"))
        let tokenStore = MemoryTokenStore()
        await tokenStore.write("saved-secret", for: id)
        let session = stubSession(host: "same.example") { request in
            #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer saved-secret")
            return (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"same-instance","ready":true,"capabilities":[]}"#.utf8))
        }
        let model = AppModel(tokenStore: tokenStore, configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        let draft = ServerDraft(id: id, name: "Updated", baseURL: "https://same.example")
        let identity = try await model.testServer(draft)
        try await model.addOrUpdateServer(draft, identity: identity)
        #expect(await tokenStore.read(for: id) == "saved-secret")
    }

    @Test func sourceAwareCreationSendsSourceIDAndConfiguredRootAgent() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let session = stubSession(host: "source.example") { request in
            if request.url?.path == "/api/v2/sessions", request.httpMethod == "POST" {
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["source"] as? String == "workspace-source.yaml")
                #expect(body?["agent_name"] as? String == "planner")
                return (201, Data())
            }
            if request.url?.path == "/api/v2/sessions", request.httpMethod == "GET" {
                return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8))
            }
            return (404, Data(#"{"message":"not found"}"#.utf8))
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Server", baseURL: "https://source.example"), token: "", session: session)
        let ref = try await model.createSession(serverID: id, source: AgentOption(name: "workspace-source.yaml", rootAgentName: "planner"))
        #expect(ref.serverID == id)
        await #expect(throws: APIError.self) {
            try await model.createSession(serverID: id, source: AgentOption(name: "legacy-source"))
        }
    }

    nonisolated private static func body(_ request: URLRequest) -> Data {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var result = Data(); let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096); defer { buffer.deallocate() }
        while stream.hasBytesAvailable { let count = stream.read(buffer, maxLength: 4096); if count <= 0 { break }; result.append(buffer, count: count) }
        return result
    }

    @Test func sourceRegistrationUsesServerPathAndExplicitRemoteWorkingDirectory() async throws {
        let serverID = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://source-register.example", instanceID: "instance-register")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-register", ready: true)
        let session = stubSession(host: "source-register.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sources":
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(request.httpMethod == "POST")
                #expect(body?["path"] as? String == "teams/new.yaml")
                #expect(body?["working_dir"] as? String == "/workspace/project")
                return (200, Data(#"{"source_id":"workspace:teams/new.yaml","revision":"sha256:new"}"#.utf8))
            case "/api/agents": return (200, Data(#"[]"#.utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.servers = [ServerProjection(id: serverID, name: "Server", baseURL: config.baseURL, identity: identity, state: .live)]
        model.clients[serverID] = try APIClient(config: config, token: "", session: session)
        let result = try await model.registerSource(serverID: serverID, path: "teams/new.yaml", workingDir: "/workspace/project")
        #expect(result.sourceID == "workspace:teams/new.yaml")
        #expect(model.agentsByServer[serverID] == [])
    }

    @Test func liveIdentityMismatchBlocksAmbiguousRetryWithoutChangingPinnedIdentity() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(InstanceConfig(id: id, name: "Server", baseURL: "https://server.example", instanceID: "pinned"))
        let model = AppModel(tokenStore: TestTokenStore(), configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: stubSession(host: "server.example") { _ in
            (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"replaced","ready":true,"capabilities":[]}"#.utf8))
        }, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Server", baseURL: "https://server.example", instanceID: "pinned"), token: "", session: stubSession(host: "server.example") { _ in
            (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"replaced","ready":true,"capabilities":[]}"#.utf8))
        })
        await #expect(throws: APIError.self) { try await model.validateConnectedIdentity(serverID: id, expectedInstanceID: "pinned") }
        #expect(model.servers.first?.identity == nil)
        #expect(configStore.load().first?.instanceID == "pinned")
    }

    private func stubSession(host: String, handler: @escaping @Sendable (URLRequest) -> (Int, Data)) -> URLSession {
        StubURLProtocol.install(handler, host: host)
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StubURLProtocol.self]
        return URLSession(configuration: configuration)
    }
}

private actor MemoryTokenStore: TokenStoring {
    private var values: [UUID: String] = [:]
    func read(for id: UUID) -> String? { values[id] }
    func write(_ token: String, for id: UUID) { values[id] = token }
    func delete(for id: UUID) { values[id] = nil }
}

private final class StubURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var requestHandlers: [String: @Sendable (URLRequest) -> (Int, Data)] = [:]
    static func install(_ handler: @escaping @Sendable (URLRequest) -> (Int, Data), host: String) {
        lock.lock(); defer { lock.unlock() }; requestHandlers[host] = handler
        URLProtocol.registerClass(self)
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.lock.lock(); let handler = request.url.flatMap { Self.requestHandlers[$0.host ?? ""] }; Self.lock.unlock()
        guard let handler, let url = request.url else { client?.urlProtocol(self, didFailWithError: APIError("Missing stub handler.")); return }
        let (status, data) = handler(request)
        let response = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

extension AppModelTests {
    @Test func forkSkillEventReducesAcceptedToTerminalAndLateAcceptanceDoesNotDowngrade() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let model = makeModel()
        let projection = try snapshot(sessionID: ref.sessionID, epoch: "epoch-a", cursor: 0).project(ref: ref, connection: .live)
        model.openSessions[ref] = projection
        var cursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: "epoch-a", sequence: 0, isReady: true)
        var projector: SessionEventProjector? = SessionEventProjector(projection: projection)
        let accepted = StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: 1, transcriptPosition: -1, event: ["type": .string("skill_operation"), "session_id": .string(ref.sessionID), "operation_id": .string("op-1"), "skill": .string("review-plan"), "status": .string("accepted")])
        try model.consumeStreamEvent("event", envelope: accepted, ref: ref, cursor: &cursor, projector: &projector)
        let completed = StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: 2, transcriptPosition: -1, event: ["type": .string("skill_operation"), "session_id": .string(ref.sessionID), "operation_id": .string("op-1"), "skill": .string("review-plan"), "status": .string("completed")])
        try model.consumeStreamEvent("event", envelope: completed, ref: ref, cursor: &cursor, projector: &projector)
        let lateAccepted = StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: 3, transcriptPosition: -1, event: ["type": .string("skill_operation"), "session_id": .string(ref.sessionID), "operation_id": .string("op-1"), "skill": .string("review-plan"), "status": .string("accepted")])
        try model.consumeStreamEvent("event", envelope: lateAccepted, ref: ref, cursor: &cursor, projector: &projector)
        #expect(model.skillOperations[ref]?.first?.state == .completed)
    }

    @Test func unresolvedForkSkillBecomesUnknownAcrossEpochButSurvivesDetach() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let model = makeModel()
        model.skillOperations[ref] = [SkillOperationProjection(operationID: "op-unknown", skillName: "review-plan", state: .accepted)]
        let old = try snapshot(sessionID: ref.sessionID, epoch: "epoch-old", cursor: 2)
        let new = try snapshot(sessionID: ref.sessionID, epoch: "epoch-new", cursor: 0)
        model.openSessions[ref] = old.project(ref: ref, connection: .live)
        var cursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: "epoch-old", sequence: 2, isReady: true)
        var projector: SessionEventProjector? = SessionEventProjector(projection: old.project(ref: ref, connection: .live))
        try model.consumeStreamSnapshot(new, ref: ref, cursor: &cursor, projector: &projector)
        #expect(model.skillOperations[ref]?.first?.state == .unknown)
        model.closeSession(ref)
        #expect(model.skillOperations[ref]?.first?.operationID == "op-unknown")
    }

    @Test func localSmokeModelUsesOnlyEphemeralStoresAndDisablesNetwork() async throws {
        let model = try AppModel.localSmokeModel()
        #expect(model.servers.isEmpty)
        #expect(model.sessions.isEmpty)
        await #expect(throws: APIError.self) {
            try await model.testServer(ServerDraft(name: "blocked", baseURL: "https://example.invalid"))
        }
    }
}

extension AppModelTests {
    @Test func recentUserEditPreservesRawSessionMessageMetadataAndPatchesOnlyItsContent() async throws {
        let serverID = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://edit.example", instanceID: "instance-edit")
        try configStore.upsert(config)
        let snapshot = Data(#"{"session":{"id":"edit-session","messages":[{"message":{"agent_name":"root","message":{"role":"user","content":"Original request","created_at":"2025-01-02T03:04:05Z","multi_content":[]},"actor_accepted":true,"actor_turn_id":"turn-edit-1","actor_input_mode":"submit","input_origin":"user","unknown_future_field":{"keep":7}}}]},"status":{"session_id":"edit-session","agent_name":"root","state":"settled","pending":0}}"#.utf8)
        let session = stubSession(host: "edit.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"instance-edit","ready":true,"capabilities":[]}"#.utf8))
            case "/api/v2/sessions/edit-session/status": return (200, Data(#"{"metadata":{"session_id":"edit-session","agent_name":"root","historical":false,"execution_available":true,"capabilities":{"execution_available":true,"session_editing":true}},"status":{"session_id":"edit-session","agent_name":"root","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/edit-session/snapshot": return (200, snapshot)
            case "/api/v2/sessions/edit-session/messages/4242":
                #expect(request.httpMethod == "PATCH")
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                let outerMessage = body?["message"] as? [String: Any]
                #expect(outerMessage?["actor_accepted"] as? Bool == true)
                #expect(outerMessage?["actor_turn_id"] as? String == "turn-edit-1")
                #expect(outerMessage?["actor_input_mode"] as? String == "submit")
                #expect(outerMessage?["input_origin"] as? String == "user")
                #expect(outerMessage?["unknown_future_field"] as? [String: Int] == ["keep": 7])
                let inner = outerMessage?["message"] as? [String: Any]
                #expect(inner?["content"] as? String == "Updated request")
                #expect(inner?["created_at"] as? String == "2025-01-02T03:04:05Z")
                #expect(body?.keys.count == 1)
                return (200, Data(#"{"status":"updated"}"#.utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-edit", ready: true)
        let model = AppModel(tokenStore: TestTokenStore(), configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.servers = [ServerProjection(id: serverID, name: "Server", baseURL: config.baseURL, identity: identity, state: .live)]
        model.clients[serverID] = try APIClient(config: config, token: "", session: session)
        let ref = SessionRef(serverID: serverID, sessionID: "edit-session")
        let recentData = Data(#"{"version":1,"session_id":"edit-session","root_session_id":"edit-session","runtime_status":"active","execution_available":true,"pending":0,"attention_known":true,"approval_count":0,"elicitation_count":0,"messages":[{"id":4242,"position":0,"role":"user","agent_name":"root","text":"Original request","truncated":false}],"truncated_before":false}"#.utf8)
        let recent = try JSONDecoder().decode(RecentChatResponse.self, from: recentData)
        model.recentHistoryBySession[ref] = RecentHistoryProjection(ref: ref, messages: recent.messages, beforeCursor: recent.beforeCursor, truncatedBefore: recent.truncatedBefore)
        try await model.editRecentUserMessage(ref, messageID: 4242, replacementText: "Updated request")
    }
}

extension AppModelTests {
    @Test func verifiedBranchAnchorRequiresCurrentInstanceAndItemAndUsesExactGoProof() async throws {
        let serverID = UUID()
        let ref = SessionRef(serverID: serverID, sessionID: "branch-source")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://branch-proof.example", instanceID: "instance-branch")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try configStore.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-branch", ready: true)
        let messages = #"[{"message":{"agent_name":"","message":{"role":"user","content":"first","created_at":"2025-01-02T03:04:05Z"}}},{"message":{"agent_name":"root","message":{"role":"assistant","content":"answer"}}}]"#
        let snapshot = Data((#"{"session":{"id":"branch-source","messages": "# + messages + #"},"status":{"session_id":"branch-source","agent_name":"root","state":"settled","pending":0},"epoch":"branch-epoch","cursor":5}"#).utf8)
        let recorder = AppModelRequestRecorder()
        let session = stubSession(host: "branch-proof.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/branch-source/status":
                return (200, Data(#"{"metadata":{"session_id":"branch-source","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"branching":true}},"status":{"session_id":"branch-source","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/branch-source/snapshot": return (200, snapshot)
            case "/api/v2/sessions/branch-source/branches":
                recorder.record(request)
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["position"] as? Int == 1)
                #expect(body?["expected_snapshot"] as? String == "756ba302c5b2d89ee2c08e9f4041fe05fe4ab902d4ffb85243e2f42961c07c27")
                return (201, Data(#"{"metadata":{"session_id":"branch-child","agent_name":"root"},"session":{"id":"branch-child","title":"Branch"}}"#.utf8))
            case "/api/v2/sessions": return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: configStore, identity: identity, session: session)
        let proof = try RawSnapshotProof.extract(from: snapshot, expectedSessionID: ref.sessionID)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: snapshot)
        model.openSessions[ref] = wire.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: "instance-branch")
        let anchor = try #require(model.openSessions[ref]?.transcript.first?.branchAnchor)
        #expect(anchor.itemPosition == 0)
        #expect(anchor.position == 1)
        let child = try await model.branchSessionItem(ref, anchor: anchor, confirmed: true)
        #expect(child.sessionID == "branch-child")
        #expect(recorder.requests.count == 1)
    }

    @Test func staleOrCrossInstanceBranchAnchorNeverPosts() async throws {
        let serverID = UUID()
        let ref = SessionRef(serverID: serverID, sessionID: "branch-stale")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://branch-stale.example", instanceID: "instance-stale")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try configStore.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-stale", ready: true)
        let original = Data(#"{"session":{"id":"branch-stale","messages":[{"summary":"one"}]},"status":{"session_id":"branch-stale","state":"settled","pending":0},"epoch":"e1","cursor":1}"#.utf8)
        let changed = Data(#"{"session":{"id":"branch-stale","messages":[{"summary":"changed"}]},"status":{"session_id":"branch-stale","state":"settled","pending":0},"epoch":"e1","cursor":1}"#.utf8)
        let currentSnapshot = AppModelSnapshotBox(changed)
        let recorder = AppModelRequestRecorder()
        let session = stubSession(host: "branch-stale.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/branch-stale/status": return (200, Data(#"{"metadata":{"session_id":"branch-stale","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"branching":true}},"status":{"session_id":"branch-stale","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/branch-stale/snapshot": return (200, currentSnapshot.next())
            case "/api/v2/sessions/branch-stale/branches": recorder.record(request); return (201, Data())
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: configStore, identity: identity, session: session)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: original)
        let proof = try RawSnapshotProof.extract(from: original, expectedSessionID: ref.sessionID)
        model.openSessions[ref] = wire.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: "instance-stale")
        let anchor = try #require(model.openSessions[ref]?.transcript.first?.branchAnchor)
        let foreign = SessionBranchAnchor(ref: ref, instanceID: "other-instance", epoch: anchor.epoch, cursor: anchor.cursor, messagesSHA256: anchor.messagesSHA256, itemPosition: anchor.itemPosition, itemSHA256: anchor.itemSHA256)
        await #expect(throws: SessionAdvancedError.staleBranchAnchor) { try await model.branchSessionItem(ref, anchor: foreign, confirmed: true) }
        await #expect(throws: SessionAdvancedError.staleBranchAnchor) { try await model.branchSessionItem(ref, anchor: anchor, confirmed: true) }
        #expect(recorder.requests.isEmpty)
    }

    @Test func toolsApprovalToggleRequiresAcknowledgmentComparesFreshStateAndDoesNotRetryUnknownPost() async throws {
        let serverID = UUID()
        let ref = SessionRef(serverID: serverID, sessionID: "toggle-session")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://toggle-app.example", instanceID: "instance-toggle")
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try configStore.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-toggle", ready: true)
        let permissionsRecorder = AppModelRequestRecorder()
        let toggleRecorder = AppModelRequestRecorder()
        let session = stubSession(host: "toggle-app.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/toggle-session/status": return (200, Data(#"{"metadata":{"session_id":"toggle-session","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true,"permissions_inspection":true}},"status":{"session_id":"toggle-session","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/toggle-session/permissions":
                let read = permissionsRecorder.record(request)
                let enabled = read >= 3
                return (200, Data((#"{"policy":"balanced","tools_approved":"# + String(enabled ? "true" : "false") + #","source":{},"session":{}}"#).utf8))
            case "/api/v2/sessions/toggle-session/tools/toggle":
                toggleRecorder.record(request)
                #expect(request.httpMethod == "POST")
                #expect(Self.body(request).isEmpty)
                return (200, Data("null".utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: configStore, identity: identity, session: session)
        await #expect(throws: SessionAdvancedError.autonomousConfirmationRequired) { try await model.toggleToolsApproval(ref, confirmedAutonomous: false) }
        #expect(toggleRecorder.requests.isEmpty)
        await #expect(throws: APIError.self) { try await model.toggleToolsApproval(ref, confirmedAutonomous: true) }
        #expect(toggleRecorder.requests.isEmpty)
    }

    @Test func serverBranchCASConflictIsReturnedWithoutRetryOrRetarget() async throws {
        let serverID = UUID(), ref = SessionRef(serverID: serverID, sessionID: "branch-cas")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://branch-cas.example", instanceID: "instance-cas")
        let store = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try store.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-cas", ready: true)
        let raw = Data(#"{"session":{"id":"branch-cas","messages":[{"summary":"selected item"}]},"status":{"session_id":"branch-cas","state":"settled","pending":0},"epoch":"cas-epoch","cursor":3}"#.utf8)
        let posts = AppModelRequestRecorder()
        let session = stubSession(host: "branch-cas.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/branch-cas/status": return (200, Data(#"{"metadata":{"session_id":"branch-cas","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"branching":true}},"status":{"session_id":"branch-cas","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/branch-cas/snapshot": return (200, raw)
            case "/api/v2/sessions/branch-cas/branches":
                posts.record(request)
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["position"] as? Int == 1)
                return (409, Data(#"{"message":"snapshot proof is stale"}"#.utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: store, identity: identity, session: session)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: raw)
        let proof = try RawSnapshotProof.extract(from: raw, expectedSessionID: ref.sessionID)
        model.openSessions[ref] = wire.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: identity.instanceID)
        let anchor = try #require(model.openSessions[ref]?.transcript.first?.branchAnchor)
        let originalProjection = try #require(model.openSessions[ref])
        await #expect(throws: APIError.self) { try await model.branchSessionItem(ref, anchor: anchor, confirmed: true) }
        #expect(posts.requests.count == 1)
        #expect(model.openSessions[ref] == originalProjection)
    }

    @Test func toolsApprovalTogglePostsOnceAndRefreshesRememberedServerMode() async throws {
        let serverID = UUID(), ref = SessionRef(serverID: serverID, sessionID: "toggle-success")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://toggle-success.example", instanceID: "instance-toggle-success")
        let store = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try store.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-toggle-success", ready: true)
        let reads = AppModelRequestRecorder(), posts = AppModelRequestRecorder()
        let session = stubSession(host: "toggle-success.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/toggle-success/status": return (200, Data(#"{"metadata":{"session_id":"toggle-success","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true,"permissions_inspection":true}},"status":{"session_id":"toggle-success","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/toggle-success/permissions":
                let enabled = reads.record(request) >= 3
                return (200, Data((#"{"policy":"balanced","tools_approved":"# + String(enabled) + #","source":{},"session":{}}"#).utf8))
            case "/api/v2/sessions/toggle-success/tools/toggle":
                posts.record(request); #expect(Self.body(request).isEmpty); return (200, Data("null".utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: store, identity: identity, session: session)
        let result = try await model.toggleToolsApproval(ref, confirmedAutonomous: true)
        #expect(result.toolsApproved)
        #expect(posts.requests.count == 1)
        #expect(posts.requests[0].url?.path == "/api/v2/sessions/toggle-success/tools/toggle")
    }

    @Test func uncertainToolsToggleFailureIsNeverAutomaticallyRetried() async throws {
        let serverID = UUID(), ref = SessionRef(serverID: serverID, sessionID: "toggle-unknown")
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://toggle-unknown.example", instanceID: "instance-toggle-unknown")
        let store = InstanceConfigStore(fileURL: root.appending(path: "instances.json")); try store.upsert(config)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "instance-toggle-unknown", ready: true)
        let posts = AppModelRequestRecorder()
        let session = stubSession(host: "toggle-unknown.example") { request in
            switch request.url?.path {
            case "/api/v2/server": return (200, try! JSONEncoder().encode(identity))
            case "/api/v2/sessions/toggle-unknown/status": return (200, Data(#"{"metadata":{"session_id":"toggle-unknown","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true,"permissions_inspection":true}},"status":{"session_id":"toggle-unknown","state":"settled","pending":0}}"#.utf8))
            case "/api/v2/sessions/toggle-unknown/permissions": return (200, Data(#"{"policy":"balanced","tools_approved":false,"source":{},"session":{}}"#.utf8))
            case "/api/v2/sessions/toggle-unknown/tools/toggle": posts.record(request); return (500, Data(#"{"message":"connection lost after dispatch"}"#.utf8))
            default: return (404, Data(#"{"message":"unexpected request"}"#.utf8))
            }
        }
        let model = makeNetworkModel(serverID: serverID, config: config, configStore: store, identity: identity, session: session)
        await #expect(throws: APIError.self) { try await model.toggleToolsApproval(ref, confirmedAutonomous: true) }
        #expect(posts.requests.count == 1)
    }

    @Test func transcriptEventsInvalidateBranchProofUntilVerifiedRefresh() throws {
        let serverID = UUID(), ref = SessionRef(serverID: serverID, sessionID: "branch-refresh")
        let raw = Data(#"{"session":{"id":"branch-refresh","messages":[{"summary":"safe point"}]},"status":{"session_id":"branch-refresh","state":"settled","pending":0},"epoch":"refresh-epoch","cursor":4}"#.utf8)
        let proof = try RawSnapshotProof.extract(from: raw, expectedSessionID: ref.sessionID)
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: raw)
        let model = makeModel()
        let projected = snapshot.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: "instance")
        model.openSessions[ref] = projected
        var cursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: "refresh-epoch", sequence: 4, isReady: true)
        var projector: SessionEventProjector? = SessionEventProjector(projection: projected)
        let event = StreamEnvelope(sessionID: ref.sessionID, epoch: "refresh-epoch", sequence: 5, transcriptPosition: -1, event: ["type": .string("session_title"), "title": .string("Changed")])
        try model.consumeStreamEvent("event", envelope: event, ref: ref, cursor: &cursor, projector: &projector)
        #expect(model.openSessions[ref]?.transcript.first?.branchAnchor == nil)
        projector?.replace(with: snapshot, rawProof: proof, branchInstanceID: "instance")
        #expect(projector?.projection.transcript.first?.branchAnchor?.messagesSHA256 == proof.messagesSHA256)
    }

    private func makeNetworkModel(serverID: UUID, config: InstanceConfig, configStore: InstanceConfigStore, identity: ServerIdentity, session: URLSession) -> AppModel {
        let model = AppModel(tokenStore: TestTokenStore(), configStore: configStore,
                             outbox: try! DurableSendOutbox(fileURL: FileManager.default.temporaryDirectory.appending(path: "outbox-\\(UUID().uuidString).json")),
                             reviewDrafts: ReviewDraftStore(fileURL: FileManager.default.temporaryDirectory.appending(path: "draft-\\(UUID().uuidString).json")),
                             urlSession: session, autoConnect: false)
        model.servers = [ServerProjection(id: serverID, name: "Server", baseURL: config.baseURL, identity: identity, state: .live)]
        model.clients[serverID] = try! APIClient(config: config, token: "", session: session)
        return model
    }
}

private final class AppModelRequestRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var storage: [URLRequest] = []
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return storage }
    @discardableResult func record(_ request: URLRequest) -> Int { lock.lock(); defer { lock.unlock() }; storage.append(request); return storage.count }
}

private final class AppModelSnapshotBox: @unchecked Sendable {
    private let value: Data
    init(_ value: Data) { self.value = value }
    func next() -> Data { value }
}
