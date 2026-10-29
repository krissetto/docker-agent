import SwiftUI
import AgentKit

struct TeamWidgetSummarySync: View {
    let model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var refreshTask: Task<Void, Never>?

    var body: some View {
        Color.clear
            .frame(width: 0, height: 0)
            .onChange(of: scenePhase, initial: true) { _, phase in
                refreshTask?.cancel()
                guard phase == .active else { return }
                refreshTask = Task { @MainActor in
                    while !Task.isCancelled {
                        publishLatest()
                        try? await Task.sleep(for: .seconds(300))
                    }
                }
            }
            .onDisappear { refreshTask?.cancel() }
    }

    private func publishLatest() {
        TeamWidgetPublisher.publish(TeamWidgetPublisher.summary(sessions: Array(model.openSessions.values)))
    }
}
