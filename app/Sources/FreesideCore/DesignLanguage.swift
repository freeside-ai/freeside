import CoreText
import SwiftUI

#if canImport(AppKit)
    import AppKit
#elseif canImport(UIKit)
    import UIKit
#endif

// The Freeside design language (plan §15) as the app's shared styling
// vocabulary: the two grounds (light arrives as Freeside, dark as
// Straylight), the three faces, and the semantic mapping every surface
// follows. Values mirror the handoff's tokens.css; the mode follows the
// system (or `-FreesideColorScheme`), never a manual toggle.

extension Color {
    /// One adaptive color from a day and a dusk hex value.
    fileprivate static func freeside(day: UInt32, dusk: UInt32) -> Color {
        #if canImport(AppKit)
            Color(
                nsColor: NSColor(name: nil) { appearance in
                    let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                    return NSColor(hex: isDark ? dusk : day)
                })
        #elseif canImport(UIKit)
            Color(
                uiColor: UIColor { traits in
                    UIColor(hex: traits.userInterfaceStyle == .dark ? dusk : day)
                })
        #endif
    }

    static let ground = freeside(day: 0xEDE7D6, dusk: 0x16120E)
    static let ground2 = freeside(day: 0xF3EEE1, dusk: 0x1E1812)
    static let ground3 = freeside(day: 0xE4DDC7, dusk: 0x292117)
    /// Sidebars and secondary panes: ground-3 by day, ground-2 by dusk.
    static let sidebarGround = freeside(day: 0xE4DDC7, dusk: 0x1E1812)
    static let rule = freeside(day: 0xD6CDB2, dusk: 0x322A1E)
    static let ink = freeside(day: 0x2B2416, dusk: 0xEAE3CF)
    static let inkDim = freeside(day: 0x6E6450, dusk: 0xB3A88E)
    static let inkFaint = freeside(day: 0x94896E, dusk: 0x7D7460)
    /// Bronze by day, tawny by dusk: attention, never success.
    static let accent = freeside(day: 0x8F6B14, dusk: 0xC2912E)
    static let accentDim = freeside(day: 0xB99A4A, dusk: 0x8A6A26)
    /// Failure, revocation, loss.
    static let wax = freeside(day: 0x8A2D1C, dusk: 0x9A3520)
    /// In progress and informational-live.
    static let water = freeside(day: 0x6F9EA3, dusk: 0x5D8489)

    // Tinted washes for banners and the hold card.
    static let accentWash = freeside(day: 0xE9DFC2, dusk: 0x26200F)
    static let accentWashSoft = freeside(day: 0xECE4CD, dusk: 0x221C11)
    static let waxWash = freeside(day: 0xE8D5C9, dusk: 0x241310)
    static let neutralWash = freeside(day: 0xE8E2CD, dusk: 0x221C11)
    static let waterWash = freeside(day: 0xDCE6E4, dusk: 0x17201F)
    static let milestonePrior = freeside(day: 0xB9AF92, dusk: 0x4A3F2C)
    static let milestoneConnector = freeside(day: 0xDDD4B9, dusk: 0x292117)
}

#if canImport(AppKit)
    extension NSColor {
        fileprivate convenience init(hex: UInt32) {
            self.init(
                srgbRed: CGFloat((hex >> 16) & 0xFF) / 255,
                green: CGFloat((hex >> 8) & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255,
                alpha: 1)
        }
    }
#elseif canImport(UIKit)
    extension UIColor {
        fileprivate convenience init(hex: UInt32) {
            self.init(
                red: CGFloat((hex >> 16) & 0xFF) / 255,
                green: CGFloat((hex >> 8) & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255,
                alpha: 1)
        }
    }
#endif

/// The three faces, bundled in `Fonts/` and registered once per process.
/// Serif carries screen and item titles only; Plex Sans is the chrome;
/// Plex Mono is the evidence register for every stated fact.
enum FreesideFont {
    /// Registers the bundled faces with CoreText. Idempotent: a repeat
    /// registration of the same file reports an error CoreText already
    /// tolerates, so the result is deliberately not asserted on.
    static let registration: Void = {
        let urls = Bundle.module.urls(forResourcesWithExtension: "ttf", subdirectory: "Fonts") ?? []
        for url in urls {
            CTFontManagerRegisterFontsForURL(url as CFURL, .process, nil)
        }
    }()

    /// The platform's own point size for a text style, so the faces sit
    /// at the size the system would give `.body`, `.caption`, and so on
    /// on each platform (17pt body on iOS, 13pt on macOS). On iOS the
    /// size is read at the default content size, since `relativeTo:`
    /// applies the user's Dynamic Type scaling afterwards.
    static func size(of style: Font.TextStyle) -> CGFloat {
        #if canImport(AppKit)
            NSFont.preferredFont(forTextStyle: platformStyle(style)).pointSize
        #elseif canImport(UIKit)
            UIFont.preferredFont(
                forTextStyle: platformStyle(style),
                compatibleWith: UITraitCollection(preferredContentSizeCategory: .large)
            ).pointSize
        #endif
    }

    #if canImport(AppKit)
        private static func platformStyle(_ style: Font.TextStyle) -> NSFont.TextStyle {
            switch style {
            case .largeTitle: .largeTitle
            case .title: .title1
            case .title2: .title2
            case .title3: .title3
            case .headline: .headline
            case .subheadline: .subheadline
            case .body: .body
            case .callout: .callout
            case .footnote: .footnote
            case .caption: .caption1
            case .caption2: .caption2
            case .extraLargeTitle, .extraLargeTitle2: .largeTitle
            @unknown default: .body
            }
        }
    #elseif canImport(UIKit)
        private static func platformStyle(_ style: Font.TextStyle) -> UIFont.TextStyle {
            switch style {
            case .largeTitle: .largeTitle
            case .title: .title1
            case .title2: .title2
            case .title3: .title3
            case .headline: .headline
            case .subheadline: .subheadline
            case .body: .body
            case .callout: .callout
            case .footnote: .footnote
            case .caption: .caption1
            case .caption2: .caption2
            case .extraLargeTitle, .extraLargeTitle2: .largeTitle
            @unknown default: .body
            }
        }
    #endif

    static func serif(_ style: Font.TextStyle, scale: CGFloat = 1) -> Font {
        .custom("FreesideSerif-Medium", size: size(of: style) * scale, relativeTo: style)
    }

    static func sans(_ style: Font.TextStyle, weight: Font.Weight = .regular) -> Font {
        let name =
            switch weight {
            case .semibold, .bold, .heavy, .black: "IBMPlexSans-SmBld"
            case .medium: "IBMPlexSans-Medm"
            default: "IBMPlexSans"
            }
        return .custom(name, size: size(of: style), relativeTo: style)
    }

    static func mono(_ style: Font.TextStyle, weight: Font.Weight = .regular) -> Font {
        let name =
            switch weight {
            case .semibold, .bold, .heavy, .black: "IBMPlexMono-SmBld"
            case .medium: "IBMPlexMono-Medm"
            default: "IBMPlexMono"
            }
        return .custom(name, size: size(of: style), relativeTo: style)
    }

    // The platform text styles, in the language's faces.
    static let title = serif(.title2)
    static let largeTitle = serif(.largeTitle)
    static let itemTitle = serif(.headline, scale: 1.1)
    static let sectionTitle = serif(.title3)
    static let body = sans(.body)
    static let callout = sans(.callout)
    static let subheadline = sans(.subheadline)
    static let caption = sans(.caption)
    static let monoCallout = mono(.callout)
    static let monoCaption = mono(.caption)
    /// Small-caps mono keyword used by banners and section headers.
    static let keyword = mono(.caption2, weight: .medium)
    /// Medium, not regular: at chip size on the day ground the water and
    /// faint tones need the weight to stay legible.
    static let chip = mono(.caption2, weight: .medium)
}

/// A bordered state chip: mono, lowercase, 1px border and text in the
/// state color, no fill. `dashed` marks the not-observed idle state.
struct StateChip: View {
    let label: String
    let color: Color
    var dashed = false
    /// A leading state glyph (a tick, a live dot); VoiceOver reads the
    /// label alone.
    var glyph: String? = nil

    var body: some View {
        Text((glyph.map { "\($0) " } ?? "") + label.lowercased())
            .font(FreesideFont.chip)
            .tracking(0.6)
            .foregroundStyle(color)
            .padding(.horizontal, 5)
            .padding(.vertical, 1.5)
            .overlay(
                RoundedRectangle(cornerRadius: 3)
                    .strokeBorder(color, style: StrokeStyle(lineWidth: 1, dash: dashed ? [2, 2] : []))
            )
            .accessibilityLabel(label)
    }
}

/// A small-caps mono keyword: the banner's leading state word and the
/// card section header.
struct KeywordLabel: View {
    let text: String
    var color: Color = .inkFaint

    var body: some View {
        Text(text)
            .textCase(.uppercase)
            .font(FreesideFont.keyword)
            .tracking(0.8)
            .foregroundStyle(color)
    }
}

/// The full-width bordered action button: border and text in the tone's
/// color, 6pt radius, no fill; disabled drops to 45% opacity.
struct FreesideActionButtonStyle: ButtonStyle {
    enum Tone {
        case primary
        case neutral
        case destructive

        var color: Color {
            switch self {
            case .primary: .accent
            case .neutral: .inkDim
            case .destructive: .wax
            }
        }
    }

    let tone: Tone
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(FreesideFont.sans(.body, weight: .medium))
            .foregroundStyle(tone.color)
            .padding(.horizontal, 12)
            .padding(.vertical, 7)
            .frame(maxWidth: .infinity)
            .background(
                RoundedRectangle(cornerRadius: 6)
                    .fill(configuration.isPressed ? Color.ground3 : Color.ground2)
            )
            .overlay(
                RoundedRectangle(cornerRadius: 6)
                    .strokeBorder(tone == .neutral ? Color.rule : tone.color, lineWidth: 1)
            )
            .opacity(isEnabled ? 1 : 0.45)
            .contentShape(RoundedRectangle(cornerRadius: 6))
    }
}

extension View {
    /// A card: ground-2 on ground, 1px rule border, 8pt radius.
    func freesideCard(border: Color = .rule, dashed: Bool = false, cornerRadius: CGFloat = 8) -> some View {
        background(RoundedRectangle(cornerRadius: cornerRadius).fill(Color.ground2))
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius)
                    .strokeBorder(border, style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : []))
            )
    }
}

/// Screen titles in the serif where the platform lets a `navigationTitle`
/// be styled: the iOS navigation bar. The macOS window title stays system
/// chrome.
enum FreesideNavigationChrome {
    @MainActor
    static func apply() {
        #if canImport(UIKit)
            let appearance = UINavigationBar.appearance()
            if let large = UIFont(name: "FreesideSerif-Medium", size: 30) {
                appearance.largeTitleTextAttributes = [
                    .font: UIFontMetrics(forTextStyle: .largeTitle).scaledFont(for: large)
                ]
            }
            if let inline = UIFont(name: "FreesideSerif-Medium", size: 18) {
                appearance.titleTextAttributes = [
                    .font: UIFontMetrics(forTextStyle: .headline).scaledFont(for: inline)
                ]
            }
        #endif
    }
}
