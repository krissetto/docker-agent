import SwiftUI
import AgentKit

extension SessionState {
    var label: String {
        switch self {
        case .idle: "Idle"
        case .queued: "Queued"
        case .cancelling: "Stopping"
        case .running: "Running"
        case .waiting: "Waiting for you"
        case .settled: "Done"
        case .failed: "Failed"
        case .unknown: "Unknown"
        }
    }

    var symbol: String {
        switch self {
        case .idle, .settled: "circle"
        case .queued: "tray.and.arrow.down"
        case .cancelling: "stop.circle"
        case .running: "circle.dotted.circle"
        case .waiting: "hand.raised.fill"
        case .failed: "exclamationmark.triangle.fill"
        case .unknown: "questionmark.circle"
        }
    }

    var tint: Color {
        switch self {
        case .running: .accentColor
        case .waiting: .orange
        case .failed: .red
        default: .secondary
        }
    }

    var isActive: Bool { self == .running || self == .waiting }
}

extension ConnectionState {
    var label: String {
        switch self {
        case .unknown: "Not checked"
        case .testing: "Checking…"
        case .live: "Live"
        case .reconnecting: "Reconnecting…"
        case .offline: "Offline"
        case .unauthorized: "Unauthorized"
        }
    }

    var tint: Color {
        switch self {
        case .live: .green
        case .reconnecting, .testing: .orange
        case .offline, .unauthorized: .red
        case .unknown: .secondary
        }
    }
}

struct StateIcon: View {
    let state: SessionState

    var body: some View {
        Image(systemName: state.symbol)
            .foregroundStyle(state.tint)
            .symbolEffect(.pulse, isActive: state == .running)
            .accessibilityLabel(state.label)
    }
}

struct HealthDot: View {
    let state: ConnectionState

    var body: some View {
        Circle()
            .fill(state.tint)
            .frame(width: 8, height: 8)
            .accessibilityLabel(state.label)
    }
}

struct ConnectionStatusView: View {
    let state: ConnectionState
    let refreshedAt: Date?

    var body: some View {
        HStack(spacing: Metrics.xs) {
            HealthDot(state: state)
            Text(state.label)
            if state != .live, let when = Format.relative(refreshedAt) {
                Text("· \(when)").foregroundStyle(.secondary)
            }
        }
        .font(.caption)
        .accessibilityElement(children: .combine)
    }
}
