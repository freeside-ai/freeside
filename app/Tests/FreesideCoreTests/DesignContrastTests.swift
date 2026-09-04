import Foundation
import Testing

@testable import FreesideCore

@Suite struct DesignContrastTests {
    enum Cut: String, CaseIterable {
        case day
        case dusk
        case dayIC
        case duskIC
    }

    private struct Pair {
        let foregroundName: String
        let foreground: FreesideColorCuts
        let backgroundName: String
        let background: FreesideColorCuts
        let minimum: Double
    }

    @Test func everyUsedSemanticPairMeetsItsContrastThreshold() {
        #expect(FreesidePalette.rule.dayIC == FreesidePalette.ruleStrong.day)
        #expect(FreesidePalette.rule.duskIC == FreesidePalette.ruleStrong.dusk)
        #expect(FreesidePalette.secondaryBorder.dayIC == FreesidePalette.ruleStrong.day)
        #expect(FreesidePalette.secondaryBorder.duskIC == FreesidePalette.ruleStrong.dusk)
        #expect(FreesidePalette.inkFaint.dayIC == FreesidePalette.inkDim.day)
        #expect(FreesidePalette.inkFaint.duskIC == FreesidePalette.inkDim.dusk)

        for pair in Self.usedPairs {
            for cut in Cut.allCases {
                let ratio = contrastRatio(pair.foreground[cut], pair.background[cut])
                #expect(
                    ratio >= pair.minimum,
                    "\(pair.foregroundName) on \(pair.backgroundName), \(cut.rawValue): \(ratio) < \(pair.minimum)"
                )
            }
        }
    }

    private static let usedPairs: [Pair] = [
        // Body, metadata, and controls on the grounds and washes they use.
        text("ink", FreesidePalette.ink, on: "ground", FreesidePalette.ground),
        text("ink", FreesidePalette.ink, on: "ground2", FreesidePalette.ground2),
        text("ink", FreesidePalette.ink, on: "ground3", FreesidePalette.ground3),
        text(
            "ink", FreesidePalette.ink, on: "sidebarGround", FreesidePalette.sidebarGround),
        text(
            "ink", FreesidePalette.ink, on: "accentWashSoft",
            FreesidePalette.accentWashSoft),
        text("inkDim", FreesidePalette.inkDim, on: "ground", FreesidePalette.ground),
        text("inkDim", FreesidePalette.inkDim, on: "ground2", FreesidePalette.ground2),
        text("inkDim", FreesidePalette.inkDim, on: "ground3", FreesidePalette.ground3),
        text(
            "inkDim", FreesidePalette.inkDim, on: "sidebarGround",
            FreesidePalette.sidebarGround),
        text(
            "inkDim", FreesidePalette.inkDim, on: "accentWash", FreesidePalette.accentWash),
        text("inkDim", FreesidePalette.inkDim, on: "waxWash", FreesidePalette.waxWash),
        text(
            "inkDim", FreesidePalette.inkDim, on: "neutralWash", FreesidePalette.neutralWash),
        text("ground2", FreesidePalette.ground2, on: "accentText", FreesidePalette.accentText),

        // Semantic text appears on ordinary grounds, pressed-button fills,
        // and its own wash. The own-wash pair is intrinsic even when a
        // current composition does not place text there yet.
        text(
            "accentText", FreesidePalette.accentText, on: "ground", FreesidePalette.ground),
        text(
            "accentText", FreesidePalette.accentText, on: "ground2", FreesidePalette.ground2),
        text(
            "accentText", FreesidePalette.accentText, on: "ground3", FreesidePalette.ground3),
        text(
            "accentText", FreesidePalette.accentText, on: "sidebarGround",
            FreesidePalette.sidebarGround),
        text(
            "accentText", FreesidePalette.accentText, on: "accentWash",
            FreesidePalette.accentWash),
        text(
            "accentText", FreesidePalette.accentText, on: "accentWashSoft",
            FreesidePalette.accentWashSoft),
        text("waxText", FreesidePalette.waxText, on: "ground", FreesidePalette.ground),
        text("waxText", FreesidePalette.waxText, on: "ground2", FreesidePalette.ground2),
        text("waxText", FreesidePalette.waxText, on: "ground3", FreesidePalette.ground3),
        text(
            "waxText", FreesidePalette.waxText, on: "sidebarGround",
            FreesidePalette.sidebarGround),
        text("waxText", FreesidePalette.waxText, on: "waxWash", FreesidePalette.waxWash),
        text("waterText", FreesidePalette.waterText, on: "ground", FreesidePalette.ground),
        text(
            "waterText", FreesidePalette.waterText, on: "ground2", FreesidePalette.ground2),
        text(
            "waterText", FreesidePalette.waterText, on: "waterWash",
            FreesidePalette.waterWash),

        // Disabled and validating text. It must stay readable while reading
        // as unavailable, so it clears the disabled floor rather than the
        // body one, on every ground a disabled control can sit on.
        disabledText("inkFaint", FreesidePalette.inkFaint, on: "ground", FreesidePalette.ground),
        disabledText(
            "inkFaint", FreesidePalette.inkFaint, on: "ground2", FreesidePalette.ground2),
        disabledText(
            "inkFaint", FreesidePalette.inkFaint, on: "ground3", FreesidePalette.ground3),

        // Meaningful outlines and indicators. Standard structural hairlines
        // stay decorative; Increased Contrast promotes them to ruleStrong.
        // secondaryBorder is intentionally absent for the same reason rule
        // is: it is a hairline at 1.58:1 by day and relies on the Increased
        // Contrast promotion to ruleStrong, asserted above.
        border(
            "accentBorder", FreesidePalette.accentBorder, on: "ground",
            FreesidePalette.ground),
        border(
            "accentBorder", FreesidePalette.accentBorder, on: "ground2",
            FreesidePalette.ground2),
        border(
            "accentBorder", FreesidePalette.accentBorder, on: "ground3",
            FreesidePalette.ground3),
        border(
            "accentBorder", FreesidePalette.accentBorder, on: "sidebarGround",
            FreesidePalette.sidebarGround),
        border(
            "accentBorder", FreesidePalette.accentBorder, on: "accentWash",
            FreesidePalette.accentWash),
        border(
            "ruleStrong", FreesidePalette.ruleStrong, on: "ground", FreesidePalette.ground),
        border(
            "ruleStrong", FreesidePalette.ruleStrong, on: "ground2", FreesidePalette.ground2),
        border(
            "ruleStrong", FreesidePalette.ruleStrong, on: "ground3", FreesidePalette.ground3),
        border(
            "ruleStrong", FreesidePalette.ruleStrong, on: "sidebarGround",
            FreesidePalette.sidebarGround),

        // milestonePrior and milestoneConnector are intentionally absent:
        // stage-rail decoration is paired with mono labels and never carries
        // state or meaning alone.
    ]

    private static func text(
        _ foregroundName: String, _ foreground: FreesideColorCuts,
        on backgroundName: String, _ background: FreesideColorCuts
    ) -> Pair {
        Pair(
            foregroundName: foregroundName, foreground: foreground,
            backgroundName: backgroundName, background: background, minimum: 4.5)
    }

    /// Disabled text carries no action, so the project floor for it is the
    /// 3:1 non-text minimum rather than the 4.5:1 body minimum: it must be
    /// readable enough to name the unavailable action, and no more.
    private static func disabledText(
        _ foregroundName: String, _ foreground: FreesideColorCuts,
        on backgroundName: String, _ background: FreesideColorCuts
    ) -> Pair {
        Pair(
            foregroundName: foregroundName, foreground: foreground,
            backgroundName: backgroundName, background: background, minimum: 3)
    }

    private static func border(
        _ foregroundName: String, _ foreground: FreesideColorCuts,
        on backgroundName: String, _ background: FreesideColorCuts
    ) -> Pair {
        Pair(
            foregroundName: foregroundName, foreground: foreground,
            backgroundName: backgroundName, background: background, minimum: 3)
    }

    private func contrastRatio(_ lhs: UInt32, _ rhs: UInt32) -> Double {
        let left = relativeLuminance(lhs)
        let right = relativeLuminance(rhs)
        return (max(left, right) + 0.05) / (min(left, right) + 0.05)
    }

    private func relativeLuminance(_ hex: UInt32) -> Double {
        let red = linear(Double((hex >> 16) & 0xFF) / 255)
        let green = linear(Double((hex >> 8) & 0xFF) / 255)
        let blue = linear(Double(hex & 0xFF) / 255)
        return 0.2126 * red + 0.7152 * green + 0.0722 * blue
    }

    private func linear(_ component: Double) -> Double {
        component <= 0.04045
            ? component / 12.92
            : pow((component + 0.055) / 1.055, 2.4)
    }
}

extension FreesideColorCuts {
    fileprivate subscript(cut: DesignContrastTests.Cut) -> UInt32 {
        switch cut {
        case .day: day
        case .dusk: dusk
        case .dayIC: dayIC
        case .duskIC: duskIC
        }
    }
}
