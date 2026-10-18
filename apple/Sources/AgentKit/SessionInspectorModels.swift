import Foundation

public enum SafetyPolicyValue: String, Codable, Sendable, CaseIterable {
    case strict, balanced, restricted, autonomous
}

public enum PermissionList: String, Codable, Sendable, CaseIterable, Identifiable {
    case allow, ask, deny
    public var id: String { rawValue }
}

public struct PermissionRules: Codable, Sendable, Equatable {
    public var allow: [String]
    public var ask: [String]
    public var deny: [String]
    public init(allow: [String] = [], ask: [String] = [], deny: [String] = []) {
        self.allow = allow; self.ask = ask; self.deny = deny
    }
    private enum CodingKeys: String, CodingKey { case allow, ask, deny, Allow, Ask, Deny }
    public init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        allow = try values.decodeIfPresent([String].self, forKey: .allow) ?? values.decodeIfPresent([String].self, forKey: .Allow) ?? []
        ask = try values.decodeIfPresent([String].self, forKey: .ask) ?? values.decodeIfPresent([String].self, forKey: .Ask) ?? []
        deny = try values.decodeIfPresent([String].self, forKey: .deny) ?? values.decodeIfPresent([String].self, forKey: .Deny) ?? []
    }
    public func encode(to encoder: Encoder) throws {
        var values = encoder.container(keyedBy: CodingKeys.self)
        try values.encode(allow, forKey: .allow); try values.encode(ask, forKey: .ask); try values.encode(deny, forKey: .deny)
    }
    public subscript(_ list: PermissionList) -> [String] {
        get { switch list { case .allow: allow; case .ask: ask; case .deny: deny } }
        set { switch list { case .allow: allow = newValue; case .ask: ask = newValue; case .deny: deny = newValue } }
    }
}

public struct SafetyProjection: Sendable, Equatable {
    public let ref: SessionRef
    public var policy: SafetyPolicyValue?
    public var toolsApproved: Bool
    public var sourceRules: PermissionRules
    public var sessionRules: PermissionRules
    public var canEditPolicy: Bool
    public var canEditSessionRules: Bool
    public init(ref: SessionRef, policy: SafetyPolicyValue?, toolsApproved: Bool, sourceRules: PermissionRules, sessionRules: PermissionRules, canEditPolicy: Bool, canEditSessionRules: Bool) {
        self.ref = ref; self.policy = policy; self.toolsApproved = toolsApproved; self.sourceRules = sourceRules; self.sessionRules = sessionRules; self.canEditPolicy = canEditPolicy; self.canEditSessionRules = canEditSessionRules
    }
}

public struct SessionToolDescriptor: Sendable, Equatable, Identifiable {
    public let name: String
    public let category: String
    public let description: String
    public var id: String { name }
    public init(name: String, category: String, description: String = "") { self.name = name; self.category = category; self.description = description }
}

public struct SessionToolsetProjection: Sendable, Equatable, Identifiable {
    public let name: String
    public let kind: String
    public let description: String
    public let state: String
    public let lastError: String?
    public let restartCount: Int
    public let restartable: Bool
    public let tools: [String]
    public var id: String { name }
    public init(name: String, kind: String = "", description: String = "", state: String, lastError: String? = nil, restartCount: Int = 0, restartable: Bool, tools: [String] = []) {
        self.name = name; self.kind = kind; self.description = description; self.state = state; self.lastError = lastError; self.restartCount = restartCount; self.restartable = restartable; self.tools = tools
    }
}

public struct SessionToolsProjection: Sendable, Equatable {
    public let ref: SessionRef
    public let tools: [SessionToolDescriptor]
    public let toolsets: [SessionToolsetProjection]
    public let canRestartToolsets: Bool
    public init(ref: SessionRef, tools: [SessionToolDescriptor], toolsets: [SessionToolsetProjection], canRestartToolsets: Bool) {
        self.ref = ref; self.tools = tools; self.toolsets = toolsets; self.canRestartToolsets = canRestartToolsets
    }
}

public struct TeamAgentProjection: Sendable, Equatable, Identifiable {
    public let name: String
    public let description: String
    public let model: String?
    public let isCurrent: Bool
    public var id: String { name }
    public init(name: String, description: String = "", model: String? = nil, isCurrent: Bool = false) {
        self.name = name; self.description = description; self.model = model; self.isCurrent = isCurrent
    }
}

public struct AgentDetailsProjection: Sendable, Equatable {
    public let ref: SessionRef
    public let agentName: String
    public let description: String
    public let model: String
    public let fallbacks: [String]
    public let subAgents: [String]
    public let handoffs: [String]
    public let skills: [String]
    public let options: [String]
    public let maxIterations: Int
    public let historyItems: Int
    public let consecutiveToolCalls: Int
    public let toolsets: [SessionToolsetProjection]
    public let historical: Bool
    public let historyReason: String?
    public let isCurrent: Bool
    public init(ref: SessionRef, agentName: String, description: String, model: String, fallbacks: [String], subAgents: [String], handoffs: [String], skills: [String], options: [String], maxIterations: Int, historyItems: Int, consecutiveToolCalls: Int, toolsets: [SessionToolsetProjection], historical: Bool, historyReason: String?, isCurrent: Bool) {
        self.ref = ref; self.agentName = agentName; self.description = description; self.model = model; self.fallbacks = fallbacks; self.subAgents = subAgents; self.handoffs = handoffs; self.skills = skills; self.options = options; self.maxIterations = maxIterations; self.historyItems = historyItems; self.consecutiveToolCalls = consecutiveToolCalls; self.toolsets = toolsets; self.historical = historical; self.historyReason = historyReason; self.isCurrent = isCurrent
    }
}

public struct SessionPermissionsWireResponse: Decodable, Sendable {
    public let policy: String
    public let toolsApproved: Bool
    public let source: PermissionRules?
    public let session: PermissionRules?
    enum CodingKeys: String, CodingKey { case policy, toolsApproved = "tools_approved", source, session }
}

public struct SessionPermissionsPatch: Encodable, Sendable {
    public let permissions: PermissionRules
    public init(permissions: PermissionRules) { self.permissions = permissions }
}

public struct SessionSafetyPolicyPatch: Encodable, Sendable {
    public let safetyPolicy: String
    enum CodingKeys: String, CodingKey { case safetyPolicy = "safety_policy" }
    public init(safetyPolicy: String) { self.safetyPolicy = safetyPolicy }
}

public struct SessionToolsWireResponse: Decodable, Sendable {
    public let tools: [SessionToolDescriptorWire]
    public let statuses: [SessionToolsetStatusWire]
}

public struct SessionToolDescriptorWire: Decodable, Sendable {
    public let name: String
    public let category: String
    public let description: String?
}

public struct SessionToolsetStatusWire: Decodable, Sendable {
    public let name: String
    public let kind: String?
    public let description: String?
    public let state: Int
    public let lastError: String?
    public let restartCount: Int
    public let restartable: Bool
    enum CodingKeys: String, CodingKey { case name, kind, description, state, lastError = "last_error", restartCount = "restart_count", restartable }
}

public struct SessionToolsetRestartRequest: Encodable, Sendable {
    public let name: String
    public init(name: String) { self.name = name }
}
