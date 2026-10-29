import Foundation
import Testing
@testable import AgentKit

@MainActor
struct ProjectionTests {
    @Test func snapshotPairsHistoricalToolCallsAndResultsOnlyByUniqueOwnerAndExactID() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "tool-pairing")
        func json(_ value: Any) throws -> String { String(decoding: try JSONSerialization.data(withJSONObject: value), as: UTF8.self) }
        func assistantCall(_ id: String, _ name: String, _ arguments: String) -> [String: Any] {
            ["id": id, "type": "function", "function": ["name": name, "arguments": arguments]]
        }
        func message(_ owner: String, _ role: String, _ rest: [String: Any]) -> [String: Any] {
            ["message": ["agent_name": owner, "message": ["role": role].merging(rest) { _, new in new }]]
        }
        let calls = [
            assistantCall("call-1", "update_todos", try json(["updates": [["id": "a", "status": "completed"]]])),
            assistantCall("call-2", "update_todos", try json(["updates": [["id": "b", "status": "in-progress"]]])),
            assistantCall("duplicate", "shell", try json(["cmd": "first"])),
            assistantCall("duplicate", "shell", try json(["cmd": "first"]))
        ]
        let messages: [[String: Any]] = [
            ["message": ["agent_name": "root", "message": ["role": "assistant", "content": "", "tool_calls": calls]]],
            message("root", "tool", ["tool_call_id": "call-2", "content": try json(["all_todos": [["id": "b", "description": "second", "status": "in-progress"]]])]),
            message("root", "tool", ["tool_call_id": "call-1", "content": try json(["all_todos": [["id": "a", "description": "first", "status": "completed"]]])]),
            message("other", "tool", ["tool_call_id": "call-1", "content": try json(["leak": true])]),
            message("root", "tool", ["tool_call_id": "duplicate", "content": try json(["unknown": true])]),
            message("root", "tool", ["tool_call_id": "duplicate", "content": try json(["second_unknown": true])]),
            message("root", "tool", ["tool_call_id": "missing", "content": try json(["orphan": true])]),
            ["message": ["message": ["role": "assistant", "tool_calls": [assistantCall("no-owner", "shell", try json(["cmd": "unknown-owner"]))]]]],
            ["message": ["message": ["role": "tool", "tool_call_id": "no-owner", "content": try json(["must_not_link": true])]]]
        ]
        let snapshot: [String: Any] = [
            "session": ["id": ref.sessionID, "messages": messages],
            "status": ["session_id": ref.sessionID, "agent_name": "root", "state": "settled"],
            "epoch": "pairing-epoch", "cursor": 7
        ]
        let data = try JSONSerialization.data(withJSONObject: snapshot)
        let proof = try RawSnapshotProof.extract(from: data, expectedSessionID: ref.sessionID)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
        let projection = wire.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: "instance")

        let callsProjected = projection.transcript.filter { $0.role == "assistant" && $0.toolCallID != nil }
        let results = projection.transcript.filter { $0.role == "tool" }
        let callIDs = callsProjected.compactMap { $0.toolCallID }
        let resultIDs = results.map { $0.id }
        #expect(callIDs == ["call-1", "call-2", "duplicate", "duplicate", "no-owner"])
        #expect(resultIDs == ["snapshot:1:tool-output", "snapshot:2:tool-output", "snapshot:3:tool-output", "snapshot:4:tool-output", "snapshot:5:tool-output", "snapshot:6:tool-output", "snapshot:8:tool-output"])
        let linkedSecond = try #require(results.first)
        let linkedFirst = try #require(results.dropFirst().first)
        #expect(linkedSecond.toolCallID == "call-2")
        #expect(linkedSecond.toolName == "update_todos")
        #expect(linkedSecond.toolArguments?.contains("b") == true)
        #expect(linkedSecond.toolOutput?.contains("second") == true)
        #expect(callsProjected.first?.toolCallID == "call-1")
        #expect(callsProjected.first?.toolOutput == nil, "separate tool output row must be the only visible result payload")
        #expect(linkedFirst.toolCallID == "call-1")
        #expect(linkedFirst.toolArguments?.contains("a") == true)
        #expect(linkedFirst.toolOutput?.contains("first") == true)
        #expect(results[2].toolCallID == "call-1")
        #expect(results[2].toolName == nil)
        #expect(results[2].text == "Unlinked tool result")
        #expect(results[3].toolCallID == "duplicate")
        #expect(results[3].toolName == nil, "duplicate call IDs must remain unlinked")
        #expect(results[4].toolName == nil, "duplicate result IDs must remain unlinked")
        #expect(results[5].toolName == nil, "missing call ID must remain unlinked")
        #expect(results[5].toolOutput?.contains("orphan") == true)
        #expect(results[5].toolStatus == "complete")
        #expect(results[6].toolCallID == "no-owner")
        #expect(results[6].toolName == nil, "missing agent ownership must remain unlinked")
        #expect(callsProjected.allSatisfy { $0.branchAnchor?.messagesSHA256 == proof.messagesSHA256 })
        #expect(results.allSatisfy { $0.branchAnchor?.messagesSHA256 == proof.messagesSHA256 })
        #expect(resultIDs.count == 7, "presentation adds metadata without deleting or merging source message rows")
    }

    @Test func duplicateHistoricalToolCallIDsCannotTransferErrorStatusToInlineCall() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "ambiguous-inline")
        let sameCall: [String: Any] = ["id": "dup", "function": ["name": "shell", "arguments": "{}"]]
        let badResult = ["message": ["agent_name": "root", "message": ["role": "tool", "tool_call_id": "dup", "content": "failed during execution", "is_error": true]]]
        let assistant = ["message": ["agent_name": "root", "message": ["role": "assistant", "tool_calls": [sameCall, sameCall], "presentation": [
            ["type": "tool_call", "tool_call_id": "dup", "tool": ["call": sameCall]]
        ]]]]
        let snapshot: [String: Any] = ["session": ["id": ref.sessionID, "messages": [assistant, badResult]], "status": ["session_id": ref.sessionID, "agent_name": "root", "state": "settled"]]
        let projected = try JSONDecoder().decode(SessionSnapshotWire.self, from: JSONSerialization.data(withJSONObject: snapshot)).project(ref: ref, connection: .live)
        let inline = try #require(projected.transcript.first { $0.id == "snapshot:0:part:0" })
        #expect(inline.toolCallID == "dup")
        #expect(inline.toolStatus == "complete", "ambiguous unrelated failures cannot poison an inline call status")
        #expect(inline.toolOutput == nil)
        let result = try #require(projected.transcript.first { $0.role == "tool" })
        #expect(result.text == "Unlinked tool result")
        #expect(result.toolStatus == "failed", "the source result keeps its actual error")
        #expect(result.toolOutput == "failed during execution")
    }

    @Test func snapshotUsesEmbeddedPresentationToolResultWithoutRawJSONMessage() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "presentation-tool")
        let result = String(decoding: try JSONSerialization.data(withJSONObject: ["all_todos": [["description": "Finish UI", "status": "pending"]]]), as: UTF8.self)
        let call: [String: Any] = ["id": "inline-1", "function": ["name": "list_todos", "arguments": "{}"]]
        let part: [String: Any] = ["type": "tool_call", "tool_call_id": "inline-1", "tool": ["call": call, "result": result]]
        let item: [String: Any] = ["message": ["agent_name": "root", "message": ["role": "assistant", "presentation": [part]]]]
        let snapshot: [String: Any] = ["session": ["id": ref.sessionID, "messages": [item]], "status": ["session_id": ref.sessionID, "agent_name": "root", "state": "settled"]]
        let data = try JSONSerialization.data(withJSONObject: snapshot)
        let projection = try JSONDecoder().decode(SessionSnapshotWire.self, from: data).project(ref: ref, connection: .live)
        let tool = try #require(projection.transcript.first)
        #expect(projection.transcript.count == 1)
        #expect(tool.id == "snapshot:0:part:0")
        #expect(tool.toolCallID == "inline-1")
        #expect(tool.toolName == "list_todos")
        #expect(tool.toolOutput?.contains("Finish UI") == true)
    }

    @Test func historicalAndSSEToolCallsProjectEquivalentCallIdentityAndResult() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "sse-tool-pairing")
        var live = SessionEventProjector(projection: OpenSessionProjection(ref: ref, title: "Fixture", agentName: "root", connection: .live))
        let args = #"{"path":"README.md"}"#
        let function: JSONValue = .object(["name": .string("read_file"), "arguments": .string(args)])
        let call: JSONValue = .object(["id": .string("same-call-id"), "type": .string("function"), "function": function])
        live.apply(StreamEnvelope(sessionID: ref.sessionID, turnID: "turn", epoch: "e", sequence: 1, transcriptPosition: 0,
                                  event: ["type": .string("tool_call"), "agent_name": .string("root"), "tool_call": call]))
        live.apply(StreamEnvelope(sessionID: ref.sessionID, turnID: "turn", epoch: "e", sequence: 2, transcriptPosition: 1,
                                  event: ["type": .string("tool_call_output"), "agent_name": .string("root"), "tool_call_id": .string("same-call-id"), "tool_definition": .object(["name": .string("read_file")]), "output": .string("# README")]))

        let historyCall = ["id": "same-call-id", "type": "function", "function": ["name": "read_file", "arguments": args]] as [String: Any]
        let history: [String: Any] = ["session": ["id": ref.sessionID, "messages": [
            ["message": ["agent_name": "root", "message": ["role": "assistant", "tool_calls": [historyCall]]]],
            ["message": ["agent_name": "root", "message": ["role": "tool", "tool_call_id": "same-call-id", "content": "# README"]]]
        ]], "status": ["session_id": ref.sessionID, "agent_name": "root", "state": "settled"]]
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: JSONSerialization.data(withJSONObject: history)).project(ref: ref, connection: .live)
        let streamedRows = live.projection.transcript.filter { $0.toolCallID == "same-call-id" }
        let streamedCallAndResult = try #require(streamedRows.first)
        let snapshotCalls = snapshot.transcript.filter { $0.toolCallID == "same-call-id" && $0.role == "assistant" }
        let snapshotCall = try #require(snapshotCalls.first)
        let snapshotResults = snapshot.transcript.filter { $0.toolCallID == "same-call-id" && $0.role == "tool" }
        let snapshotResult = try #require(snapshotResults.first)
        #expect(streamedCallAndResult.toolName == snapshotCall.toolName)
        #expect(streamedCallAndResult.toolArguments == snapshotCall.toolArguments)
        #expect(streamedCallAndResult.toolCallID == snapshotResult.toolCallID)
        #expect(streamedCallAndResult.toolName == snapshotResult.toolName)
        #expect(streamedCallAndResult.toolOutput == snapshotResult.toolOutput)
    }

    @Test func historicalTodoToolOutputMapsToActualChecklistContentAndState() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "todo-history")
        let arguments = String(decoding: try JSONSerialization.data(withJSONObject: ["updates": [["id": "todo-1", "status": "completed"]]]), as: UTF8.self)
        let output = String(decoding: try JSONSerialization.data(withJSONObject: ["all_todos": [["id": "todo-1", "description": "Close pairing gap", "status": "completed"]]]), as: UTF8.self)
        let call: [String: Any] = ["id": "todo-call", "type": "function", "function": ["name": "update_todos", "arguments": arguments]]
        let snapshot: [String: Any] = ["session": ["id": ref.sessionID, "messages": [
            ["message": ["agent_name": "root", "message": ["role": "assistant", "tool_calls": [call]]]],
            ["message": ["agent_name": "root", "message": ["role": "tool", "tool_call_id": "todo-call", "content": output]]]
        ]], "status": ["session_id": ref.sessionID, "agent_name": "root", "state": "settled"]]
        let projection = try JSONDecoder().decode(SessionSnapshotWire.self, from: JSONSerialization.data(withJSONObject: snapshot)).project(ref: ref, connection: .live)
        let result = try #require(projection.transcript.first { $0.role == "tool" })
        #expect(result.toolCallID == "todo-call")
        #expect(result.toolName == "update_todos")
        #expect(result.toolArguments?.contains("todo-1") == true)
        #expect(result.toolOutput?.contains("Close pairing gap") == true)
        #expect(result.toolStatus == "complete")
        #expect(result.id == "snapshot:1:tool-output")
    }

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

    @Test func enormousSnapshotRetainsRecentTranscriptTailAndOriginalBranchProofPositions() throws {
        let itemCount = SessionEventProjector.maximumTranscriptItems + 3
        var messages = String()
        messages.reserveCapacity(itemCount * 110)
        messages.append("[")
        for index in 0..<itemCount {
            if index > 0 { messages.append(",") }
            messages.append(#"{"message":{"agent_name":"root","input_origin":"user","message":{"role":"user","content":"message-"#)
            messages.append(String(index))
            messages.append(#""}}}"#)
        }
        messages.append("]")
        let raw = Data((#"{"session":{"id":"large","messages":"# + messages + #"},"status":{"session_id":"large","agent_name":"root"},"epoch":"large-epoch","cursor":17}"#).utf8)
        let proof = try RawSnapshotProof.extract(from: raw, expectedSessionID: "large")
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: raw)
        let projection = snapshot.project(ref: SessionRef(serverID: UUID(), sessionID: "large"), connection: .live,
                                          rawProof: proof, branchInstanceID: "instance")

        #expect(projection.transcript.count == SessionEventProjector.maximumTranscriptItems)
        #expect(projection.omittedTranscriptItemCount == 3)
        #expect(projection.transcript.first?.text == "message-3")
        #expect(projection.transcript.first?.userMessageIndex == 3)
        #expect(projection.transcript.first?.branchAnchor?.itemPosition == 3)
        #expect(projection.transcript.first?.branchAnchor?.itemSHA256 == proof.items[3].sha256)
        #expect(projection.transcript.last?.text == "message-\(itemCount - 1)")
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

    @Test func queuedUserInputIsVisibleImmediatelyThenReconcilesWithExecutionAndSnapshot() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "queued-user-turn")
        var projector = SessionEventProjector(projection: OpenSessionProjection(ref: ref, title: "Session", agentName: "root", connection: .live, epoch: "epoch"))
        func event(_ sequence: UInt64, _ type: String, _ extra: [String: JSONValue] = [:]) -> StreamEnvelope {
            var value: [String: JSONValue] = ["type": .string(type), "turn_id": .string("turn-queued"), "input_origin": .string("user")]
            for (key, item) in extra { value[key] = item }
            return StreamEnvelope(sessionID: ref.sessionID, turnID: "turn-queued", epoch: "epoch", sequence: sequence, transcriptPosition: 0, event: value)
        }

        projector.apply(event(1, "pending_user_message_accepted", ["message": .string("first text")]))
        var row = try #require(projector.projection.transcript.first)
        #expect(projector.projection.transcript.count == 1)
        #expect(row.id == "pending:turn-queued")
        #expect(row.text == "first text")
        #expect(row.userInputState == .queued)

        projector.apply(event(2, "pending_user_message_edited", ["message": .string("edited text")]))
        row = try #require(projector.projection.transcript.first)
        #expect(row.id == "pending:turn-queued")
        #expect(row.text == "edited text")
        #expect(row.userInputState == .queued)

        projector.apply(event(3, "pending_user_message_promoted", ["message": .string("edited text")]))
        row = try #require(projector.projection.transcript.first)
        #expect(row.id == "pending:turn-queued")
        #expect(row.text == "edited text")
        #expect(row.userInputState == nil, "promotion is executed input, not queued")

        projector.apply(event(4, "user_message", ["message": .string("edited text"), "session_id": .string(ref.sessionID)]))
        #expect(projector.projection.transcript.count == 1)
        row = try #require(projector.projection.transcript.first)
        #expect(row.id == "pending:turn-queued")
        #expect(row.turnID == "turn-queued")
        #expect(row.userInputState == nil)
        #expect(row.text == "edited text")

        let snapshotJSON = #"{"session":{"id":"queued-user-turn","messages":[{"message":{"agent_name":"root","actor_turn_id":"turn-queued","actor_accepted":true,"input_origin":"user","pending":false,"message":{"role":"user","content":"edited text"}}}]},"status":{"session_id":"queued-user-turn","agent_name":"root","state":"running"},"epoch":"epoch","cursor":4}"#
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(snapshotJSON.utf8))
        let projectedSnapshot = snapshot.project(ref: ref, connection: .live)
        #expect(projectedSnapshot.transcript.first?.turnID == "turn-queued")
        projector.replace(with: snapshot)
        #expect(projector.projection.transcript.count == 1)
        #expect(projector.projection.transcript.first?.id == "pending:turn-queued")
        #expect(projector.projection.transcript.first?.userInputState == nil)

        projector.replace(with: snapshot)
        #expect(projector.projection.transcript.count == 1, "repeated authoritative snapshots do not duplicate the user turn")
    }

    @Test func queuedUserInputCancellationAndOriginsRemainTruthful() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "queued-user-cancel")
        var projector = SessionEventProjector(projection: OpenSessionProjection(ref: ref, title: "Session", agentName: "root", connection: .live, epoch: "epoch"))
        func event(_ sequence: UInt64, _ type: String, turnID: String, origin: String, immediate: Bool? = nil) -> StreamEnvelope {
            var value: [String: JSONValue] = ["type": .string(type), "turn_id": .string(turnID), "input_origin": .string(origin), "message": .string("body")]
            if let immediate { value["immediate"] = .bool(immediate) }
            return StreamEnvelope(sessionID: ref.sessionID, turnID: turnID, epoch: "epoch", sequence: sequence, transcriptPosition: 0, event: value)
        }
        projector.apply(event(1, "pending_user_message_accepted", turnID: "cancel-me", origin: "user"))
        projector.apply(event(2, "pending_user_message_canceled", turnID: "cancel-me", origin: "user"))
        #expect(projector.projection.transcript.isEmpty)

        projector.apply(event(3, "pending_user_message_accepted", turnID: "already-promoted", origin: "user", immediate: true))
        let immediate = try #require(projector.projection.transcript.first)
        #expect(immediate.id == "pending:already-promoted")
        #expect(immediate.userInputState == nil)
        #expect(immediate.text == "body")

        projector.apply(event(4, "pending_user_message_accepted", turnID: "agent-input", origin: "agent"))
        #expect(projector.projection.transcript.count == 1, "agent-origin input is not a user-origin pending turn")
        projector.apply(event(5, "pending_user_message_accepted", turnID: "runtime-input", origin: "runtime"))
        #expect(projector.projection.transcript.count == 1, "runtime input is not rendered as a user turn")
    }

    @Test func transcriptWindowCoverageCannotImplyProofAndStripsAnchorsWhenIncomplete() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "partial-transcript")
        let anchor = SessionBranchAnchor(ref: ref, instanceID: "instance", epoch: "epoch", cursor: 7,
                                         messagesSHA256: String(repeating: "a", count: 64), itemPosition: 3,
                                         itemSHA256: String(repeating: "b", count: 64))
        let item = TranscriptItem(id: "snapshot:3:message", kind: .message, role: "user", text: "visible tail",
                                  userMessageIndex: 3, branchAnchor: anchor)
        let legacy = OpenSessionProjection(ref: ref, title: "Session", agentName: "root", transcript: [item])
        #expect(legacy.transcriptWindow.isComplete)
        #expect(legacy.transcriptWindow.hasCompleteCanonicalCoverage)
        #expect(legacy.transcript.first?.branchAnchor == anchor)
        #expect(legacy.transcript.first?.userMessageIndex == 3)

        let paged = OpenSessionProjection(ref: ref, title: "Session", agentName: "root", transcript: [item],
                                          transcriptWindow: TranscriptWindow(isComplete: true, omittedItemCount: 12, olderCursor: "opaque-next-page"))
        #expect(!paged.transcriptWindow.hasCompleteCanonicalCoverage, "more older pages means full transcript proof is unavailable")
        #expect(paged.transcriptWindow.omittedItemCount == 12)
        #expect(paged.transcript.first?.branchAnchor == nil)
        #expect(paged.transcript.first?.userMessageIndex == nil)

        let truncated = TranscriptWindow(isComplete: true, containsTruncatedItems: true)
        #expect(!truncated.hasCompleteCanonicalCoverage, "an omitted oversized item cannot be proof complete")
        let incomplete = TranscriptWindow(isComplete: false)
        #expect(!incomplete.hasCompleteCanonicalCoverage)
    }

    @Test func tailProjectionDecodesAbsolutePositionsWithoutProofOrBranchAnchors() throws {
        let ref = SessionRef(serverID: UUID(), sessionID: "tail-session")
        let data = Data(#"{"projection":"tail","session":{"id":"tail-session","title":"Tail"},"status":{"session_id":"tail-session","agent_name":"root","state":"settled"},"epoch":"epoch-tail","cursor":12,"transcript_position":100,"items":[{"position":98,"item":{"message":{"agent_name":"root","message":{"role":"user","content":"old user"},"actor_turn_id":"turn-98","input_origin":"user"}}},{"position":99,"item":{"message":{"agent_name":"root","message":{"role":"assistant","content":"latest"}}}}],"history":{"start_position":98,"end_position":100,"total_items":100,"omitted_ranges":[],"next_cursor":"opaque-cursor","truncated":true}}"#.utf8)
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
        let proof = try? RawSnapshotProof.extract(from: data, expectedSessionID: ref.sessionID)
        #expect(proof == nil)
        let projection = wire.project(ref: ref, connection: .live, rawProof: proof, branchInstanceID: "instance")
        #expect(projection.transcript.map(\.sourcePosition) == [98, 99])
        #expect(projection.transcript.map(\.id) == ["snapshot:98:message", "snapshot:99:message"])
        #expect(projection.transcript.allSatisfy { $0.branchAnchor == nil && $0.userMessageIndex == nil })
        #expect(projection.transcript.allSatisfy { $0.sourceHistoryGeneration != nil })
        #expect(projection.transcriptWindow.hasOlderOnServer)
        #expect(projection.transcriptWindow.anchorEndPosition == 100)
        #expect(!projection.transcriptWindow.hasCompleteCanonicalCoverage)
        let omittedWire = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(#"{"projection":"tail","session":{"id":"tail-session"},"status":{"session_id":"tail-session"},"epoch":"epoch-tail","cursor":12,"transcript_position":3,"items":[{"position":0,"item":{"summary":"oldest"}},{"position":2,"item":{"summary":"newest"}}],"history":{"start_position":0,"end_position":3,"total_items":3,"omitted_ranges":[{"start_position":1,"end_position":2,"reason":"item_exceeds_response_byte_limit"}],"truncated":false}}"#.utf8))
        try omittedWire.validateTail(expectedSessionID: ref.sessionID)
        let omittedProjection = omittedWire.project(ref: ref, connection: .live)
        #expect(omittedProjection.transcriptWindow.containsTruncatedItems)
        #expect(!omittedProjection.transcriptWindow.hasCompleteCanonicalCoverage)
        #expect(omittedProjection.transcriptWindow.omittedRanges.map(\.startPosition) == [1])
    }

    @Test func olderHistoryPageRequiresPinnedAnchorAndRejectsOverlapOrUnknownOmissionReason() throws {
        let wire = try JSONDecoder().decode(SessionHistoryPageWire.self, from: Data(#"{"projection":"tail","session_id":"s","epoch":"e","cursor":7,"transcript_position":100,"items":[{"position":90,"item":{"message":{"message":{"role":"user","content":"older"}}}}],"history":{"start_position":90,"end_position":95,"total_items":100,"omitted_ranges":[],"next_cursor":"next","truncated":true}}"#.utf8))
        #expect(throws: APIError.self) { try wire.validate(sessionID: "s", epoch: "e", cursor: 7, anchorEndPosition: 100, totalItems: 100, previousStartPosition: 90) }
        let validItems = (88..<100).map { position in "{\"position\":\(position),\"item\":{\"summary\":\"older\"}}" }.joined(separator: ",")
        let valid = try JSONDecoder().decode(SessionHistoryPageWire.self, from: Data("{\"projection\":\"tail\",\"session_id\":\"s\",\"epoch\":\"e\",\"cursor\":7,\"transcript_position\":100,\"items\":[\(validItems)],\"history\":{\"start_position\":88,\"end_position\":100,\"total_items\":100,\"omitted_ranges\":[],\"next_cursor\":\"next\",\"truncated\":true}}".utf8))
        try valid.validate(sessionID: "s", epoch: "e", cursor: 7, anchorEndPosition: 100, totalItems: 100, previousStartPosition: 100)
        let responseOmitted = Data("{\"projection\":\"tail\",\"session_id\":\"s\",\"epoch\":\"e\",\"cursor\":7,\"transcript_position\":100,\"items\":[{\"position\":88,\"item\":{\"summary\":\"older\"}}],\"history\":{\"start_position\":88,\"end_position\":100,\"total_items\":100,\"omitted_ranges\":[{\"start_position\":89,\"end_position\":100,\"reason\":\"item_exceeds_response_byte_limit\"}],\"next_cursor\":\"next\",\"truncated\":true}}".utf8)
        let responsePage = try JSONDecoder().decode(SessionHistoryPageWire.self, from: responseOmitted)
        try responsePage.validate(sessionID: "s", epoch: "e", cursor: 7, anchorEndPosition: 100, totalItems: 100, previousStartPosition: 100)
        let wrongItems = ([88] + Array(90..<100)).map { position in "{\"position\":\(position),\"item\":{\"summary\":\"older\"}}" }.joined(separator: ",")
        let wrongReason = Data("{\"projection\":\"tail\",\"session_id\":\"s\",\"epoch\":\"e\",\"cursor\":7,\"transcript_position\":100,\"items\":[\(wrongItems)],\"history\":{\"start_position\":88,\"end_position\":100,\"total_items\":100,\"omitted_ranges\":[{\"start_position\":89,\"end_position\":90,\"reason\":\"future_unrecognized_reason\"}],\"next_cursor\":\"next\",\"truncated\":true}}".utf8)
        let wrong = try JSONDecoder().decode(SessionHistoryPageWire.self, from: wrongReason)
        #expect(throws: APIError.self) { try wrong.validate(sessionID: "s", epoch: "e", cursor: 7, anchorEndPosition: 100, totalItems: 100, previousStartPosition: 90) }
    }

    @Test func boundedHistoryRejectsOversizedScanAndItemBeforeRangeExpansion() throws {
        let hugeOmission = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(#"{"projection":"tail","session":{"id":"bounded"},"status":{"session_id":"bounded"},"epoch":"e","cursor":1,"transcript_position":1000000,"items":[],"history":{"start_position":0,"end_position":1000000,"total_items":1000000,"omitted_ranges":[{"start_position":0,"end_position":1000000,"reason":"item_exceeds_item_byte_limit"}],"truncated":false}}"#.utf8))
        #expect(throws: APIError.self) { try hugeOmission.validateTail(expectedSessionID: "bounded") }

        let oversizedItem = String(repeating: "x", count: 140 * 1024)
        let hugeBody = """
        {"projection":"tail","session":{"id":"bounded"},"status":{"session_id":"bounded"},"epoch":"e","cursor":1,"transcript_position":1,"items":[{"position":0,"item":{"message":{"message":{"role":"assistant","content":"\(oversizedItem)"}}}}],"history":{"start_position":0,"end_position":1,"total_items":1,"omitted_ranges":[],"truncated":false}}
        """
        let itemWire = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(hugeBody.utf8))
        #expect(throws: APIError.self) { try itemWire.validateTail(expectedSessionID: "bounded") }
    }

    @Test func boundedTailSnapshotProjectsSmallPageForVeryLargeHistory() throws {
        let content = String(repeating: "x", count: 65_536)
        let body = """
        {"projection":"tail","session":{"id":"big-tail"},"status":{"session_id":"big-tail","agent_name":"root"},"epoch":"e","cursor":1,"transcript_position":200000,"items":[{"position":199999,"item":{"message":{"agent_name":"root","message":{"role":"assistant","content":"\(content)"}}}}],"history":{"start_position":199999,"end_position":200000,"total_items":200000,"next_cursor":"next","truncated":true}}
        """
        let data = Data(body.utf8)
        #expect(data.count < 128 * 1024, "wire bytes for a large canonical transcript remain one bounded page")
        let wire = try JSONDecoder().decode(SessionSnapshotWire.self, from: data)
        try wire.validateTail(expectedSessionID: "big-tail")
        let projected = wire.project(ref: SessionRef(serverID: UUID(), sessionID: "big-tail"), connection: .live)
        #expect(projected.transcript.count == 1)
        #expect(projected.transcript[0].sourcePosition == 199_999)
        #expect(projected.transcript[0].text.utf8.count == 65_536)
        #expect(projected.transcriptWindow.totalItemCount == 200_000)
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
