import Foundation
import Testing
@testable import AgentKit

struct SessionWorkflowsTests {
    @Test func goMCPPromptFixturePreservesOpaqueDiscoveryKeyAndReturnsTextOnly() async throws {
        let state = WorkflowStubState()
        let metadata = try sessionMetadata(capability: "mcp_prompts")
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/mcp/prompts") { _ in (200, try Self.fixture("mcp_prompts.json")) }
        state.respond(method: "POST", path: "/api/v2/sessions/session-demo-001/mcp/prompts/execute") { request in
            let body = try Self.jsonObject(Self.body(request))
            #expect(body?["name"] as? String == "0/review_summary")
            #expect((body?["args"] as? [String: String]) == ["diff": "patch", "focus": "correctness"])
            return (200, try Self.fixture("mcp_prompt_execute_response.json"))
        }
        let client = try makeClient(state)
        let prompts = try await client.sessionPrompts("session-demo-001", metadata: metadata)
        #expect(prompts.map(\.key) == ["0/review_summary"])
        #expect(prompts[0].arguments.map(\.name) == ["diff", "focus"])
        let text = try await client.executeSessionPrompt("session-demo-001", key: "0/review_summary", args: ["diff": "patch", "focus": "correctness"], metadata: metadata, discoveredPrompts: prompts)
        #expect(text == "Review notes: inspect the changed condition.")
        await #expect(throws: SessionWorkflowError.invalidPromptKey) {
            try await client.executeSessionPrompt("session-demo-001", key: "review_summary", metadata: metadata, discoveredPrompts: prompts)
        }
    }

    @Test func goForkSkillFixtureProjectsOnlySafeMetadataAndStartsCorrelatedAsyncRun() async throws {
        let state = WorkflowStubState()
        let metadata = try sessionMetadata(capability: "fork_skills")
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/skills") { _ in (200, try Self.fixture("fork_skills.json")) }
        state.respond(method: "POST", path: "/api/v2/sessions/session-demo-001/skills/run") { request in
            let body = try Self.jsonObject(Self.body(request))
            #expect(body?.asNSDictionary.isEqual(to: ["name": "review-plan", "task": "Draft a short checklist for the sample diff.", "operation_id": "skill-op-demo-001"]) == true)
            return (202, try Self.fixture("fork_skill_run_response.json"))
        }
        let client = try makeClient(state)
        let skills = try await client.forkSkills("session-demo-001", metadata: metadata)
        #expect(skills.map(\.name) == ["review-plan"])
        #expect(skills.first?.context == "fork")
        #expect(skills.first?.model == "demo/model")
        #expect(skills.first?.description == "Produce a review plan")
        let runRequest = SessionForkSkillRunRequest(name: "review-plan", task: "Draft a short checklist for the sample diff.", operationID: "skill-op-demo-001")
        let operation = try await client.startForkSkill("session-demo-001", runRequest: runRequest, metadata: metadata, availableSkills: skills)
        #expect(operation.operationID == "skill-op-demo-001")
        let skillPosts = state.requests.filter { request in request.httpMethod == "POST" && request.url?.path.hasSuffix("/skills/run") == true }
        #expect(skillPosts.count == 1)

    }

    @Test func contextFixtureDecodesEstimatedTokenBucketsAndFileDetails() async throws {
        let state = WorkflowStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/context") { _ in (200, try Self.fixture("context_breakdown.json")) }
        let result = try await makeClient(state).sessionContext("session-demo-001", metadata: sessionMetadata(capability: "context_inspection"))
        #expect(result.estimatedTotalTokens == 1110)
        #expect(result.contextLimit == 8192)
        #expect(result.promptFileItems?.first?.path == "/workspace/demo/AGENTS.md")
        #expect(result.attachedFiles?.first?.tokens == 17)
    }

    @Test func recoveryExportOmitsSensitiveRawErrorsAndPermissions() async throws {
        _ = try JSONDecoder().decode(SessionRecoveryExport.self, from: Self.fixture("recovery_response.json"))
        let recoveryFixture = try Self.fixture("recovery_response.json")
        _ = try JSONDecoder().decode(SessionRecoveryExport.self, from: recoveryFixture)
        let state = WorkflowStubState()
        state.respond(method: "GET", path: "/api/v2/server") { _ in (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"fixture-instance-001","ready":true,"capabilities":[]}"#.utf8)) }
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/recovery") { _ in (200, try Self.fixture("recovery_response.json")) }
        let recovery = try await makeClient(state).recoveryExport("session-demo-001", expectedInstanceID: "fixture-instance-001")
        #expect(recovery.id == "session-demo-001")
        #expect(recovery.messages?.count == 2)
        #expect(recovery.workingDir == "/workspace/demo")
        let safe = String(decoding: try JSONEncoder().encode(recovery), as: UTF8.self)
        #expect(!safe.contains("Synthetic model error"))
        #expect(!safe.contains("permissions"))
    }

    @Test func batchExportReturnsActualLosslessJsonMapAndDeleteConfirmsExactIDs() async throws {
        _ = try JSONDecoder().decode(JSONValue.self, from: Self.fixture("batch_export_response.json"))
        let exportFixture = try Self.fixture("batch_export_response.json")
        _ = try JSONDecoder().decode(JSONValue.self, from: exportFixture)
        let state = WorkflowStubState()
        state.respond(method: "GET", path: "/api/v2/server") { _ in (200, Data(#"{"version":1,"session_api_version":2,"instance_id":"fixture-instance-001","ready":true,"capabilities":[]}"#.utf8)) }
        state.respond(method: "POST", path: "/api/v2/sessions/batch/export") { request in
            let body = try Self.jsonObject(Self.body(request))
            #expect((body?["session_ids"] as? [String]) == ["session-demo-001", "session-missing-002"])
            #expect(body?["format"] as? String == "json")
            return (200, try Self.fixture("batch_export_response.json"))
        }
        state.respond(method: "POST", path: "/api/v2/sessions/batch/delete") { request in
            let body = try Self.jsonObject(Self.body(request))
            #expect((body?["session_ids"] as? [String]) == ["session-demo-001", "session-missing-002", "session-demo-child"])
            return (200, try Self.fixture("batch_delete_response.json"))
        }
        let client = try makeClient(state)
        let exported = try await client.exportSessions(["session-demo-001", "session-missing-002"], expectedInstanceID: "fixture-instance-001")
        #expect(exported.exportFormat == "json")
        #expect(exported.sessionCount == 1)
        #expect(exported.exportedSessionIDs == ["session-demo-001"])
        let raw = try Self.jsonObject(exported.encodedJSON())
        let sessions = raw?["sessions"] as? [[String: Any]]
        let messages = sessions?.first?["messages"] as? [[String: Any]]
        #expect(messages?.count == 2)
        #expect(messages?.first?["message"] != nil)

        await #expect(throws: SessionWorkflowError.destructiveConfirmationRequired) {
            try await client.deleteSessions(["session-demo-001", "session-missing-002", "session-demo-child"], expectedInstanceID: "fixture-instance-001", confirmedSessionIDs: ["session-demo-001"])
        }
        let noDeleteBeforeConfirmation = !state.requests.contains { request in request.httpMethod == "POST" && request.url?.path.hasSuffix("/batch/delete") == true }
        #expect(noDeleteBeforeConfirmation)
        let deleted = try await client.deleteSessions(["session-demo-001", "session-missing-002", "session-demo-child"], expectedInstanceID: "fixture-instance-001", confirmedSessionIDs: ["session-demo-001", "session-missing-002", "session-demo-child"])
        let deletedIDs = try deleted.confirmedDeletedIDs(from: ["session-demo-001", "session-missing-002", "session-demo-child"])
        #expect(deletedIDs == ["session-demo-001", "session-demo-child"])
    }

    @Test func workspaceRestoreRequiresSettledProofAndConfirmation() async throws {
        let state = WorkflowStubState()
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/status") { _ in
            (200, Data(#"{"metadata":{"session_id":"session-demo-001","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true,"snapshots":true}},"status":{"session_id":"session-demo-001","agent_name":"root","state":"settled","pending":0}}"#.utf8))
        }
        state.respond(method: "GET", path: "/api/v2/sessions/session-demo-001/workspace-snapshots") { _ in (200, try Self.fixture("workspace_snapshots.json")) }
        state.respond(method: "POST", path: "/api/v2/sessions/session-demo-001/workspace-snapshots/reset") { request in
            let body = try Self.jsonObject(Self.body(request))
            #expect(body?["expected_proof"] as? String == "94a5117535ddbf1641fbb64a4d4b9b62daacaa7170c84ac29eaa93b83d0edbcc")
            #expect(body?["keep"] as? Int == 1)
            return (200, try Self.fixture("workspace_snapshot_result.json"))
        }
        let client = try makeClient(state)
        let metadata = try sessionMetadata(capability: "snapshots")
        let snapshots = try await client.workspaceSnapshots("session-demo-001", metadata: metadata)
        #expect(snapshots.enabled)
        #expect(snapshots.files == [2, 3])
        let running = makeStatus("running")
        #expect(throws: SessionWorkflowError.sessionNotIdle) {
            try client.validateWorkspaceSnapshotRestore(sessionID: "session-demo-001", snapshots: snapshots, proof: snapshots.proof, keep: 1, confirmed: true, metadata: metadata, currentStatus: running)
        }
        #expect(throws: SessionWorkflowError.destructiveConfirmationRequired) {
            try client.validateWorkspaceSnapshotRestore(sessionID: "session-demo-001", snapshots: snapshots, proof: snapshots.proof, keep: 1, confirmed: false, metadata: metadata, currentStatus: makeStatus("settled"))
        }
        let result = try await client.resetWorkspaceSnapshot("session-demo-001", proof: snapshots.proof, keep: 1, confirmed: true)
        #expect(result.restored)
        #expect(result.restoredFiles == 3)
        let restorePosts = state.requests.filter { request in request.httpMethod == "POST" && request.url?.path.hasSuffix("/workspace-snapshots/reset") == true }
        #expect(restorePosts.count == 1)
    }

    @Test func batchDeleteWithoutExplicitFailedIDsDoesNotInventSuccesses() throws {
        let result = try JSONDecoder().decode(SessionBatchDeleteResult.self, from: Data(#"{"deleted_count":2,"failed_count":1}"#.utf8))
        #expect(throws: SessionWorkflowError.identityMismatch) { try result.confirmedDeletedIDs(from: ["a", "b", "c"]) }
    }

    private func sessionMetadata(capability: String) throws -> SessionControlMetadata {
        let object: [String: JSONValue] = ["session_id": .string("session-demo-001"), "agent_name": .string("root"), "execution_available": .bool(true), "capabilities": .object(["execution_available": .bool(true), capability: .bool(true)])]
        return try JSONDecoder().decode(SessionControlMetadata.self, from: JSONEncoder().encode(object))
    }

    private func makeStatus(_ state: String) -> StatusWire {
        StatusWire(interruptedTurns: nil, sessionID: "session-demo-001", agentName: "root", state: state, pending: 0, turnID: nil, lastError: nil, dormant: nil, pauseArmed: nil, paused: nil, pauseGeneration: nil)
    }

    private func makeClient(_ state: WorkflowStubState) throws -> APIClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WorkflowURLProtocol.self]
        WorkflowURLProtocol.install(state, for: state.host)
        return try APIClient(config: InstanceConfig(id: UUID(), name: "Workflow", baseURL: "https://\(state.host)"), token: "synthetic-workflow-token", session: URLSession(configuration: configuration))
    }

    private static func body(_ request: URLRequest) -> Data {
        if let data = request.httpBody { return data }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var result = Data()
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let count = stream.read(buffer, maxLength: 4096)
            if count <= 0 { break }
            result.append(buffer, count: count)
        }
        return result
    }

    private static func jsonObject(_ data: Data) throws -> [String: Any]? {
        try JSONSerialization.jsonObject(with: data) as? [String: Any]
    }
}

private extension Dictionary where Key == String, Value == Any {
    var asNSDictionary: NSDictionary { self as NSDictionary }
}

private extension SessionWorkflowsTests {
    nonisolated static func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else { throw APIError("Missing fixture \(name)") }
        return try Data(contentsOf: url)
    }
}

private final class WorkflowStubState: @unchecked Sendable {
    let host = "workflow-\(UUID().uuidString.lowercased()).example"
    private let lock = NSLock()
    private var handlers: [String: @Sendable (URLRequest) throws -> (Int, Data)] = [:]
    private var requestsStorage: [URLRequest] = []
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return requestsStorage }
    func respond(method: String, path: String, handler: @escaping @Sendable (URLRequest) throws -> (Int, Data)) {
        lock.lock(); defer { lock.unlock() }; handlers["\(method)|\(path)"] = handler
    }
    func handle(_ request: URLRequest) throws -> (Int, Data) {
        lock.lock(); requestsStorage.append(request); let handler = handlers["\(request.httpMethod ?? "GET")|\(request.url?.path ?? "")"]; lock.unlock()
        guard let handler else { return (404, Data(#"{"message":"not stubbed"}"#.utf8)) }
        return try handler(request)
    }
}

private final class WorkflowURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: WorkflowStubState] = [:]
    static func install(_ state: WorkflowStubState, for host: String) { lock.lock(); defer { lock.unlock() }; states[host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock()
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        do {
            let (status, responseData) = try state.handle(request)
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            if !responseData.isEmpty { client?.urlProtocol(self, didLoad: responseData) }
            client?.urlProtocolDidFinishLoading(self)
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}
