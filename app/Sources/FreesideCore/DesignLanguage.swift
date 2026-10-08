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
    // The outline of quiet chrome (the faint chip, the segmented control,
    // the menu panel): promoted to ruleStrong under Increased Contrast the
    // way every other structural hairline is. An outlined action takes
    // ruleStrong itself (R6), since its outline is what makes it a button.
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
    // The selected segment of the sidebar's segmented control: ground-2 by
    // day, ground-3 by dusk, so it lifts off the ground container in both.
    static let segmentSelected = FreesideColorCuts(day: ground2.day, dusk: ground3.dusk)
    static let waterText = FreesideColorCuts(
        day: 0x3E6D72, dusk: 0x7FAAAF, dayIC: 0x33595E, duskIC: 0x9CC3C7)
    static let waterWash = FreesideColorCuts(day: 0xDCE6E4, dusk: 0x17201F)

    // Stage-rail decoration pairs with labels and does not carry meaning alone.
    static let milestonePrior = FreesideColorCuts(day: 0xB9AF92, dusk: 0x4A3F2C)
    static let milestoneConnector = FreesideColorCuts(day: 0xDDD4B9, dusk: 0x292117)

    // The refined-interfaces cuts (handoff of 2026-10-06). By day each one
    // repeats an existing cut under the name of its job. By dusk the token
    // mirror leaves a wash or a hairline invisible on ground-2, so the quote,
    // the bordered item, and the notices take a cut lifted one step.

    /// The agent's voice (R5): a soft wash behind a leading rule.
    static let quoteWash = FreesideColorCuts(day: accentWashSoft.day, dusk: ground3.dusk)
    static let quoteRule = FreesideColorCuts(day: ruleStrong.day, dusk: accentBorder.dusk)
    /// A bordered item's hairline. Structural, so Increased Contrast
    /// promotes it to ruleStrong as it does `rule`.
    static let itemBorder = FreesideColorCuts(
        day: rule.day, dusk: milestonePrior.dusk,
        dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
    static let noticeAccentWash = FreesideColorCuts(day: accentWash.day, dusk: 0x2C2412)
    static let noticeWaxWash = FreesideColorCuts(day: waxWash.day, dusk: 0x2E1812)
    // A hovered control (R19). The handoff lifts dusk to #2F261A, which
    // leaves a wax-outlined control's label at 4.27:1; ground-3 is the
    // lightest dusk fill that keeps it at the 4.5:1 text floor.
    static let hover = FreesideColorCuts(day: ground3.day, dusk: ground3.dusk)

    // Diff counts and diff lines only (R28). The day cuts are darker than
    // the handoff's #3B7A47 and #B0412F, which are 4.46:1 on ground-2 and
    // 4.08:1 and 4.27:1 on their own washes; these are the nearest that
    // clear the 4.5:1 text floor on both.
    static let diffAdd = FreesideColorCuts(day: 0x377142, dusk: 0x7FB38A)
    static let diffRemove = FreesideColorCuts(day: 0xA93E2D, dusk: 0xE07A62)
    static let diffAddWash = FreesideColorCuts(day: 0xDCE8D9, dusk: 0x16261A)
    static let diffRemoveWash = FreesideColorCuts(day: 0xF0D9D2, dusk: 0x2E1812)
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
    static let segmentSelected = freeside(FreesidePalette.segmentSelected)
    static let waterWash = freeside(FreesidePalette.waterWash)
    static let milestonePrior = freeside(FreesidePalette.milestonePrior)
    static let milestoneConnector = freeside(FreesidePalette.milestoneConnector)

    /// The quote: the wash and leading rule behind an agent's own words.
    static let quoteWash = freeside(FreesidePalette.quoteWash)
    static let quoteRule = freeside(FreesidePalette.quoteRule)
    /// The hairline around a bordered item inside a card.
    static let itemBorder = freeside(FreesidePalette.itemBorder)
    static let noticeAccentWash = freeside(FreesidePalette.noticeAccentWash)
    static let noticeWaxWash = freeside(FreesidePalette.noticeWaxWash)
    /// The fill of a hovered control.
    static let hover = freeside(FreesidePalette.hover)
    /// Additions and removals, in a diff count or a diff line and nowhere else.
    static let diffAdd = freeside(FreesidePalette.diffAdd)
    static let diffRemove = freeside(FreesidePalette.diffRemove)
    static let diffAddWash = freeside(FreesidePalette.diffAddWash)
    static let diffRemoveWash = freeside(FreesidePalette.diffRemoveWash)
}

#if canImport(AppKit)
    extension NSColor {
        convenience init(hex: UInt32) {
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

    /// A face the refined scale fixes at one point size on both platforms
    /// (R10, survey card 4b), scaled by Dynamic Type through `style`. The
    /// platform faces above follow each platform's own size for a text
    /// style, which sets a card 13pt on macOS and 17pt on iOS; the card
    /// scale is one set of sizes. `screenshotMetricBase` applies the iOS
    /// ratio inside the screenshot bridge, where `relativeTo:` scales
    /// nothing, so the accessibility digests still render enlarged.
    private static func fixed(
        _ name: String, _ size: CGFloat, relativeTo style: Font.TextStyle
    ) -> Font {
        .custom(name, size: screenshotMetricBase(size, relativeTo: style), relativeTo: style)
    }

    // The card scale. Mono states a fact; sans speaks to the operator; the
    // serif carries the ask and the statements worth reading as sentences.
    static let cardBodySize: CGFloat = 14
    static let keywordSize: CGFloat = 12.5
    static let chipSize: CGFloat = 12
    /// 0.08em at the keyword's size.
    static let keywordTracking: CGFloat = 1.0
    /// 0.04em at the chip's size.
    static let chipTracking: CGFloat = 0.48

    static var cardBody: Font { fixed("IBMPlexSans", cardBodySize, relativeTo: .body) }
    /// What a card or a sheet asks: the one large serif line.
    static var ask: Font { fixed("FreesideSerif-Medium", 25, relativeTo: .title2) }
    /// A statement at text size: the agent's summary inside its quote.
    static var statement: Font { fixed("FreesideSerif-Regular", 17, relativeTo: .body) }
    /// An agent's message in a thread: the statement face a point smaller,
    /// since a thread is read as running text, not as one summary.
    static var message: Font { fixed("FreesideSerif-Regular", 16, relativeTo: .body) }
    /// An option's label: the statement face at medium weight, so the
    /// thing chosen reads above the text that qualifies it.
    static var optionLabel: Font { fixed("FreesideSerif-Medium", 17, relativeTo: .body) }
    /// A fact's label and a disclosure's label.
    static var factLabel: Font { fixed("IBMPlexSans", 16, relativeTo: .callout) }
    /// A fact's value.
    static var monoValue: Font { fixed("IBMPlexMono", 14.5, relativeTo: .callout) }
    /// The dim summary trailing a disclosure's label.
    static var trailingSummary: Font { fixed("IBMPlexMono", 13.5, relativeTo: .footnote) }
    static var actionLabel: Font { fixed("IBMPlexSans-Medm", 15, relativeTo: .body) }
    /// A text action inside a notice: the card body size, medium.
    static var noticeAction: Font { fixed("IBMPlexSans-Medm", cardBodySize, relativeTo: .body) }
    /// The disclosure chevron, sized as a glyph beside the fact label.
    static var disclosureGlyph: Font { fixed("IBMPlexSans", 11, relativeTo: .callout) }

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
    /// The uppercase mono keyword that heads a section, a banner, and a
    /// card: a heading, so one size above the chip rather than a footnote.
    /// Drawn tracked by `keywordTracking`.
    static var keyword: Font { fixed("IBMPlexMono-Medm", keywordSize, relativeTo: .caption2) }
    /// Medium, not regular: the compact register stays readable without
    /// asking semantic color to compensate for a light face. Drawn tracked
    /// by `chipTracking`.
    static var chip: Font { fixed("IBMPlexMono-Medm", chipSize, relativeTo: .caption2) }
}

/// A bordered state chip: mono, the label in the case it is given (Title
/// Case, R4), 1px border and text in the state color, no fill. `dashed`
/// marks the not-observed idle state.
///
/// A `cut` chip is the task surfaces' status chip: the same shape, free to
/// wrap (a task status is the daemon's full phrase), with the border and
/// text toned separately.
struct StateChip: View {
    /// Which of the three task-status tones a chip takes. A caller picks
    /// the cut from what the operator can do, never from the status word.
    enum Cut: CaseIterable {
        /// A bound Inbox item waits on the operator.
        case attention
        /// The daemon is working or has spoken; nothing to do here.
        case ink
        /// Historical, never current readiness.
        case faint

        var border: Color {
            switch self {
            case .attention: .accentBorder
            case .ink: .ruleStrong
            case .faint: .secondaryBorder
            }
        }

        var text: Color {
            switch self {
            case .attention: .accentText
            case .ink: .ink
            case .faint: .inkDim
            }
        }
    }

    let label: String
    let color: Color
    var dashed = false
    /// A leading state glyph (a tick, a live dot); VoiceOver reads the
    /// label alone.
    var glyph: String? = nil
    var cut: Cut? = nil
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if dynamicTypeSize >= .accessibility1 {
                Text((glyph.map { "\($0) " } ?? "") + label)
                    .font(FreesideFont.callout)
            } else {
                Text((glyph.map { "\($0) " } ?? "") + label)
                    .font(FreesideFont.chip)
                    .tracking(FreesideFont.chipTracking)
                    .lineLimit(cut == nil ? 1 : nil)
                    .fixedSize(horizontal: cut == nil, vertical: true)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .overlay(
                        RoundedRectangle(cornerRadius: 3)
                            .strokeBorder(
                                cut?.border ?? color,
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

extension StateChip {
    /// A task-status chip in one of the three cuts.
    init(label: String, cut: Cut) {
        self.init(label: label, color: cut.text, cut: cut)
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
            .tracking(FreesideFont.keywordTracking)
            .foregroundStyle(color)
    }
}

/// The keyword label for agent prose the daemon has not checked: the label,
/// the visible "(unverified)" register, and an info button that explains the
/// register on demand (visual audit D03). The register stays in the label so
/// agent prose never reads as a fact; only the sentence explaining it moves
/// behind the button.
///
/// The button opens a popover and does nothing else, so it can sit beside a
/// decision without triggering one, and it is a real `Button` so the keyboard
/// and VoiceOver reach it without hover. `rendersInteractiveControls` false
/// draws the same glyph without the button, for a surface rendered offscreen.
struct UnverifiedLabel: View {
    static let explanation = "Written by the agent, not checked by the daemon."

    let text: String
    /// Whether this label draws the card's explanation control. A card has
    /// one, on its first unverified keyword in reading order (R25), so every
    /// other label is the keyword and its register alone.
    var carriesInfo = false
    var rendersInteractiveControls = true

    @State private var showsExplanation = false

    var body: some View {
        // The last baseline, so a label that wraps keeps the button after its
        // final word rather than beside its first line.
        HStack(alignment: .lastTextBaseline, spacing: 6) {
            KeywordLabel(text: "\(text) (unverified)")
            if carriesInfo {
                infoControl
            }
        }
    }

    @ViewBuilder private var infoControl: some View {
        if rendersInteractiveControls {
            Button {
                showsExplanation = true
            } label: {
                glyph
                    // A touch target larger than the glyph, without
                    // growing the label's line.
                    .padding(12)
                    .contentShape(Rectangle())
                    .padding(-12)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("About unverified content")
            .popover(isPresented: $showsExplanation) {
                Text(Self.explanation)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
                    .frame(maxWidth: 280, alignment: .leading)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(12)
                    // A phone keeps the popover anchored to the label
                    // rather than promoting one sentence to a sheet.
                    .presentationCompactAdaptation(.popover)
            }
        } else {
            glyph.accessibilityHidden(true)
        }
    }

    private var glyph: some View {
        Image(systemName: "info.circle")
            .font(FreesideFont.keyword)
            .foregroundStyle(Color.inkDim)
    }
}

/// The one disclosure (R2): an accent chevron, a sentence-case label in the
/// fact-label face, and an optional trailing mono summary (a count, an id,
/// the newest time), so a closed section still says what it holds. The
/// summary sits beside the label while both fit one line and stacks under it
/// when they do not (R22), never truncating either. The caller owns
/// `isExpanded`, which is how a surface persists the state.
struct SentenceDisclosure<Content: View>: View {
    let label: String
    var summary: String? = nil
    @Binding var isExpanded: Bool
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 11) {
            Button {
                isExpanded.toggle()
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Image(systemName: "arrowtriangle.right.fill")
                        .font(FreesideFont.disclosureGlyph)
                        .foregroundStyle(Color.accentText)
                        .rotationEffect(.degrees(isExpanded ? 90 : 0))
                        .accessibilityHidden(true)
                    labelText
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                // A touch target taller than the line, without spreading the
                // folds a card stacks 12pt apart.
                .padding(.vertical, 8)
                .contentShape(Rectangle())
                .padding(.vertical, -8)
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            .accessibilityValue(isExpanded ? "Expanded" : "Collapsed")
            if isExpanded {
                content()
            }
        }
    }

    @ViewBuilder private var labelText: some View {
        if let summary {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    title
                    summaryText(summary)
                }
                VStack(alignment: .leading, spacing: 3) {
                    title
                    summaryText(summary)
                }
            }
        } else {
            title
        }
    }

    private var title: some View {
        Text(label)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
            .multilineTextAlignment(.leading)
    }

    private func summaryText(_ summary: String) -> some View {
        Text(summary)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .multilineTextAlignment(.leading)
    }
}

/// The agent's voice (R5): its prose on the quote wash behind a 3pt rule, so
/// a summary, a claim, or a reason the agent wrote never reads as the
/// daemon's own statement. With a `producer` the block opens with that
/// keyword and the unverified register (R7); a caller that heads the block
/// with its own label passes none.
struct QuoteBlock<Content: View>: View {
    var producer: String? = nil
    /// Whether the producer label draws the card's one explanation control.
    var carriesInfo = false
    var rendersInteractiveControls = true
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if let producer {
                UnverifiedLabel(
                    text: producer, carriesInfo: carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls)
            }
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
        // The rule sits inside the leading padding's edge, so the wash and
        // the rule clip to one 6pt corner.
        .padding(.leading, 3)
        .background(alignment: .leading) { Color.quoteRule.frame(width: 3) }
        .background(Color.quoteWash)
        .clipShape(RoundedRectangle(cornerRadius: 6))
    }
}

/// The daemon's own statement set apart inside a card (R5): the accent wash
/// behind a 4pt accent bar, the same pairing a selected row takes (R19). It
/// is never drawn around agent prose, which takes `QuoteBlock`.
struct SystemCallout<Content: View>: View {
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .padding(.leading, 4)
        .background(alignment: .leading) { Color.accentBorder.frame(width: 4) }
        .background(Color.accentWash)
        .clipShape(RoundedRectangle(cornerRadius: 6))
    }
}

/// The one notice (R14): a full-width wash, a keyword in the tone's tint, a
/// dim sentence, and an optional trailing text action in the same tint. The
/// sentence sits beside the keyword while the line fits and stacks under it
/// when it does not. A notice never folds and never takes the accent bar.
struct Notice: View {
    enum Tone: CaseIterable {
        /// A record of something done: nothing to act on.
        case neutral
        /// Worth a look, and usually carries the action that resolves it.
        case accent
        /// A failure or a stop.
        case wax

        var wash: Color {
            switch self {
            case .neutral: .neutralWash
            case .accent: .noticeAccentWash
            case .wax: .noticeWaxWash
            }
        }

        var tint: Color {
            switch self {
            case .neutral: .inkDim
            case .accent: .accentText
            case .wax: .waxText
            }
        }
    }

    struct Action {
        let label: String
        let handler: () -> Void
    }

    let tone: Tone
    let keyword: String
    let sentence: String
    var action: Action? = nil

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                KeywordLabel(text: keyword, color: tone.tint)
                sentenceText
                Spacer(minLength: 0)
                actionButton
            }
            VStack(alignment: .leading, spacing: 4) {
                KeywordLabel(text: keyword, color: tone.tint)
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    sentenceText
                    Spacer(minLength: 0)
                    actionButton
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(RoundedRectangle(cornerRadius: 6).fill(tone.wash))
    }

    private var sentenceText: some View {
        Text(sentence)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .multilineTextAlignment(.leading)
    }

    @ViewBuilder private var actionButton: some View {
        if let action {
            Button(action.label, action: action.handler)
                .buttonStyle(.plain)
                .font(FreesideFont.noticeAction)
                .foregroundStyle(tone.tint)
                .fixedSize()
                .freesideFocusRing(cornerRadius: 4)
        }
    }
}

/// The type eyebrow every card opens with (R27): the type keyword on the
/// left and the card's state on the right. When the card's lead is
/// agent-authored the keyword carries the unverified register and the card's
/// one explanation control. The trailing slot drops to its own line at an
/// accessibility size (R22).
struct CardEyebrow<Trailing: View>: View {
    let keyword: String
    var carriesInfo = false
    var rendersInteractiveControls = true
    @ViewBuilder let trailing: () -> Trailing
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        if dynamicTypeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 8) {
                keywordLabel
                trailing()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            HStack(alignment: .center, spacing: 12) {
                keywordLabel
                Spacer(minLength: 0)
                trailing()
            }
        }
    }

    @ViewBuilder private var keywordLabel: some View {
        if carriesInfo {
            UnverifiedLabel(
                text: keyword, carriesInfo: true,
                rendersInteractiveControls: rendersInteractiveControls)
        } else {
            KeywordLabel(text: keyword)
                .accessibilityAddTraits(.isHeader)
        }
    }
}

extension CardEyebrow where Trailing == StateChip? {
    /// An eyebrow whose state is one chip, or none.
    init(
        keyword: String, chip: StateChip? = nil, carriesInfo: Bool = false,
        rendersInteractiveControls: Bool = true
    ) {
        self.init(
            keyword: keyword, carriesInfo: carriesInfo,
            rendersInteractiveControls: rendersInteractiveControls
        ) { chip }
    }
}

/// A compact mark (R21): `AGENT`, `PROPOSED`, `AGENT RECOMMENDS` trailing
/// the line it marks, in the accent. It never carries a glyph or the
/// explanation control (R25); the card's one control sits on a full label.
struct CompactMark: View {
    let text: String

    var body: some View {
        KeywordLabel(text: text, color: .accentText)
    }
}

/// The choice list (R29): where the operator picks one of several, the
/// options are the control. Each option leads with a binary mark (filled ink
/// for the pick, hollow for the rest) and draws in the register of whoever
/// wrote it; the pick is outlined in ink. Picking only moves the selection:
/// a list never submits, so the surface that composes it owns the submit.
///
/// The list is one focus stop, as a radio group is: Tab lands on it, the
/// arrows move the pick, and the ring sits on the picked option. VoiceOver
/// reads each option as a button with the selected trait, inside a group
/// named by `accessibilityLabel`.
struct ChoiceList<Value: Hashable>: View {
    /// Whose words an option is, which decides its shape.
    enum Register {
        /// An agent wrote it: the quote.
        case quote
        /// The daemon typed it: a bordered item.
        case item
    }

    struct Option: Identifiable {
        let value: Value
        let label: String
        /// What picking this option does, in dim under the label.
        var consequence: String? = nil
        /// A compact mark trailing the label (`PROPOSED` on a default an
        /// agent proposed).
        var mark: String? = nil
        let register: Register
        var id: Value { value }
    }

    /// What the list chooses ("What to do with the answer"): spoken, never
    /// drawn. A caller that wants a visible head draws its own keyword.
    let accessibilityLabel: String
    let options: [Option]
    @Binding var selection: Value

    @FocusState private var isFocused: Bool
    @State private var hoveredOption: Value?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(options) { optionButton($0) }
        }
        .focusable()
        .focused($isFocused)
        .focusEffectDisabled()
        .onKeyPress(.upArrow) { move(by: -1) }
        .onKeyPress(.downArrow) { move(by: 1) }
        .accessibilityElement(children: .contain)
        .accessibilityLabel(accessibilityLabel)
    }

    private func optionButton(_ option: Option) -> some View {
        let isSelected = option.value == selection
        return Button {
            selection = option.value
        } label: {
            VStack(alignment: .leading, spacing: 3) {
                optionLabel(option)
                if let consequence = option.consequence {
                    Text(consequence)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .multilineTextAlignment(.leading)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .buttonStyle(
            ChoiceOptionStyle(
                register: option.register,
                isSelected: isSelected,
                isHovered: hoveredOption == option.value,
                isFocused: isFocused && isSelected)
        )
        // Not a Tab stop of its own: the list is the single stop.
        .focusable(false)
        .onHover { hovering in
            if hovering {
                hoveredOption = option.value
            } else if hoveredOption == option.value {
                hoveredOption = nil
            }
        }
        .accessibilityAddTraits(isSelected ? .isSelected : [])
    }

    /// The label with its mark trailing on the same line while both fit,
    /// and under it when they do not (R22).
    @ViewBuilder
    private func optionLabel(_ option: Option) -> some View {
        let label = Text(option.label)
            .font(FreesideFont.optionLabel)
            .foregroundStyle(Color.ink)
            .multilineTextAlignment(.leading)
            .fixedSize(horizontal: false, vertical: true)
        if let mark = option.mark {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    label
                    Spacer(minLength: 0)
                    CompactMark(text: mark)
                }
                VStack(alignment: .leading, spacing: 3) {
                    label
                    CompactMark(text: mark)
                }
            }
        } else {
            label
        }
    }

    private func move(by offset: Int) -> KeyPress.Result {
        guard let index = options.firstIndex(where: { $0.value == selection }) else {
            return .ignored
        }
        let next = index + offset
        guard options.indices.contains(next) else { return .handled }
        selection = options[next].value
        return .handled
    }
}

/// One option of a `ChoiceList`: the mark, then the option in its register.
/// Hover and press take one cut each (R19) in place of the option's own
/// ground, and keyboard focus is the 1pt accent ring outside the shape.
private struct ChoiceOptionStyle<Value: Hashable>: ButtonStyle {
    let register: ChoiceList<Value>.Register
    let isSelected: Bool
    let isHovered: Bool
    let isFocused: Bool

    func makeBody(configuration: Configuration) -> some View {
        HStack(alignment: .top, spacing: 10) {
            mark
                .padding(.top, 4)
            surface(
                configuration.label
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 10),
                wash: fill(isPressed: configuration.isPressed)
            )
            .overlay(shape.strokeBorder(border, lineWidth: 1))
            .overlay(
                shape.inset(by: -3)
                    .strokeBorder(isFocused ? Color.accentBorder : .clear, lineWidth: 1)
            )
        }
        .contentShape(Rectangle())
    }

    private var shape: RoundedRectangle {
        RoundedRectangle(cornerRadius: register == .quote ? 6 : 8)
    }

    @ViewBuilder
    private func surface(_ content: some View, wash: Color) -> some View {
        switch register {
        case .quote: content.quoteSurface(wash: wash)
        case .item: content.background(wash).clipShape(shape)
        }
    }

    /// The binary mark: filled is the pick, hollow is not.
    private var mark: some View {
        Group {
            if isSelected {
                Circle().fill(Color.ink)
            } else {
                Circle().strokeBorder(Color.ruleStrong, lineWidth: 1.5)
            }
        }
        .frame(width: 14, height: 14)
        .accessibilityHidden(true)
    }

    private var border: Color {
        if isSelected { return .ink }
        return register == .quote ? .clear : .itemBorder
    }

    private func fill(isPressed: Bool) -> Color {
        if isPressed { return .accentWashSoft }
        if isHovered { return .hover }
        return register == .quote ? .quoteWash : .clear
    }
}

/// The one chronology marker: every newest-first list (milestones, review
/// rounds, task events) marks entry 0 with a filled ink dot and every later
/// entry with a hollow ring. The stage rail draws the same two markers.
struct ChronologyMarker: View {
    static let diameter: CGFloat = 10
    let isCurrent: Bool

    var body: some View {
        Group {
            if isCurrent {
                Circle().fill(Color.ink)
            } else {
                Circle().strokeBorder(Color.milestonePrior, lineWidth: 1.5)
            }
        }
        .frame(width: Self.diameter, height: Self.diameter)
        .accessibilityHidden(true)
    }
}

/// Navigation text: accent, medium, a trailing "›". Wraps a `Button` (or
/// `NavigationLink`) label; an action that submits keeps a plain `Button`.
/// VoiceOver reads the title alone, since the chevron is decoration.
struct FreesideLink: View {
    let title: String
    private let font: Font

    /// A link in the platform face of the text style it sits among.
    init(title: String, style: Font.TextStyle = .callout) {
        self.title = title
        font = FreesideFont.sans(style, weight: .medium)
    }

    /// A link in one of the fixed faces (`FreesideFont.noticeAction` in a
    /// list row), which hold one point size on both platforms. The platform
    /// caption a row link used to take draws below the 11.5pt floor (R10)
    /// on macOS.
    init(title: String, face: Font) {
        self.title = title
        font = face
    }

    var body: some View {
        Text("\(title) ›")
            .font(font)
            .foregroundStyle(Color.accentText)
            .accessibilityLabel(title)
    }
}

/// The Freeside key (R13): the mark an empty state leads with. Drawn from
/// the one path of the handoff's `assets/key/freeside-key-mono.svg`, in that
/// file's view box, so the mark sits where the image would. The path cuts
/// the bow's openings out of the outline, so fill it with the even-odd rule
/// (`FillStyle(eoFill: true)`).
struct KeyMark: Shape {
    private static let viewBox = CGRect(x: 153, y: 54, width: 242, height: 404)
    /// Width over height, for a caller that sets one dimension.
    static let aspectRatio = viewBox.width / viewBox.height

    func path(in rect: CGRect) -> Path {
        var path = Path()
        func point(_ x: CGFloat, _ y: CGFloat) -> CGPoint { CGPoint(x: x, y: y) }

        // The outline: bit, stem, and bow.
        path.move(to: point(211, 54))
        path.addLine(to: point(352, 54))
        path.addQuadCurve(to: point(358, 60), control: point(358, 54))
        path.addLine(to: point(358, 90))
        path.addQuadCurve(to: point(352, 96), control: point(358, 96))
        path.addLine(to: point(270, 96))
        path.addLine(to: point(270, 144))
        path.addLine(to: point(331, 144))
        path.addQuadCurve(to: point(337, 150), control: point(337, 144))
        path.addLine(to: point(337, 178))
        path.addQuadCurve(to: point(331, 184), control: point(337, 184))
        path.addLine(to: point(270, 184))
        path.addLine(to: point(270, 257))
        path.addCurve(to: point(282, 279), control1: point(270, 270), control2: point(272, 276))
        path.addCurve(to: point(328, 358), control1: point(282, 310), control2: point(298, 331))
        path.addLine(to: point(240, 458))
        path.addLine(to: point(153, 358))
        path.addCurve(to: point(198, 279), control1: point(184, 330), control2: point(198, 311))
        path.addCurve(to: point(210, 257), control1: point(208, 276), control2: point(210, 270))
        path.addLine(to: point(210, 96))
        path.addCurve(to: point(201, 83), control1: point(210, 88), control2: point(207, 86))
        path.addCurve(to: point(190, 68), control1: point(194, 80), control2: point(190, 75))
        path.addLine(to: point(190, 61))
        path.addQuadCurve(to: point(197, 54), control: point(190, 54))
        path.closeSubpath()

        // The bow's two openings.
        path.move(to: point(228, 317))
        path.addCurve(to: point(205, 358), control1: point(212, 326), control2: point(205, 342))
        path.addCurve(to: point(228, 412), control1: point(205, 379), control2: point(213, 396))
        path.closeSubpath()
        path.move(to: point(252, 317))
        path.addLine(to: point(252, 412))
        path.addCurve(to: point(276, 358), control1: point(268, 396), control2: point(276, 379))
        path.addCurve(to: point(252, 317), control1: point(276, 342), control2: point(269, 326))
        path.closeSubpath()

        // The eye above them: the source's full-circle arc of radius 7.4
        // hanging from (240.15, 295.45).
        path.addEllipse(in: CGRect(x: 232.75, y: 295.45, width: 14.8, height: 14.8))

        let box = Self.viewBox
        let scale = min(rect.width / box.width, rect.height / box.height)
        return path.applying(
            CGAffineTransform(translationX: rect.midX, y: rect.midY)
                .scaledBy(x: scale, y: scale)
                .translatedBy(x: -box.midX, y: -box.midY))
    }
}

/// One label-and-value fact (R9): a sans label and a mono value, both in
/// ink, with the shared rule that decides between a trailing value and a
/// stacked one. Used by the decision detail facts, the stop cause, the
/// pairing details, the operational summary, and technical details, so a
/// fact reads the same on every surface.
struct FactRow: View {
    /// A value longer than this stacks at every type size
    /// (`--fs-fact-row-stack-threshold`). A digest or an invocation id has
    /// nowhere to wrap, so a trailing value either breaks mid-token or
    /// squeezes the label out of the row. Counted in grapheme clusters,
    /// which is what the token's character count means for the ASCII
    /// labels, ids, and digests these rows carry.
    static let stackThreshold = 40

    /// An accessibility size stacks every row; below that, only a value
    /// too long for the trailing column does.
    static func stacks(_ value: String, at size: DynamicTypeSize) -> Bool {
        size >= .accessibility1 || value.count > stackThreshold
    }

    let label: String
    let value: String
    /// Set where the value carries its own color; nil draws it in ink.
    var valueColor: Color? = nil
    /// A state drawn in the value slot in place of the text (R9). `value`
    /// holds the chip's label, so the stacking rule reads one string.
    private(set) var chip: StateChip? = nil
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        if Self.stacks(value, at: dynamicTypeSize) {
            VStack(alignment: .leading, spacing: 3) {
                labelText
                valueContent
                    .fixedSize(horizontal: false, vertical: true)
            }
        } else {
            // An explicit trailing column on both platforms: macOS's
            // LabeledContent set the value inline after the label, so the
            // summary's values never lined up.
            HStack(alignment: .firstTextBaseline) {
                labelText
                Spacer(minLength: 12)
                valueContent
                    .multilineTextAlignment(.trailing)
            }
        }
    }

    private var labelText: some View {
        Text(label)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
    }

    @ViewBuilder private var valueContent: some View {
        if let chip {
            chip
        } else {
            Text(value)
                .font(FreesideFont.monoValue)
                .foregroundStyle(valueColor ?? .ink)
        }
    }
}

/// A fact whose value is a link (R6), laid out as `FactRow` lays out a
/// plain value so linked and plain rows share one trailing column. The
/// whole row is the button, so the target isn't only the value's width.
struct FactLinkRow: View {
    let label: String
    let value: String
    /// What VoiceOver names the row's destination; nil uses the label.
    var accessibilityName: String? = nil
    let action: () -> Void
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Button(action: action) {
            Group {
                if FactRow.stacks(value, at: dynamicTypeSize) {
                    VStack(alignment: .leading, spacing: 3) {
                        labelText
                        valueLink
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                } else {
                    HStack(alignment: .firstTextBaseline) {
                        labelText
                        Spacer(minLength: 12)
                        valueLink
                            .multilineTextAlignment(.trailing)
                    }
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Open \(accessibilityName ?? label): \(value)")
    }

    private var labelText: some View {
        Text(label)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
    }

    private var valueLink: some View {
        FreesideLink(title: value, face: FreesideFont.noticeAction)
            .fixedSize(horizontal: false, vertical: true)
    }
}

extension FactRow {
    /// A fact whose value is a state: the chip sits in the value slot.
    init(label: String, chip: StateChip) {
        self.init(label: label, value: chip.label, chip: chip)
    }
}

/// The four control states of the design language (plan §15) in one
/// recipe: a filled primary, an outlined secondary, an unadorned tertiary,
/// and a disabled state drawn as its own rule-bordered, faint-text shape.
/// Disabled is never the enabled look faded out, because opacity dims the
/// border and the label together and leaves neither reliably legible.
///
/// Weight means state (R6): the fill is the one forward action, an outline
/// is any other command, the wax outline is destructive, and text is the
/// overflow. Every control is cut with the 6pt radius the rest of the card
/// chrome uses, and takes the five interaction states of R19.
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

    let tone: Tone
    /// A dense-chrome control (the menu-bar panel): 28pt minimum height in
    /// place of the 46 a sheet or card control takes, at every type size.
    var compact: Bool = false
    /// Whether the control fills its row. A tertiary button always hugs its
    /// label: a full-width control with no fill and no border reads as a
    /// row of dead space rather than as a button.
    var expands: Bool = true

    func makeBody(configuration: Configuration) -> some View {
        FreesideActionButtonBody(
            tone: tone, compact: compact, expands: expands, configuration: configuration)
    }
}

/// The style's drawing, as a view so it can hold the hover state and read
/// the keyboard focus a `ButtonStyle` is not handed.
private struct FreesideActionButtonBody: View {
    typealias Tone = FreesideActionButtonStyle.Tone

    let tone: Tone
    let compact: Bool
    let expands: Bool
    let configuration: ButtonStyleConfiguration
    @State private var isHovered = false
    @Environment(\.isEnabled) private var isEnabled
    @Environment(\.isFocused) private var isFocused
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    private let shape = RoundedRectangle(cornerRadius: 6)

    var body: some View {
        configuration.label
            .font(FreesideFont.actionLabel)
            .foregroundStyle(labelColor)
            .lineLimit(2)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.horizontal, 12)
            .padding(.vertical, verticalPadding)
            .frame(minHeight: minHeight)
            .frame(maxWidth: hugsLabel ? nil : .infinity)
            .background(shape.fill(fillColor))
            .overlay(shape.strokeBorder(borderColor, lineWidth: 1))
            // Keyboard focus is its own 1pt accent ring outside the border,
            // so it reads on a filled control and an outlined one alike.
            .overlay(
                shape.inset(by: -3)
                    .strokeBorder(isFocused && isEnabled ? Color.accentBorder : .clear, lineWidth: 1)
            )
            .contentShape(shape)
            .onHover { isHovered = $0 }
    }

    private var hugsLabel: Bool {
        tone == .tertiary || !expands
    }

    private var verticalPadding: CGFloat {
        if compact { return 4 }
        return dynamicTypeSize >= .accessibility1 ? 12 : 7
    }

    private var minHeight: CGFloat {
        if compact { return 28 }
        return dynamicTypeSize >= .accessibility1 ? 56 : 46
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
        case .secondary: return .ruleStrong
        case .tertiary: return .clear
        case .destructive: return .waxText
        }
    }

    /// Hover and press are one cut each on every tone that has no fill of
    /// its own (R19). The filled primary keeps its fill: it is already the
    /// heaviest thing in its region, and neither wash is legible under its
    /// ground-2 label.
    private var fillColor: Color {
        guard isEnabled else { return .clear }
        if tone == .primary { return .accentText }
        if configuration.isPressed { return .accentWashSoft }
        if isHovered { return .hover }
        return tone == .tertiary ? .clear : .ground2
    }
}

/// What a sheet opens with in place of a navigation bar (R11): an optional
/// eyebrow keyword naming the kind of sheet, the serif ask, the consequence
/// of answering it in dim sans, and the binding the answer applies to in
/// mono, 16pt in from the edge.
struct FreesideSheetHeader: View {
    var eyebrow: String? = nil
    let ask: String
    var consequence: String? = nil
    var binding: String? = nil
    /// Two lines by default: an attachment sheet's ask is the agent's claim
    /// label, which the contract admits at any length, and this header never
    /// compresses vertically, so an unbounded ask would push the sheet body
    /// and its Done footer off-screen. The system navigation title this
    /// header replaces truncated to one line. A sheet that scrolls its
    /// header passes `nil`, so its own ask stays whole at every text size.
    var askLineLimit: Int? = 2

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let eyebrow {
                KeywordLabel(text: eyebrow)
            }
            Text(ask)
                .font(FreesideFont.sectionTitle)
                .foregroundStyle(Color.ink)
                .lineLimit(askLineLimit)
                .accessibilityAddTraits(.isHeader)
            if let consequence {
                Text(consequence)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            if let binding {
                Text(binding)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .fixedSize(horizontal: false, vertical: true)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
    }
}

/// The submit row a sheet ends with: Cancel as a tertiary text button and
/// the submit filled by default and wax-outlined for a consequential
/// confirmation (R11), both hugging their labels. It carries the
/// Return and Escape bindings the system toolbar placements used to supply.
/// A reader's footer (`done`) has the one secondary button and no Cancel.
struct FreesideSheetActionRow: View {
    let submitLabel: String
    var tone: FreesideActionButtonStyle.Tone = .primary
    /// Read by VoiceOver after the submit label: the consequence sentence
    /// a destructive submit carries.
    var submitHint: String? = nil
    var isSubmitEnabled: Bool = true
    /// The refined footer (R11): Cancel as an outline that shares the row
    /// equally with the submit, in place of the hugging text button.
    var cancelIsOutlined = false
    let submit: () -> Void
    /// `nil` for a reader's single dismiss: no Cancel is drawn and Escape
    /// routes to `submit`, so both keys close the sheet.
    let cancel: (() -> Void)?
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    /// A reader or attachment sheet's footer: one secondary Done button on
    /// the right, dismissed by Return and Escape alike.
    static func done(_ dismiss: @escaping () -> Void) -> FreesideSheetActionRow {
        FreesideSheetActionRow(submitLabel: "Done", tone: .secondary, submit: dismiss, cancel: nil)
    }

    var body: some View {
        VStack(spacing: 0) {
            Divider()
            content
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
        }
    }

    @ViewBuilder private var content: some View {
        if let cancel {
            // Side by side the two labels cannot both hug their text at an
            // accessibility size without wrapping mid-word, so they stack
            // and the submit takes the full width.
            if dynamicTypeSize.isAccessibilitySize {
                VStack(spacing: 12) {
                    submitButton(expands: true)
                    cancelButton(cancel).frame(maxWidth: .infinity)
                }
            } else if cancelIsOutlined {
                HStack(spacing: 10) {
                    cancelButton(cancel)
                    submitButton(expands: true)
                }
            } else {
                HStack(spacing: 12) {
                    cancelButton(cancel)
                    Spacer(minLength: 12)
                    submitButton(expands: false)
                }
            }
        } else {
            HStack(spacing: 0) {
                Spacer(minLength: 0)
                submitButton(expands: dynamicTypeSize.isAccessibilitySize)
            }
            // Escape reaches the dismiss through a button that exists for
            // its shortcut alone; one button cannot carry both key
            // equivalents.
            .background(
                Button("", action: submit)
                    .buttonStyle(.plain)
                    .keyboardShortcut(.cancelAction)
                    .frame(width: 0, height: 0)
                    .opacity(0)
                    .accessibilityHidden(true)
            )
        }
    }

    private func cancelButton(_ cancel: @escaping () -> Void) -> some View {
        Button("Cancel", action: cancel)
            .buttonStyle(
                FreesideActionButtonStyle(
                    tone: cancelIsOutlined ? .secondary : .tertiary, expands: cancelIsOutlined)
            )
            .keyboardShortcut(.cancelAction)
    }

    private func submitButton(expands: Bool) -> some View {
        Button(submitLabel, action: submit)
            .buttonStyle(
                FreesideActionButtonStyle(tone: tone, expands: expands)
            )
            .keyboardShortcut(.defaultAction)
            .disabled(!isSubmitEnabled)
            .accessibilityHint(submitHint ?? "")
    }
}

extension View {
    /// The iOS sheet chrome the design language keeps: a visible drag
    /// indicator. A no-op on macOS, where a sheet has none.
    func freesideSheetPresentation() -> some View {
        #if os(iOS)
            presentationDragIndicator(.visible)
        #else
            self
        #endif
    }

    /// The pointer-hover state (R19): the `hover` cut behind a control that
    /// has no fill of its own. A disabled control does not respond.
    func freesideHover(cornerRadius: CGFloat = 6) -> some View {
        modifier(FreesideHover(cornerRadius: cornerRadius))
    }

    /// The keyboard-focus state (R19): a 1pt accent ring in place of the
    /// system focus effect, which is drawn in the system's own color.
    func freesideFocusRing(cornerRadius: CGFloat = 6) -> some View {
        modifier(FreesideFocusRing(cornerRadius: cornerRadius))
    }

    /// The quote's ground (R5) under content that sets its own padding: the
    /// 3pt rule inside the leading edge of the wash, both clipped to one
    /// corner. `QuoteBlock` is the same ground with a card section's padding;
    /// a conversation message and a choice-list option size themselves.
    func quoteSurface(wash: Color = .quoteWash, cornerRadius: CGFloat = 6) -> some View {
        padding(.leading, 3)
            .background(alignment: .leading) { Color.quoteRule.frame(width: 3) }
            .background(wash)
            .clipShape(RoundedRectangle(cornerRadius: cornerRadius))
    }

    /// A card: ground-2 on ground, 1px rule border, 8pt radius.
    func freesideCard(border: Color = .rule, dashed: Bool = false, cornerRadius: CGFloat = 8) -> some View {
        background(RoundedRectangle(cornerRadius: cornerRadius).fill(Color.ground2))
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius)
                    .strokeBorder(border, style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : []))
            )
    }
}

private struct FreesideHover: ViewModifier {
    let cornerRadius: CGFloat
    @State private var isHovered = false
    @Environment(\.isEnabled) private var isEnabled

    func body(content: Content) -> some View {
        content
            .background(
                RoundedRectangle(cornerRadius: cornerRadius)
                    .fill(isHovered && isEnabled ? Color.hover : .clear)
            )
            .onHover { isHovered = $0 }
    }
}

private struct FreesideFocusRing: ViewModifier {
    let cornerRadius: CGFloat
    @FocusState private var isFocused: Bool

    func body(content: Content) -> some View {
        content
            .focused($isFocused)
            .focusEffectDisabled()
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius)
                    .strokeBorder(isFocused ? Color.accentBorder : .clear, lineWidth: 1)
                    // The ring sits just outside the label it frames.
                    .padding(-3)
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
