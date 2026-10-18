import SwiftUI
import AgentKit

/// Fork skill runs that haven't completed. Unknown means the start wasn't confirmed; there is no retry.
struct SkillOperationsView: View {
    let operations: [SkillOperationProjection]

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            ForEach(operations) { operation in
                HStack(spacing: Metrics.s) {
                    icon(operation.state)
                    Text(operation.skillName).font(.callout.weight(.medium))
                    Text(label(operation)).font(.caption).foregroundStyle(tint(operation.state)).lineLimit(2)
                    Spacer(minLength: 0)
                    Text(operation.operationID.prefix(8))
                        .font(.caption2.monospaced())
                        .foregroundStyle(.tertiary)
                        .help("Operation \(operation.operationID)")
                }
                .padding(.horizontal, Metrics.m)
                .padding(.vertical, Metrics.xs)
                .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
                .accessibilityElement(children: .combine)
            }
        }
    }

    @ViewBuilder
    private func icon(_ state: ForkSkillOperationState) -> some View {
        switch state {
        case .accepted: ProgressView().controlSize(.small)
        case .unknown: Image(systemName: "questionmark.circle").foregroundStyle(.orange)
        case .failed: Image(systemName: "xmark.octagon").foregroundStyle(.red)
        case .completed: Image(systemName: "checkmark.circle").foregroundStyle(.green)
        }
    }

    private func label(_ operation: SkillOperationProjection) -> String {
        switch operation.state {
        case .accepted: "Running in a separate agent"
        case .unknown: "Status unknown — it may be running; check the agent tree before starting again"
        case .failed: operation.error ?? "Failed"
        case .completed: "Completed"
        }
    }

    private func tint(_ state: ForkSkillOperationState) -> Color {
        switch state {
        case .unknown: .orange
        case .failed: .red
        default: .secondary
        }
    }
}
