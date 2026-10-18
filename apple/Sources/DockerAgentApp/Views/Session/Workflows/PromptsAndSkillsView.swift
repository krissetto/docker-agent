import SwiftUI
import AgentKit

/// MCP prompts expand into the composer for review; fork skills start a separate agent.
struct PromptsAndSkillsView: View {
    let loadPrompts: (() async throws -> [SessionPrompt])?
    let loadSkills: (() async throws -> [SessionForkSkill])?
    let expand: (SessionPrompt, [String: String]) async throws -> String
    let insert: (String) -> Void
    let runSkill: (SessionForkSkill, String) async throws -> SkillOperationProjection

    @Environment(\.dismiss) private var dismiss
    @State private var prompts: [SessionPrompt] = []
    @State private var skills: [SessionForkSkill] = []
    @State private var query = ""
    @State private var loading = true
    @State private var error: String?
    @State private var selectedPrompt: Selection?
    @State private var selectedSkill: Selection?

    private func matches(_ name: String, _ description: String?) -> Bool {
        query.isEmpty || name.localizedCaseInsensitiveContains(query) || (description ?? "").localizedCaseInsensitiveContains(query)
    }

    var body: some View {
        NavigationStack {
            List {
                if loadPrompts != nil {
                    Section("Prompts") {
                        let shown = prompts.filter { matches($0.name, $0.description) }
                        if shown.isEmpty && !loading { Text("No prompts").foregroundStyle(.secondary) }
                        ForEach(shown) { prompt in
                            Button { selectedPrompt = Selection(id: prompt.id) } label: {
                                ItemLabel(symbol: "text.insert", title: prompt.name, detail: prompt.description)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
                if loadSkills != nil {
                    Section {
                        let shown = skills.filter { matches($0.name, $0.description) }
                        if shown.isEmpty && !loading { Text("No skills").foregroundStyle(.secondary) }
                        ForEach(shown) { skill in
                            Button { selectedSkill = Selection(id: skill.id) } label: {
                                ItemLabel(symbol: "arrow.triangle.branch", title: skill.name, detail: skill.description ?? "")
                            }
                            .buttonStyle(.plain)
                        }
                    } header: {
                        Text("Skills")
                    } footer: {
                        Text("Skills run in a separate agent and report back to this session.")
                    }
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .overlay { if loading { ProgressView() } }
            .searchable(text: $query, prompt: "Filter")
            .navigationTitle("Prompts & Skills")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Close") { dismiss() } }
            }
            .navigationDestination(item: $selectedPrompt) { selection in
                if let prompt = prompts.first(where: { $0.id == selection.id }) {
                    PromptForm(prompt: prompt, expand: expand) { text in
                        insert(text)
                        dismiss()
                    }
                }
            }
            .navigationDestination(item: $selectedSkill) { selection in
                if let skill = skills.first(where: { $0.id == selection.id }) {
                    SkillRunForm(skill: skill, run: runSkill) { dismiss() }
                }
            }
        }
        .frame(minWidth: 420, minHeight: 440)
        .task { await load() }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            if let loadPrompts { prompts = try await loadPrompts() }
            if let loadSkills { skills = try await loadSkills() }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

private struct Selection: Hashable {
    let id: String
}

private struct ItemLabel: View {
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.m) {
            Image(systemName: symbol).foregroundStyle(.secondary).frame(width: 18)
            VStack(alignment: .leading, spacing: 1) {
                Text(title)
                if !detail.isEmpty {
                    Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
            }
            Spacer()
            Image(systemName: "chevron.right").font(.caption).foregroundStyle(.tertiary)
        }
        .contentShape(.rect)
    }
}

private struct PromptForm: View {
    let prompt: SessionPrompt
    let expand: (SessionPrompt, [String: String]) async throws -> String
    let insert: (String) -> Void

    @State private var values: [String: String] = [:]
    @State private var preview: String?
    @State private var working = false
    @State private var error: String?

    private var missingRequired: Bool {
        prompt.arguments.contains { $0.required && (values[$0.name] ?? "").trimmingCharacters(in: .whitespaces).isEmpty }
    }

    var body: some View {
        Form {
            if !prompt.description.isEmpty {
                Section { Text(prompt.description).foregroundStyle(.secondary) }
            }
            if !prompt.arguments.isEmpty {
                Section("Arguments") {
                    ForEach(prompt.arguments) { argument in
                        VStack(alignment: .leading, spacing: Metrics.xxs) {
                            TextField(argument.required ? "\(argument.name) (required)" : argument.name, text: Binding(
                                get: { values[argument.name] ?? "" },
                                set: { values[argument.name] = $0; preview = nil }
                            ))
                            if !argument.description.isEmpty {
                                Text(argument.description).font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                }
            }
            if let preview {
                Section {
                    Text(preview).font(.callout).textSelection(.enabled).lineLimit(20)
                } header: {
                    Text("Preview")
                } footer: {
                    Text("Inserted into the composer for you to review. Nothing is sent until you send it.")
                }
            }
            if let error {
                Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
            }
        }
        .formStyle(.grouped)
        .navigationTitle(prompt.name)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                if let preview {
                    Button("Insert") { insert(preview) }
                } else {
                    Button("Preview") { Task { await run() } }
                        .disabled(missingRequired || working)
                }
            }
        }
    }

    private func run() async {
        working = true
        error = nil
        defer { working = false }
        let args = values.filter { !$0.value.trimmingCharacters(in: .whitespaces).isEmpty }
        do { preview = try await expand(prompt, args) } catch { self.error = error.localizedDescription }
    }
}

private struct SkillRunForm: View {
    let skill: SessionForkSkill
    let run: (SessionForkSkill, String) async throws -> SkillOperationProjection
    let done: () -> Void

    @State private var task = ""
    @State private var working = false
    @State private var outcome: Outcome?

    private enum Outcome {
        case started(String)
        case failed(String)
        case unknown(String)
    }

    var body: some View {
        Form {
            Section {
                if let description = skill.description, !description.isEmpty {
                    Text(description).foregroundStyle(.secondary)
                }
                if let model = skill.model, !model.isEmpty {
                    LabeledContent("Model") { Text(model).font(.monoSmall) }
                }
            }
            Section {
                TextField("What should it do?", text: $task, axis: .vertical)
                    .lineLimit(3...8)
            } header: {
                Text("Task")
            } footer: {
                Text("Starts a separate agent in its own session. It keeps running even if you close this window.")
            }
            switch outcome {
            case .started(let id):
                Label("Started · \(id.prefix(8))", systemImage: "checkmark.circle").foregroundStyle(.green)
            case .failed(let message):
                Label(message, systemImage: "xmark.octagon").foregroundStyle(.red)
            case .unknown(let message):
                VStack(alignment: .leading, spacing: Metrics.xs) {
                    Label("Run status unknown", systemImage: "questionmark.circle").foregroundStyle(.orange)
                    Text("\(message) The skill may already be running. Check the agent tree before starting it again; a new run can create a duplicate.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            case nil:
                EmptyView()
            }
        }
        .formStyle(.grouped)
        .navigationTitle(skill.name)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                switch outcome {
                case .started:
                    Button("Done", action: done)
                case .unknown:
                    Button("Start New Run", role: .destructive) { Task { await start() } }
                        .disabled(working)
                default:
                    Button("Run") { Task { await start() } }
                        .disabled(task.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || working)
                }
            }
        }
    }

    private func start() async {
        working = true
        defer { working = false }
        do {
            let operation = try await run(skill, task.trimmingCharacters(in: .whitespacesAndNewlines))
            switch operation.state {
            case .accepted, .completed: outcome = .started(operation.operationID)
            case .failed: outcome = .failed(operation.error ?? "The skill couldn't start.")
            case .unknown: outcome = .unknown(operation.error ?? "The server didn't confirm the run.")
            }
        } catch let error as APIError where error.statusCode.map({ (400..<500).contains($0) && $0 != 408 && $0 != 429 }) ?? false {
            outcome = .failed(error.localizedDescription)
        } catch {
            // No operation-ID deduplication on the server, so an unanswered request can't be retried safely.
            outcome = .unknown(error.localizedDescription)
        }
    }
}
