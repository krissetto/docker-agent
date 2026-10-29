import SwiftUI
import AgentKit

enum InteractionReply {
    case confirmation(ConfirmationDecision, reason: String?)
    case elicitation(action: String, content: [String: JSONValue])
    case maxIterations(continue: Bool)
}

struct InteractionStack: View {
    let interactions: [InteractionProjection]
    let reply: (InteractionProjection, InteractionReply) async throws -> Void
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        VStack(spacing: Metrics.s) {
            ForEach(interactions) { interaction in
                InteractionCard(interaction: interaction, reply: reply)
                    .transition(reduceMotion ? .opacity : .move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(Motion.reveal(reduceMotion), value: interactions)
        .sensoryFeedback(.warning, trigger: interactions.count) { old, new in new > old }
    }
}

private struct InteractionCard: View {
    let interaction: InteractionProjection
    let reply: (InteractionProjection, InteractionReply) async throws -> Void
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            switch interaction {
            case let .confirmation(_, owner, toolName, arguments, decisions):
                ConfirmationContent(owner: owner, toolName: toolName, arguments: arguments, decisions: decisions, busy: busy) { send(.confirmation($0, reason: $1)) }
            case let .elicitation(_, owner, title, fields, _):
                ElicitationContent(owner: owner, title: title, fields: fields, busy: busy) { send(.elicitation(action: $0, content: $1)) }
            case let .maxIterations(_, owner, limit):
                MaxIterationsContent(owner: owner, limit: limit, busy: busy) { send(.maxIterations(continue: $0)) }
            }
            if let error {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
        .padding(.vertical, Metrics.m)
        .padding(.trailing, Metrics.l)
        // Same language as a board tag, with the Signal spine: this one asks for a decision.
        .agentGutter(owner, inset: 0, attention: true)
        .padding(.leading, Metrics.s)
        // Opaque: a decision card sits over scrolling transcript and must never show text through it.
        .background(.background.secondary, in: .rect(cornerRadius: Metrics.promptRadius, style: .continuous))
        .accessibilityElement(children: .contain)
    }

    private var owner: String {
        switch interaction {
        case let .confirmation(_, owner, _, _, _): owner
        case let .elicitation(_, owner, _, _, _): owner
        case let .maxIterations(_, owner, _): owner
        }
    }

    private func send(_ value: InteractionReply) {
        busy = true
        error = nil
        Task {
            do { try await reply(interaction, value) } catch { self.error = error.localizedDescription }
            busy = false
        }
    }
}

private struct CardHeader: View {
    let owner: String
    let text: String

    var body: some View {
        HStack(spacing: Metrics.xs) {
            AgentLabel(name: owner)
            Text(text).font(.subheadline).foregroundStyle(.secondary)
        }
    }
}

private struct ConfirmationContent: View {
    let owner: String
    let toolName: String
    let arguments: String
    let decisions: [ConfirmationDecision]
    let busy: Bool
    let decide: (ConfirmationDecision, String?) -> Void

    @State private var showDetails = false
    @State private var askingReason = false
    @State private var reason = ""
    @FocusState private var focused: Bool

    private func allows(_ decision: ConfirmationDecision) -> Bool { decisions.contains(decision) }

    var body: some View {
        CardHeader(owner: owner, text: "wants to run")
        HStack(spacing: Metrics.xs) {
            Image(systemName: "wrench.and.screwdriver").foregroundStyle(.secondary)
            Text(toolName).font(.mono.weight(.medium))
        }
        if !arguments.isEmpty {
            let preview = ToolCallPresentation(name: toolName, arguments: arguments, status: nil)
            VStack(alignment: .leading, spacing: Metrics.xs) {
                if !showDetails, let command = preview.shellCommand {
                    Text("$ \(command)")
                        .font(.mono)
                        .lineLimit(3)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                } else if !showDetails, let target = preview.target {
                    Text(target)
                        .font(.mono)
                        .lineLimit(2)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                Text(arguments)
                    .font(.monoSmall)
                    .foregroundStyle(.secondary)
                    .lineLimit(showDetails ? 30 : 3)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .opacity(showDetails || (preview.shellCommand == nil && preview.target == nil) ? 1 : 0)
                    .frame(height: showDetails || (preview.shellCommand == nil && preview.target == nil) ? nil : 0)
                Button(showDetails ? "Less" : "Details") { showDetails.toggle() }
                    .buttonStyle(.borderless)
                    .font(.caption)
            }
            .padding(Metrics.s)
            .cardBackground(.fill.quaternary)
        }
        ViewThatFits(in: .horizontal) {
            HStack(spacing: Metrics.s) { buttons(fill: false) }
            // Two rows on narrow screens: the once/deny pair first, then the session-wide options.
            // Half the height of a four-button stack, which matters in landscape on a small phone.
            VStack(spacing: Metrics.s) {
                HStack(spacing: Metrics.s) { primaryButtons(fill: true) }
                HStack(spacing: Metrics.s) { secondaryButtons(fill: true) }
            }
            .controlSize(.large)
        }
        .disabled(busy)
        .focusable()
        .focused($focused)
        .focusEffectDisabled()
        // TUI parity: letter shortcuts only while this card has focus; nothing approves globally.
        .onKeyPress(characters: CharacterSet(charactersIn: "ytnrYTNR")) { press in
            switch press.characters.lowercased() {
            case "y" where allows(.approve): decide(.approve, nil)
            case "t" where allows(.approveTool) && PermissionPattern.build(toolName: toolName, arguments: arguments) != nil: decide(.approveTool, nil)
            case "n" where allows(.reject): decide(.reject, nil)
            case "r" where allows(.reject): askingReason = true
            default: return .ignored
            }
            return .handled
        }
        .alert("Deny with Reason", isPresented: $askingReason) {
            TextField("Reason", text: $reason)
            Button("Deny", role: .destructive) { decide(.reject, reason.isEmpty ? nil : reason) }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("The agent sees this reason.")
        }
    }

    @ViewBuilder
    private func buttons(fill: Bool) -> some View {
        primaryButtons(fill: fill)
        secondaryButtons(fill: fill)
    }

    private func title(_ text: String, fill: Bool) -> some View {
        Text(text).frame(maxWidth: fill ? .infinity : nil)
    }

    @ViewBuilder
    private func primaryButtons(fill: Bool) -> some View {
        if allows(.approve) {
            Button { decide(.approve, nil) } label: { title("Allow Once", fill: fill) }
                .buttonStyle(.borderedProminent)
        }
        if allows(.reject) {
            Button(role: .destructive) { decide(.reject, nil) } label: { title("Deny", fill: fill) }
                .buttonStyle(.bordered)
        }
    }

    @ViewBuilder
    private func secondaryButtons(fill: Bool) -> some View {
        if allows(.approveTool), let pattern = PermissionPattern.build(toolName: toolName, arguments: arguments) {
            Button { decide(.approveTool, nil) } label: { title("Always Allow \(PermissionPattern.label(pattern))", fill: fill) }
                .buttonStyle(.bordered)
                .help("Allow matching calls for the rest of this session")
        }
        if allows(.approveBalanced) || allows(.approveAutonomous) || allows(.reject) {
            Menu {
                if allows(.reject) {
                    Button("Deny with Reason…") { askingReason = true }
                }
                if allows(.approveBalanced) || allows(.approveAutonomous) {
                    Section("Changes this session") {
                        if allows(.approveBalanced) {
                            Button("Switch to Balanced Mode") { decide(.approveBalanced, nil) }
                        }
                        if allows(.approveAutonomous) {
                            Button("Allow All Tools", role: .destructive) { decide(.approveAutonomous, nil) }
                        }
                    }
                }
            } label: {
                Label("More", systemImage: "ellipsis")
            }
            .menuStyle(.button)
            .buttonStyle(.bordered)
            .fixedSize()
        }
    }
}

private struct ElicitationContent: View {
    let owner: String
    let title: String
    let fields: [ElicitationField]
    let busy: Bool
    let respond: (String, [String: JSONValue]) -> Void
    @State private var values: [String: String] = [:]
    @State private var toggles: [String: Bool] = [:]

    private var missingRequired: Bool {
        fields.contains { $0.required && $0.type != "boolean" && (values[$0.id] ?? "").isEmpty }
    }

    var body: some View {
        CardHeader(owner: owner, text: "asks")
        Text(title).font(.body).textSelection(.enabled)
        ForEach(fields) { field in
            fieldView(field)
        }
        HStack(spacing: Metrics.s) {
            Button("Submit") { respond("accept", content) }
                .buttonStyle(.borderedProminent)
                .disabled(missingRequired)
            Button("Decline") { respond("decline", [:]) }
                .buttonStyle(.bordered)
        }
        .disabled(busy)
    }

    @ViewBuilder
    private func fieldView(_ field: ElicitationField) -> some View {
        let label = field.required ? "\(field.label) (required)" : field.label
        if field.type == "boolean" {
            Toggle(label, isOn: Binding(get: { toggles[field.id] ?? false }, set: { toggles[field.id] = $0 }))
        } else if !field.options.isEmpty {
            Picker(label, selection: Binding(get: { values[field.id] ?? "" }, set: { values[field.id] = $0 })) {
                Text("Choose…").tag("")
                ForEach(field.options, id: \.self) { Text($0).tag($0) }
            }
        } else {
            TextField(label, text: Binding(get: { values[field.id] ?? "" }, set: { values[field.id] = $0 }))
                .textFieldStyle(.roundedBorder)
        }
    }

    private var content: [String: JSONValue] {
        var result: [String: JSONValue] = [:]
        for field in fields {
            if field.type == "boolean" {
                result[field.id] = .bool(toggles[field.id] ?? false)
            } else if let value = values[field.id], !value.isEmpty {
                if field.type == "number" || field.type == "integer", let number = Double(value) {
                    result[field.id] = .number(number)
                } else {
                    result[field.id] = .string(value)
                }
            }
        }
        return result
    }
}

private struct MaxIterationsContent: View {
    let owner: String
    let limit: Int
    let busy: Bool
    let decide: (Bool) -> Void

    var body: some View {
        CardHeader(owner: owner, text: "reached its iteration limit")
        Text(limit > 0 ? "It stopped after \(limit) iterations. Continue for another round?" : "It reached its iteration limit. Continue for another round?")
            .font(.callout)
        HStack(spacing: Metrics.s) {
            Button("Continue") { decide(true) }
                .buttonStyle(.borderedProminent)
            Button("Stop") { decide(false) }
                .buttonStyle(.bordered)
        }
        .disabled(busy)
    }
}
