import SwiftUI
import AgentKit

/// Session-level actions; each closure is nil when the server doesn't support it.
struct SessionMenuActions {
    var rename: ((String) async throws -> Void)?
    var setStarred: ((Bool) async throws -> Void)?
    var compact: ((String?) async throws -> Void)?
    var branch: (() async throws -> Void)?
    var switchAgent: ((String) async throws -> Void)?
    var openParent: (() -> Void)?
    var pause: (title: String, action: () -> Void)?
    var waitForTurn: (() -> Void)?
    var waitForAgents: (() -> Void)?
    var history: (() -> Void)?
    var refresh: (() -> Void)?
    var restoreWorkspace: (() -> Void)?
    var exportJSON: (() -> Void)?
    var delete: (() -> Void)?
}

struct SessionMenu: View {
    let title: String
    let starred: Bool
    let currentAgent: String
    let agents: [TeamAgentProjection]
    let readOnly: Bool
    let actions: SessionMenuActions
    /// On iPhone the mode picker lives here so the title keeps its width; nil elsewhere.
    var mode: Binding<SessionMode>? = nil

    @State private var renaming = false
    @State private var newTitle = ""
    @State private var compacting = false
    @State private var error: String?

    private var otherAgents: [TeamAgentProjection] { agents.filter { !$0.isCurrent && $0.name != currentAgent } }

    var body: some View {
        Menu {
            if let mode {
                Picker("View", selection: mode) {
                    ForEach(SessionMode.allCases) { mode in
                        Label(mode.rawValue, systemImage: mode.symbol).tag(mode)
                    }
                }
                .pickerStyle(.inline)
            }
            if let openParent = actions.openParent {
                Button("Open Parent Session", systemImage: "arrow.up.left", action: openParent)
                Divider()
            }
            if let setStarred = actions.setStarred {
                Button(starred ? "Unstar" : "Star", systemImage: starred ? "star.slash" : "star") {
                    run { try await setStarred(!starred) }
                }
            }
            if let pause = actions.pause {
                Button(pause.title, systemImage: pause.title == "Resume" ? "play.circle" : "pause.circle", action: pause.action)
            }
            if let waitForTurn = actions.waitForTurn {
                Button("Wait for This Turn", systemImage: "hourglass.bottomhalf.filled", action: waitForTurn)
            }
            if let waitForAgents = actions.waitForAgents {
                Button("Wait for All Agents to Finish", systemImage: "hourglass", action: waitForAgents)
            }
            if actions.pause != nil || actions.waitForTurn != nil || actions.waitForAgents != nil { Divider() }
            if actions.rename != nil {
                Button("Rename…", systemImage: "pencil") {
                    newTitle = title
                    renaming = true
                }
                .disabled(readOnly)
            }
            if actions.compact != nil {
                Button("Compact Conversation…", systemImage: "rectangle.compress.vertical") { compacting = true }
                    .disabled(readOnly)
            }
            if actions.branch != nil || (actions.switchAgent != nil && !otherAgents.isEmpty) {
                Divider()
            }
            if let branch = actions.branch {
                Button("Branch Session", systemImage: "arrow.triangle.branch") { run { try await branch() } }
                    .disabled(readOnly)
            }
            if let switchAgent = actions.switchAgent, !otherAgents.isEmpty {
                Menu("Continue with Another Agent", systemImage: "person.2") {
                    ForEach(otherAgents) { agent in
                        Button {
                            run { try await switchAgent(agent.name) }
                        } label: {
                            Text(agent.name)
                            if !agent.description.isEmpty { Text(agent.description) }
                        }
                    }
                }
                .disabled(readOnly)
            }
            if actions.refresh != nil || actions.history != nil || actions.restoreWorkspace != nil || actions.exportJSON != nil || actions.delete != nil {
                Divider()
            }
            if let refresh = actions.refresh {
                Button("Refresh Conversation", systemImage: "arrow.clockwise", action: refresh)
            }
            if let history = actions.history {
                Button("Recent Messages…", systemImage: "clock", action: history)
            }
            if let restoreWorkspace = actions.restoreWorkspace {
                Button("Restore Workspace…", systemImage: "clock.arrow.circlepath", action: restoreWorkspace)
                    .disabled(readOnly)
            }
            if let exportJSON = actions.exportJSON {
                Button("Export as JSON…", systemImage: "square.and.arrow.up", action: exportJSON)
            }
            if let delete = actions.delete {
                Button("Delete Session…", systemImage: "trash", role: .destructive, action: delete)
            }
        } label: {
            Label("Session", systemImage: "ellipsis.circle")
        }
        .help("Session actions")
        .accessibilityLabel(mode.map { "Session actions, showing \($0.wrappedValue.rawValue)" } ?? "Session actions")
        .alert("Rename Session", isPresented: $renaming) {
            TextField("Title", text: $newTitle)
            Button("Rename") {
                let value = newTitle.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !value.isEmpty, let rename = actions.rename else { return }
                run { try await rename(value) }
            }
            Button("Cancel", role: .cancel) {}
        }
        .sheet(isPresented: $compacting) {
            CompactSheet { prompt in
                guard let compact = actions.compact else { return }
                try await compact(prompt)
            }
        }
        .alert("Couldn't Complete Action", isPresented: Binding(get: { error != nil }, set: { if !$0 { error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(error ?? "")
        }
    }

    private func run(_ action: @escaping () async throws -> Void) {
        Task {
            do { try await action() } catch { self.error = error.localizedDescription }
        }
    }
}

private struct CompactSheet: View {
    @Environment(\.dismiss) private var dismiss
    let compact: (String?) async throws -> Void
    @State private var instructions = ""
    @State private var working = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Focus on…", text: $instructions, axis: .vertical)
                        .lineLimit(2...5)
                } header: {
                    Text("Instructions (optional)")
                } footer: {
                    Text("The conversation is summarized at the next safe point to free context. Earlier messages stay in the session history.")
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Compact Conversation")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Compact") {
                        working = true
                        Task {
                            do {
                                let trimmed = instructions.trimmingCharacters(in: .whitespacesAndNewlines)
                                try await compact(trimmed.isEmpty ? nil : trimmed)
                                dismiss()
                            } catch {
                                self.error = error.localizedDescription
                            }
                            working = false
                        }
                    }
                    .disabled(working)
                }
            }
        }
        .frame(minWidth: 400, minHeight: 260)
        .presentationDetents([.medium])
    }
}
