import Foundation
import Testing
@testable import AgentKit

@MainActor
struct ProjectionTests {
    @Test func snapshotProjectionFiltersRuntimeAndPreservesOrigins() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "s")
        let data = Data(#"{"session":{"id":"s","title":"Fixture","messages":[{"message":{"agent_name":"root","input_origin":"user","message":{"role":"user","content":"visible"}}},{"message":{"agent_name":"root","input_origin":"runtime","message":{"role":"user","content":"hidden"}}},{"message":{"agent_name":"root","message":{"role":"assistant","content":"answer","reasoning_content":"thought"}}}]},"status":{"session_id":"s","agent_name":"root","state":"running","pending":0},"epoch":"e"}"#.utf8)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
        let projected = wire.project(ref: ref, connection: .live)
        #expect(projected.title == "Fixture")
        #expect(projected.transcript.count == 3)
        #expect(projected.transcript.allSatisfy { $0.text != "hidden" })
        #expect(projected.transcript[0].origin == "user")
        #expect(projected.transcript[1].kind == .reasoning)
    }

    @Test func cursorReplayContinuationStartsAtSnapshotCursorAndRequiresBarrierOnGap() throws {
        let data = Data(#"{"session":{"id":"s"},"status":{"session_id":"s","state":"running"},"epoch":"e","cursor":8}"#.utf8)
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
        var cursor = SessionEventCursor(expectedSessionID: "s", epoch: snapshot.epoch, sequence: snapshot.cursor ?? 0, isReady: true)
        let replay = StreamEnvelope(sessionID: "s", epoch: "e", sequence: 9, transcriptPosition: 0, event: ["type": .string("agent_choice"), "content": .string("hello")])
        guard case .apply = cursor.accept(type: "event", envelope: replay) else { Issue.record("valid replay was rejected") ; return }
        let gap = StreamEnvelope(sessionID: "s", epoch: "e", sequence: 11, transcriptPosition: 2, event: ["type": .string("agent_choice"), "content": .string("world")])
        guard case .resnapshotRequired = cursor.accept(type: "event", envelope: gap) else { Issue.record("gap did not require snapshot") ; return }
    }

    @Test func goWireFixtureAssistantRemainsVisibleAfterMessageCommitAndTurnSettlement() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "session-demo-001")
        let bytes = try Data(contentsOf: Bundle.module.url(forResource: "event_sequence.sse", withExtension: nil, subdirectory: "Fixtures")!)
        var parser = SSEParser(); var assembler = SnapshotAssembler(); var cursor = SessionEventCursor(expectedSessionID: ref.sessionID); var projector: SessionEventProjector?
        for byte in bytes {
            for frame in try parser.append(Data([byte])) {
                guard let payload = frame.data.data(using: .utf8) else { continue }
                let message = try JSONDecoder().decode(StreamMessage.self, from: payload)
                if let snapshot = try assembler.accept(message) { projector = SessionEventProjector(projection: snapshot.project(ref: ref, connection: .live)); _ = cursor.accept(snapshot: snapshot); continue }
                switch cursor.accept(type: message.type, envelope: message.envelope) {
                case .ready: break
                case .apply(let envelope), .seed(let envelope): projector?.apply(envelope)
                case .snapshot(let snapshot): projector = SessionEventProjector(projection: snapshot.project(ref: ref, connection: .live))
                case .resnapshotRequired(let reason): Issue.record("unexpected resnapshot: \(reason)")
                case .duplicate, .ignored: break
                }
            }
        }
        let result = try #require(projector?.projection)
        #expect(result.transcript.contains { $0.kind == .message && $0.text.contains("README.md documents the demo.") })
        #expect(result.transcript.contains { $0.text.contains("README.md documents the demo.") && !$0.isStreaming })
    }

    @Test func goSSESubagentTreeMapsStateAttentionCostTokensAndChildSession() throws {
        let bytes = try Data(contentsOf: Bundle.module.url(forResource: "event_sequence.sse", withExtension: nil, subdirectory: "Fixtures")!)
        var parser = SSEParser()
        var found: [AgentNodeProjection] = []
        for byte in bytes {
            for frame in try parser.append(Data([byte])) {
                guard let text = frame.data.data(using: .utf8), let message = try? JSONDecoder().decode(StreamMessage.self, from: text),
                      let envelope = message.envelope,
                      envelope.event?["type"]?.stringValue == "subagent_tree" else { continue }
                found = SessionSnapshotWire.agentTree(envelope.event?["snapshot"], serverID: UUID())
            }
        }
        let node = try #require(found.first)
        #expect(node.agentName == "root")
        #expect(node.state == .running)
        #expect(node.needsAttention)
        #expect(node.waitingOn == "approval")
        #expect(node.cost == 0.0125)
        #expect(node.inputTokens == 120)
        #expect(node.outputTokens == 48)
        #expect(node.session?.sessionID == "session-demo-001")
    }

    @Test func snapshotParentSessionIsBoundToSameServerAndRejectsSelf() throws {
        let serverID = UUID()
        let childData = Data(#"{"parent_session_id":"parent-1","session":{"id":"child-1","parent_id":"ignored-parent"},"status":{"session_id":"child-1","agent_name":"worker","state":"settled"}}"#.utf8)
        let child = try JSONDecoder().decode(SessionSnapshotWire.self, from: childData).project(ref: SessionRef(serverID: serverID, sessionID: "child-1"), connection: .live)
        #expect(child.parentSession == SessionRef(serverID: serverID, sessionID: "parent-1"))
        let rootData = Data(#"{"session":{"id":"root-1","parent_id":"root-1"},"status":{"session_id":"root-1","state":"settled"}}"#.utf8)
        let root = try JSONDecoder().decode(SessionSnapshotWire.self, from: rootData).project(ref: SessionRef(serverID: serverID, sessionID: "root-1"), connection: .live)
        #expect(root.parentSession == nil)
    }

    @Test func forkUserMessageOrdinalsCountHiddenAndNestedUsersWithoutMislabelingTopLevelTargets() throws {
        let data = Data(#"{"id":"root","messages":[{"message":{"agent_name":"","message":{"role":"user","content":"hidden runtime"},"input_origin":"runtime"}},{"sub_session":{"id":"child","messages":[{"message":{"message":{"role":"user","content":"nested"},"input_origin":"agent"}}]}},{"message":{"agent_name":"root","message":{"role":"user","content":"visible target","multi_content":[{"type":"document","document":{"name":"x.txt","mime_type":"text/plain","source":{"inline_text":"x"}}}]},"input_origin":"user"}}]}"#.utf8)
        let session = try JSONDecoder().decode(SessionHistoryWire.self, from: data)
        let candidates = session.forkUserMessages
        #expect(candidates.map(\.index) == [0, 1, 2])
        #expect(candidates.map(\.isTopLevel) == [true, false, true])
        #expect(candidates.last?.text == "visible target")
        #expect(candidates.last?.containsAttachments == true)
        #expect(candidates.last?.canForkBefore == false)
        #expect(candidates[1].canForkBefore == false)
        #expect(session.topLevelForkOrdinalByItemIndex == [0: 0, 2: 2])
    }

    @Test func agentTreeSkipsNodesWithoutAuthoritativeIDsInsteadOfInventingIdentity() {
        let raw: JSONValue = .object(["nodes": .array([
            .object(["node": .object(["agent": .string("unidentified")]), "children": .array([
                .object(["node": .object(["id": .string("child-1"), "agent": .string("child")])])
            ])]),
            .object(["node": .object(["id": .string("valid-1"), "agent": .string("valid"), "state": .string("starting")])])
        ])])
        let nodes = SessionSnapshotWire.agentTree(raw, serverID: UUID())
        #expect(nodes.count == 1)
        #expect(nodes.first?.id == "valid-1")
        #expect(nodes.first?.state == .queued)
    }

    @Test func anchorSerializationPreservesLineKindsAndCoordinates() async throws {
        let url = FileManager.default.temporaryDirectory.appending(path: "ProjectionReview/\(UUID().uuidString).json")
        let store = ReviewDraftStore(fileURL: url)
        let scope = ReviewDraftScope(serverID: UUID(), instanceID: "i", sessionID: "s", repositoryID: "r", worktreeID: "w", workspaceLabel: "repo")
        let anchor = ReviewAnchor(path: "file.swift", mode: .changes, group: .unstaged, generation: 4, documentFingerprint: "h", oldStart: 1, oldEnd: 1, newStart: 1, newEnd: 1, contextLines: [.init(kind: .remove, oldLine: 1, text: "old"), .init(kind: .add, newLine: 1, text: "new")])
        _ = try store.add(scope: scope, anchor: anchor, body: "why?")
        let payload = store.serializeForSend(store.draft(for: scope).comments, workspaceLabel: "repo")
        #expect(payload.contains("remove old=1 new=0: old"))
        #expect(payload.contains("add old=0 new=1: new"))
    }
}
