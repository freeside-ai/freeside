#if os(macOS)
    import AppKit
    import SwiftUI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    /// R32's meter, read from the pixels `ImageRenderer` draws, in every
    /// palette cut. The screenshot suite draws through `ImageRenderer`, which
    /// draws the built-in `.linearCapacity` gauge style as the system's
    /// unsupported placeholder, so this pins that `CapacityGaugeStyle` draws
    /// the track, the fill of what is left, and the used-up rim (#2579).
    @Suite @MainActor struct CapacityMeterTests {
        typealias Cut = DesignContrastTests.Cut

        private static let width = 200

        @Test(arguments: Cut.allCases)
        func aFineMeterFillsWhatIsLeftInTheAccent(_ cut: Cut) throws {
            let meter = try Drawn(CapacityMeter(left: 60, level: .fine), cut: cut)
            #expect(meter.color(40, 5) == FreesidePalette.accentText[cut])
            #expect(meter.color(160, 5) == FreesidePalette.ground3[cut])
        }

        @Test(arguments: Cut.allCases)
        func aLowMeterFillsInWax(_ cut: Cut) throws {
            let meter = try Drawn(CapacityMeter(left: 10, level: .low), cut: cut)
            #expect(meter.color(10, 5) == FreesidePalette.waxText[cut])
            #expect(meter.color(100, 5) == FreesidePalette.ground3[cut])
        }

        @Test(arguments: Cut.allCases)
        func aUsedUpMeterIsAnEmptyTrackInsideAWaxRim(_ cut: Cut) throws {
            let meter = try Drawn(CapacityMeter(left: 0, level: .usedUp), cut: cut)
            #expect(meter.color(100, 5) == FreesidePalette.ground3[cut])
            #expect(meter.color(100, 0) == FreesidePalette.waxText[cut])
            #expect(meter.color(100, 9) == FreesidePalette.waxText[cut])
        }

        /// One meter drawn at one pixel per point on ground-2, 10pt tall.
        private struct Drawn {
            let pixels: [UInt8]
            let size: (width: Int, height: Int)

            @MainActor init(_ meter: CapacityMeter, cut: Cut) throws {
                let view =
                    meter
                    .frame(width: CGFloat(CapacityMeterTests.width))
                    .background(Color.ground2)
                    .environment(\.colorScheme, cut == .dusk || cut == .duskIC ? .dark : .light)
                let renderer = ImageRenderer(content: view)
                var drawn: (pixels: [UInt8], width: Int, height: Int)?
                // The palette resolves its contrast cut inside the AppKit
                // color callback, so the pin has to hold while it draws.
                LaunchInputs.$screenshotIncreasedContrast.withValue(cut == .dayIC || cut == .duskIC) {
                    renderer.render(rasterizationScale: 1) { size, draw in
                        let width = Int(ceil(size.width))
                        let height = Int(ceil(size.height))
                        guard
                            let space = CGColorSpace(name: CGColorSpace.sRGB),
                            let context = CGContext(
                                data: nil, width: width, height: height, bitsPerComponent: 8,
                                bytesPerRow: width * 4, space: space,
                                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue),
                            let data = context.data
                        else { return }
                        context.translateBy(x: 0, y: CGFloat(height) - size.height)
                        draw(context)
                        let bytes = UnsafeBufferPointer(
                            start: data.assumingMemoryBound(to: UInt8.self), count: width * height * 4)
                        drawn = (Array(bytes), width, height)
                    }
                }
                let image = try #require(drawn)
                #expect(image.height == 10)
                pixels = image.pixels
                size = (image.width, image.height)
            }

            func color(_ x: Int, _ y: Int) -> UInt32 {
                let offset = (y * size.width + x) * 4
                return UInt32(pixels[offset]) << 16 | UInt32(pixels[offset + 1]) << 8 | UInt32(pixels[offset + 2])
            }
        }
    }
#endif
