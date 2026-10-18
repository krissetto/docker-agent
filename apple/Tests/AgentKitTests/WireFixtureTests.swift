import Foundation
import Testing
@testable import AgentKit

struct WireFixtureTests {
    @Test func serverCatalogConfirmedOpenAndSnapshotDecodeFromGoFixtures() throws {
        let identity = try decode(ServerIdentity.self, "server_identity.json")
        #expect(identity.sessionAPIVersion == 2)
        #expect(identity.instanceID == "fixture-instance-001")
        let catalog = try decode(CatalogResponse.self, "catalog.json")
        #expect(catalog.sessions.count == 1)
        #expect(catalog.sessions[0].sessionID == "session-demo-001")
        #expect(catalog.sessions[0].source == "demo-team")
        let prepared = try decode(PreparedViewResponse.self, "prepare_info.json")
        #expect(prepared.view == "prepare-info")
        #expect(prepared.info.sessionID == "session-demo-001")
        #expect(prepared.confirmation?.sessionID == "session-demo-001")
        let opened = try decode(OpenedViewResponse.self, "open_view.json")
        #expect(opened.view == "open-view")
        #expect(opened.version == 2)
        let wire = try decode(SessionSnapshotWire.self, "snapshot.json")
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let projection = wire.project(ref: ref, connection: .live)
        #expect(projection.transcript.contains { $0.text == "Review the sample changes." })
        #expect(projection.transcript.contains { $0.text == "I found one issue." })
        #expect(projection.transcript.contains { $0.text == "Check the boundary condition." })
    }

    @Test func goSerializedSSEAppliesAssistantToolApprovalUsageTreeAndSettles() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let source = try fixture("event_sequence.sse")
        var parser = SSEParser()
        var assembler = SnapshotAssembler()
        var cursor = SessionEventCursor(expectedSessionID: ref.sessionID)
        var projector: SessionEventProjector?
        for byte in source {
            for frame in try parser.append(Data([byte])) {
                guard let data = frame.data.data(using: .utf8) else { continue }
                let message = try JSONDecoder().decode(StreamMessage.self, from: data)
                if let snapshot = try assembler.accept(message) {
                    if case .snapshot = cursor.accept(snapshot: snapshot) { projector = SessionEventProjector(projection: snapshot.project(ref: ref, connection: .live)) }
                    continue
                }
                switch cursor.accept(type: message.type, envelope: message.envelope) {
                case .ready: break
                case .seed(let envelope), .apply(let envelope): projector?.apply(envelope)
                case .snapshot(let snapshot): projector = SessionEventProjector(projection: snapshot.project(ref: ref, connection: .live))
                case .resnapshotRequired: Issue.record("Unexpected resnapshot")
                case .duplicate, .ignored: break
                }
            }
        }
        try assembler.finish()
        #expect(try parser.finish().isEmpty)
        let result = try #require(projector?.projection)
        #expect(result.title == "Fixture conversation")
        #expect(result.transcript.contains { $0.kind == .message && $0.text.contains("I will inspect README.md.") })
        #expect(result.transcript.contains { $0.kind == .message && $0.text.contains("README.md documents the demo.") })
        #expect(result.transcript.contains { $0.kind == .reasoning && $0.text.contains("I should inspect") })
        #expect(result.transcript.contains { $0.kind == .tool && $0.toolArguments?.contains("README.md") == true && $0.toolStatus == "complete" })
        #expect(result.interactions.isEmpty, "the real interaction_resolved event must clear the approval")
        #expect(result.inputTokens == 120)
        #expect(result.outputTokens == 48)
        #expect(result.contextLimit == 8192)
        #expect(result.agentTree.first?.needsAttention == true)
        #expect(result.state == .settled)
        #expect(result.transcript.count < 30)
    }

    @Test func chunkedSnapshotUsesGoBase64ByteChunksAndIdentity() throws {
        let frames = try decode([StreamMessage].self, "snapshot_chunk_frames.json")
        var assembler = SnapshotAssembler()
        var completed: SessionSnapshotWire?
        for frame in frames { if let snapshot = try assembler.accept(frame) { completed = snapshot } }
        try assembler.finish()
        let snapshot = try #require(completed)
        #expect(snapshot.session?.id == "session-demo-large")
        #expect(snapshot.cursor == 4)
        #expect(snapshot.session?.title?.hasPrefix("fixture-large-") == true)
    }

    private func decode<T: Decodable>(_ type: T.Type, _ name: String) throws -> T { try JSONDecoder().decode(type, from: fixture(name)) }
    private func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else { Issue.record("Missing fixture \\(name)"); throw APIError("Missing wire fixture") }
        return try Data(contentsOf: url)
    }
}

extension WireFixtureTests {
    @Test func agentCatalogKeepsSourceIdentitySeparateFromRootAgent() throws {
        let url = try #require(Bundle.module.url(forResource: "agents.json", withExtension: nil, subdirectory: "Fixtures"))
        let options = try JSONDecoder().decode([AgentOption].self, from: Data(contentsOf: url))
        let source = try #require(options.first)
        #expect(source.name == "workspace-source.yaml")
        #expect(source.rootAgentName == "planner")
        #expect(source.id == "workspace-source.yaml")
        let legacy = try JSONDecoder().decode(AgentOption.self, from: Data(#"{"name":"old-source","description":"legacy","multi":false,"commands":[]}"#.utf8))
        #expect(legacy.rootAgentName == nil)
    }
}
