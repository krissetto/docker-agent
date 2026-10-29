import SwiftUI
import Foundation
import AgentKit

enum Metrics {
    static let xxs: CGFloat = 2
    static let xs: CGFloat = 4
    static let s: CGFloat = 8
    static let m: CGFloat = 12
    static let l: CGFloat = 16
    static let xl: CGFloat = 20
    static let xxl: CGFloat = 32
    static let readingWidth: CGFloat = 720
    /// Radii encode hierarchy: chips < cards < interaction cards that ask for a decision.
    static let chipRadius: CGFloat = 8
    static let cardRadius: CGFloat = 12
    static let promptRadius: CGFloat = 16
    /// Agent ownership bar at the leading edge of rows and transcript groups.
    static let gutterWidth: CGFloat = 3
    static let conversationColumn: CGFloat = 380
    static let navigatorColumn: CGFloat = 280
    /// Board tags on the home screen: wider corners than a chip, tighter than a decision card.
    static let boardRadius: CGFloat = 12
    static let boardSpacing: CGFloat = 6
    /// Minimum hit target for icon-only controls.
    static let target: CGFloat = 44
    /// Your messages hug their length and never span the whole column, so they read as replies.
    static let userTurnMaxWidth: CGFloat = 560
}

/// Motion answers the person or marks live work; with Reduce Motion on, changes land without animation.
enum Motion {
    static func reveal(_ reduceMotion: Bool) -> Animation? { reduceMotion ? nil : .snappy }
    static func crossfade(_ reduceMotion: Bool) -> Animation? { reduceMotion ? nil : .easeInOut(duration: 0.2) }
}

/// The app's two owned colors. Everything else is a semantic system color so appearance stays automatic.
enum Palette {
    /// Harbor: the app tint. Used for you (your messages, Send), running state and focus.
    static let harbor = Color(red: 0x1D / 255, green: 0x63 / 255, blue: 0xED / 255)
    /// Signal: attention only. Something is waiting on a person. For dots, spines and glyphs.
    static let signal = Color.orange
    /// Signal as *text*: system orange is ~2.2:1 on light paper, so words use a deeper amber there and
    /// system orange in dark mode (≥ 9:1 on black). Keep `signal` for marks so the hue reads the same.
    static let signalText = Color(light: signalTextLight, dark: signalTextDark)
    /// sRGB components, exposed so tests can assert contrast against paper.
    static let signalTextLight = RGB(0xC2 / 255, 0x41 / 255, 0x0C / 255)
    static let signalTextDark = RGB(0xFF / 255, 0x9F / 255, 0x0A / 255)
    static let paperLight = RGB(1, 1, 1)
    static let paperDark = RGB(0, 0, 0)

    struct RGB: Equatable {
        let r: Double, g: Double, b: Double
        init(_ r: Double, _ g: Double, _ b: Double) { self.r = r; self.g = g; self.b = b }
        var color: Color { Color(red: r, green: g, blue: b) }
        /// WCAG relative luminance.
        var luminance: Double {
            func lin(_ c: Double) -> Double { c <= 0.03928 ? c / 12.92 : pow((c + 0.055) / 1.055, 2.4) }
            return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)
        }
        /// WCAG contrast ratio against another color.
        func contrast(with other: RGB) -> Double {
            let a = luminance, b = other.luminance
            return (max(a, b) + 0.05) / (min(a, b) + 0.05)
        }
    }
}

extension Color {
    /// A color that follows the appearance; the only non-system colors in the app are built this way.
    init(light: Palette.RGB, dark: Palette.RGB) {
        #if canImport(UIKit)
        self.init(uiColor: UIColor { trait in
            let c = trait.userInterfaceStyle == .dark ? dark : light
            return UIColor(red: c.r, green: c.g, blue: c.b, alpha: 1)
        })
        #else
        self.init(nsColor: NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
            let c = isDark ? dark : light
            return NSColor(srgbRed: c.r, green: c.g, blue: c.b, alpha: 1)
        })
        #endif
    }
}

enum AgentPalette {
    // Red and green are reserved for status, so agents never use them.
    private static let hues: [Color] = [.blue, .purple, .pink, .orange, .teal, .indigo, .mint, .brown]

    static func color(for name: String) -> Color {
        guard !name.isEmpty else { return .secondary }
        // FNV-1a: String.hashValue is seeded per launch and would reshuffle colors.
        var hash: UInt64 = 0xcbf29ce484222325
        for byte in name.utf8 {
            hash ^= UInt64(byte)
            hash &*= 0x100000001b3
        }
        return hues[Int(hash % UInt64(hues.count))]
    }
}

extension Font {
    static let mono = Font.system(.callout, design: .monospaced)
    static let monoSmall = Font.system(.caption, design: .monospaced)
    static let rowTitle = Font.body.weight(.medium)
    static let rowDetail = Font.footnote
    static let meta = Font.caption
    /// Board tags carry the live work; their titles sit a step above log rows.
    static let boardTitle = Font.headline
    /// Operational numbers (cost, tokens, ages) read as data, never as prose.
    static let figure = Font.footnote.monospacedDigit()
    static let figureSmall = Font.caption.monospacedDigit()
}

extension SessionState {
    /// Quiet opacity for spines of sessions that aren't doing anything right now.
    var spineOpacity: Double { isActive ? 1 : 0.4 }
}

extension View {
    func cardBackground(_ style: some ShapeStyle = .background.secondary) -> some View {
        background(style, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
    }

    /// For rows in the session's bottom stack: transcript content scrolls beneath them, so a translucent fill
    /// would collide with text. Glass keeps them legible and matches the composer.
    func floatingChip(radius: CGFloat = Metrics.chipRadius) -> some View {
        glassEffect(.regular, in: .rect(cornerRadius: radius, style: .continuous))
    }

    /// Ownership bar in the agent's hue; `nil` keeps the layout but draws nothing.
    /// `attention` switches the hue to Signal: someone is waiting on a person, whoever owns the row.
    func agentGutter(_ name: String?, inset: CGFloat = Metrics.xxs, attention: Bool = false, opacity: Double = 1) -> some View {
        padding(.leading, Metrics.gutterWidth + Metrics.s)
            .overlay(alignment: .leading) {
                if let name {
                    RoundedRectangle(cornerRadius: Metrics.gutterWidth / 2, style: .continuous)
                        .fill(attention ? Palette.signal : AgentPalette.color(for: name))
                        .frame(width: Metrics.gutterWidth)
                        .padding(.vertical, inset)
                        .opacity(attention ? 1 : opacity)
                        .accessibilityHidden(true)
                }
            }
    }
}

enum Format {
    static func cost(_ value: Double) -> String {
        value.formatted(.currency(code: "USD").precision(.fractionLength(value < 1 ? 3 : 2)))
    }

    static func tokens(_ value: Int64) -> String {
        value.formatted(.number.notation(.compactName))
    }

    static func relative(_ date: Date?) -> String? {
        date.map { $0.formatted(.relative(presentation: .named, unitsStyle: .abbreviated)) }
    }

    /// Short clock-style age for dense rows: "now", "4m", "2h", "3d", else a day.
    static func age(_ date: Date?, now: Date = .now) -> String? {
        guard let date else { return nil }
        let seconds = max(0, now.timeIntervalSince(date))
        switch seconds {
        case ..<45: return "now"
        case ..<3_600: return "\(Int(seconds / 60))m"
        case ..<86_400: return "\(Int(seconds / 3_600))h"
        case ..<(7 * 86_400): return "\(Int(seconds / 86_400))d"
        default: return date.formatted(.dateTime.day().month(.abbreviated))
        }
    }
}

extension SessionProjection {
    /// The team key when it reads as a name (e.g. "hackerspace"), never a path or file; nil when it only
    /// repeats the agent name or isn't human-readable.
    var teamLabel: String? {
        guard let source = source?.trimmingCharacters(in: .whitespaces), !source.isEmpty,
              source != agentName, !source.contains("/"), !source.contains("\\"), !source.hasSuffix(".yaml"), !source.hasSuffix(".yml"), !source.hasSuffix(".json") else { return nil }
        return source
    }
}

/// Stable, non-visual identifiers so UI tests can verify *which* session a surface shows without reading titles.
/// Identifiers carry only the server UUID and session ID; never credentials, titles or content.
enum SessionAccessibility {
    static func identity(_ ref: SessionRef) -> String {
        "session.detail.\(ref.serverID.uuidString).\(ref.sessionID)"
    }
}

struct OpenSessionAction {
    var handler: @MainActor (SessionRef, String?) -> Void = { _, _ in }
    /// `prefill` seeds the destination composer once (e.g. the message a fork stopped before); it is never sent.
    @MainActor func callAsFunction(_ ref: SessionRef, prefill: String? = nil) { handler(ref, prefill) }
}

/// Per-window composer state: one-shot drafts handed to a session that is about to open,
/// and unsent drafts kept while the user navigates to another session (memory only).
@Observable @MainActor
final class ComposerDraftHandoff {
    struct Saved {
        let text: String
        let attachments: [SessionDocumentAttachment]
    }

    private var drafts: [SessionRef: String] = [:]
    private var saved: [SessionRef: Saved] = [:]

    func put(_ text: String, for ref: SessionRef) { drafts[ref] = text }

    func take(for ref: SessionRef) -> String? { drafts.removeValue(forKey: ref) }

    func save(text: String, attachments: [SessionDocumentAttachment], for ref: SessionRef) {
        if text.isEmpty && attachments.isEmpty { saved[ref] = nil } else { saved[ref] = Saved(text: text, attachments: attachments) }
    }

    func restore(for ref: SessionRef) -> Saved? { saved.removeValue(forKey: ref) }
}

struct CloseSessionAction {
    var handler: @MainActor (SessionRef) -> Void = { _ in }
    @MainActor func callAsFunction(_ ref: SessionRef) { handler(ref) }
}

extension EnvironmentValues {
    @Entry var openSession = OpenSessionAction()
    @Entry var composerHandoff: ComposerDraftHandoff? = nil
    /// Called after a session is deleted so its container can drop the selection.
    @Entry var sessionDeleted = CloseSessionAction()
}
