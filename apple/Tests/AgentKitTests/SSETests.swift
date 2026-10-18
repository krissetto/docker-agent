import Foundation
import Testing
@testable import AgentKit

struct SSETests {
    @Test func parsesSplitCRLFMultilineCommentsAndUTF8() throws {
        var parser = SSEParser()
        let first = try parser.append(Data("event: event\r\ndata: {\"message\":\"caf".utf8))
        #expect(first.isEmpty)
        let bytes = Data("é\"}\r\nid: 7\r\n\r\n: heartbeat\r\n\r\nevent: ready\ndata:\ndata: second\n\n".utf8)
        let frames = try parser.append(bytes)
        #expect(frames.count == 2)
        #expect(frames[0] == SSEFrame(event: "event", data: "{\"message\":\"café\"}", id: "7"))
        #expect(frames[1] == SSEFrame(event: "ready", data: "\nsecond", id: "7"))
    }

    @Test func parsesResetEventIDAndRetryAndSuppressesFieldOnlyBlocks() throws {
        var parser = SSEParser()
        let frames = try parser.append(Data("id: 9\n\nevent: empty\n\nretry: 1500\nid:\ndata: ready\n\ndata: next\n\n".utf8))
        #expect(frames == [SSEFrame(event: nil, data: "ready", id: "", retry: 1500), SSEFrame(data: "next", id: "", retry: 1500)])
    }

    @Test func carriesMultibyteUTF8AcrossArbitraryByteBoundaries() throws {
        var parser = SSEParser()
        var output: [SSEFrame] = []
        for byte in Data("data: 雪\n\n".utf8) {
            output += try parser.append(Data([byte]))
        }
        #expect(output == [SSEFrame(data: "雪")])
    }

    @Test func enforcesLineAndFrameByteBounds() throws {
        var lineParser = SSEParser(maximumLineBytes: 4, maximumFrameBytes: 10)
        #expect(throws: SSEParserError.lineTooLarge) { try lineParser.append(Data("data: x".utf8)) }
        var frameParser = SSEParser(maximumLineBytes: 64, maximumFrameBytes: 4)
        #expect(throws: SSEParserError.frameTooLarge) { try frameParser.append(Data("data: abcde\n".utf8)) }
    }

    @Test func rejectsInvalidUTF8AtLineBoundaryAndFlushesAtEOF() throws {
        var bad = SSEParser()
        #expect(throws: SSEParserError.invalidUTF8) {
            try bad.append(Data([0x64, 0x61, 0x74, 0x61, 0x3A, 0x20, 0xFF, 0x0A]))
        }

        var final = SSEParser()
        #expect(try final.append(Data("data: last".utf8)).isEmpty)
        #expect(try final.finish() == [SSEFrame(data: "last")])
    }

    @Test func assemblesContractSnapshotChunksContiguously() throws {
        let json = Data(snapshotJSON(cursor: 12).utf8)
        let split = json.count / 2
        var assembler = SnapshotAssembler()
        #expect(try assembler.accept(StreamMessage(type: "snapshot_begin", cursor: 12)) == nil)
        #expect(try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 12, chunk: json.prefix(split))) == nil)
        #expect(try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 12, chunk: json.suffix(from: split))) == nil)
        let snapshot = try assembler.accept(StreamMessage(type: "snapshot_end", cursor: 12))
        #expect(snapshot?.epoch == "epoch-a")
        #expect(snapshot?.cursor == 12)
        #expect(!assembler.isAssembling)
    }

    @Test func rejectsBadChunkBoundariesInterleavingAndTruncation() throws {
        var noBegin = SnapshotAssembler()
        #expect(throws: SnapshotAssemblyError.missingBegin) {
            try noBegin.accept(StreamMessage(type: "snapshot_chunk", cursor: 1, chunk: Data([1])))
        }

        var wrongCursor = SnapshotAssembler()
        _ = try wrongCursor.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        #expect(throws: SnapshotAssemblyError.cursorMismatch) {
            try wrongCursor.accept(StreamMessage(type: "snapshot_chunk", cursor: 2, chunk: Data([1])))
        }
        #expect(!wrongCursor.isAssembling)

        var interleaved = SnapshotAssembler()
        _ = try interleaved.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        #expect(throws: SnapshotAssemblyError.unexpectedMessage("event")) {
            try interleaved.accept(StreamMessage(type: "event"))
        }
        #expect(!interleaved.isAssembling)

        var truncated = SnapshotAssembler()
        _ = try truncated.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        _ = try truncated.accept(StreamMessage(type: "snapshot_chunk", cursor: 1, chunk: Data([123])))
        #expect(throws: SnapshotAssemblyError.incompleteSnapshot) { try truncated.finish() }
    }

    @Test func boundsSnapshotChunkAndAggregateBytes() throws {
        var chunkLimited = SnapshotAssembler()
        _ = try chunkLimited.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        #expect(throws: SnapshotAssemblyError.chunkTooLarge) {
            try chunkLimited.accept(StreamMessage(type: "snapshot_chunk", cursor: 1, chunk: Data(repeating: 0, count: 64 * 1024 + 1)))
        }

        var aggregateLimited = SnapshotAssembler(maximumSnapshotBytes: 3)
        _ = try aggregateLimited.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        #expect(throws: SnapshotAssemblyError.snapshotTooLarge) {
            try aggregateLimited.accept(StreamMessage(type: "snapshot_chunk", cursor: 1, chunk: Data([1, 2, 3, 4])))
        }
    }

    @Test func snapshotCursorMismatchIsRejected() throws {
        var assembler = SnapshotAssembler()
        _ = try assembler.accept(StreamMessage(type: "snapshot_begin", cursor: 3))
        _ = try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 3, chunk: Data(snapshotJSON(cursor: 4).utf8)))
        #expect(throws: SnapshotAssemblyError.cursorMismatch) {
            try assembler.accept(StreamMessage(type: "snapshot_end", cursor: 3))
        }
    }

    @Test func acceptsOmittedZeroCursorFromContractSnapshot() throws {
        let wire = """
        {"session":{"id":"session-1"},"status":{"session_id":"session-1","agent_name":"agent","state":"idle"},"epoch":"epoch-zero"}
        """
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(wire.utf8))
        var cursor = SessionEventCursor(expectedSessionID: "session-1")
        if case .snapshot = cursor.accept(snapshot: snapshot) {} else { Issue.record("zero-cursor snapshot not accepted") }
        #expect(cursor.sequence == 0)
        #expect(cursor.epoch == "epoch-zero")
        let mismatched = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(snapshotJSON(cursor: 0).replacingOccurrences(of: "session-1", with: "other").utf8))
        if case .resnapshotRequired = cursor.accept(snapshot: mismatched) {} else { Issue.record("wrong-session snapshot accepted") }
    }

    @Test func cursorSeedsDuplicatesGapsOrderingAndEpochReset() throws {
        var cursor = SessionEventCursor(expectedSessionID: "session-1")
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(snapshotJSON(cursor: 0).utf8))
        if case .snapshot = cursor.accept(snapshot: snapshot) {} else { Issue.record("snapshot not accepted") }

        let seed = StreamEnvelope(sessionID: "session-1", epoch: "epoch-a", sequence: 0, transcriptPosition: -1, event: ["type": .string("stream_started"), "session_id": .string("session-1")])
        if case .seed = cursor.accept(type: "event", envelope: seed) {} else { Issue.record("sequence-zero seed not accepted") }
        _ = cursor.markReady()
        if case .resnapshotRequired = cursor.accept(type: "event", envelope: seed) {} else { Issue.record("late seed accepted") }

        cursor = SessionEventCursor(expectedSessionID: "session-1")
        _ = cursor.accept(snapshot: snapshot)
        _ = cursor.markReady()
        let first = StreamEnvelope(sessionID: "session-1", epoch: "epoch-a", sequence: 1, transcriptPosition: 0, event: ["type": .string("user_message")])
        if case .apply = cursor.accept(type: "event", envelope: first) {} else { Issue.record("first sequence not applied") }
        if case .duplicate = cursor.accept(type: "event", envelope: first) {} else { Issue.record("duplicate not dropped") }
        let gap = StreamEnvelope(sessionID: "session-1", epoch: "epoch-a", sequence: 3, transcriptPosition: 2)
        if case .resnapshotRequired = cursor.accept(type: "event", envelope: gap) {} else { Issue.record("out-of-order event not blocked") }
        if case .resnapshotRequired = cursor.accept(type: "event", envelope: first) {} else { Issue.record("barrier did not persist") }

        _ = cursor.accept(snapshot: try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(snapshotJSON(cursor: 0, epoch: "epoch-b").utf8)))
        #expect(cursor.epoch == "epoch-b")
        #expect(cursor.sequence == 0)
        #expect(!cursor.requiresSnapshot)
    }

    @Test func readyRequiresSnapshotOrReconnectionCursor() {
        var cursor = SessionEventCursor(expectedSessionID: "session-1")
        if case .resnapshotRequired = cursor.markReady() {} else { Issue.record("ready without baseline accepted") }
        #expect(cursor.requiresSnapshot)
    }

    @Test func resumesAtExistingEpochAndCursorForReplay() {
        var cursor = SessionEventCursor(expectedSessionID: "session-1", epoch: "epoch-a", sequence: 4, isReady: true)
        let replay = StreamEnvelope(sessionID: "session-1", epoch: "epoch-a", sequence: 5, transcriptPosition: 4, event: ["type": .string("message_added")])
        if case .apply = cursor.accept(type: "event", envelope: replay) {} else { Issue.record("next reconnect event not applied") }
        #expect(cursor.sequence == 5)
    }

    @Test func gapAndEpochMismatchAreHardBarriers() throws {
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: Data(snapshotJSON(cursor: 5).utf8))
        var cursor = SessionEventCursor(expectedSessionID: "session-1")
        _ = cursor.accept(snapshot: snapshot)
        let gap = StreamEnvelope(sessionID: "session-1", epoch: "epoch-a", sequence: 6, transcriptPosition: 5, gap: true)
        if case .resnapshotRequired = cursor.accept(type: "event", envelope: gap) {} else { Issue.record("gap not blocked") }

        _ = cursor.accept(snapshot: snapshot)
        let changedEpoch = StreamEnvelope(sessionID: "session-1", epoch: "epoch-b", sequence: 6, transcriptPosition: 5)
        if case .resnapshotRequired = cursor.accept(type: "event", envelope: changedEpoch) {} else { Issue.record("epoch change not blocked") }
    }

    private func snapshotJSON(cursor: UInt64, epoch: String = "epoch-a") -> String {
        """
        {"parent_session_id":"","session":{"id":"session-1","messages":[]},"status":{"session_id":"session-1","agent_name":"agent","state":"running","pending":0},"interactions":[],"pending_inputs":[],"epoch":"\(epoch)","cursor":\(cursor),"transcript_position":0}
        """
    }
}
