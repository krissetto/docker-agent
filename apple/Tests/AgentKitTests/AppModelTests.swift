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

    @Test func crossPageToolResultsLinkOnlyToOneOwnerScopedCanonicalCall() {
        let call = TranscriptItem(id: "history-call", kind: .tool, owner: "root", role: "assistant", text: "read_file", toolName: "read_file", toolArguments: "{\"path\":\"a.txt\"}", toolStatus: "complete", toolCallID: "call-a", sourcePosition: 0, sourcePartIndex: 1, sourceHistoryGeneration: "g")
        let result = TranscriptItem(id: "history-result", kind: .tool, owner: "root", role: "tool", text: "Tool result", toolOutput: "contents", toolStatus: "complete", toolCallID: "call-a", sourcePosition: 1, sourcePartIndex: 0, sourceHistoryGeneration: "g")
        let merged = AppModel.mergeOlderHistory(older: [call], current: [result])
        #expect(merged.map(\.sourcePosition) == [0, 1], "separate canonical call and result records survive page boundaries")
        let linked = AppModel.linkHistoricalToolRows(merged)
        #expect(linked[1].toolName == "read_file")
        #expect(linked[1].toolArguments == "{\"path\":\"a.txt\"}")
        #expect(linked[1].toolOutput == "contents")

        let ambiguous = AppModel.linkHistoricalToolRows([call, call, result])
        #expect(ambiguous[2].toolName == nil, "duplicate calls do not guess a pairing across pages")
        let otherAgent = TranscriptItem(id: "other-call", kind: .tool, owner: "worker", role: "assistant", text: "read_file", toolName: "read_file", toolCallID: "call-a", sourcePosition: 2, sourcePartIndex: 1, sourceHistoryGeneration: "g")
        let scoped = AppModel.linkHistoricalToolRows([call, otherAgent, result])
        #expect(scoped[2].toolName == "read_file")
        #expect(scoped[2].toolArguments == "{\"path\":\"a.txt\"}")
    }

    @Test func tailStreamResetKeepsOnlyQueuedInputsAcceptedAfterRebaselineCursor() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "tail-reset")
        func wire(_ cursor: UInt64) throws -> SessionSnapshotWire {
            try JSONDecoder().decode(SessionSnapshotWire.self, from: Data("""
            {"projection":"tail","session":{"id":"tail-reset"},"status":{"session_id":"tail-reset","agent_name":"root"},"epoch":"epoch-a","cursor":\(cursor),"transcript_position":2,"items":[{"position":0,"item":{"summary":"older"}},{"position":1,"item":{"message":{"agent_name":"root","message":{"role":"assistant","content":"newer"}}}}],"history":{"start_position":0,"end_position":2,"total_items":2,"items":[],"truncated":false}}
            """.utf8))
        }
        let first = try wire(1)
        var projector = SessionEventProjector(projection: first.project(ref: ref, connection: .live))
        var queued = StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: 2, transcriptPosition: -1,
                                    event: ["type": .string("pending_user_message_accepted"), "turn_id": .string("turn-a"),
                                            "message": .string("cancelable"), "input_origin": .string("user")])
        projector.apply(queued)
        projector.replace(with: try wire(4))
        #expect(!projector.projection.transcript.contains { $0.turnID == "turn-a" }, "the authoritative rebaseline subsumes queued events at or before its cursor")
        queued = StreamEnvelope(sessionID: ref.sessionID, epoch: "epoch-a", sequence: 5, transcriptPosition: -1,
                                event: ["type": .string("pending_user_message_accepted"), "turn_id": .string("turn-b"),
                                        "message": .string("new after baseline"), "input_origin": .string("user")])
        projector.apply(queued)
        #expect(projector.projection.transcript.contains { $0.turnID == "turn-b" }, "post-baseline accepted input events remain visible")
        projector.replace(with: try wire(5))
        #expect(!projector.projection.transcript.contains { $0.turnID == "turn-b" }, "the next tail baseline subsumes the accepted event")
        #expect(projector.projection.transcript.map(\.text) == ["older", "newer"])
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

    private func waitUntilPublished(_ predicate: @escaping () -> Bool) async throws {
        let start = ContinuousClock.now
        while !predicate() {
            if ContinuousClock.now - start > .seconds(2) { throw APIError("Projection publication did not complete.") }
            try await Task.sleep(for: .milliseconds(10))
        }
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
    @Test func projectionPublicationThrottlePublishesLatestBurstAfterIdle() async throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "s")
        var published: [UInt64] = []
        let throttle = ProjectionPublicationThrottle(interval: .milliseconds(10)) { projection in published.append(projection.cursor) }
        defer { throttle.cancel() }
        var first = OpenSessionProjection(ref: ref, title: "test", agentName: "root")
        first.cursor = 1
        var latest = first
        latest.cursor = 2
        throttle.schedule(first)
        throttle.schedule(latest)
        try await waitUntilPublished { published == [2] }
        #expect(published == [2])
    }

    @Test func projectionPublicationThrottleDropsPendingUpdateWhenStreamIsSuperseded() async throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "s")
        let generation = PublicationGeneration()
        var published: [UInt64] = []
        let throttle = ProjectionPublicationThrottle(interval: .milliseconds(10)) { projection in
            guard generation.isCurrent else { return }
            published.append(projection.cursor)
        }
        defer { throttle.cancel() }
        var projection = OpenSessionProjection(ref: ref, title: "test", agentName: "root")
        projection.cursor = 1
        throttle.schedule(projection)
        generation.invalidate()
        try await Task.sleep(for: .milliseconds(100))
        #expect(published.isEmpty)
    }

    @Test func urgentProjectionPublicationCancelsPendingOlderValue() async throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "s")
        var published: [UInt64] = []
        let throttle = ProjectionPublicationThrottle(interval: .milliseconds(10)) { projection in published.append(projection.cursor) }
        defer { throttle.cancel() }
        var ordinary = OpenSessionProjection(ref: ref, title: "test", agentName: "root")
        ordinary.cursor = 1
        var terminal = ordinary
        terminal.cursor = 2
        throttle.schedule(ordinary)
        throttle.publishImmediately(terminal)
        try await waitUntilPublished { published == [2] }
        #expect(published == [2])
    }

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

    @Test func sourceAwareCreationSendsSourceIDRootAgentAndServerWorkspaceAndCoalescesDuplicateTaps() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let store = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "source-instance", ready: true, workspaceRoot: "/srv/work/project", source: "workspace-source.yaml")
        try store.upsert(InstanceConfig(id: id, name: "Server", baseURL: "https://source.example", instanceID: identity.instanceID))
        let createCount = LockedCounter()
        let session = stubSession(host: "source.example") { request in
            if request.url?.path == "/api/v2/sessions", request.httpMethod == "POST" {
                createCount.increment()
                Thread.sleep(forTimeInterval: 0.1)
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["source"] as? String == "workspace-source.yaml")
                #expect(body?["agent_name"] as? String == "planner")
                #expect(body?["working_dir"] as? String == "/srv/work/project")
                return (201, Data())
            }
            if request.url?.path == "/api/v2/sessions", request.httpMethod == "GET" {
                return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8))
            }
            return (404, Data(#"{"message":"not found"}"#.utf8))
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: store, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Server", baseURL: "https://source.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Server", baseURL: "https://source.example", identity: identity, state: .live)]
        let source = AgentOption(name: "workspace-source.yaml", rootAgentName: "planner")
        let operationID = UUID()
        let first = Task { try await model.createSession(serverID: id, source: source, operationID: operationID) }
        let second = Task { try await model.createSession(serverID: id, source: source, operationID: operationID) }
        let firstRef = try await first.value
        let secondRef = try await second.value
        #expect(firstRef == secondRef)
        #expect(createCount.value == 1)
        #expect(firstRef.serverID == id)
        await #expect(throws: APIError.self) {
            try await model.createSession(serverID: id, source: AgentOption(name: "legacy-source"))
        }
        #expect(createCount.value == 1)
    }

    @Test func defaultSourceUsesServerIdentityNotAlphabeticOrderAndHonorsExplicitChoice() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let store = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "defaults-instance", ready: true, workspaceRoot: "/kit/project", source: "z-kit-default.yaml")
        let config = InstanceConfig(id: id, name: "Kit", baseURL: "https://defaults.example", instanceID: identity.instanceID)
        try store.upsert(config)
        let session = stubSession(host: "defaults.example") { request in
            if request.url?.path == "/api/agents" {
                return (200, Data(#"[{"name":"a-alphabetic.yaml","root_agent_name":"wrong-root","description":"","multi":false},{"name":"z-kit-default.yaml","root_agent_name":"kit-root","description":"","multi":false}]"#.utf8))
            }
            return (404, Data(#"{"message":"not found"}"#.utf8))
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: store, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.clients[id] = try APIClient(config: config, token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Kit", baseURL: config.baseURL, identity: identity, state: .live)]
        #expect(try await model.defaultSessionSource(serverID: id).name == "z-kit-default.yaml")
        let explicit = AgentOption(name: "a-alphabetic.yaml", rootAgentName: "wrong-root")
        _ = try await model.loadAgents(serverID: id)
        try model.rememberSessionSource(explicit, serverID: id)
        #expect(try await model.defaultSessionSource(serverID: id).name == "a-alphabetic.yaml")

        let reloaded = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        #expect(reloaded.load().first?.preferredSource == "a-alphabetic.yaml")
    }

    @Test func defaultSourceReportsEmptyCatalogDisconnectedServerAndUnresolvableMultiSource() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let session = stubSession(host: "empty.example") { _ in (200, Data("[]".utf8)) }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Empty", baseURL: "https://empty.example"), token: "", session: session)
        await #expect(throws: APIError.self) { try await model.defaultSessionSource(serverID: id) }
        model.clients[id] = nil
        await #expect(throws: APIError.self) { try await model.defaultSessionSource(serverID: id) }

        let multiID = UUID()
        let multiSession = stubSession(host: "multi.example") { _ in
            (200, Data(#"[{"name":"a.yaml","root_agent_name":"one","description":"","multi":false},{"name":"b.yaml","root_agent_name":"two","description":"","multi":false}]"#.utf8))
        }
        model.clients[multiID] = try APIClient(config: InstanceConfig(id: multiID, name: "Multi", baseURL: "https://multi.example"), token: "", session: multiSession)
        model.servers.append(ServerProjection(id: multiID, name: "Multi", baseURL: "https://multi.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "multi", ready: true), state: .live))
        await #expect(throws: APIError.self) { try await model.defaultSessionSource(serverID: multiID) }
    }

    @Test func soleStartableEntryInMultiSourceCatalogIsNotGuessedAsDefault() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let session = stubSession(host: "mixed.example") { _ in
            (200, Data(#"[{"name":"a-incomplete.yaml","description":"","multi":false},{"name":"z-startable.yaml","root_agent_name":"planner","description":"","multi":false}]"#.utf8))
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Mixed", baseURL: "https://mixed.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Mixed", baseURL: "https://mixed.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "mixed", ready: true), state: .live)]
        await #expect(throws: APIError.self) { try await model.defaultSessionSource(serverID: id) }
    }

    @Test func unresolvedServerDefaultAndMissingRootFailWithoutPosting() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let posts = LockedCounter()
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "default-missing", ready: true, source: "kit-default.yaml")
        let session = stubSession(host: "missing-root.example") { request in
            if request.url?.path == "/api/agents" {
                return (200, Data(#"[{"name":"kit-default.yaml","description":"","multi":false}]"#.utf8))
            }
            if request.httpMethod == "POST" { posts.increment() }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Kit", baseURL: "https://missing-root.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Kit", baseURL: "https://missing-root.example", identity: identity, state: .live)]
        await #expect(throws: APIError.self) { _ = try await model.createDefaultSession(serverID: id) }
        await #expect(throws: APIError.self) { try await model.createSession(serverID: id, source: AgentOption(name: "kit-default.yaml")) }
        #expect(posts.value == 0)
    }

    @Test func soleConfiguredCatalogEntryIsAnAllowedFallbackDefault() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let session = stubSession(host: "single-source.example") { _ in
            (200, Data(#"[{"name":"single.yaml","root_agent_name":"planner","description":"","multi":false}]"#.utf8))
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Single", baseURL: "https://single-source.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Single", baseURL: "https://single-source.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "single", ready: true), state: .live)]
        #expect(try await model.defaultSessionSource(serverID: id).name == "single.yaml")
    }

    @Test func defaultCreateFencesSlowSourceLookupBeforePostingAfterProfileReplacement() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let started = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let posts = LockedCounter()
        let oldSession = stubSession(host: "slow-default.example") { request in
            if request.url?.path == "/api/agents" {
                started.signal()
                Thread.sleep(forTimeInterval: 0.08)
                return (200, Data(#"[{"name":"team.yaml","root_agent_name":"planner","description":"","multi":false}]"#.utf8))
            }
            if request.httpMethod == "POST" { posts.increment() }
            return (404, Data())
        }
        let newSession = stubSession(host: "replacement-default.example") { request in
            if request.url?.path == "/api/agents" {
                return (200, Data(#"[{"name":"team.yaml","root_agent_name":"planner","description":"","multi":false}]"#.utf8))
            }
            if request.httpMethod == "POST" { posts.increment() }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Old", baseURL: "https://slow-default.example"), token: "", session: oldSession)
        model.servers = [ServerProjection(id: id, name: "Old", baseURL: "https://slow-default.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "old-default", ready: true, source: "team.yaml"), state: .live)]
        let creating = Task { try await model.createDefaultSession(serverID: id) }
        let didStart = await Task.detached { Self.waitForRequest(started) }.value
        #expect(didStart == .success)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "New", baseURL: "https://replacement-default.example"), token: "", session: newSession)
        model.servers = [ServerProjection(id: id, name: "New", baseURL: "https://replacement-default.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "new-default", ready: true, source: "team.yaml"), state: .live)]
        release.signal()
        await #expect(throws: APIError.self) { _ = try await creating.value }
        #expect(posts.value == 0)
    }

    @Test func simultaneousProfileCreatesStayOnTheirOwnServer() async throws {
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let firstID = UUID()
        let secondID = UUID()
        let firstSession = stubSession(host: "first-profile.example") { request in
            if request.httpMethod == "POST", request.url?.path == "/api/v2/sessions" {
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["source"] as? String == "first.yaml")
                return (201, Data())
            }
            if request.httpMethod == "GET", request.url?.path == "/api/v2/sessions" { return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8)) }
            return (404, Data())
        }
        let secondSession = stubSession(host: "second-profile.example") { request in
            if request.httpMethod == "POST", request.url?.path == "/api/v2/sessions" {
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["source"] as? String == "second.yaml")
                return (201, Data())
            }
            if request.httpMethod == "GET", request.url?.path == "/api/v2/sessions" { return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8)) }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[firstID] = try APIClient(config: InstanceConfig(id: firstID, name: "First", baseURL: "https://first-profile.example"), token: "", session: firstSession)
        model.clients[secondID] = try APIClient(config: InstanceConfig(id: secondID, name: "Second", baseURL: "https://second-profile.example"), token: "", session: secondSession)
        model.servers = [
            ServerProjection(id: firstID, name: "First", baseURL: "https://first-profile.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "first", ready: true), state: .live),
            ServerProjection(id: secondID, name: "Second", baseURL: "https://second-profile.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "second", ready: true), state: .live),
        ]
        async let first = model.createSession(serverID: firstID, source: AgentOption(name: "first.yaml", rootAgentName: "first-root"))
        async let second = model.createSession(serverID: secondID, source: AgentOption(name: "second.yaml", rootAgentName: "second-root"))
        let (firstRef, secondRef) = try await (first, second)
        #expect(firstRef.serverID == firstID)
        #expect(secondRef.serverID == secondID)
        #expect(firstRef.sessionID != secondRef.sessionID)
    }

    @Test func defaultCreateCoalescesSourceResolutionAndPostAsOneOperation() async throws {
        let id = UUID()
        let operationID = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let agentGets = LockedCounter()
        let posts = LockedCounter()
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "coalesce-default", ready: true,
                                      workspaceRoot: "/srv/work", source: "kit.yaml")
        let session = stubSession(host: "coalesce-default.example") { request in
            if request.url?.path == "/api/agents" {
                agentGets.increment()
                Thread.sleep(forTimeInterval: 0.08)
                return (200, Data(#"[{"name":"kit.yaml","root_agent_name":"planner","description":"","multi":false}]"#.utf8))
            }
            if request.httpMethod == "POST", request.url?.path == "/api/v2/sessions" {
                posts.increment()
                let body = (try? JSONSerialization.jsonObject(with: Self.body(request))) as? [String: Any]
                #expect(body?["working_dir"] as? String == "/srv/work")
                return (201, Data())
            }
            if request.httpMethod == "GET", request.url?.path == "/api/v2/sessions" { return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8)) }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Kit", baseURL: "https://coalesce-default.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Kit", baseURL: "https://coalesce-default.example", identity: identity, state: .live)]
        let first = Task { try await model.createDefaultSession(serverID: id, operationID: operationID) }
        let second = Task { try await model.createDefaultSession(serverID: id, operationID: operationID) }
        #expect(try await first.value == second.value)
        #expect(agentGets.value == 1)
        #expect(posts.value == 1)
    }

    @Test func retryAfterCreateConflictResolvesOnlyMatchingStableSessionID() async throws {
        let id = UUID()
        let operationID = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let posts = LockedCounter()
        let config = InstanceConfig(id: id, name: "Kit", baseURL: "https://create-conflict.example")
        let session = stubSession(host: "create-conflict.example") { request in
            if request.httpMethod == "POST" {
                posts.increment()
                return (409, Data(#"{"message":"session id already exists"}"#.utf8))
            }
            if request.httpMethod == "GET", request.url?.path == "/api/v2/sessions" {
                let limit = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "limit" })?.value
                #expect(limit == "50")
                return (200, Data(#"{"version":2,"sessions":[],"sources":[]}"#.utf8))
            }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), autoConnect: false)
        model.clients[id] = try APIClient(config: config, token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Kit", baseURL: config.baseURL, identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "conflict", ready: true), state: .live)]
        do {
            _ = try await model.createSession(serverID: id, source: AgentOption(name: "team.yaml", rootAgentName: "planner"), operationID: operationID)
            Issue.record("A 409 must be confirmed in the catalog before returning a session reference.")
        } catch let error as APIError {
            #expect(error.message.contains("previous create attempt may have committed"))
        }
        #expect(posts.value == 1)
    }

    @Test func sourceAwareCreateFailureRemainsVisibleAndDoesNotRefreshAsSuccess() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let createCount = LockedCounter()
        let session = stubSession(host: "reject.example") { request in
            if request.httpMethod == "POST", request.url?.path == "/api/v2/sessions" {
                createCount.increment()
                return (422, Data(#"{"message":"agent team is unavailable"}"#.utf8))
            }
            return (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: session, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Reject", baseURL: "https://reject.example"), token: "", session: session)
        model.servers = [ServerProjection(id: id, name: "Reject", baseURL: "https://reject.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "reject", ready: true), state: .live)]
        let operationID = UUID()
        do {
            _ = try await model.createSession(serverID: id, source: AgentOption(name: "team.yaml", rootAgentName: "planner"), operationID: operationID)
            Issue.record("Expected source rejection")
        } catch let error as APIError {
            #expect(error.message == "agent team is unavailable")
        }
        do {
            _ = try await model.createSession(serverID: id, source: AgentOption(name: "edited-team.yaml", rootAgentName: "planner"), operationID: operationID)
            Issue.record("Expected edited source rejection")
        } catch let error as APIError {
            #expect(error.message == "agent team is unavailable")
        }
        #expect(createCount.value == 2)
        #expect(model.sessions.isEmpty)
    }

    @Test func profileSwitchDuringCreateFencesResultFromNewClient() async throws {
        let id = UUID()
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppModelTests/\(UUID().uuidString)")
        let started = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let original = stubSession(host: "fence.example") { request in
            if request.httpMethod == "POST" {
                started.signal()
                _ = release.wait(timeout: .now() + 2)
                return (201, Data())
            }
            return (404, Data())
        }
        let replacement = stubSession(host: "replacement.example") { _ in (404, Data()) }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: InstanceConfigStore(fileURL: root.appending(path: "instances.json")), outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: original, autoConnect: false)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Original", baseURL: "https://fence.example"), token: "", session: original)
        model.servers = [ServerProjection(id: id, name: "Original", baseURL: "https://fence.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "original", ready: true), state: .live)]
        let creating = Task { try await model.createSession(serverID: id, source: AgentOption(name: "team.yaml", rootAgentName: "planner")) }
        let requestStarted = await Task.detached { Self.waitForRequest(started) }.value
        #expect(requestStarted == .success)
        model.clients[id] = try APIClient(config: InstanceConfig(id: id, name: "Replacement", baseURL: "https://replacement.example"), token: "", session: replacement)
        model.servers = [ServerProjection(id: id, name: "Replacement", baseURL: "https://replacement.example", identity: ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "replacement", ready: true), state: .live)]
        release.signal()
        await #expect(throws: APIError.self) { _ = try await creating.value }
    }

    nonisolated private static func waitForRequest(_ semaphore: DispatchSemaphore) -> DispatchTimeoutResult {
        semaphore.wait(timeout: .now() + 2)
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

    @Test func forkFromPartialTranscriptFailsBeforeAnyNetworkRequest() async throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "partial-fork")
        let model = makeModel()
        model.openSessions[ref] = OpenSessionProjection(ref: ref, title: "Session", agentName: "root",
                                                         transcriptWindow: TranscriptWindow(isComplete: false, omittedItemCount: 25, olderCursor: "opaque"))
        await #expect(throws: SessionAdvancedError.transcriptWindowIncomplete) {
            try await model.forkBeforeUserMessage(ref, index: 0, confirmed: true)
        }
        #expect(model.clients[ref.serverID] == nil, "the incomplete-history check runs before attempting to acquire a client")
    }

    @Test func sessionRefreshKeepsUserTurnIdentityAcrossChangedSnapshotText() async throws {
        let serverID = UUID(), ref = SessionRef(serverID: serverID, sessionID: "refresh-user-turn")
        let config = InstanceConfig(id: serverID, name: "Server", baseURL: "https://refresh-user-turn.example", instanceID: "instance")
        let store = InstanceConfigStore(fileURL: FileManager.default.temporaryDirectory.appending(path: "refresh-user-turn-\(UUID().uuidString)/instances.json"))
        try store.upsert(config)
        let response = Data(#"{"session":{"id":"refresh-user-turn","messages":[{"message":{"agent_name":"root","actor_turn_id":"stable-turn","actor_accepted":true,"input_origin":"user","pending":false,"message":{"role":"user","content":"server-final text"}}}]},"status":{"session_id":"refresh-user-turn","agent_name":"root","state":"settled"},"epoch":"refresh-epoch","cursor":5}"#.utf8)
        let urlSession = stubSession(host: "refresh-user-turn.example") { request in
            request.url?.path == "/api/v2/sessions/refresh-user-turn/snapshot" ? (200, response) : (404, Data())
        }
        let model = AppModel(tokenStore: TestTokenStore(), configStore: store, outbox: try DurableSendOutbox(fileURL: FileManager.default.temporaryDirectory.appending(path: "refresh-user-turn-\(UUID().uuidString)-outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: FileManager.default.temporaryDirectory.appending(path: "refresh-user-turn-\(UUID().uuidString)-drafts.json")), urlSession: urlSession, autoConnect: false)
        model.clients[serverID] = try APIClient(config: config, token: "", session: urlSession)
        model.openSessions[ref] = OpenSessionProjection(ref: ref, title: "Session", agentName: "root", connection: .live, transcript: [TranscriptItem(id: "pending:stable-turn", kind: .message, role: "user", text: "accepted text", origin: "user", turnID: "stable-turn", userInputState: .queued)], epoch: "refresh-epoch")

        try await model.refreshSession(ref)
        let rows = try #require(model.openSessions[ref]?.transcript)
        #expect(rows.count == 1)
        #expect(rows[0].id == "pending:stable-turn")
        #expect(rows[0].turnID == "stable-turn")
        #expect(rows[0].text == "server-final text")
        #expect(rows[0].userInputState == nil)
    }

    @Test func generatedSessionTitleUpdatesOnlyItsExactServerScopedCatalogSession() throws {
        let firstServerID = UUID(), secondServerID = UUID()
        let firstRef = SessionRef(serverID: firstServerID, sessionID: "same-session-id")
        let secondRef = SessionRef(serverID: secondServerID, sessionID: "same-session-id")
        let model = makeModel()
        model.sessions = [
            SessionProjection(ref: firstRef, title: "First default", agentName: "root"),
            SessionProjection(ref: secondRef, title: "Second default", agentName: "root")
        ]
        model.openSessions[firstRef] = OpenSessionProjection(ref: firstRef, title: "First default", agentName: "root", connection: .live, epoch: "epoch-a")
        model.openSessions[secondRef] = OpenSessionProjection(ref: secondRef, title: "Second default", agentName: "root", connection: .live, epoch: "epoch-a")
        var firstCursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: firstRef.sessionID, epoch: "epoch-a", sequence: 0, isReady: true)
        var firstProjector: SessionEventProjector? = SessionEventProjector(projection: model.openSessions[firstRef]!)
        var secondCursor: SessionEventCursor? = SessionEventCursor(expectedSessionID: secondRef.sessionID, epoch: "epoch-a", sequence: 0, isReady: true)
        var secondProjector: SessionEventProjector? = SessionEventProjector(projection: model.openSessions[secondRef]!)

        let generated = StreamEnvelope(sessionID: firstRef.sessionID, epoch: "epoch-a", sequence: 1, transcriptPosition: -1,
                                       event: ["type": .string("session_title"), "session_id": .string(firstRef.sessionID), "title": .string("Generated first title")])
        try model.consumeStreamEvent("event", envelope: generated, ref: firstRef, cursor: &firstCursor, projector: &firstProjector)
        #expect(model.openSessions[firstRef]?.title == "Generated first title")
        #expect(model.sessions.first(where: { $0.ref == firstRef })?.title == "Generated first title")
        #expect(model.sessions.first(where: { $0.ref == secondRef })?.title == "Second default")
        #expect(model.openSessions[secondRef]?.title == "Second default")

        let wrongSession = StreamEnvelope(sessionID: secondRef.sessionID, epoch: "epoch-a", sequence: 1, transcriptPosition: -1,
                                          event: ["type": .string("session_title"), "session_id": .string("different-session"), "title": .string("Must not be applied")])
        try model.consumeStreamEvent("event", envelope: wrongSession, ref: secondRef, cursor: &secondCursor, projector: &secondProjector)
        #expect(model.openSessions[secondRef]?.title == "Must not be applied", "the event projector remains scoped by its validated envelope")
        #expect(model.sessions.first(where: { $0.ref == secondRef })?.title == "Second default", "a mismatched embedded session ID cannot rename a catalog row")
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

private final class LockedCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    func increment() { lock.lock(); defer { lock.unlock() }; count += 1 }
}

private final class PublicationGeneration: @unchecked Sendable {
    private let lock = NSLock()
    private var current = true
    var isCurrent: Bool { lock.lock(); defer { lock.unlock() }; return current }
    func invalidate() { lock.lock(); defer { lock.unlock() }; current = false }
}
