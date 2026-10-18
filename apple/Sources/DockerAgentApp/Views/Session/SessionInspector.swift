import SwiftUI
import AgentKit

enum InspectorTab: String, CaseIterable, Identifiable {
    case agents = "Agents"
    case todos = "Todos"
    case safety = "Safety"
    case tools = "Tools"
    case session = "Session"
    var id: Self { self }

    var symbol: String {
        switch self {
        case .agents: "person.2"
        case .todos: "checklist"
        case .safety: "checkmark.shield"
        case .tools: "wrench.and.screwdriver"
        case .session: "gauge.with.dots.needle.33percent"
        }
    }
}

/// Capability keys reported per session by the runtime.
enum SessionCapability {
    static let editing = "session_editing"
    static let pendingRemoval = "pending_message_removal"
    static let branching = "branching"
    static let modelSwitching = "model_switching"
    static let modelCatalogRefresh = "model_catalog_refresh"
    static let compaction = "compaction"
    static let thinkingLevels = "thinking_levels"
    static let todoEditing = "todo_editing"
    static let todos = "todos"
    static let executionAvailable = "execution_available"
    static let snapshots = "snapshots"
    static let forkSkills = "fork_skills"
    static let contextInspection = "context_inspection"
    static let mcpPrompts = "mcp_prompts"
    static let pause = "pause"
    static let stopSubtree = "stop_subtree"
    static let delegationPolicy = "delegation_policy"
}

extension SessionControlsProjection {
    var isReadOnly: Bool { historical || !executionAvailable }
    func allows(_ capability: String) -> Bool { !isReadOnly && capabilities.supports(capability) }
}

struct SessionInspector: View {
    @Environment(AppModel.self) private var model
    let session: OpenSessionProjection
    let controls: SessionControlsProjection?
    @Binding var tab: InspectorTab

    private var tabs: [InspectorTab] {
        InspectorTab.allCases.filter { $0 != .todos || controls?.capabilities.supports(SessionCapability.todos) == true }
    }

    var body: some View {
        VStack(spacing: 0) {
            Picker("Inspector", selection: $tab) {
                ForEach(tabs) { tab in
                    Image(systemName: tab.symbol)
                        .accessibilityLabel(tab.rawValue)
                        .help(tab.rawValue)
                        .tag(tab)
                }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(Metrics.m)
            if let controls, controls.isReadOnly {
                Label(controls.historical ? "Read-only history" : "Execution unavailable", systemImage: "lock")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.bottom, Metrics.s)
            }
            Text(tab.rawValue)
                .font(.headline)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, Metrics.l)
            switch tabs.contains(tab) ? tab : .agents {
            case .agents: AgentsInspector(session: session, tree: controls?.agentTree.nonEmpty ?? session.agentTree, canStop: controls?.allows(SessionCapability.stopSubtree) == true)
            case .todos: todos
            case .safety: SafetyTab(ref: session.ref, readOnly: controls?.isReadOnly ?? false)
            case .tools: ToolsTab(ref: session.ref, readOnly: controls?.isReadOnly ?? false)
            case .session: SessionDetailsInspector(session: session, controls: controls)
            }
        }
        .background {
            Group {
                Button("Agents") { tab = .agents }.keyboardShortcut("1", modifiers: .command)
                Button("Todos") { tab = .todos }.keyboardShortcut("2", modifiers: .command)
                Button("Safety") { tab = .safety }.keyboardShortcut("3", modifiers: .command)
                Button("Tools") { tab = .tools }.keyboardShortcut("4", modifiers: .command)
                Button("Session") { tab = .session }.keyboardShortcut("5", modifiers: .command)
            }
            .hidden()
        }
    }

    @ViewBuilder
    private var todos: some View {
        if let controls {
            let ref = session.ref
            let editable = controls.allows(SessionCapability.todoEditing)
            TodosInspector(todos: controls.todos, readOnly: !editable, actions: TodoActions(
                setStatus: editable ? { todo, status in try await model.setTodoStatus(ref, todoID: todo.id, status: status) } : nil,
                updateDescription: editable ? { todo, text in try await model.updateTodoDescription(ref, todoID: todo.id, expected: todo.description, description: text) } : nil,
                remove: editable ? { todo in try await model.removeTodo(ref, todoID: todo.id) } : nil
            ))
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

extension Array {
    var nonEmpty: Self? { isEmpty ? nil : self }
}

private struct AgentsInspector: View {
    @Environment(AppModel.self) private var model
    let session: OpenSessionProjection
    let tree: [AgentNodeProjection]
    var canStop = false
    @Environment(\.openSession) private var openSession
    @State private var inspecting: InspectTarget?
    @State private var stopping: AgentNodeProjection?
    @State private var stopRequested: Set<String> = []
    @State private var stopError: String?

    private struct InspectTarget: Identifiable {
        let agent: String
        var id: String { agent }
    }

    var body: some View {
        List {
            if tree.isEmpty {
                row(name: session.agentName, ref: nil) {
                    AgentNodeRow(name: session.agentName, state: session.state, needsAttention: !session.interactions.isEmpty, isCurrent: true)
                }
            } else {
                OutlineGroup(tree, children: \.childrenOrNil) { node in
                    row(name: node.agentName, ref: node.session, node: node) {
                        AgentNodeRow(name: node.agentName, state: node.state, needsAttention: node.needsAttention, isCurrent: node.session == session.ref, waitingOn: stopRequested.contains(node.id) && node.state.isActive ? "stopping…" : node.waitingOn, cost: node.cost)
                    }
                }
            }
        }
        .listStyle(.sidebar)
        .sheet(item: $inspecting) { target in
            AgentDetailsSheet(ref: session.ref, agent: target.agent)
        }
        .confirmationDialog(stopTitle, isPresented: Binding(get: { stopping != nil }, set: { if !$0 { stopping = nil } }), titleVisibility: .visible, presenting: stopping) { node in
            Button("Stop", role: .destructive) { stop(node) }
        } message: { node in
            Text(stopMessage(node))
        }
        .alert("Couldn't Stop", isPresented: Binding(get: { stopError != nil }, set: { if !$0 { stopError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(stopError ?? "")
        }
    }

    private func descendants(_ node: AgentNodeProjection) -> Int {
        node.children.reduce(0) { $0 + 1 + descendants($1) }
    }

    private func isRoot(_ node: AgentNodeProjection) -> Bool {
        tree.contains { $0.id == node.id }
    }

    private var stopTitle: String {
        guard let node = stopping else { return "" }
        let count = descendants(node)
        return count == 0 ? "Stop \(node.agentName)?" : "Stop \(node.agentName) and \(count) sub-agent\(count == 1 ? "" : "s")?"
    }

    private func stopMessage(_ node: AgentNodeProjection) -> String {
        if isRoot(node) {
            return "Stops the whole tree, including all sub-agents, and withdraws queued messages. History is kept."
        }
        return "Stops this agent and everything it started. Its parent and other agents keep running. History is kept."
    }

    private func stop(_ node: AgentNodeProjection) {
        stopRequested.insert(node.id)
        Task {
            do {
                _ = try await model.stopSessionSubtree(node, confirmed: true)
            } catch {
                stopRequested.remove(node.id)
                stopError = error.localizedDescription
            }
        }
    }

    private func row(name: String, ref: SessionRef?, node: AgentNodeProjection? = nil, @ViewBuilder label: () -> some View) -> some View {
        HStack(spacing: Metrics.xs) {
            Button {
                if let ref, ref != session.ref { openSession(ref) }
            } label: {
                label()
            }
            .buttonStyle(.plain)
            .disabled(ref == nil || ref == session.ref)
            Button {
                inspecting = InspectTarget(agent: name)
            } label: {
                Image(systemName: "info.circle")
            }
            .buttonStyle(.borderless)
            .help("Inspect \(name)")
            .accessibilityLabel("Inspect \(name)")
        }
        .contextMenu {
            Button("Inspect", systemImage: "info.circle") { inspecting = InspectTarget(agent: name) }
            if let ref, ref != session.ref {
                Button("Open Session", systemImage: "arrow.right.circle") { openSession(ref) }
            }
            if canStop, let node, node.session != nil, node.state.isActive {
                Divider()
                Button("Stop…", systemImage: "stop.circle", role: .destructive) { stopping = node }
            }
        }
    }
}

private struct AgentDetailsSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let ref: SessionRef
    let agent: String
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Group {
                if let details = model.agentDetailsBySession[ref]?[agent] {
                    AgentDetailsView(details: details)
                } else if let error {
                    ContentUnavailableView("Couldn't Load Agent", systemImage: "exclamationmark.triangle", description: Text(error))
                } else {
                    ProgressView()
                }
            }
            .navigationTitle(agent)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
            }
        }
        .frame(minWidth: 420, minHeight: 480)
        .presentationDetents([.medium, .large])
        .task {
            do { _ = try await model.loadAgentDetails(ref, agent: agent) } catch { self.error = error.localizedDescription }
        }
    }
}

private struct SafetyTab: View {
    @Environment(AppModel.self) private var model
    let ref: SessionRef
    let readOnly: Bool
    @State private var error: String?

    var body: some View {
        Group {
            if let safety = model.safetyBySession[ref] {
                SafetyPanel(safety: safety, readOnly: readOnly) { policy in
                    _ = try await model.setSafetyPolicy(ref, policy: policy)
                } setRule: { list, pattern, enabled, expected in
                    _ = try await model.setPermissionRule(ref, list: list, pattern: pattern, enabled: enabled, expected: expected)
                } toggleAllTools: { confirmed in
                    try await model.toggleToolsApproval(ref, confirmedAutonomous: confirmed)
                } reload: {
                    _ = try? await model.loadSafety(ref)
                }
            } else if let error {
                ContentUnavailableView("Safety Unavailable", systemImage: "shield.slash", description: Text(error))
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: ref) {
            do { _ = try await model.loadSafety(ref) } catch { self.error = error.localizedDescription }
        }
    }
}

private struct ToolsTab: View {
    @Environment(AppModel.self) private var model
    let ref: SessionRef
    let readOnly: Bool
    @State private var error: String?

    var body: some View {
        Group {
            if let tools = model.toolsBySession[ref] {
                ToolsPanel(tools: tools, canRestart: !readOnly) { name in
                    _ = try await model.restartToolset(ref, name: name)
                }
            } else if let error {
                ContentUnavailableView("Tools Unavailable", systemImage: "wrench.and.screwdriver", description: Text(error))
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: ref) {
            do { _ = try await model.loadSessionTools(ref) } catch { self.error = error.localizedDescription }
        }
    }
}

private struct AgentNodeRow: View {
    let name: String
    let state: SessionState
    let needsAttention: Bool
    let isCurrent: Bool
    // Populated once the tree projection carries them; hidden while nil.
    var waitingOn: String? = nil
    var cost: Double? = nil

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            AgentDot(name: name)
            VStack(alignment: .leading, spacing: 1) {
                Text(name).fontWeight(isCurrent ? .semibold : .regular)
                if let waitingOn, !waitingOn.isEmpty {
                    Text(waitingOn.capitalized)
                        .font(.caption)
                        .foregroundStyle(waitingOn == "failed" ? .red : .orange)
                }
            }
            Spacer()
            if let cost, cost > 0 {
                Text(Format.cost(cost))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            if needsAttention && waitingOn == nil {
                Image(systemName: "hand.raised.fill").foregroundStyle(.orange).accessibilityLabel("Needs you")
            }
            StateIcon(state: state).font(.caption)
        }
        .accessibilityElement(children: .combine)
    }
}

extension AgentNodeProjection {
    var childrenOrNil: [AgentNodeProjection]? { children.isEmpty ? nil : children }
}

private struct SessionDetailsInspector: View {
    @Environment(AppModel.self) private var model
    let session: OpenSessionProjection
    let controls: SessionControlsProjection?
    @State private var breakdown: SessionContextBreakdown?
    @State private var delegationEnabled: Bool?
    @State private var detailError: String?

    var body: some View {
        let ref = session.ref
        Form {
            if let controls {
                ModelControls(
                    currentModel: controls.model,
                    thinkingLevel: controls.thinkingLevel,
                    thinkingLevels: controls.thinkingLevels,
                    readOnly: controls.isReadOnly,
                    loadModels: controls.capabilities.supports(SessionCapability.modelSwitching) ? { try await model.sessionModels(ref) } : nil,
                    refreshModels: controls.allows(SessionCapability.modelCatalogRefresh) ? { try await model.refreshSessionModels(ref) } : nil,
                    selectModel: controls.allows(SessionCapability.modelSwitching) ? { try await model.setSessionModel(ref, model: $0) } : nil,
                    setThinking: controls.allows(SessionCapability.thinkingLevels) ? { try await model.setThinkingLevel(ref, level: $0) } : nil
                )
            }
            if session.contextLimit > 0 {
                Section("Context") {
                    ContextGauge(used: session.contextLength, limit: session.contextLimit, compactionThreshold: session.compactionThreshold)
                }
            }
            if let breakdown {
                ContextBreakdownView(breakdown: breakdown, stopIncluding: controls?.allows(SessionCapability.editing) == true ? { path in
                    try await model.removeSessionAttachment(session.ref, path: path)
                    self.breakdown = try? await model.loadContextBreakdown(session.ref)
                } : nil)
            }
            if let delegationEnabled {
                Section {
                    Toggle("Allow new sub-agents", isOn: Binding(get: { delegationEnabled }, set: { value in setDelegation(value) }))
                        .disabled(controls?.isReadOnly ?? true)
                } footer: {
                    Text("Applies to new delegations across this session tree. Work already running continues.")
                }
            }
            if let detailError {
                Label(detailError, systemImage: "exclamationmark.triangle.fill").font(.caption).foregroundStyle(.red)
            }
            Section("Usage") {
                LabeledContent("Input tokens", value: Format.tokens(session.inputTokens))
                LabeledContent("Output tokens", value: Format.tokens(session.outputTokens))
                LabeledContent("Cost", value: Format.cost(session.cost))
            }
            .monospacedDigit()
        }
        .formStyle(.grouped)
        .task(id: session.ref) {
            if controls?.capabilities.supports(SessionCapability.delegationPolicy) == true {
                delegationEnabled = try? await model.delegationPolicy(session.ref).enabled
            }
            guard controls?.allows(SessionCapability.contextInspection) == true else { return }
            breakdown = try? await model.loadContextBreakdown(session.ref)
        }
    }

    private func setDelegation(_ enabled: Bool) {
        let previous = delegationEnabled
        delegationEnabled = enabled
        detailError = nil
        Task {
            do {
                delegationEnabled = try await model.setDelegationPolicy(session.ref, enabled: enabled).enabled
            } catch {
                delegationEnabled = previous
                detailError = error.localizedDescription
            }
        }
    }
}

/// Matches the TUI: orange from 75% of the compaction threshold, red at the threshold.
struct ContextGauge: View {
    let used: Int64
    let limit: Int64
    let compactionThreshold: Double?

    private var fraction: Double { limit > 0 ? min(1, Double(used) / Double(limit)) : 0 }
    private var threshold: Double { compactionThreshold.map { $0 > 1 ? $0 / 100 : $0 } ?? 1 }
    private var tint: Color {
        if fraction >= threshold { return .red }
        if fraction >= threshold * 0.75 { return .orange }
        return .accentColor
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            ProgressView(value: fraction).tint(tint)
            HStack {
                Text("\(Format.tokens(used)) of \(Format.tokens(limit)) tokens")
                Spacer()
                Text(fraction.formatted(.percent.precision(.fractionLength(0))))
            }
            .font(.caption.monospacedDigit())
            .foregroundStyle(.secondary)
            if compactionThreshold != nil {
                Text("Compacts automatically near \(threshold.formatted(.percent.precision(.fractionLength(0)))).")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Context \(fraction.formatted(.percent.precision(.fractionLength(0)))) used")
    }
}
