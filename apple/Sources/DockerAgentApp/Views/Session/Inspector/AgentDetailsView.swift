import SwiftUI
import AgentKit

/// Read-only agent configuration; never shows instructions or raw config.
struct AgentDetailsView: View {
    let details: AgentDetailsProjection

    var body: some View {
        Form {
            Section {
                HStack {
                    AgentLabel(name: details.agentName, font: .title3.weight(.semibold))
                    if details.isCurrent {
                        Text("current").font(.caption2.weight(.medium)).padding(.horizontal, Metrics.xs).background(.fill.tertiary, in: .capsule)
                    }
                }
                if !details.description.isEmpty {
                    Text(details.description).foregroundStyle(.secondary)
                }
                if details.historical {
                    Label(details.historyReason ?? "Recorded configuration from this session's history.", systemImage: "clock")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            if !details.model.isEmpty || !details.fallbacks.isEmpty {
                Section("Model") {
                    if !details.model.isEmpty { LabeledContent("Model") { Text(details.model).font(.monoSmall) } }
                    if !details.fallbacks.isEmpty { LabeledContent("Fallbacks") { Text(details.fallbacks.joined(separator: ", ")).font(.monoSmall) } }
                }
            }
            list("Sub-agents", details.subAgents)
            list("Handoffs", details.handoffs)
            list("Skills", details.skills)
            let limits = [
                details.maxIterations > 0 ? ("Max iterations", details.maxIterations) : nil,
                details.historyItems > 0 ? ("History items", details.historyItems) : nil,
                details.consecutiveToolCalls > 0 ? ("Consecutive tool calls", details.consecutiveToolCalls) : nil,
            ].compactMap { $0 }
            if !limits.isEmpty {
                Section("Limits") {
                    ForEach(limits, id: \.0) { LabeledContent($0.0, value: "\($0.1)") }
                }
            }
            list("Options", details.options)
            if !details.toolsets.isEmpty {
                Section("Toolsets") {
                    ForEach(details.toolsets) { ToolsetRow(toolset: $0) }
                }
            }
        }
        .formStyle(.grouped)
    }

    @ViewBuilder
    private func list(_ title: String, _ values: [String]) -> some View {
        if !values.isEmpty {
            Section(title) {
                Text(values.joined(separator: ", ")).font(.callout).textSelection(.enabled)
            }
        }
    }
}

struct ToolsetRow: View {
    let toolset: SessionToolsetProjection
    var restart: (() async throws -> Void)?
    @State private var working = false
    @State private var error: String?

    private var tint: Color {
        switch toolset.state.lowercased() {
        case "started", "ready", "running", "serving": .green
        case "error", "failed": .red
        case "starting", "restarting": .orange
        default: .secondary
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xxs) {
            HStack(spacing: Metrics.s) {
                Circle().fill(tint).frame(width: 8, height: 8).accessibilityHidden(true)
                Text(toolset.displayName).font(.callout.weight(.medium)).help(toolset.name)
                if !toolset.kind.isEmpty { Text(toolset.kind).font(.caption).foregroundStyle(.secondary) }
                Spacer()
                Text(toolset.state).font(.caption).foregroundStyle(.secondary)
                if let restart, toolset.restartable {
                    Button {
                        working = true
                        Task {
                            do { try await restart() } catch { self.error = error.localizedDescription }
                            working = false
                        }
                    } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                    .buttonStyle(.borderless)
                    .disabled(working)
                    .help("Restart at the next safe point")
                    .accessibilityLabel("Restart \(toolset.name)")
                }
            }
            if let lastError = toolset.lastError ?? error, !lastError.isEmpty {
                Text(lastError).font(.caption).foregroundStyle(.red).lineLimit(3)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityValue(toolset.state)
    }
}

struct ToolsPanel: View {
    let tools: SessionToolsProjection
    var canRestart = true
    let restart: (String) async throws -> Void

    var body: some View {
        Form {
            if !tools.toolsets.isEmpty {
                Section("Toolsets") {
                    ForEach(tools.toolsets) { toolset in
                        ToolsetRow(toolset: toolset, restart: tools.canRestartToolsets && canRestart ? { try await restart(toolset.name) } : nil)
                    }
                }
            }
            let byCategory = Dictionary(grouping: tools.tools, by: \.category).sorted { $0.key < $1.key }
            ForEach(byCategory, id: \.key) { category, items in
                Section(category.isEmpty ? "Tools" : category) {
                    ForEach(items) { tool in
                        VStack(alignment: .leading, spacing: 1) {
                            Text(tool.name).font(.monoSmall)
                            if !tool.description.isEmpty {
                                Text(tool.description).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                            }
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }
}
