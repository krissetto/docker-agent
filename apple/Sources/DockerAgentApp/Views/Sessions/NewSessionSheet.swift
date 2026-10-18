import SwiftUI
import AgentKit

struct NewSessionSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let initialServerID: UUID?
    let onCreated: (SessionRef) -> Void

    @State private var serverID: UUID?
    @State private var agents: [AgentOption] = []
    @State private var sourceID: String?
    @State private var title = ""
    @State private var workingDir = ""
    @State private var registering = false
    @State private var loading = false
    @State private var creating = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                if model.servers.count > 1 {
                    Picker("Server", selection: $serverID) {
                        ForEach(model.servers) { server in
                            Text(server.name).tag(Optional(server.id))
                        }
                    }
                }

                Section("Agent Team") {
                    if loading {
                        ProgressView().frame(maxWidth: .infinity)
                    } else if agents.isEmpty && error == nil {
                        Text("This server has no agent teams.").foregroundStyle(.secondary)
                    }
                    ForEach(agents) { agent in
                        SourceRow(agent: agent, selected: sourceID == agent.id) {
                            sourceID = agent.id
                        }
                    }
                }

                Section {
                    Button("Register Source…", systemImage: "plus.rectangle.on.folder") { registering = true }
                        .disabled(serverID == nil)
                } footer: {
                    Text("Add an agent configuration that already exists in the server's workspace.")
                }

                Section {
                    TextField("Title", text: $title, prompt: Text("Optional"))
                    TextField("Working directory on server", text: $workingDir, prompt: Text("Optional, e.g. projects/app"))
                        .font(.mono)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                } footer: {
                    Text("A directory on the server, not on this device. Leave empty to start without a workspace.")
                }

                if let error {
                    Label(error, systemImage: "xmark.octagon.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("New Session")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Start") { Task { await create() } }
                        .disabled(selected == nil || serverID == nil || creating)
                }
            }
        }
        .frame(minWidth: 420, idealWidth: 480, minHeight: 420)
        .onAppear { serverID = initialServerID ?? model.servers.first?.id }
        .task(id: serverID) { await loadAgents() }
        .sheet(isPresented: $registering) {
            if let serverID {
                RegisterSourceSheet(serverID: serverID) { sourceID in
                    Task {
                        await loadAgents()
                        if agents.contains(where: { $0.id == sourceID && $0.isStartable }) { self.sourceID = sourceID }
                    }
                }
            }
        }
    }

    private var selected: AgentOption? {
        agents.first { $0.id == sourceID && $0.isStartable }
    }

    private func loadAgents() async {
        guard let serverID else { return }
        loading = true
        defer { loading = false }
        do {
            agents = try await model.loadAgents(serverID: serverID)
            error = nil
            let startable = agents.filter(\.isStartable)
            if sourceID == nil || !startable.contains(where: { $0.id == sourceID }) {
                sourceID = startable.count == 1 ? startable.first?.id : nil
            }
        } catch {
            agents = []
            self.error = error.localizedDescription
        }
    }

    private func create() async {
        guard let serverID, let selected else { return }
        creating = true
        defer { creating = false }
        do {
            let directory = workingDir.trimmingCharacters(in: .whitespacesAndNewlines)
            let ref = try await model.createSession(serverID: serverID, source: selected, title: title, workingDir: directory.isEmpty ? nil : directory)
            onCreated(ref)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension AgentOption {
    /// The server must name the root agent; the source ID is never used in its place.
    var isStartable: Bool { !(rootAgentName ?? "").trimmingCharacters(in: .whitespaces).isEmpty }
}

private struct SourceRow: View {
    let agent: AgentOption
    let selected: Bool
    let select: () -> Void

    var body: some View {
        Button(action: select) {
            HStack(alignment: .firstTextBaseline) {
                VStack(alignment: .leading, spacing: Metrics.xxs) {
                    Text(agent.name)
                        .font(.body.weight(.medium))
                    if let root = agent.rootAgentName, agent.isStartable {
                        HStack(spacing: Metrics.xs) {
                            Text("Starts with")
                            AgentLabel(name: root, font: .caption.weight(.semibold))
                            if agent.multi { Text("· team") }
                        }
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    } else {
                        Label("This server doesn't report the root agent, so it can't be started here.", systemImage: "exclamationmark.circle")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                    if !agent.description.isEmpty {
                        Text(agent.description)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(3)
                    }
                }
                Spacer()
                if selected {
                    Image(systemName: "checkmark").foregroundStyle(.tint)
                }
            }
            .contentShape(.rect)
        }
        .buttonStyle(.plain)
        .disabled(!agent.isStartable)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}
