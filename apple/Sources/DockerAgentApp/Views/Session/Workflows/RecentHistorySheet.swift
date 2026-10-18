import SwiftUI
import AgentKit

/// Bounded server history pages; the full conversation stays in the transcript.
struct RecentHistorySheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let ref: SessionRef

    @State private var loading = false
    @State private var error: String?
    @State private var editing: RecentChatMessageWire?

    private var history: RecentHistoryProjection? { model.recentHistoryBySession[ref] }

    private var canEdit: Bool {
        guard let controls = model.sessionControls[ref] else { return false }
        return controls.allows(SessionCapability.editing) && !(model.openSessions[ref]?.state.isActive ?? true) && controls.pendingInputs.isEmpty
    }

    private func editable(_ message: RecentChatMessageWire) -> Bool {
        canEdit && message.role == "user" && message.truncated != true
    }

    var body: some View {
        NavigationStack {
            List {
                if let history {
                    Section {
                        // A cursor means earlier pages exist; truncation without a cursor means they can't be reached here.
                        if history.beforeCursor != nil {
                            Button {
                                Task { await page { try await model.loadEarlierHistory(ref) } }
                            } label: {
                                HStack {
                                    Text("Load Earlier")
                                    if loading { Spacer(); ProgressView().controlSize(.small) }
                                }
                            }
                            .disabled(loading)
                        } else if history.truncatedBefore {
                            Label("Earlier messages aren't available in this view.", systemImage: "ellipsis")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        } else if !history.messages.isEmpty {
                            Text("Start of conversation").font(.caption).foregroundStyle(.secondary)
                        }
                        ForEach(history.messages) { message in
                            HistoryRow(message: message, edit: editable(message) ? { editing = message } : nil)
                        }
                    } footer: {
                        Text("A bounded, text-only view of recent messages. The conversation shows the full transcript.")
                    }
                } else if error == nil {
                    ProgressView().frame(maxWidth: .infinity)
                }
                if let error {
                    Section {
                        Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                        Button("Reload") { Task { await page { try await model.loadRecentHistory(ref) } } }
                    }
                }
            }
            .navigationTitle("Recent Messages")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
            }
        }
        .frame(minWidth: 460, minHeight: 480)
        .task { await page { try await model.loadRecentHistory(ref) } }
        .sheet(item: $editing) { message in
            EditMessageSheet(original: message.text) { text in
                try await model.editRecentUserMessage(ref, messageID: message.id, replacementText: text)
                await page { try await model.loadRecentHistory(ref) }
            } reload: {
                editing = nil
                Task { await page { try await model.loadRecentHistory(ref) } }
            }
        }
    }

    private func page(_ load: @escaping () async throws -> RecentHistoryProjection) async {
        loading = true
        error = nil
        defer { loading = false }
        do {
            _ = try await load()
        } catch let api as APIError where api.statusCode == 409 {
            error = "The conversation changed since this page was loaded. Reload to start from the latest messages."
        } catch {
            self.error = error.localizedDescription
        }
    }
}

private struct HistoryRow: View {
    let message: RecentChatMessageWire
    var edit: (() -> Void)?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xxs) {
            HStack(spacing: Metrics.xs) {
                if message.role == "user" {
                    Text("You").font(.caption.weight(.semibold))
                } else if let agent = message.agentName {
                    AgentLabel(name: agent, font: .caption.weight(.semibold))
                } else {
                    Text(message.role.capitalized).font(.caption.weight(.semibold))
                }
                Spacer()
                if let edit {
                    Button("Edit", action: edit).buttonStyle(.borderless).font(.caption)
                }
                Text("#\(message.position)").font(.caption2.monospacedDigit()).foregroundStyle(.tertiary)
            }
            Text(message.text)
                .font(.callout)
                .textSelection(.enabled)
                .lineLimit(12)
            if message.truncated == true {
                Text("Shortened in this view").font(.caption2).foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, Metrics.xxs)
        .accessibilityElement(children: .combine)
    }
}

private struct EditMessageSheet: View {
    @Environment(\.dismiss) private var dismiss
    let original: String
    let save: (String) async throws -> Void
    let reload: () -> Void

    @State private var text = ""
    @State private var working = false
    @State private var error: String?

    private var changed: Bool {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        return !trimmed.isEmpty && trimmed != original.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    var body: some View {
        NavigationStack {
            Form {
                Section("Original") {
                    Text(original).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
                }
                Section {
                    TextEditor(text: $text).frame(minHeight: 140)
                } header: {
                    Text("Edited message")
                } footer: {
                    Text("Changes the stored message only; nothing is re-run. If someone else edits the session at the same time, the last save wins.")
                }
                if let error {
                    Section {
                        Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                        Button("Reload History", action: reload)
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Edit Message")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        working = true
                        error = nil
                        Task {
                            do { try await save(text); dismiss() } catch { self.error = error.localizedDescription }
                            working = false
                        }
                    }
                    .disabled(!changed || working)
                }
            }
        }
        .frame(minWidth: 460, minHeight: 420)
        .onAppear { text = original }
    }
}
