#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct ScreenshotCaptureTests {
        private enum ProbeError: Error { case stopped }

        @Test func uniformImageRowsStayUniform() throws {
            // This fixture's gradient varies only vertically. The old cgImage
            // path adds horizontal pixel variation during rasterization.
            let source = try #require(NSImage(data: AttentionFixtures.fixtureImagePNG))
            let renderer = ImageRenderer(
                content: Image(nsImage: source).resizable().frame(width: 320, height: 200))
            let image = try ScreenshotCapture.bitmap(renderer)
            let data = try #require(image.dataProvider?.data) as Data
            for row in [20, 100, 180] {
                let start = row * image.bytesPerRow
                let firstPixel = data[start..<(start + 4)]
                #expect(
                    (1..<image.width).allSatisfy { column in
                        let offset = start + column * 4
                        return data[offset..<(offset + 4)].elementsEqual(firstPixel)
                    })
            }
        }

        @Test func fractionalHeightKeepsTheTopEdgeAligned() throws {
            let image = try ScreenshotCapture.bitmap(
                ImageRenderer(content: Color.red.frame(width: 8, height: 8.25)))
            let data = try #require(image.dataProvider?.data) as Data
            #expect(image.height == 9)
            #expect(data[3] == 255)
            // SwiftUI snaps the solid rectangle to eight pixels; rounding
            // the bitmap up must leave the spare row below, not above it.
            #expect(data[8 * image.bytesPerRow + 3] == 0)
        }

        @Test func repeatedVectorCapturesKeepTheSamePixels() throws {
            var digests: Set<String> = []
            for _ in 0..<20 {
                let renderer = ImageRenderer(
                    content: VStack {
                        Circle().fill(Color(red: 0.7, green: 0.5, blue: 0.2)).frame(width: 7, height: 7)
                        Text("Render probe").font(.system(size: 13))
                    }.padding(13.33).frame(width: 200).background(Color.white))
                digests.insert(try ScreenshotCapture.digest(ScreenshotCapture.bitmap(renderer)))
            }
            #expect(digests.count == 1)
        }

        @Test func aSettlingCaptureReturnsTheSettledImage() async throws {
            let first = try solid(.red)
            let final = try solid(.blue)
            var images = [first, final, final].makeIterator()
            let image = try await ScreenshotCapture.settled(key: "settling") {
                let next = images.next()
                return try #require(next)
            }
            #expect(try ScreenshotCapture.digest(image) == ScreenshotCapture.digest(final))
            #expect(images.next() == nil)
        }

        @Test func oscillationFailsWithTheKeyAndEverySample() async throws {
            let red = try solid(.red)
            let blue = try solid(.blue)
            var images = [red, blue, red, blue].makeIterator()
            do {
                _ = try await ScreenshotCapture.settled(key: "oscillating") {
                    let next = images.next()
                    return try #require(next)
                }
                Issue.record("An oscillating capture must fail")
            } catch ScreenshotCapture.Failure.unstable(let key, let samples) {
                #expect(key == "oscillating")
                #expect(samples.count == 3)
                #expect(
                    samples.map(\.digest) == [
                        try ScreenshotCapture.digest(red), try ScreenshotCapture.digest(blue),
                        try ScreenshotCapture.digest(red),
                    ])
                #expect(samples.allSatisfy { $0.width == 8 && $0.height == 8 })
                #expect(images.next() != nil)
            }
        }

        @Test func stableWrongPixelsAreNotSelectedToMatchAnExpectation() async throws {
            let red = try solid(.red)
            let expected = try ScreenshotCapture.digest(solid(.blue))
            let actual = try await ScreenshotCapture.settled(key: "wrong") { red }
            #expect(try ScreenshotCapture.digest(actual) != expected)
        }

        @Test func unstableRecordingLeavesTheExistingManifestUntouched() async throws {
            let url = FileManager.default.temporaryDirectory.appendingPathComponent(
                "screenshot-manifest-\(UUID()).json")
            let existing = Data("previous reviewed manifest".utf8)
            try existing.write(to: url)
            defer { try? FileManager.default.removeItem(at: url) }
            let red = try solid(.red)
            let blue = try solid(.blue)
            var images = [red, blue, red].makeIterator()
            await #expect(throws: ScreenshotCapture.Failure.self) {
                try await ScreenshotCapture.record(to: url) {
                    var manifest = ["stable": try ScreenshotCapture.digest(red)]
                    let unstable = try await ScreenshotCapture.settled(key: "unstable") {
                        let next = images.next()
                        return try #require(next)
                    }
                    manifest["unstable"] = try ScreenshotCapture.digest(unstable)
                    return manifest
                }
            }
            #expect(try Data(contentsOf: url) == existing)
        }

        @Test func contrastScopeReachesAppKitAndRestoresAfterNestedFailure() throws {
            let prior = UserDefaults.standard.object(forKey: "FreesideContrast") as? String
            let normal = try LaunchInputs.$screenshotIncreasedContrast.withValue(false) {
                let normal = try ScreenshotCapture.digest(solid(.rule))
                #expect(LaunchInputs.accessibilityContrastOverride() == .standard)
                #expect(throws: ProbeError.self) {
                    try LaunchInputs.$screenshotIncreasedContrast.withValue(true) {
                        #expect(LaunchInputs.accessibilityContrastOverride() == .increased)
                        #expect(try ScreenshotCapture.digest(solid(.rule)) != normal)
                        throw ProbeError.stopped
                    }
                }
                #expect(LaunchInputs.accessibilityContrastOverride() == .standard)
                #expect(try ScreenshotCapture.digest(solid(.rule)) == normal)
                return normal
            }
            #expect(!normal.isEmpty)
            #expect(LaunchInputs.screenshotIncreasedContrast == nil)
            #expect(UserDefaults.standard.object(forKey: "FreesideContrast") as? String == prior)
        }

        private func solid(_ color: Color) throws -> CGImage {
            try ScreenshotCapture.bitmap(ImageRenderer(content: color.frame(width: 8, height: 8)))
        }
    }
#endif
