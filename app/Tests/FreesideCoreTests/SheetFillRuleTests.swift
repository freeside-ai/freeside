#if os(macOS)
    import AppKit
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The sheet fill rule (design handoff 8 Oct 2026, item 6), read from
    /// the pixels a sheet's footer draws: the submit fills only while it is
    /// enabled, a disabled one is faint ink on the rule border with no fill,
    /// and Cancel is the outline either way. Each recipe is checked against
    /// the palette cut of the appearance it is drawn in, so one that held by
    /// day and not by dusk fails here. The footer is drawn on the page's
    /// ground, as the revision and snooze sheets draw it: there a control
    /// with no fill and one filled with the card ground differ.
    @Suite @MainActor struct SheetFillRuleTests {
        typealias Cut = DesignContrastTests.Cut

        /// What a control is drawn with: three palette values.
        struct Recipe: Equatable, CustomStringConvertible {
            let fill: UInt32
            let border: UInt32
            let label: UInt32

            var description: String {
                String(format: "fill %06X, border %06X, label %06X", fill, border, label)
            }
        }

        @Test(arguments: Cut.allCases)
        func aDisabledSubmitIsFaintInkOnTheRuleBorderWithNoFill(_ cut: Cut) throws {
            let footer = try Footer(isSubmitEnabled: false, cut: cut)
            #expect(
                try footer.recipe(of: .submit)
                    == Recipe(
                        fill: FreesidePalette.ground[cut], border: FreesidePalette.rule[cut],
                        label: FreesidePalette.inkFaint[cut]))
        }

        @Test(arguments: Cut.allCases)
        func anEnabledSubmitFills(_ cut: Cut) throws {
            let footer = try Footer(isSubmitEnabled: true, cut: cut)
            #expect(
                try footer.recipe(of: .submit)
                    == Recipe(
                        fill: FreesidePalette.accentText[cut], border: FreesidePalette.accentBorder[cut],
                        label: FreesidePalette.ground2[cut]))
        }

        @Test(arguments: Cut.allCases, [true, false])
        func cancelIsTheOutlineBesideEitherSubmit(_ cut: Cut, isSubmitEnabled: Bool) throws {
            let footer = try Footer(isSubmitEnabled: isSubmitEnabled, cut: cut)
            #expect(
                try footer.recipe(of: .cancel)
                    == Recipe(
                        fill: FreesidePalette.ground2[cut], border: FreesidePalette.ruleStrong[cut],
                        label: FreesidePalette.ink[cut]))
            // The two share the row equally (R11).
            #expect(try footer.frame(of: .cancel).width == footer.frame(of: .submit).width)
        }

        /// A form sheet's footer on the page's ground, drawn in one cut.
        private struct Footer {
            enum Control {
                case cancel
                case submit
            }

            /// Pixels per point: enough that a label's stems hold pixels the
            /// glyph covers whole, which are the label's own color.
            static let scale = 3
            static let width = 380

            let pixels: [UInt8]
            let size: (width: Int, height: Int)
            let ground: UInt32

            @MainActor init(isSubmitEnabled: Bool, cut: Cut) throws {
                _ = FreesideFont.registration
                let row = FreesideSheetActionRow(
                    submitLabel: "Submit", isSubmitEnabled: isSubmitEnabled, submit: {}, cancel: {}
                )
                .frame(width: CGFloat(Self.width))
                .background(Color.ground)
                .environment(\.dynamicTypeSize, .large)
                .environment(\.colorScheme, cut.isDusk ? .dark : .light)
                let renderer = ImageRenderer(content: row)
                var drawn: (pixels: [UInt8], width: Int, height: Int)?
                // The palette resolves its contrast cut inside the AppKit
                // color callback, so the pin has to hold while it draws.
                LaunchInputs.$screenshotIncreasedContrast.withValue(cut.isIncreasedContrast) {
                    renderer.render(rasterizationScale: CGFloat(Self.scale)) { size, draw in
                        let width = Int(ceil(size.width)) * Self.scale
                        let height = Int(ceil(size.height)) * Self.scale
                        guard
                            let space = CGColorSpace(name: CGColorSpace.sRGB),
                            let context = CGContext(
                                data: nil, width: width, height: height, bitsPerComponent: 8,
                                bytesPerRow: width * 4, space: space,
                                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue),
                            let data = context.data
                        else { return }
                        context.translateBy(x: 0, y: CGFloat(height) - size.height * CGFloat(Self.scale))
                        context.scaleBy(x: CGFloat(Self.scale), y: CGFloat(Self.scale))
                        draw(context)
                        let bytes = UnsafeBufferPointer(
                            start: data.assumingMemoryBound(to: UInt8.self), count: width * height * 4)
                        drawn = (Array(bytes), width, height)
                    }
                }
                let image = try #require(drawn)
                pixels = image.pixels
                size = (image.width, image.height)
                ground = FreesidePalette.ground[cut]
            }

            func color(_ x: Int, _ y: Int) -> UInt32 {
                let offset = (y * size.width + x) * 4
                return UInt32(pixels[offset]) << 16 | UInt32(pixels[offset + 1]) << 8 | UInt32(pixels[offset + 2])
            }

            /// The control's frame in pixels: the bounds of everything in
            /// its half of the row that is not the ground behind the row,
            /// under the divider the row opens with.
            func frame(of control: Control) throws -> CGRect {
                let half = size.width / 2
                let columns = control == .cancel ? 0..<half : half..<size.width
                var bounds: (minX: Int, minY: Int, maxX: Int, maxY: Int)?
                for y in (3 * Self.scale)..<size.height {
                    for x in columns where color(x, y) != ground {
                        bounds = (
                            min(bounds?.minX ?? x, x), min(bounds?.minY ?? y, y),
                            max(bounds?.maxX ?? x, x), max(bounds?.maxY ?? y, y)
                        )
                    }
                }
                let found = try #require(bounds)
                return CGRect(
                    x: found.minX, y: found.minY, width: found.maxX - found.minX + 1,
                    height: found.maxY - found.minY + 1)
            }

            /// The border where its left edge runs straight, the fill just
            /// inside it and clear of the centered label, and the label as
            /// the color furthest from the fill between the corners.
            func recipe(of control: Control) throws -> Recipe {
                let frame = try frame(of: control)
                let midY = Int(frame.midY)
                let fill = color(Int(frame.minX) + 6 * Self.scale, midY)
                var label = fill
                for y in (Int(frame.minY) + 2 * Self.scale)..<(Int(frame.maxY) - 2 * Self.scale) {
                    for x in (Int(frame.minX) + 8 * Self.scale)..<(Int(frame.maxX) - 8 * Self.scale)
                    where Self.distance(color(x, y), fill) > Self.distance(label, fill) {
                        label = color(x, y)
                    }
                }
                return Recipe(fill: fill, border: color(Int(frame.minX) + 1, midY), label: label)
            }

            private static func distance(_ lhs: UInt32, _ rhs: UInt32) -> Int {
                [16, 8, 0].reduce(0) { $0 + abs(Int((lhs >> $1) & 0xFF) - Int((rhs >> $1) & 0xFF)) }
            }
        }
    }

    extension DesignContrastTests.Cut {
        fileprivate var isDusk: Bool { self == .dusk || self == .duskIC }
        fileprivate var isIncreasedContrast: Bool { self == .dayIC || self == .duskIC }
    }
#endif
