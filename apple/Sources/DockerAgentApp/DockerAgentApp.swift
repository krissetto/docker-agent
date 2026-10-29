import SwiftUI
import AgentKit

@main struct DockerAgentApp: App {
    @State private var model: AppModel
    private let localSmoke: Bool

    init() {
        #if os(macOS)
        localSmoke = ProcessInfo.processInfo.arguments.contains("--ui-smoke-test")
        if localSmoke {
            model = try! AppModel.localSmokeModel()
        } else {
            model = AppModel()
        }
        #else
        localSmoke = false
        model = AppModel()
        #endif
    }

    var body: some Scene {
        WindowGroup {
            #if os(macOS)
            if localSmoke {
                LocalSmokeRootView(model: model)
            } else {
                RootView().environment(model).background(TeamWidgetSummarySync(model: model))
            }
            #else
            RootView().environment(model).background(TeamWidgetSummarySync(model: model))
            #endif
        }
        WindowGroup("Session", for: SessionRef.self) { $ref in
            if let ref { SessionWindowView(ref: ref).environment(model) }
        }
        #if os(macOS)
        Settings { ServerSettingsView().environment(model) }
        #endif
    }
}

private struct LocalSmokeRootView: View {
    let model: AppModel

    var body: some View {
        VStack(spacing: 0) {
            Label("Local smoke test — no server connected", systemImage: "checkmark.shield")
                .font(.callout.weight(.medium))
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 8)
                .background(.quaternary)
            RootView().environment(model)
        }
        .frame(minWidth: 900, minHeight: 620)
        .accessibilityLabel("Local smoke test. No server connected.")
    }
}
