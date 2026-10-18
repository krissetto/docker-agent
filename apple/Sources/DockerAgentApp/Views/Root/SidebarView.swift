import SwiftUI
import AgentKit

struct SidebarView: View {
    @Environment(AppModel.self) private var model
    @Binding var selection: SidebarItem?
    @Binding var editor: ServerEditorTarget?
    @State private var pendingRemoval: ServerProjection?
    @State private var removalError: String?
    @State private var reconnectError: (server: String, message: String)?

    private var runningCount: Int {
        model.sessions.filter { $0.state.isActive }.count
    }

    var body: some View {
        List(selection: $selection) {
            Section {
                Label("Needs You", systemImage: "hand.raised")
                    .badge(model.needsYou.count)
                    .tag(SidebarItem.needsYou)
                Label("Running", systemImage: "bolt.horizontal")
                    .badge(runningCount)
                    .tag(SidebarItem.running)
            }
            Section("Servers") {
                ForEach(model.servers) { server in
                    serverRow(server)
                }
                Button {
                    editor = .add
                } label: {
                    Label("Add Server…", systemImage: "plus")
                }
                .buttonStyle(.borderless)
                .foregroundStyle(.secondary)
            }
        }
        .navigationTitle("Docker Agent")
        .overlay { if model.servers.isEmpty { emptyState } }
        .refreshable { await model.refreshForeground() }
        .confirmationDialog(
            "Remove \(pendingRemoval?.name ?? "server")?",
            isPresented: Binding(get: { pendingRemoval != nil }, set: { if !$0 { pendingRemoval = nil } }),
            titleVisibility: .visible,
            presenting: pendingRemoval
        ) { server in
            Button("Remove", role: .destructive) { remove(server) }
        } message: { _ in
            Text("The server and its saved token are removed from this device. Sessions keep running on the server.")
        }
        .alert("Couldn't Reconnect to \(reconnectError?.server ?? "Server")", isPresented: Binding(get: { reconnectError != nil }, set: { if !$0 { reconnectError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text("\(reconnectError?.message ?? "")\n\nIf the token changed, use Edit to enter the new one.")
        }
        .alert("Couldn't Remove Server", isPresented: Binding(get: { removalError != nil }, set: { if !$0 { removalError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(removalError ?? "")
        }
    }

    private func serverRow(_ server: ServerProjection) -> some View {
        ServerRow(server: server)
            .tag(SidebarItem.server(server.id))
            .contextMenu {
                Button("Reconnect", systemImage: "bolt.horizontal.circle") { reconnect(server) }
                Button("Refresh Sessions", systemImage: "arrow.clockwise") {
                    Task { await model.refreshCatalog(server.id) }
                }
                Button("Edit…", systemImage: "pencil") { editor = .edit(server.id) }
                Divider()
                Button("Remove…", systemImage: "trash", role: .destructive) { pendingRemoval = server }
            }
    }

    private var emptyState: some View {
        ContentUnavailableView {
            Label("No Servers", systemImage: "server.rack")
        } description: {
            Text("Connect to a docker-agent API server to see its sessions.")
        } actions: {
            Button("Add Server…") { editor = .add }
                .buttonStyle(.borderedProminent)
        }
    }

    private func reconnect(_ server: ServerProjection) {
        Task {
            do { try await model.reconnectServer(server.id) } catch { reconnectError = (server.name, error.localizedDescription) }
        }
    }

    private func remove(_ server: ServerProjection) {
        if selection == .server(server.id) { selection = .needsYou }
        Task {
            do { try await model.removeServer(server.id) } catch { removalError = error.localizedDescription }
        }
    }
}
