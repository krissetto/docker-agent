import SwiftUI
import AgentKit

struct QueuedInputsView: View {
    let inputs: [PendingInputProjection]
    var edit: ((PendingInputProjection, String) async throws -> Void)?
    var cancelAll: (() async throws -> Void)?

    @State private var editing: PendingInputProjection?
    @State private var draft = ""
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack {
                Label("Queued · \(inputs.count)", systemImage: "tray.full")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.secondary)
                Spacer()
                if let cancelAll {
                    Button("Clear Queue") { run { try await cancelAll() } }
                        .buttonStyle(.borderless)
                        .font(.caption)
                }
            }
            ForEach(inputs) { input in
                HStack(spacing: Metrics.s) {
                    if let sender = input.senderName, !input.isUserEditable {
                        AgentDot(name: sender, size: 6)
                    }
                    Text(input.content)
                        .font(.callout)
                        .lineLimit(2)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    if input.isUserEditable, edit != nil {
                        Button("Edit") {
                            draft = input.content
                            editing = input
                        }
                        .buttonStyle(.borderless)
                        .font(.caption)
                    }
                }
                .padding(.horizontal, Metrics.s)
                .padding(.vertical, Metrics.xs)
                .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
            }
            if let error {
                Text(error).font(.caption).foregroundStyle(.red)
            }
        }
        .alert("Edit Queued Message", isPresented: Binding(get: { editing != nil }, set: { if !$0 { editing = nil } })) {
            TextField("Message", text: $draft)
            Button("Save") {
                guard let input = editing, let edit else { return }
                let value = draft.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !value.isEmpty, value != input.content else { return }
                run { try await edit(input, value) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Only messages that haven't started can be edited.")
        }
    }

    private func run(_ action: @escaping () async throws -> Void) {
        error = nil
        Task {
            do { try await action() } catch { self.error = error.localizedDescription }
        }
    }
}
