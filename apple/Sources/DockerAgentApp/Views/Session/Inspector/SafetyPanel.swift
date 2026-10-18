import SwiftUI
import AgentKit

extension SafetyPolicyValue {
    var title: String { rawValue.capitalized }

    var consequence: String {
        switch self {
        case .strict: "Asks before every tool call, including read-only ones."
        case .balanced: "Runs safe, read-only calls; asks before destructive or unknown ones."
        case .restricted: "Runs safe calls and denies destructive or unknown ones without asking. Meant for unattended work."
        case .autonomous: "Runs every call without asking. Only deny and ask rules still apply."
        }
    }

    var symbol: String {
        switch self {
        case .strict: "lock.shield"
        case .balanced: "shield.lefthalf.filled"
        case .restricted: "shield.slash"
        case .autonomous: "bolt.shield"
        }
    }
}

extension PermissionList {
    var title: String {
        switch self {
        case .allow: "Allow"
        case .ask: "Always Ask"
        case .deny: "Deny"
        }
    }

    var footnote: String {
        switch self {
        case .allow: "Run without asking."
        case .ask: "Always ask, even for normally safe tools."
        case .deny: "Always rejected."
        }
    }
}

struct SafetyPanel: View {
    let safety: SafetyProjection
    let readOnly: Bool
    let setPolicy: (SafetyPolicyValue) async throws -> Void
    let setRule: (_ list: PermissionList, _ pattern: String, _ enabled: Bool, _ expected: PermissionRules) async throws -> Void
    var toggleAllTools: ((_ confirmedAutonomous: Bool) async throws -> Void)?
    var reload: (() async -> Void)?

    @State private var confirmingToggle = false
    @State private var togglingAllTools = false
    @State private var toggleOutcomeUnknown = false

    @State private var pendingPolicy: PolicyChoice?
    @State private var addingRule = false
    @State private var error: String?

    var body: some View {
        Form {
            if toggleAllTools != nil {
                Section {
                    LabeledContent("All tools allowed") {
                        Text(safety.toolsApproved ? "On" : "Off")
                            .foregroundStyle(safety.toolsApproved ? .orange : .secondary)
                    }
                    Button("Toggle Autonomy…", systemImage: "bolt.shield") { confirmingToggle = true }
                        .disabled(readOnly || togglingAllTools)
                    if toggleOutcomeUnknown {
                        HStack {
                            Label("The result wasn't confirmed.", systemImage: "questionmark.circle").foregroundStyle(.orange)
                            Spacer()
                            if let reload {
                                Button("Refresh") {
                                    Task {
                                        await reload()
                                        toggleOutcomeUnknown = false
                                    }
                                }
                            }
                        }
                        .font(.callout)
                    }
                } header: {
                    Text("Autonomy")
                } footer: {
                    Text("The server switches its remembered all-tools setting, so this can turn autonomy on. To set a specific safer mode instead, choose Strict or Balanced below; other clients can still change it later.")
                }
            }
            Section {
                ForEach(SafetyPolicyValue.allCases, id: \.self) { policy in
                    PolicyRow(policy: policy, selected: safety.policy == policy, enabled: safety.canEditPolicy && !readOnly) {
                        if policy != safety.policy { pendingPolicy = PolicyChoice(policy: policy) }
                    }
                }
                if safety.policy == nil {
                    Text("Using the agent's default mode.").font(.caption).foregroundStyle(.secondary)
                }
            } header: {
                Text("Safety Mode")
            } footer: {
                Text("Applies to this session only. Approval cards still let you decide individual calls.")
            }

            ForEach(PermissionList.allCases) { list in
                rulesSection(list)
            }

            if safety.canEditSessionRules && !readOnly {
                Section {
                    Button("Add Rule…", systemImage: "plus") { addingRule = true }
                }
            }

            if let error {
                Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
            }
        }
        .formStyle(.grouped)
        .sheet(item: $pendingPolicy) { choice in
            PolicyChangeSheet(from: safety.policy, to: choice.policy) {
                try await setPolicy(choice.policy)
            }
        }
        // The server flips the setting rather than setting a value, and another client may change it,
        // so both directions are confirmed with the same consequence.
        .confirmationDialog("Toggle autonomy for this session?", isPresented: $confirmingToggle, titleVisibility: .visible) {
            Button("Toggle Autonomy", role: .destructive) {
                if let toggleAllTools { runToggle(toggleAllTools, confirmed: true) }
            }
        } message: {
            Text(safety.toolsApproved
                ? "All tools are currently allowed. Toggling returns this session to the mode it used before. If another client changed the setting meanwhile, the result may differ — including allowing all tools."
                : "Toggling lets agents run any tool, including shell commands that change or delete files, without asking. Deny and ask rules still apply. If another client changed the setting meanwhile, the result may differ.")
        }
        .sheet(isPresented: $addingRule) {
            AddRuleSheet { list, pattern in
                try await setRule(list, pattern, true, safety.sessionRules)
            }
        }
    }

    @ViewBuilder
    private func rulesSection(_ list: PermissionList) -> some View {
        let session = safety.sessionRules[list]
        let source = safety.sourceRules[list]
        if !session.isEmpty || !source.isEmpty {
            Section {
                ForEach(session, id: \.self) { pattern in
                    RuleRow(pattern: pattern, origin: "Session")
                        .swipeActions {
                            if canRemove { Button("Remove", role: .destructive) { remove(list, pattern) } }
                        }
                        .contextMenu {
                            if canRemove { Button("Remove Rule", role: .destructive) { remove(list, pattern) } }
                        }
                }
                ForEach(source, id: \.self) { pattern in
                    RuleRow(pattern: pattern, origin: "Agent config")
                }
            } header: {
                Text(list.title)
            } footer: {
                Text(list.footnote)
            }
        }
    }

    private var canRemove: Bool { safety.canEditSessionRules && !readOnly }

    private func runToggle(_ toggle: @escaping (Bool) async throws -> Void, confirmed: Bool) {
        togglingAllTools = true
        toggleOutcomeUnknown = false
        error = nil
        Task {
            do {
                try await toggle(confirmed)
            } catch {
                // No retry: repeating a toggle could flip it back. Show the error and offer a refresh of the actual mode.
                self.error = error.localizedDescription
                toggleOutcomeUnknown = true
            }
            togglingAllTools = false
        }
    }

    private func remove(_ list: PermissionList, _ pattern: String) {
        error = nil
        Task {
            do { try await setRule(list, pattern, false, safety.sessionRules) } catch { self.error = error.localizedDescription }
        }
    }
}

private struct PolicyChoice: Identifiable {
    let policy: SafetyPolicyValue
    var id: String { policy.rawValue }
}

private struct PolicyRow: View {
    let policy: SafetyPolicyValue
    let selected: Bool
    let enabled: Bool
    let select: () -> Void

    var body: some View {
        Button(action: select) {
            HStack(alignment: .firstTextBaseline, spacing: Metrics.m) {
                Image(systemName: policy.symbol)
                    .foregroundStyle(policy == .autonomous ? .orange : .secondary)
                    .frame(width: 20)
                VStack(alignment: .leading, spacing: Metrics.xxs) {
                    Text(policy.title).fontWeight(selected ? .semibold : .regular)
                    Text(policy.consequence).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if selected { Image(systemName: "checkmark").foregroundStyle(.tint) }
            }
            .contentShape(.rect)
        }
        .buttonStyle(.plain)
        .disabled(!enabled && !selected)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}

private struct RuleRow: View {
    let pattern: String
    let origin: String

    var body: some View {
        HStack {
            Text(pattern).font(.monoSmall).textSelection(.enabled)
            Spacer()
            Text(origin).font(.caption2).foregroundStyle(.secondary)
        }
        .accessibilityElement(children: .combine)
    }
}

private struct PolicyChangeSheet: View {
    @Environment(\.dismiss) private var dismiss
    let from: SafetyPolicyValue?
    let to: SafetyPolicyValue
    let apply: () async throws -> Void

    @State private var acknowledged = false
    @State private var working = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("Current", value: from?.title ?? "Agent default")
                    LabeledContent("New", value: to.title)
                } footer: {
                    Text(to.consequence)
                }
                if to == .autonomous {
                    Section {
                        Toggle("I understand agents will run any tool, including shell commands that change or delete files, without asking.", isOn: $acknowledged)
                    }
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Change Safety Mode")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(to == .autonomous ? "Allow Everything" : "Change", role: to == .autonomous ? .destructive : nil) {
                        working = true
                        Task {
                            do { try await apply(); dismiss() } catch { self.error = error.localizedDescription }
                            working = false
                        }
                    }
                    .disabled(working || (to == .autonomous && !acknowledged))
                }
            }
        }
        .frame(minWidth: 420, minHeight: 300)
    }
}

private struct AddRuleSheet: View {
    @Environment(\.dismiss) private var dismiss
    let add: (PermissionList, String) async throws -> Void

    @State private var list: PermissionList = .allow
    @State private var pattern = ""
    @State private var working = false
    @State private var error: String?

    private var trimmed: String { pattern.trimmingCharacters(in: .whitespaces) }

    var body: some View {
        NavigationStack {
            Form {
                Picker("List", selection: $list) {
                    ForEach(PermissionList.allCases) { Text($0.title).tag($0) }
                }
                .pickerStyle(.segmented)
                Section {
                    TextField("Pattern", text: $pattern, prompt: Text("shell:cmd=git status*"))
                        .font(.mono)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                } footer: {
                    Text("A tool name such as read_file, or a tool with an argument pattern such as shell:cmd=ls*. * matches anything.")
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Add Rule")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Add") {
                        working = true
                        Task {
                            do { try await add(list, trimmed); dismiss() } catch { self.error = error.localizedDescription }
                            working = false
                        }
                    }
                    .disabled(trimmed.isEmpty || trimmed.contains(where: \.isNewline) || working)
                }
            }
        }
        .frame(minWidth: 420, minHeight: 280)
    }
}
