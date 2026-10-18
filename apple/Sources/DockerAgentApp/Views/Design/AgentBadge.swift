import SwiftUI

struct AgentDot: View {
    let name: String
    var size: CGFloat = 8

    var body: some View {
        Circle()
            .fill(AgentPalette.color(for: name))
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }
}

struct AgentLabel: View {
    let name: String
    var font: Font = .subheadline.weight(.semibold)

    var body: some View {
        HStack(spacing: Metrics.s - 2) {
            AgentDot(name: name)
            Text(name.isEmpty ? "Agent" : name)
                .font(font)
                .lineLimit(1)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Agent \(name)")
    }
}
