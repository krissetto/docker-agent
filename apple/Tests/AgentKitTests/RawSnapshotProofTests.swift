import CryptoKit
import Foundation
import Testing
@testable import AgentKit

struct RawSnapshotProofTests {
    @Test func extractsExactCompactGoMessagesArrayAndTopLevelByteRanges() throws {
        let items = #"[{"message":{"agent_name":"","message":{"role":"user","content":"\u003c\u003e\u0026 snow 雪","created_at":"2025-01-02T03:04:05Z"}},"actor_accepted":true,"actor_turn_id":"turn-a","input_origin":"user"},{"summary":"a \"quoted\" brace } and nested { value"}]"#
        let rawString = " { \"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s-raw\",\"messages\":" + items + "},\"status\":{\"session_id\":\"s-raw\"},\"epoch\":\"epoch-raw\",\"cursor\":8}} \n"
        let raw = Data(rawString.utf8)
        let proof = try RawSnapshotProof.extract(from: raw, expectedSessionID: "s-raw")
        #expect(proof.sessionID == "s-raw")
        #expect(proof.epoch == "epoch-raw")
        #expect(proof.cursor == 8)
        #expect(proof.messagesJSON == Data(items.utf8))
        #expect(proof.items.map(\.position) == [0, 1])
        #expect(proof.items[0].rawJSON == Data(#"{"message":{"agent_name":"","message":{"role":"user","content":"\u003c\u003e\u0026 snow 雪","created_at":"2025-01-02T03:04:05Z"}},"actor_accepted":true,"actor_turn_id":"turn-a","input_origin":"user"}"#.utf8))
        #expect(proof.messagesSHA256 == SHA256.hash(data: Data(items.utf8)).map { String(format: "%02x", $0) }.joined())
        #expect(proof.items.allSatisfy { $0.sha256 == SHA256.hash(data: $0.rawJSON).map { String(format: "%02x", $0) }.joined() })
    }

    @Test func acceptsAbsentZeroCursorButRejectsNullMessagesAndIdentityMismatch() throws {
        let zero = Data(#"{"session":{"id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e"}"#.utf8)
        let proof = try RawSnapshotProof.extract(from: zero, expectedSessionID: "s")
        #expect(proof.cursor == 0)
        #expect(proof.messagesJSON == Data("[]".utf8))
        #expect(proof.items.isEmpty)

        let null = Data(#"{"session":{"id":"s","messages":null},"status":{"session_id":"s"},"epoch":"e"}"#.utf8)
        #expect(throws: RawSnapshotProofError.nullMessages) { try RawSnapshotProof.extract(from: null, expectedSessionID: "s") }
        #expect(throws: RawSnapshotProofError.invalidIdentity) { try RawSnapshotProof.extract(from: zero, expectedSessionID: "other") }
        let badCursor = Data(#"{"session":{"id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e","cursor":-1}"#.utf8)
        #expect(throws: RawSnapshotProofError.invalidCursor) { try RawSnapshotProof.extract(from: badCursor, expectedSessionID: "s") }
    }

    @Test func rejectsAmbiguousOrTransformedRawMessageArray() throws {
        let prettyArray = Data(#"{"session":{"id":"s","messages":[ {"message":{"agent_name":"","message":{"role":"user","content":"x"}}}]},"status":{"session_id":"s"},"epoch":"e"}"#.utf8)
        #expect(throws: RawSnapshotProofError.nonCanonicalMessages) { try RawSnapshotProof.extract(from: prettyArray, expectedSessionID: "s") }
        let duplicate = Data(#"{"session":{"id":"s","id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e"}"#.utf8)
        #expect(throws: RawSnapshotProofError.duplicateKey) { try RawSnapshotProof.extract(from: duplicate, expectedSessionID: "s") }
        let trailing = Data(#"{"session":{"id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e"} {}"#.utf8)
        #expect(throws: RawSnapshotProofError.invalidJSON) { try RawSnapshotProof.extract(from: trailing, expectedSessionID: "s") }
    }

    @Test func assemblesRawChunkedSnapshotWithoutReencoding() throws {
        let raw = Data(#"{"session":{"id":"s","messages":[{"summary":"<>& 雪"}]},"status":{"session_id":"s"},"epoch":"e","cursor":7}"#.utf8)
        let split = raw.count / 2
        var assembler = SnapshotAssembler()
        #expect(try assembler.acceptRawJSON(Data(#"{"type":"snapshot_begin","cursor":7}"#.utf8)) == nil)
        #expect(try assembler.acceptRawJSON(Data((#"{"type":"snapshot_chunk","cursor":7,"chunk":""# + raw.prefix(split).base64EncodedString() + #""}"#).utf8)) == nil)
        #expect(try assembler.acceptRawJSON(Data((#"{"type":"snapshot_chunk","cursor":7,"chunk":""# + raw.suffix(from: split).base64EncodedString() + #""}"#).utf8)) == nil)
        let payload = try assembler.acceptRawJSON(Data(#"{"type":"snapshot_end","cursor":7}"#.utf8))
        #expect(payload?.rawJSON == raw)
        #expect(payload?.snapshot.session?.id == "s")
        let proof = try RawSnapshotProof.extract(from: payload!.rawJSON, expectedSessionID: "s")
        #expect(proof.messagesJSON == Data(#"[{"summary":"<>& 雪"}]"#.utf8))
    }

    @Test func matchesGoSnapshotProofFixtureIncludingNullToEmptyMessagesSemantics() throws {
        let fixtures = try #require(Bundle.module.url(forResource: "branch_snapshot_proof_cases", withExtension: "json", subdirectory: "Fixtures"))
        let cases = try JSONSerialization.jsonObject(with: Data(contentsOf: fixtures)) as? [[String: Any]] ?? []
        #expect(cases.count >= 2)
        for example in cases {
            if let frames = example["snapshot_chunk_frames"] as? [[String: Any]] {
                var assembler = SnapshotAssembler()
                var payload: RawSnapshotPayload?
                for frame in frames {
                    let frameData = try JSONSerialization.data(withJSONObject: frame, options: [.fragmentsAllowed, .sortedKeys])
                    payload = try assembler.acceptRawJSON(frameData) ?? payload
                }
                let complete = try #require(payload)
                let decoded = try JSONSerialization.jsonObject(with: complete.rawJSON) as? [String: Any]
                let session = decoded?["session"] as? [String: Any]
                let id = try #require(session?["id"] as? String)
                let proof = try RawSnapshotProof.extract(from: complete.rawJSON, expectedSessionID: id)
                #expect(proof.messagesJSON == Data((example["proof_messages_raw"] as? String ?? "").utf8))
                #expect(proof.messagesSHA256 == example["expected_snapshot"] as? String)
                continue
            }
            guard let raw = example["snapshot_raw"] as? String else { continue }
            let sessionID: String
            if let session = try JSONSerialization.jsonObject(with: Data(raw.utf8)) as? [String: Any],
               let obj = session["session"] as? [String: Any], let id = obj["id"] as? String { sessionID = id }
            else { Issue.record("fixture lacks session identity"); continue }
            let actualMessages = try JSONSerialization.jsonObject(with: Data(raw.utf8)) as? [String: Any]
            let sessionObject = actualMessages?["session"] as? [String: Any]
            if sessionObject?["messages"] is NSNull {
                #expect(throws: RawSnapshotProofError.nullMessages) { try RawSnapshotProof.extract(from: Data(raw.utf8), expectedSessionID: sessionID) }
            } else {
                let proof = try RawSnapshotProof.extract(from: Data(raw.utf8), expectedSessionID: sessionID)
                #expect(proof.messagesJSON == Data((example["proof_messages_raw"] as? String ?? "").utf8))
                #expect(proof.messagesSHA256 == example["expected_snapshot"] as? String)
                #expect(proof.items.count == (try JSONSerialization.jsonObject(with: proof.messagesJSON) as? [Any])?.count)
            }
        }
    }

    @Test func chunkAssemblerAcceptsImplicitZeroCursorAndDoesNotLeakFailedSnapshotBytes() throws {
        let raw = Data(#"{"session":{"id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e"}"#.utf8)
        var assembler = SnapshotAssembler()
        _ = try assembler.accept(StreamMessage(type: "snapshot_begin", cursor: 0))
        _ = try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 0, chunk: raw))
        let snapshot = try assembler.accept(StreamMessage(type: "snapshot_end", cursor: 0))
        #expect(snapshot?.cursor == nil)

        _ = try assembler.accept(StreamMessage(type: "snapshot_begin", cursor: 1))
        _ = try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 1, chunk: Data("{".utf8)))
        #expect(throws: SnapshotAssemblyError.invalidSnapshot) { try assembler.accept(StreamMessage(type: "snapshot_end", cursor: 1)) }
        _ = try assembler.accept(StreamMessage(type: "snapshot_begin", cursor: 0))
        _ = try assembler.accept(StreamMessage(type: "snapshot_chunk", cursor: 0, chunk: raw))
        let recovered = try assembler.accept(StreamMessage(type: "snapshot_end", cursor: 0))
        #expect(recovered?.epoch == "e")
    }

    @Test func preservesRawSingleFramePayloadForProofExtraction() throws {
        let frame = Data(#"{"version":2,"type":"snapshot","snapshot":{"session":{"id":"s","messages":[]},"status":{"session_id":"s"},"epoch":"e","cursor":0}}"#.utf8)
        var assembler = SnapshotAssembler()
        let result = try assembler.acceptRawJSON(frame)
        #expect(result?.rawJSON == frame)
        #expect(result?.snapshot.epoch == "e")
    }
}
