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
// system (or the screenshot launch inputs), never a manual toggle.

struct FreesideColorCuts: Sendable {
    let day: UInt32
    let dusk: UInt32
    let dayIC: UInt32
    let duskIC: UInt32

    init(day: UInt32, dusk: UInt32, dayIC: UInt32? = nil, duskIC: UInt32? = nil) {
        self.day = day
        self.dusk = dusk
        self.dayIC = dayIC ?? day
        self.duskIC = duskIC ?? dusk
    }
}

enum FreesidePalette {
    static let ground = FreesideColorCuts(day: 0xEDE7D6, dusk: 0x16120E)
    static let ground2 = FreesideColorCuts(day: 0xF3EEE1, dusk: 0x1E1812)
    static let ground3 = FreesideColorCuts(day: 0xE4DDC7, dusk: 0x292117)
    static let sidebarGround = FreesideColorCuts(day: 0xE4DDC7, dusk: 0x1E1812)
    static let ruleStrong = FreesideColorCuts(day: 0x877D5C, dusk: 0x786D58)
    static let rule = FreesideColorCuts(
        day: 0xD6CDB2, dusk: 0x322A1E,
        dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
    // A secondary control's outline: quiet enough that a filled primary
    // still leads, and promoted to ruleStrong under Increased Contrast the
    // way every other structural hairline is.
    static let secondaryBorder = FreesideColorCuts(
        day: 0xC9BFA2, dusk: 0x3D3426,
        dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
    static let ink = FreesideColorCuts(day: 0x2B2416, dusk: 0xEAE3CF)
    static let inkDim = FreesideColorCuts(day: 0x675D49, dusk: 0xB3A88E)
    // Disabled and validating text. Darker than the handoff's #94896E,
    // which is 2.99:1 on ground-2 by day and misses the project's 3:1
    // floor for disabled text; Increased Contrast promotes it to inkDim.
    static let inkFaint = FreesideColorCuts(
        day: 0x827858, dusk: 0x7D7460,
        dayIC: inkDim.day, duskIC: inkDim.dusk)
    static let accentText = FreesideColorCuts(
        day: 0x7D5C0E, dusk: 0xC2912E, dayIC: 0x6B4E0B, duskIC: 0xE0AE46)
    static let accentBorder = FreesideColorCuts(
        day: 0x8F6B14, dusk: 0x8A6A26, dayIC: 0x6B4E0B, duskIC: 0xE0AE46)
    static let accentWash = FreesideColorCuts(day: 0xE9DFC2, dusk: 0x26200F)
    static let accentWashSoft = FreesideColorCuts(day: 0xECE4CD, dusk: 0x221C11)
    static let waxText = FreesideColorCuts(
        day: 0x8A2D1C, dusk: 0xD26D4A, dayIC: 0x71230F, duskIC: 0xDC7A57)
    static let waxWash = FreesideColorCuts(day: 0xE8D5C9, dusk: 0x241310)
    static let neutralWash = FreesideColorCuts(day: 0xE8E2CD, dusk: 0x221C11)
    static let waterText = FreesideColorCuts(
        day: 0x3E6D72, dusk: 0x7FAAAF, dayIC: 0x33595E, duskIC: 0x9CC3C7)
    static let waterWash = FreesideColorCuts(day: 0xDCE6E4, dusk: 0x17201F)

    // Stage-rail decoration pairs with labels and does not carry meaning alone.
    static let milestonePrior = FreesideColorCuts(day: 0xB9AF92, dusk: 0x4A3F2C)
    static let milestoneConnector = FreesideColorCuts(day: 0xDDD4B9, dusk: 0x292117)
}

extension Color {
    /// One adaptive color from a day and a dusk hex value.
    fileprivate static func freeside(day: UInt32, dusk: UInt32) -> Color {
        freeside(day: day, dusk: dusk, dayIC: day, duskIC: dusk)
    }

    /// One adaptive color with explicit Increased Contrast cuts.
    fileprivate static func freeside(
        day: UInt32, dusk: UInt32, dayIC: UInt32, duskIC: UInt32
    ) -> Color {
        #if canImport(AppKit)
            Color(
                nsColor: NSColor(name: nil) { appearance in
                    let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                    // Modern AppKit normalizes the high-contrast appearance
                    // names to base Aqua. The workspace setting is the live
                    // contrast trait; the launch argument pins it for captures.
                    let isIncreasedContrast =
                        LaunchInputs.accessibilityContrastOverride()
                        .map { $0 == .increased }
                        ?? NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast
                    switch (isDark, isIncreasedContrast) {
                    case (true, true): return NSColor(hex: duskIC)
                    case (true, false): return NSColor(hex: dusk)
                    case (false, true): return NSColor(hex: dayIC)
                    case (false, false): return NSColor(hex: day)
                    }
                })
        #elseif canImport(UIKit)
            Color(
                uiColor: UIColor { traits in
                    switch (traits.userInterfaceStyle, traits.accessibilityContrast) {
                    case (.dark, .high): UIColor(hex: duskIC)
                    case (.dark, _): UIColor(hex: dusk)
                    case (_, .high): UIColor(hex: dayIC)
                    default: UIColor(hex: day)
                    }
                })
        #endif
    }

    fileprivate static func freeside(_ cuts: FreesideColorCuts) -> Color {
        freeside(day: cuts.day, dusk: cuts.dusk, dayIC: cuts.dayIC, duskIC: cuts.duskIC)
    }

    static let ground = freeside(FreesidePalette.ground)
    static let ground2 = freeside(FreesidePalette.ground2)
    static let ground3 = freeside(FreesidePalette.ground3)
    /// Sidebars and secondary panes: ground-3 by day, ground-2 by dusk.
    static let sidebarGround = freeside(FreesidePalette.sidebarGround)
    static let rule = freeside(FreesidePalette.rule)
    static let ruleStrong = freeside(FreesidePalette.ruleStrong)
    /// The outline of a secondary control: present, never competing.
    static let secondaryBorder = freeside(FreesidePalette.secondaryBorder)
    static let ink = freeside(FreesidePalette.ink)
    static let inkDim = freeside(FreesidePalette.inkDim)
    /// Disabled and validating text: readable, plainly not actionable.
    static let inkFaint = freeside(FreesidePalette.inkFaint)
    /// Bronze by day, tawny by dusk: attention, never success.
    static let accentText = freeside(FreesidePalette.accentText)
    static let accentBorder = freeside(FreesidePalette.accentBorder)
    /// Failure, revocation, loss.
    static let waxText = freeside(FreesidePalette.waxText)
    /// In progress and informational-live.
    static let waterText = freeside(FreesidePalette.waterText)

    // Tinted washes for banners and the hold card.
    static let accentWash = freeside(FreesidePalette.accentWash)
    static let accentWashSoft = freeside(FreesidePalette.accentWashSoft)
    static let waxWash = freeside(FreesidePalette.waxWash)
    static let neutralWash = freeside(FreesidePalette.neutralWash)
    static let waterWash = freeside(FreesidePalette.waterWash)
    static let milestonePrior = freeside(FreesidePalette.milestonePrior)
    static let milestoneConnector = freeside(FreesidePalette.milestoneConnector)
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
    /// A screenshot-only bridge for exercising iOS Dynamic Type metrics in
    /// macOS ImageRenderer. Production never sets this task-local value and
    /// continues to use the platform's native text sizing below.
    @TaskLocal static var screenshotDynamicTypeSize: DynamicTypeSize?

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
            if let screenshotDynamicTypeSize {
                return iOSPointSize(of: style, at: screenshotDynamicTypeSize)
            }
            return NSFont.preferredFont(forTextStyle: platformStyle(style)).pointSize
        #elseif canImport(UIKit)
            UIFont.preferredFont(
                forTextStyle: platformStyle(style),
                compatibleWith: UITraitCollection(preferredContentSizeCategory: .large)
            ).pointSize
        #endif
    }

    #if canImport(AppKit)
        /// Apple HIG iOS/iPadOS Dynamic Type point sizes for the six
        /// categories exercised by the screenshot regression matrix.
        private static func iOSPointSize(
            of style: Font.TextStyle,
            at dynamicTypeSize: DynamicTypeSize
        ) -> CGFloat {
            let sizes:
                (
                    xSmall: CGFloat, large: CGFloat, xxxLarge: CGFloat,
                    accessibility1: CGFloat, accessibility3: CGFloat,
                    accessibility5: CGFloat
                ) =
                    switch style {
                    case .largeTitle, .extraLargeTitle, .extraLargeTitle2:
                        (31, 34, 40, 44, 52, 60)
                    case .title:
                        (25, 28, 34, 38, 48, 58)
                    case .title2:
                        (19, 22, 28, 30, 38, 46)
                    case .title3:
                        (17, 20, 26, 28, 34, 40)
                    case .headline, .body:
                        (14, 17, 23, 25, 31, 37)
                    case .callout:
                        (13, 16, 22, 24, 30, 36)
                    case .subheadline:
                        (12, 15, 21, 23, 28, 34)
                    case .footnote:
                        (12, 13, 19, 21, 25, 29)
                    case .caption:
                        (11, 12, 18, 20, 24, 28)
                    case .caption2:
                        (11, 11, 17, 18, 22, 26)
                    @unknown default:
                        (14, 17, 23, 25, 31, 37)
                    }

            return switch dynamicTypeSize {
            case .xSmall: sizes.xSmall
            case .large: sizes.large
            case .xxxLarge: sizes.xxxLarge
            case .accessibility1: sizes.accessibility1
            case .accessibility3: sizes.accessibility3
            case .accessibility5: sizes.accessibility5
            default:
                preconditionFailure("Unsupported screenshot Dynamic Type size")
            }
        }

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

    /// The macOS eyebrow's base size (`--fs-eyebrow-size`). macOS's own
    /// `.caption2` is 10pt, which leaves the tracked all-caps register too
    /// faint to read as a heading; iOS's 11pt already reads, so the lift is
    /// macOS-only.
    static let macOSEyebrowSize: CGFloat = 10.5

    /// The eyebrow's point size: the macOS lift in production, and the
    /// platform's `.caption2` everywhere else, including inside the
    /// screenshot bridge, which renders iOS metrics on purpose.
    static func eyebrowPointSize() -> CGFloat {
        #if canImport(AppKit)
            if screenshotDynamicTypeSize == nil {
                return macOSEyebrowSize
            }
        #endif
        return size(of: .caption2)
    }

    // The platform text styles, in the language's faces.
    static var title: Font { serif(.title2) }
    static var largeTitle: Font { serif(.largeTitle) }
    static var itemTitle: Font { serif(.headline, scale: 1.1) }
    static var sectionTitle: Font { serif(.title3) }
    static var body: Font { sans(.body) }
    static var callout: Font { sans(.callout) }
    static var subheadline: Font { sans(.subheadline) }
    static var caption: Font { sans(.caption) }
    static var monoCallout: Font { mono(.callout) }
    static var monoCaption: Font { mono(.caption) }
    /// Small-caps mono keyword used by banners and section headers: the
    /// medium mono face `mono(.caption2, weight: .medium)` builds, at the
    /// eyebrow's own base size.
    static var keyword: Font {
        .custom("IBMPlexMono-Medm", size: eyebrowPointSize(), relativeTo: .caption2)
    }
    /// Medium, not regular: the compact all-caps register stays readable
    /// without asking semantic color to compensate for a light face.
    static var chip: Font { mono(.caption2, weight: .medium) }
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
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if dynamicTypeSize >= .accessibility1 {
                Text((glyph.map { "\($0) " } ?? "") + label)
                    .font(FreesideFont.callout)
            } else {
                Text((glyph.map { "\($0) " } ?? "") + label.lowercased())
                    .font(FreesideFont.chip)
                    .tracking(0.6)
                    .lineLimit(1)
                    .fixedSize()
                    .padding(.horizontal, 5)
                    .padding(.vertical, 1.5)
                    .overlay(
                        RoundedRectangle(cornerRadius: 3)
                            .strokeBorder(
                                color,
                                style: StrokeStyle(
                                    lineWidth: 1,
                                    dash: dashed ? [2, 2] : []))
                    )
            }
        }
        .foregroundStyle(color)
        .accessibilityLabel(label)
    }
}

/// A small-caps mono keyword: the banner's leading state word and the
/// card section header.
struct KeywordLabel: View {
    let text: String
    var color: Color = .inkDim

    var body: some View {
        Text(text)
            .textCase(.uppercase)
            .font(FreesideFont.keyword)
            .tracking(0.8)
            .foregroundStyle(color)
    }
}

/// The four control states of the design language (plan §15) in one
/// recipe: a filled primary, an outlined secondary, an unadorned tertiary,
/// and a disabled state drawn as its own rule-bordered, faint-text shape.
/// Disabled is never the enabled look faded out, because opacity dims the
/// border and the label together and leaves neither reliably legible.
struct FreesideActionButtonStyle: ButtonStyle {
    enum Tone {
        /// The one recommended action: filled, and at most one per region.
        case primary
        /// An equally available alternative: outlined on ground-2.
        case secondary
        /// A way out or a way deeper, subordinate to both: text only.
        case tertiary
        /// Destructive in content: the wax outline, never a filled control.
        case destructive
    }

    /// The corner the control is cut with: cards use the 6pt radius the
    /// rest of the card chrome uses, sheet submits use the spec's pill.
    enum Corners {
        case rounded
        case pill
    }

    let tone: Tone
    var corners: Corners = .rounded
    /// Whether the control fills its row. A tertiary button always hugs its
    /// label: a full-width control with no fill and no border reads as a
    /// row of dead space rather than as a button.
    var expands: Bool = true
    @Environment(\.isEnabled) private var isEnabled
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(FreesideFont.sans(.body, weight: .medium))
            .foregroundStyle(labelColor)
            .lineLimit(2)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.horizontal, 12)
            .padding(.vertical, dynamicTypeSize >= .accessibility1 ? 12 : 7)
            .frame(minHeight: dynamicTypeSize >= .accessibility1 ? 52 : 44)
            .frame(maxWidth: hugsLabel ? nil : .infinity)
            .background(shape.fill(fillColor(isPressed: configuration.isPressed)))
            .overlay(shape.strokeBorder(borderColor, lineWidth: 1))
            .contentShape(shape)
    }

    private var hugsLabel: Bool {
        tone == .tertiary || !expands
    }

    /// One shape for fill, border, and hit target. A pill is the spec's
    /// 999pt radius rather than a `Capsule`, so both corner styles are the
    /// same concrete type and the border still strokes inside its bounds.
    private var shape: RoundedRectangle {
        switch corners {
        case .rounded: RoundedRectangle(cornerRadius: 6)
        case .pill: RoundedRectangle(cornerRadius: 999)
        }
    }

    /// Disabled resolves before tone: every disabled label takes the same
    /// faint cut, so "not available now" is one shape to learn rather than
    /// four.
    private var labelColor: Color {
        guard isEnabled else { return .inkFaint }
        switch tone {
        case .primary: return .ground2
        case .secondary: return .ink
        case .tertiary: return .inkDim
        case .destructive: return .waxText
        }
    }

    /// The one place a disabled control does not read uniformly: a tertiary
    /// is text-only when enabled, so drawing a border once it goes
    /// unavailable would have it gain chrome as it loses function. Its faint
    /// label carries the state instead (issue #1105, ledger line 08).
    private var borderColor: Color {
        guard isEnabled else { return tone == .tertiary ? .clear : .rule }
        switch tone {
        case .primary: return .accentBorder
        case .secondary: return .secondaryBorder
        case .tertiary: return .clear
        case .destructive: return .waxText
        }
    }

    private func fillColor(isPressed: Bool) -> Color {
        guard isEnabled else { return .clear }
        switch tone {
        case .primary: return .accentText
        case .tertiary: return .clear
        case .secondary, .destructive: return isPressed ? .ground3 : .ground2
        }
    }
}

/// The submit row a sheet ends with: Cancel as a tertiary text button and
/// the submit as a primary pill, both hugging their labels. Three sheets
/// draw it, and it carries the Return and Escape bindings the system
/// toolbar placements used to supply.
struct FreesideSheetActionRow: View {
    let submitLabel: String
    var isSubmitEnabled: Bool = true
    let submit: () -> Void
    let cancel: () -> Void
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        VStack(spacing: 0) {
            Divider()
            content
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
        }
    }

    @ViewBuilder private var content: some View {
        // Side by side the two labels cannot both hug their text at an
        // accessibility size without wrapping mid-word, so they stack and
        // the submit takes the full width, keeping the pill a pill.
        if dynamicTypeSize.isAccessibilitySize {
            VStack(spacing: 12) {
                submitButton(expands: true)
                cancelButton.frame(maxWidth: .infinity)
            }
        } else {
            HStack(spacing: 12) {
                cancelButton
                Spacer(minLength: 12)
                submitButton(expands: false)
            }
        }
    }

    private var cancelButton: some View {
        Button("Cancel", action: cancel)
            .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
            .keyboardShortcut(.cancelAction)
    }

    private func submitButton(expands: Bool) -> some View {
        Button(submitLabel, action: submit)
            .buttonStyle(
                FreesideActionButtonStyle(tone: .primary, corners: .pill, expands: expands)
            )
            .keyboardShortcut(.defaultAction)
            .disabled(!isSubmitEnabled)
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
