import Foundation

public struct GitReviewSnapshotResponse: Decodable, Sendable {
    public var snapshot: ReviewSnapshot
}
public struct GitReviewWorktreeRequest: Encodable, Sendable { public var generation: UInt64; public var worktreeID: String; enum CodingKeys: String, CodingKey { case generation = "Generation", worktreeID = "WorktreeID" }; public init(generation: UInt64, worktreeID: String) { self.generation = generation; self.worktreeID = worktreeID } }
public struct GitReviewWorktreeResponse: Decodable, Sendable { public var generation: UInt64; public var worktree: ReviewWorktree }
public struct GitReviewDirectoryRequest: Encodable, Sendable { public var generation: UInt64; public var worktreeID: String; public var path: String; enum CodingKeys: String, CodingKey { case generation = "Generation", worktreeID = "WorktreeID", path = "Path" }; public init(generation: UInt64, worktreeID: String, path: String) { self.generation = generation; self.worktreeID = worktreeID; self.path = path } }
public struct GitReviewDirectoryResponse: Decodable, Sendable { public var generation: UInt64; public var directory: ReviewDirectory }
public struct GitReviewDocumentRequest: Encodable, Sendable { public var generation: UInt64; public var key: ReviewDocumentKey; enum CodingKeys: String, CodingKey { case generation = "Generation", key = "Key" }; public init(generation: UInt64, key: ReviewDocumentKey) { self.generation = generation; self.key = key } }
public struct GitReviewDocumentResponse: Decodable, Sendable { public var generation: UInt64; public var document: ReviewDocument }

public struct AgentInfoResponse: Decodable, Sendable {
    public var agentName: String?
    public var currentAgent: String?
    public var agent: AgentInfoLeaf?
    public var team: TeamInfoLeaf?
    enum CodingKeys: String, CodingKey { case agentName = "agent_name", currentAgent = "current_agent", agent, team }
    public var focusedAgentName: String? { team?.currentAgent ?? currentAgent ?? agent?.agentName ?? agentName }
}
public struct AgentInfoLeaf: Decodable, Sendable { public var name: String?; public var agentName: String?; enum CodingKeys: String, CodingKey { case name, agentName = "agent_name" } }
public struct TeamInfoLeaf: Decodable, Sendable { public var currentAgent: String?; public var availableAgents: [TeamAgentLeaf]?; enum CodingKeys: String, CodingKey { case currentAgent = "current_agent", availableAgents = "available_agents" } }
public struct TeamAgentLeaf: Decodable, Sendable {
    public var name: String
    public var description: String
    public var model: String?
    public init(name: String, description: String = "", model: String? = nil) { self.name = name; self.description = description; self.model = model }
}
