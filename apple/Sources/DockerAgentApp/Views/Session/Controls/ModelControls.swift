import SwiftUI
import AgentKit

struct ModelControls: View {
    let currentModel: String?
    let thinkingLevel: String?
    let thinkingLevels: [String]
    let readOnly: Bool
    var loadModels: (() async throws -> [SessionModelChoice])?
    var refreshModels: (() async throws -> [SessionModelChoice])?
    var selectModel: ((String) async throws -> Void)?
    var setThinking: ((String) async throws -> Void)?

    @State private var models: [SessionModelChoice] = []
    @State private var loading = false
    @State private var error: String?

    var body: some View {
        Section("Model") {
            if let selectModel {
                Picker("Model", selection: Binding(
                    get: { models.first { $0.isCurrent == true }?.ref ?? currentModel ?? "" },
                    set: { ref in run { try await selectModel(ref); try await reload() } }
                )) {
                    if models.isEmpty, let currentModel {
                        Text(currentModel).tag(currentModel)
                    }
                    ForEach(grouped, id: \.title) { group in
                        Section(group.title) {
                            ForEach(group.models, id: \.ref) { choice in
                                ModelChoiceLabel(choice: choice).tag(choice.ref)
                            }
                        }
                    }
                }
                .disabled(readOnly || loading)
            } else if let currentModel {
                LabeledContent("Model") { Text(currentModel).font(.monoSmall) }
            }
            if let setThinking, !thinkingLevels.isEmpty {
                Picker("Thinking", selection: Binding(
                    get: { thinkingLevel ?? "" },
                    set: { level in run { try await setThinking(level) } }
                )) {
                    ForEach(thinkingLevels, id: \.self) { level in
                        HStack {
                            Text(level.capitalized)
                            Text(EffortGauge.glyphs(level: level, levels: thinkingLevels)).font(.caption.monospaced())
                        }
                        .tag(level)
                    }
                }
                .disabled(readOnly)
            }
            if refreshModels != nil {
                Button {
                    run { if let refreshModels { models = try await refreshModels() } }
                } label: {
                    HStack {
                        Text("Refresh Model Catalog")
                        if loading { Spacer(); ProgressView().controlSize(.small) }
                    }
                }
                .disabled(readOnly || loading)
            }
            if let error {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
        .task { try? await reload() }
    }

    private struct Group { let title: String; let models: [SessionModelChoice] }

    private var grouped: [Group] {
        let configured = models.filter { $0.isCatalog != true && $0.isCustom != true }
        let custom = models.filter { $0.isCustom == true }
        let catalog = models.filter { $0.isCatalog == true && $0.isCustom != true }
        return [Group(title: "Configured", models: configured), Group(title: "Custom", models: custom), Group(title: "Catalog", models: catalog)]
            .filter { !$0.models.isEmpty }
    }

    private func reload() async throws {
        guard let loadModels else { return }
        models = try await loadModels()
    }

    private func run(_ action: @escaping () async throws -> Void) {
        loading = true
        error = nil
        Task {
            do { try await action() } catch { self.error = error.localizedDescription }
            loading = false
        }
    }
}

private struct ModelChoiceLabel: View {
    let choice: SessionModelChoice

    var body: some View {
        VStack(alignment: .leading) {
            Text(choice.modelName ?? choice.name)
            if let provider = choice.provider {
                Text([provider, choice.isDefault == true ? "default" : nil].compactMap { $0 }.joined(separator: " · "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

enum EffortGauge {
    /// Six-cell gauge matching the TUI's thinking indicator.
    static func glyphs(level: String, levels: [String]) -> String {
        let ordered = levels.filter { $0 != "none" && $0 != "off" }
        guard let index = ordered.firstIndex(of: level), !ordered.isEmpty else { return "▱▱▱▱▱▱" }
        let filled = max(1, Int((Double(index + 1) / Double(ordered.count) * 6).rounded()))
        return String(repeating: "▰", count: filled) + String(repeating: "▱", count: 6 - filled)
    }
}
