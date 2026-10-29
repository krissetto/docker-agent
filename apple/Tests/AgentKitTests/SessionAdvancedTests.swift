import Foundation
import Testing
@testable import AgentKit

struct SessionAdvancedTests {
    @Test func boundedHistoryHTTPRequestsUseExactProjectionBoundsAndOpaqueCursor() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/history%2Fsession/snapshot") { _ in
            (200, Data(#"{"projection":"tail","session":{"id":"history/session"},"status":{"session_id":"history/session"},"epoch":"e","cursor":5,"transcript_position":50,"items":[{"position":49,"item":{"summary":"latest"}}],"history":{"start_position":49,"end_position":50,"total_items":50,"next_cursor":"opaque","truncated":true}}"#.utf8))
        }
        state.respond(method: "GET", path: "/api/v2/sessions/history%2Fsession/history") { _ in
            let items = (0..<49).map { position in "{\"position\":\(position),\"item\":{\"summary\":\"h\"}}" }.joined(separator: ",")
            let body = "{\"projection\":\"tail\",\"session_id\":\"history/session\",\"epoch\":\"e\",\"cursor\":5,\"transcript_position\":50,\"items\":[\(items)],\"history\":{\"start_position\":0,\"end_position\":50,\"total_items\":50,\"truncated\":false}}"
            return (200, Data(body.utf8))
        }
        let client = try client(state)
        let tail = try await client.sessionHistoryTail("history/session")
        #expect(throws: APIError.self) { try tail.validateTail(expectedSessionID: "wrong") }
        try tail.validateTail(expectedSessionID: "history/session")
        let page = try await client.sessionHistoryPage("history/session", cursor: "opaque")
        try page.validate(sessionID: "history/session", epoch: "e", cursor: 5, anchorEndPosition: 50, totalItems: 50, previousStartPosition: 49)
        #expect(state.requests.count == 2)
        let tailQuery = URLComponents(url: state.requests[0].url!, resolvingAgainstBaseURL: false)!.queryItems!
        #expect(tailQuery.map(\.name) == ["projection", "max_items", "max_bytes", "max_item_bytes"])
        #expect(tailQuery.map(\.value) == ["tail", "50", "2097152", "131072"])
        let pageQuery = URLComponents(url: state.requests[1].url!, resolvingAgainstBaseURL: false)!.queryItems!
        #expect(pageQuery.first(where: { $0.name == "cursor" })?.value == "opaque")
        #expect(pageQuery.first(where: { $0.name == "projection" })?.value == "tail")
        await #expect(throws: APIError.self) { try await client.sessionHistoryPage("history/session", cursor: String(repeating: "x", count: 8193)) }
    }

    @Test func boundedHistoryHTTPRejectsResponseOverNegotiatedBodyLimitBeforeDecode() async throws {
        let state = AdvancedStubState()
        state.contentLengthOverride = 2 * 1024 * 1024 + 1
        state.respond(method: "GET", path: "/api/v2/sessions/session-1/snapshot") { _ in (200, Data("{}".utf8)) }
        let client = try client(state)
        await #expect(throws: APIError.self) { try await client.sessionHistoryTail("session-1") }
    }

    @Test func tailCapabilityRequiresValidatedSessionAPIVersion() {
        let supported = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "i", ready: true, capabilities: ["session_history_pages"])
        let wrongVersion = ServerIdentity(version: 1, sessionAPIVersion: 1, instanceID: "i", ready: true, capabilities: ["session_history_pages"])
        #expect(supported.supportsSessionHistoryPages)
        #expect(!wrongVersion.supportsSessionHistoryPages)
    }

    @Test func toolsApprovalToggleIsFreshlyGatedAndUsesBodylessToggleRoute() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-1/status") { _ in (200, Data(#"{"metadata":{"session_id":"session-1","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true}},"status":{"session_id":"session-1","agent_name":"root","state":"settled","pending":0}}"#.utf8)) }
        state.respond(method: "POST", suffix: "/tools/toggle") { request in
            #expect(Self.body(request).isEmpty)
            return (200, Data("null".utf8))
        }
        let client = try client(state)
        try await client.toggleSessionToolsApproval("session-1")
        #expect(state.requests.map(\.httpMethod) == ["GET", "POST"])
        #expect(state.requests.last?.url?.path == "/api/v2/sessions/session-1/tools/toggle")
        #expect(Self.body(state.requests.last!).isEmpty)
    }

    @Test func pauseToggleIsBodylessAndReturnsArmNotBoundaryAcknowledgement() async throws {
        let state = AdvancedStubState()
        state.respond(method: "POST", suffix: "/pause") { request in
            #expect(Self.body(request).isEmpty)
            return (200, Data(#"{"paused":true}"#.utf8))
        }
        let client = try client(state)
        let metadata = try metadata(capability: "pause")
        let result = try await client.toggleSessionPause("session-1", metadata: metadata)
        #expect(result.paused)
        #expect(state.requests.first?.httpMethod == "POST")
        #expect(state.requests.first?.url?.path == "/api/v2/sessions/session-1/pause")
    }

    @Test func subtreeStopRequiresConfirmationOfExactTargetAndHasNoInventedBody() async throws {
        let state = AdvancedStubState()
        state.respond(method: "POST", suffix: "/stop-subtree") { request in
            #expect(Self.body(request).isEmpty)
            return (204, Data())
        }
        let client = try client(state)
        let metadata = try metadata(capability: "stop_subtree")
        await #expect(throws: SessionAdvancedError.subtreeConfirmationRequired) {
            try await client.stopSessionSubtree("session-1", confirmedSessionID: "different-session", metadata: metadata)
        }
        #expect(state.requests.isEmpty)
        let stopped = try await client.stopSessionSubtree("session-1", confirmedSessionID: "session-1", metadata: metadata)
        #expect(stopped.sessionID == "session-1")
        #expect(state.requests.count == 1)
    }

    @Test func waitsUseExactTurnAndWorkflowRoutesAndAreObservationOnly() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-1/status") { _ in (200, Data(#"{"metadata":{"session_id":"session-1","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true}},"status":{"session_id":"session-1","agent_name":"root","state":"settled","pending":0}}"#.utf8)) }
        state.respond(method: "POST", suffix: "/turns/turn%2F9/wait") { request in
            #expect(Self.body(request).isEmpty)
            return (204, Data())
        }
        state.respond(method: "POST", suffix: "/workflow/wait") { request in
            #expect(Self.body(request).isEmpty)
            return (200, Data(#"{"epoch":"epoch-a","cursor":21}"#.utf8))
        }
        let client = try client(state)
        try await client.waitForTurn("session-1", turnID: "turn/9")
        let settlement = try await client.waitForWorkflow("session-1")
        #expect(settlement == SessionWorkflowSettlement(epoch: "epoch-a", cursor: 21))
        #expect(state.requests.map(\.httpMethod) == ["GET", "POST", "GET", "POST"])
        #expect(state.requests.contains(where: { $0.url?.path.contains("/turns/turn") == true && $0.url?.path.hasSuffix("/wait") == true }))
        #expect(state.requests.filter { $0.httpMethod == "POST" }.allSatisfy { Self.body($0).isEmpty && URLComponents(url: $0.url!, resolvingAgainstBaseURL: false)?.query == nil })
        #expect(!state.requests.contains(where: { $0.url?.path.hasSuffix("/cancel") == true }))
    }

    @Test func forkIsBoundedUserMessageOrdinalAndResponseIsNewChild() async throws {
        let state = AdvancedStubState()
        state.respond(method: "POST", suffix: "/fork") { request in
            let body = (try? Self.object(Self.body(request))) ?? nil
            #expect(body?.asNSDictionary.isEqual(to: ["user_message_index": 2]) == true)
            return (200, Data(#"{"id":"child-1","title":"Forked","source":"source-a","agent_name":"planner"}"#.utf8))
        }
        let client = try client(state)
        let metadata = try metadata(capability: "branching")
        await #expect(throws: SessionAdvancedError.invalidForkPosition) {
            try await client.forkBeforeUserMessage("session-1", index: -1, availableUserMessages: 3, metadata: metadata)
        }
        await #expect(throws: SessionAdvancedError.forkPositionUnavailable) {
            try await client.forkBeforeUserMessage("session-1", index: 3, availableUserMessages: 3, metadata: metadata)
        }
        #expect(state.requests.isEmpty)
        let branch = try await client.forkBeforeUserMessage("session-1", index: 2, availableUserMessages: 3, metadata: metadata)
        #expect(branch.id == "child-1")
        #expect(branch.parentID == nil)
        #expect(state.requests.last?.httpMethod == "POST")
    }

    @Test func messageEditSendsFullPreservedMessageAndRequiresNumericDurableID() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-1/status") { _ in (200, Data(#"{"metadata":{"session_id":"session-1","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true}},"status":{"session_id":"session-1","agent_name":"root","state":"settled","pending":0}}"#.utf8)) }
        state.respond(method: "PATCH", suffix: "/messages/42") { request in
            let body = (try? Self.object(Self.body(request))) ?? nil
            let message = body?["message"] as? [String: Any] ?? [:]
            #expect(message["agent_name"] as? String == "")
            #expect(message["actor_turn_id"] as? String == "turn-a")
            #expect(message["input_origin"] as? String == "user")
            #expect(message["unmodeled_extension"] as? String == "preserve-me")
            let payload = message["message"] as? [String: Any] ?? [:]
            #expect(payload["role"] as? String == "user")
            #expect(payload["content"] as? String == "edited text")
            #expect(payload["created_at"] as? String == "2025-01-02T03:04:05Z")
            return (200, Data(#"{"status":"updated"}"#.utf8))
        }
        let client = try client(state)
        let metadata = try metadata(capability: "session_editing")
        let fullMessage: JSONValue = .object([
            "agent_name": .string(""),
            "message": .object(["role": .string("user"), "content": .string("edited text"), "created_at": .string("2025-01-02T03:04:05Z")]),
            "actor_accepted": .bool(true), "actor_turn_id": .string("turn-a"),
            "actor_input_mode": .string("submit"), "input_origin": .string("user"),
            "unmodeled_extension": .string("preserve-me")
        ])
        await #expect(throws: SessionAdvancedError.invalidMessageID) {
            try await client.updateSessionMessage("session-1", durableMessageID: 0, message: fullMessage, metadata: metadata)
        }
        #expect(state.requests.isEmpty)
        let assistantMessage: JSONValue = .object(["agent_name": .string("root"), "message": .object(["role": .string("assistant"), "content": .string("not editable")])])
        await #expect(throws: SessionAdvancedError.invalidMessage) {
            try await client.updateSessionMessage("session-1", durableMessageID: 42, message: assistantMessage, metadata: metadata)
        }
        #expect(state.requests.isEmpty)
        let result = try await client.updateSessionMessage("session-1", durableMessageID: 42, message: fullMessage, metadata: metadata)
        #expect(result.status == "updated")
        #expect(state.requests.last?.url?.path == "/api/v2/sessions/session-1/messages/42")
        #expect(state.requests.last?.httpMethod == "PATCH")
    }

    @Test func metadataProjectionRequestsAreExplicitMarkerCheckedAndSeparatelyBounded() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-meta") { request in
            let projection = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "projection" })?.value
            #expect(projection == "metadata")
            return (200, Data(#"{"version":2,"view":"prepare-info","projection":"metadata","info":{"session_id":"session-meta","root_session_id":"session-meta","active_agent_name":"root","session":{"id":"session-meta","working_dir":"/workspace","attributes":{"docker-agent.actor.source":"team.yaml"}},"binding":{"AgentName":"root","Model":"m","Durability":"durable","ParentSessionID":""},"working_dir":"/workspace"}}"#.utf8))
        }
        let client = try client(state)
        let prepared = try await client.prepareSessionView("session-meta", metadataOnly: true)
        #expect(prepared.projection == "metadata")
        #expect(prepared.confirmation?.source == "team.yaml")
        #expect(state.requests.first?.url?.query == "view=prepare-info&projection=metadata")
        #expect(APIClient.maximumMetadataSessionViewResponseBytes == 4 * 1024 * 1024)

        state.respond(method: "GET", path: "/api/v2/sessions/session-meta") { _ in
            (200, Data(#"{"version":2,"view":"prepare-info","info":{"session_id":"session-meta","root_session_id":"session-meta","active_agent_name":"root","session":{"id":"session-meta","working_dir":"/workspace","attributes":{}},"binding":{"AgentName":"root","Model":"m","Durability":"durable","ParentSessionID":""},"working_dir":"/workspace"}}"#.utf8))
        }
        let missingMarker = try #require(await captureAPIError { try await client.prepareSessionView("session-meta", metadataOnly: true) })
        #expect(missingMarker.message.contains("did not confirm the requested metadata-only session view prepare projection"))
        let legacy = try await client.prepareSessionView("session-meta")
        #expect(legacy.projection == nil)
        #expect(state.requests.last?.url?.query == "view=prepare-info")

        state.respond(method: "GET", path: "/api/v2/sessions/session-meta") { _ in
            (200, Data(#"{"version":2,"view":"prepare-info","projection":"full","info":{"session_id":"session-meta","root_session_id":"session-meta","active_agent_name":"root","session":{"id":"session-meta","working_dir":"/workspace","attributes":{}},"binding":{"AgentName":"root","Model":"m","Durability":"durable","ParentSessionID":""},"working_dir":"/workspace"}}"#.utf8))
        }
        let wrongMarker = try #require(await captureAPIError { try await client.prepareSessionView("session-meta", metadataOnly: true) })
        #expect(wrongMarker.message.contains("did not confirm the requested metadata-only session view prepare projection"))

        state.respond(method: "PATCH", path: "/api/v2/sessions/session-meta") { request in
            #expect(URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "projection" })?.value == "metadata")
            return (200, Data(#"{"version":2,"view":"open-view","projection":"metadata","info":{"session_id":"session-meta","root_session_id":"session-meta","active_agent_name":"root","session":{"id":"session-meta","working_dir":"/workspace","attributes":{"docker-agent.actor.source":"team.yaml"}},"binding":{"AgentName":"root","Model":"m","Durability":"durable","ParentSessionID":""},"working_dir":"/workspace"},"metadata":{"capabilities":{}},"status":{"session_id":"session-meta","agent_name":"root","state":"settled"}}"#.utf8))
        }
        let confirmation = try #require(prepared.confirmation)
        let opened = try await client.openSessionView("session-meta", confirmation: confirmation, metadataOnly: true)
        #expect(opened.projection == "metadata")
        #expect(state.requests.last?.url?.query == "view=open-view&projection=metadata")
        #expect(Self.body(state.requests.last!).contains(Data("view_confirmation".utf8)))
        state.respond(method: "PATCH", path: "/api/v2/sessions/session-meta") { _ in
            (200, Data(#"{"version":2,"view":"open-view","info":{"session_id":"session-meta"},"metadata":{"capabilities":{}},"status":{"session_id":"session-meta","agent_name":"root","state":"settled"}}"#.utf8))
        }
        let openMissingMarker = try #require(await captureAPIError { try await client.openSessionView("session-meta", confirmation: confirmation, metadataOnly: true) })
        #expect(openMissingMarker.message.contains("did not confirm the requested metadata-only session view open projection"))

        state.contentLengthOverride = Int64(APIClient.maximumMetadataSessionViewResponseBytes + 1)
        state.respond(method: "GET", path: "/api/v2/sessions/session-meta") { _ in (200, Data("{}".utf8)) }
        let metadataTooLarge = try #require(await captureAPIError { try await client.prepareSessionView("session-meta", metadataOnly: true) })
        #expect(metadataTooLarge.message == "Session metadata projection exceeded the 4 MiB client limit.")
    }

    @Test func largeSessionViewResponsesHaveAnEndpointSpecificBoundedBudget() async throws {
        let state = AdvancedStubState()
        let payload = Self.largePreparedViewJSON(messageBytes: 32_989_000)
        state.respond(method: "GET", path: "/api/v2/sessions/session-large") { _ in (200, payload) }
        state.respond(method: "PATCH", path: "/api/v2/sessions/session-large") { _ in
            (200, Self.largeOpenedViewJSON(messageBytes: 32_989_000))
        }
        let client = try client(state)

        let prepared = try await client.prepareSessionView("session-large")
        #expect(prepared.view == "prepare-info")
        #expect(prepared.confirmation?.sessionID == "session-large")
        #expect(prepared.confirmation?.rootSessionID == "session-large")
        #expect(prepared.confirmation?.source == "team.yaml")
        #expect(prepared.confirmation?.binding.agentName == "root")
        #expect(prepared.confirmation?.workingDir == "/workspace")

        let error = try #require(await captureAPIError {
            try await client.request(PreparedViewResponse.self, path: "api/v2/sessions/session-large",
                                     query: [URLQueryItem(name: "view", value: "prepare-info")])
        })
        #expect(error.message == "Server response exceeded the 16 MiB client limit.")

        let opened = try await client.openSessionView("session-large", confirmation: prepared.confirmation!)
        #expect(opened.version == 2)
        #expect(opened.view == "open-view")
        #expect(state.requests.filter { $0.httpMethod == "GET" }.count == 2)
        #expect(state.requests.last?.httpMethod == "PATCH")
        #expect(state.requests.last?.url?.query == "view=open-view")
        #expect(Self.body(state.requests.last!).contains(Data("team.yaml".utf8)))

        // No Content-Length: the async byte-count guard must stop as soon as the finite budget is crossed.
        let overBudget = Self.largePreparedViewJSON(messageBytes: APIClient.maximumSessionViewResponseBytes + 1)
        state.respond(method: "GET", path: "/api/v2/sessions/session-large") { _ in (200, overBudget) }
        let tooLarge = try #require(await captureAPIError { try await client.prepareSessionView("session-large") })
        #expect(tooLarge.message.contains("Update the server to use metadata-only"))

        // An advertised oversized response is rejected from headers before decoding.
        state.contentLengthOverride = Int64(APIClient.maximumSessionViewResponseBytes + 1)
        state.respond(method: "GET", path: "/api/v2/sessions/session-large") { _ in (200, Data("{}".utf8)) }
        let headerTooLarge = try #require(await captureAPIError { try await client.prepareSessionView("session-large") })
        #expect(headerTooLarge.message.contains("Update the server to use metadata-only"))
    }

    private func captureAPIError<T>(_ operation: () async throws -> T) async -> APIError? {
        do { _ = try await operation(); return nil }
        catch let error as APIError { return error }
        catch { return nil }
    }

    private static func largePreparedViewJSON(messageBytes: Int) -> Data {
        let filler = String(repeating: "x", count: messageBytes)
        return Data((#"{"version":2,"view":"prepare-info","info":{"session_id":"session-large","root_session_id":"session-large","session":{"id":"session-large","working_dir":"/workspace","attributes":{"docker-agent.actor.source":"team.yaml"},"messages":[""# + filler + #""]},"binding":{"AgentName":"root","Model":"","Durability":"","ParentSessionID":""},"working_dir":"/workspace"}}"#).utf8)
    }

    private static func largeOpenedViewJSON(messageBytes: Int) -> Data {
        let filler = String(repeating: "x", count: messageBytes)
        return Data((#"{"version":2,"view":"open-view","info":{"session_id":"session-large","root_session_id":"session-large","session":{"id":"session-large","working_dir":"/workspace","attributes":{"docker-agent.actor.source":"team.yaml"},"messages":[""# + filler + #""]},"binding":{"AgentName":"root","Model":"","Durability":"","ParentSessionID":""},"working_dir":"/workspace"},"metadata":{"session_id":"session-large","agent_name":"root","capabilities":{"historical":false,"execution_available":true,"branching":true}},"status":{"session_id":"session-large","agent_name":"root","state":"settled"}}"#).utf8)
    }

    @Test func delegationPolicyUsesRootScopedEnabledBoolean() async throws {
        let state = AdvancedStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-1/status") { _ in (200, Data(#"{"metadata":{"session_id":"session-1","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"delegation_policy":true}},"status":{"session_id":"session-1","agent_name":"root","state":"settled","pending":0}}"#.utf8)) }
        state.respond(method: "GET", suffix: "/delegation-policy") { _ in (200, Data(#"{"enabled":false}"#.utf8)) }
        state.respond(method: "PATCH", suffix: "/delegation-policy") { request in
            let body = (try? Self.object(Self.body(request))) ?? nil
            #expect(body?["enabled"] as? Bool == true)
            return (200, Data(#"{"enabled":true}"#.utf8))
        }
        let client = try client(state)
        let metadata = try metadata(capability: "delegation_policy")
        #expect(!(try await client.delegationPolicy("session-1", metadata: metadata)).enabled)
        #expect(try await client.setDelegationPolicy("session-1", enabled: true, metadata: metadata).enabled)
    }

    @Test func attachmentRemovalUsesAbsoluteListedPathAndDeletesOnlyReference() async throws {
        let state = AdvancedStubState()
        state.respond(method: "DELETE", suffix: "/attachments") { request in
            let body = (try? Self.object(Self.body(request))) ?? nil
            #expect(body?["path"] as? String == "/workspace/input/report.txt")
            return (204, Data())
        }
        let client = try client(state)
        let metadata = try metadata(capability: "session_editing")
        await #expect(throws: SessionAdvancedError.invalidAttachmentPath) {
            try await client.removeSessionAttachment("session-1", path: "relative/file.txt", metadata: metadata)
        }
        await #expect(throws: SessionAdvancedError.invalidAttachmentPath) {
            try await client.removeSessionAttachment("session-1", path: "file:///workspace/report.txt", metadata: metadata)
        }
        try await client.removeSessionAttachment("session-1", path: "/workspace/input/report.txt", metadata: metadata)
        #expect(state.requests.count == 1)
        #expect(state.requests[0].httpMethod == "DELETE")
        #expect(state.requests[0].url?.path == "/api/v2/sessions/session-1/attachments")
    }

    @Test func sourceRegistrationSendsServerPathAndMapsRegistrarUnavailable() async throws {
        let state = AdvancedStubState()
        state.respond(method: "POST", path: "/api/v2/sources") { request in
            let body = (try? Self.object(Self.body(request))) ?? nil
            #expect(body?.asNSDictionary.isEqual(to: ["path": "workspace-source.yaml", "working_dir": "/workspace/demo"]) == true)
            return (200, Data(#"{"source_id":"workspace-source.yaml","revision":"sha256:rev-a"}"#.utf8))
        }
        let client = try client(state)
        let registration = try await client.registerSource(path: "workspace-source.yaml", workingDir: "/workspace/demo")
        #expect(registration.sourceID == "workspace-source.yaml")
        #expect(registration.revision == "sha256:rev-a")
        await #expect(throws: SessionAdvancedError.invalidSourcePath) { try await client.registerSource(path: "file:///tmp/local.yaml") }

        state.respond(method: "POST", path: "/api/v2/sources") { _ in (404, Data(#"{"message":"source registration unavailable"}"#.utf8)) }
        await #expect(throws: SessionAdvancedError.sourceRegistrationUnavailable) { try await client.registerSource(path: "workspace-source.yaml") }
    }

    @Test func historicalAndMissingCapabilityMetadataBlockMutationsBeforeNetwork() async throws {
        let state = AdvancedStubState()
        let client = try client(state)
        let historical = try metadata(capability: "pause", execution: false, historical: true)
        await #expect(throws: SessionControlsError.sessionNotExecutable) { try await client.toggleSessionPause("session-1", metadata: historical) }
        let unsupported = try metadata(capability: "pause", capabilityEnabled: false)
        await #expect(throws: SessionControlsError.sessionNotExecutable) { try await client.toggleSessionPause("session-1", metadata: unsupported) }
        #expect(state.requests.isEmpty)
    }

    private func metadata(capability: String, execution: Bool = true, capabilityEnabled: Bool = true, historical: Bool = false) throws -> SessionControlMetadata {
        let json: [String: Any] = ["session_id": "session-1", "agent_name": "root", "execution_available": execution,
                                  "historical": historical, "capabilities": ["execution_available": execution, capability: capabilityEnabled]]
        return try JSONDecoder().decode(SessionControlMetadata.self, from: JSONSerialization.data(withJSONObject: json))
    }

    private func client(_ state: AdvancedStubState) throws -> APIClient {
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [AdvancedURLProtocol.self]
        AdvancedURLProtocol.install(state, host: state.host)
        return try APIClient(config: InstanceConfig(id: UUID(), name: "Advanced", baseURL: "https://\(state.host)"), token: "advanced-token", session: URLSession(configuration: config))
    }

    private static func body(_ request: URLRequest) -> Data {
        if let data = request.httpBody { return data }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var data = Data(); let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 2048); defer { buffer.deallocate() }
        while stream.hasBytesAvailable { let count = stream.read(buffer, maxLength: 2048); if count <= 0 { break }; data.append(buffer, count: count) }
        return data
    }
    private static func object(_ data: Data) throws -> [String: Any]? { try JSONSerialization.jsonObject(with: data) as? [String: Any] }
}

private extension Dictionary where Key == String, Value == Any {
    var asNSDictionary: NSDictionary { self as NSDictionary }
}

private final class AdvancedStubState: @unchecked Sendable {
    let host = "advanced-\(UUID().uuidString.lowercased()).example"
    private let lock = NSLock()
    private var handlers: [String: @Sendable (URLRequest) -> (Int, Data)] = [:]
    private var requestsStorage: [URLRequest] = []
    private var contentLengthStorage: Int64?
    var contentLengthOverride: Int64? { get { lock.lock(); defer { lock.unlock() }; return contentLengthStorage } set { lock.lock(); defer { lock.unlock() }; contentLengthStorage = newValue } }
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return requestsStorage }
    func respond(method: String, suffix: String, handler: @escaping @Sendable (URLRequest) -> (Int, Data)) { lock.lock(); defer { lock.unlock() }; handlers["\(method)|*\(suffix)"] = handler }
    func respond(method: String, path: String, handler: @escaping @Sendable (URLRequest) -> (Int, Data)) { lock.lock(); defer { lock.unlock() }; handlers["\(method)|\(path)"] = handler }
    func handle(_ request: URLRequest) -> (Int, Data) {
        lock.lock(); requestsStorage.append(request); let key = "\(request.httpMethod ?? "GET")|\(request.url?.path ?? "")"; let exact = handlers[key]; let suffix = handlers.first(where: { $0.key.hasPrefix("\(request.httpMethod ?? "GET")|*") && key.hasSuffix(String($0.key.dropFirst((request.httpMethod ?? "GET").count + 2))) })?.value; lock.unlock()
        return (exact ?? suffix)?(request) ?? (404, Data(#"{"message":"not found"}"#.utf8))
    }
}

private final class AdvancedURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: AdvancedStubState] = [:]
    static func install(_ state: AdvancedStubState, host: String) { lock.lock(); defer { lock.unlock() }; states[host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock()
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        let (status, data) = state.handle(request)
        var headers = ["Content-Type": "application/json"]
        if let contentLength = state.contentLengthOverride { headers["Content-Length"] = String(contentLength) }
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}
