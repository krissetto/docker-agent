import SwiftUI
import AgentKit

/// macOS Settings scene: manage servers outside the main window.
public struct ServerSettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var selection: UUID?
    @State private var editor: ServerEditorTarget?
    @State private var error: String?

    public init() {}

    public var body: some View {
        VStack(alignment: .leading, spacing: Metrics.m) {
            Text("Servers").font(.headline)
            List(selection: $selection) {
                ForEach(model.servers) { server in
                    ServerRow(server: server).tag(server.id)
                }
            }
            .frame(minHeight: 200)
            .overlay {
                if model.servers.isEmpty {
                    Text("No servers yet").foregroundStyle(.secondary)
                }
            }
            HStack {
                Button { editor = .add } label: { Image(systemName: "plus") }
                    .accessibilityLabel("Add server")
                Button {
                    guard let selection else { return }
                    Task {
                        do { try await model.removeServer(selection); self.selection = nil } catch { self.error = error.localizedDescription }
                    }
                } label: { Image(systemName: "minus") }
                    .disabled(selection == nil)
                    .accessibilityLabel("Remove server")
                Spacer()
                Button("Reconnect") {
                    guard let selection else { return }
                    Task {
                        do { try await model.reconnectServer(selection); error = nil } catch { self.error = error.localizedDescription }
                    }
                }
                .disabled(selection == nil)
                Button("Edit…") { if let selection { editor = .edit(selection) } }
                    .disabled(selection == nil)
            }
            if let error {
                Text(error).font(.caption).foregroundStyle(.red)
            }
            Text("Tokens are stored in the Keychain on this device. Removing a server never stops its sessions.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(Metrics.xl)
        .frame(width: 460)
        .sheet(item: $editor) { ServerEditorSheet(target: $0) }
    }
}
