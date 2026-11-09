import SwiftUI
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit

    @testable import FreesideModel
#endif

@MainActor
struct FactRowTests {
    private let fortyCharacters = String(repeating: "a", count: 40)
    private let fortyOneCharacters = String(repeating: "a", count: 41)

    @Test func valueAtTheThresholdKeepsTheTrailingLayout() {
        #expect(!FactRow.stacks(fortyCharacters, at: .large))
        #expect(!FactRow.stacks(fortyCharacters, at: .xSmall))
    }

    @Test func valueOverTheThresholdStacksAtEverySize() {
        #expect(FactRow.stacks(fortyOneCharacters, at: .xSmall))
        #expect(FactRow.stacks(fortyOneCharacters, at: .accessibility5))
    }

    @Test func accessibilitySizeStacksAShortValue() {
        #expect(FactRow.stacks("ok", at: .accessibility1))
        #expect(!FactRow.stacks("ok", at: .xxxLarge))
    }

    /// R9: a chip in the value slot stacks by the same rule as the text it
    /// replaces, so the row reads its label for the decision.
    @Test func chipRowCarriesTheChipLabelAsItsValue() {
        let row = FactRow(label: "Posture", chip: StateChip(label: "Degraded", cut: .attention))
        #expect(row.value == "Degraded")
        #expect(row.chip?.label == "Degraded")
        #expect(!FactRow.stacks(row.value, at: .large))
        #expect(FactRow.stacks(row.value, at: .accessibility1))
    }

    @Test func textRowCarriesNoChip() {
        #expect(FactRow(label: "Stage", value: "review").chip == nil)
    }
}

/// A fact group stripes its even rows, and the count restarts per group.
@MainActor
struct FactStripeTests {
    private func row(y: CGFloat) -> CGRect {
        CGRect(x: 0, y: y, width: 200, height: 20)
    }

    @Test func oneRowDrawsNoStripe() {
        #expect(FactStripe.bands(rows: [row(y: 0)], width: 300, spacing: 8).isEmpty)
    }

    @Test func secondOfTwoRowsTakesTheStripe() {
        let bands = FactStripe.bands(rows: [row(y: 0), row(y: 28)], width: 300, spacing: 8)
        let outset = FactStripe.outset
        #expect(bands == [CGRect(x: -outset, y: 24, width: 300 + 2 * outset, height: 28)])
    }

    @Test func everyEvenRowOfFiveTakesTheStripe() {
        let rows = (0..<5).map { row(y: CGFloat($0) * 30) }
        let bands = FactStripe.bands(rows: rows, width: 300, spacing: 10)
        // Rows 2 and 4, each reaching half the gap above and below.
        #expect(bands.map(\.minY) == [25, 85])
        #expect(bands.map(\.height) == [30, 30])
        #expect(bands.allSatisfy { $0.minX == -FactStripe.outset && $0.width == 300 + 2 * FactStripe.outset })
    }

    #if os(macOS)
        /// The stripes draw through `ImageRenderer`, the screenshot suite's
        /// path, and each group counts its own rows: groups of three, three,
        /// and one stripe two rows, where one count across all seven rows
        /// would stripe three.
        @Test func eachGroupRestartsTheCount() throws {
            let groups = [3, 3, 1]
            let runs = try Self.stripeRuns(
                VStack(alignment: .leading, spacing: 40) {
                    ForEach(Array(groups.enumerated()), id: \.offset) { group, count in
                        FactGroup(spacing: 10) {
                            ForEach(0..<count, id: \.self) { index in
                                FactRow(label: "Fact \(group).\(index)", value: "value")
                            }
                        }
                    }
                })
            #expect(runs == 2, "stripe runs: \(runs)")
        }

        /// A group hides its rows from an enclosing group. The outer group
        /// holds one row of its own, so it stripes nothing, and the inner
        /// group stripes its second row: one band. Were the inner rows to
        /// reach the outer count, the outer group would also stripe the
        /// inner first and third rows, in bands too thin at its 2pt spacing
        /// to meet the inner one across the inner 20pt gaps: three bands.
        @Test func aNestedGroupKeepsItsRowsFromTheEnclosingCount() throws {
            let runs = try Self.stripeRuns(
                FactGroup(spacing: 2) {
                    FactRow(label: "Outer", value: "value")
                    FactGroup(spacing: 20) {
                        ForEach(0..<3, id: \.self) { index in
                            FactRow(label: "Inner \(index)", value: "value")
                        }
                    }
                })
            #expect(runs == 1, "stripe runs: \(runs)")
        }

        /// How many separate stripe bands the content draws, read down the
        /// column inside the stripe's outset, left of every label.
        private static func stripeRuns(_ content: some View) throws -> Int {
            let renderer = ImageRenderer(
                content:
                    content
                    .padding(20)
                    .frame(width: 320)
                    .fixedSize(horizontal: false, vertical: true)
                    .background(Color.ground)
                    .environment(\.colorScheme, .light)
                    .environment(\.dynamicTypeSize, .large))
            renderer.scale = 1
            var rendered: Result<CGImage, Error>?
            LaunchInputs.$screenshotIncreasedContrast.withValue(false) {
                NSAppearance(named: .aqua)?.performAsCurrentDrawingAppearance {
                    rendered = Result { try ScreenshotCapture.bitmap(renderer) }
                }
            }
            let image = try #require(rendered).get()
            let column = Int(20 - FactStripe.outset / 2)
            return runs(of: FreesidePalette.factStripe.day, in: image, column: column)
        }

        /// How many separate vertical runs of `color` the column holds.
        private static func runs(of color: UInt32, in image: CGImage, column: Int) -> Int {
            let bytesPerRow = image.width * 4
            var pixels = [UInt8](repeating: 0, count: bytesPerRow * image.height)
            let space = CGColorSpace(name: CGColorSpace.sRGB)!
            let context = CGContext(
                data: &pixels, width: image.width, height: image.height, bitsPerComponent: 8,
                bytesPerRow: bytesPerRow, space: space,
                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
            context.draw(image, in: CGRect(x: 0, y: 0, width: image.width, height: image.height))
            let target = [(color >> 16) & 0xFF, (color >> 8) & 0xFF, color & 0xFF].map { Int($0) }
            var runs = 0
            var inRun = false
            for y in 0..<image.height {
                let offset = y * bytesPerRow + column * 4
                let pixel = (0..<3).map { Int(pixels[offset + $0]) }
                let matches = zip(pixel, target).allSatisfy { abs($0 - $1) <= 1 }
                if matches && !inRun { runs += 1 }
                inRun = matches
            }
            return runs
        }
    #endif
}
