import SwiftUI
import AgentKit

/// Outbox submissions not yet acknowledged by the server, shown above the composer.
struct PendingSendsView: View {
    let records: [DurableSendRecord]
    let retry: (DurableSendRecord) async throws -> Void
    let discard: (DurableSendRecord) async throws -> Void

    var body: some View {
        VStack(spacing: Metrics.xs) {
            ForEach(records.filter { $0.state != .accepted }) { record in
                PendingSendRow(record: record, retry: retry, discard: discard)
            }
        }
    }
}

private struct PendingSendRow: View {
    let record: DurableSendRecord
    let retry: (DurableSendRecord) async throws -> Void
    let discard: (DurableSendRecord) async throws -> Void
    @State private var busy = false
    @State private var error: String?
    @State private var confirmingDiscard = false

    private var isUnknown: Bool { record.state == .deliveryUnknown }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            Image(systemName: isUnknown ? "questionmark.circle" : "arrow.up.circle.dotted")
                .foregroundStyle(isUnknown ? .orange : .secondary)
                .symbolEffect(.pulse, isActive: record.state == .sending)
            VStack(alignment: .leading, spacing: 1) {
                Text(record.payload.isEmpty ? "Attachments" : record.payload)
                    .font(.callout)
                    .lineLimit(1)
                if !record.attachments.isEmpty {
                    Label(record.attachments.map(\.name).joined(separator: ", "), systemImage: "paperclip")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                Text(status)
                    .font(.caption)
                    .foregroundStyle(isUnknown ? AnyShapeStyle(.orange) : AnyShapeStyle(.secondary))
            }
            Spacer(minLength: Metrics.s)
            if busy || record.state == .sending {
                ProgressView().controlSize(.small)
            } else {
                Button("Retry") { run { try await retry(record) } }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                    .help("Resend this exact message with the same request ID; the server ignores duplicates")
                Button {
                    if isUnknown { confirmingDiscard = true } else { run { try await discard(record) } }
                } label: {
                    Image(systemName: "xmark")
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("Discard")
            }
        }
        .padding(.horizontal, Metrics.m)
        .padding(.vertical, Metrics.s)
        .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
        .accessibilityElement(children: .contain)
        .confirmationDialog("Discard this message?", isPresented: $confirmingDiscard, titleVisibility: .visible) {
            Button("Discard", role: .destructive) { run { try await discard(record) } }
        } message: {
            Text("The server may already have received it. Discarding only removes it from this device.")
        }
    }

    private var status: String {
        if let error { return error }
        switch record.state {
        case .prepared: return "Not sent yet"
        case .sending: return "Sending…"
        case .deliveryUnknown: return "Delivery unknown — Retry is safe"
        case .accepted: return "Delivered"
        }
    }

    private func run(_ action: @escaping () async throws -> Void) {
        busy = true
        error = nil
        Task {
            do { try await action() } catch { self.error = error.localizedDescription }
            busy = false
        }
    }
}
