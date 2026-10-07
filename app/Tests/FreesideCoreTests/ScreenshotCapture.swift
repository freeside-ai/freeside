#if os(macOS)
    import AppKit
    import CryptoKit
    import SwiftUI

    @MainActor enum ScreenshotCapture {
        struct Sample: Equatable {
            let width: Int
            let height: Int
            let digest: String
        }

        enum Failure: Error {
            case bitmapUnavailable
            case unstable(key: String, samples: [Sample])
        }

        /// Draw into an explicit bitmap. ImageRenderer.cgImage's raster path
        /// can change glyph-edge pixels after repeated identical captures.
        static func bitmap<Content: View>(_ renderer: ImageRenderer<Content>) throws -> CGImage {
            var image: CGImage?
            renderer.render(rasterizationScale: 1) { size, draw in
                guard
                    let space = CGColorSpace(name: CGColorSpace.sRGB),
                    let context = CGContext(
                        data: nil, width: Int(ceil(size.width)), height: Int(ceil(size.height)),
                        bitsPerComponent: 8, bytesPerRow: 0, space: space,
                        bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
                else { return }
                // The bitmap rounds up a fractional layout height. Keep the
                // view top-aligned, as cgImage does, instead of adding that
                // fractional remainder above the content.
                context.translateBy(x: 0, y: ceil(size.height) - size.height)
                draw(context)
                image = context.makeImage()
            }
            guard let image else { throw Failure.bitmapUnavailable }
            return image
        }

        static func settled(
            key: String, capture: () throws -> CGImage
        ) async throws -> CGImage {
            var samples: [Sample] = []
            var images: [CGImage] = []
            for _ in 0..<3 {
                let image = try capture()
                let sample = Sample(width: image.width, height: image.height, digest: try digest(image))
                if ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_TRACE"] == "1"
                    || ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_PROBE_KEYS"] != nil
                {
                    print(
                        "SCREENSHOT_SAMPLE \(key) \(samples.count + 1) \(sample.width)x\(sample.height) \(sample.digest)"
                    )
                }
                if samples.last == sample { return image }
                samples.append(sample)
                images.append(image)
                await Task.yield()
            }
            if let output = ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_OUTPUT"] {
                let directory = URL(fileURLWithPath: output, isDirectory: true)
                try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
                for (index, image) in images.enumerated() {
                    let bitmap = NSBitmapImageRep(cgImage: image)
                    guard let png = bitmap.representation(using: .png, properties: [:]) else {
                        throw Failure.bitmapUnavailable
                    }
                    try png.write(
                        to: directory.appendingPathComponent("\(key)-sample-\(index + 1).png"), options: .atomic)
                }
            }
            throw Failure.unstable(key: key, samples: samples)
        }

        // Keep the dimensions-plus-RGBA digest format used by the manifests.
        static func digest(_ image: CGImage) throws -> String {
            let bytesPerRow = image.width * 4
            var pixels = Data(count: bytesPerRow * image.height)
            try pixels.withUnsafeMutableBytes { bytes in
                guard
                    let context = CGContext(
                        data: bytes.baseAddress, width: image.width, height: image.height,
                        bitsPerComponent: 8, bytesPerRow: bytesPerRow,
                        space: CGColorSpaceCreateDeviceRGB(),
                        bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
                else { throw Failure.bitmapUnavailable }
                context.draw(image, in: CGRect(x: 0, y: 0, width: image.width, height: image.height))
            }
            var input = Data("\(image.width)x\(image.height):\(bytesPerRow)\n".utf8)
            input.append(pixels)
            return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
        }

        /// Capture every entry before opening the destination. An unstable
        /// surface must leave the previously reviewed manifest intact.
        static func record(
            to url: URL, capture: () async throws -> [String: String]
        ) async throws {
            let manifest = try await capture()
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
            var data = try encoder.encode(manifest)
            data.append(0x0A)
            try data.write(to: url, options: .atomic)
        }
    }
#endif
