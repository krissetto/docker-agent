import SwiftUI
import AgentKit

struct QueuedInputsView: View {
    let inputs: [PendingInputProjection]
    /// Turn IDs the transcript already renders as queued cards; their text isn't repeated here, only the controls.
    var shownInTranscript: Set<String> = []
    var edit: ((PendingInputProjection, String) async throws -> Void)?
    var cancelAll: (() async throws -> Void)?

    @State private var editing: PendingInputProjection?
    @State private var draft = ""
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack {
                Label(inputs.count == 1 ? "1 queued" : "\(inputs.count) queued", systemImage: "tray.full")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.secondary)
                Spacer()
                if let cancelAll {
                    Button {
                        run { try await cancelAll() }
                    } label: {
                        Text("Clear Queue")
                            .font(.callout)
                            .frame(minHeight: Metrics.target)
                            .contentShape(.rect)
                    }
                    .buttonStyle(.borderless)
                }
            }
            .frame(minHeight: Metrics.target)
            ForEach(inputs) { input in
                HStack(spacing: Metrics.s) {
                    if let sender = input.senderName, !input.isUserEditable {
                        AgentDot(name: sender, size: 6)
                    }
                    if shownInTranscript.contains(input.turnID) {
                        // The message itself is the queued card above; this row only carries its controls.
                        Text(input.isUserEditable ? "Your queued message" : "Queued message")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    } else {
                        Text(input.content)
                            .font(.callout)
                            .lineLimit(2)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if input.isUserEditable, edit != nil {
                        Button {
                            draft = input.content
                            editing = input
                        } label: {
                            Text("Edit")
                                .font(.callout)
                                .frame(minWidth: Metrics.target, minHeight: Metrics.target)
                                .contentShape(.rect)
                        }
                        .buttonStyle(.borderless)
                    }
                }
                .padding(.leading, Metrics.m)
                .padding(.trailing, Metrics.xs)
                .frame(minHeight: Metrics.target)
                .floatingChip(radius: Metrics.target / 2)
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
