#if os(macOS)
    import AppKit
    import CryptoKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    @Suite(.serialized) @MainActor struct ScreenshotRegressionTests {
        private struct Surface {
            let name: String
            let width: CGFloat?
            let view: AnyView

            init(name: String, width: CGFloat? = nil, view: AnyView) {
                self.name = name
                self.width = width
                self.view = view
            }
        }

        private struct TextSize {
            let name: String
            let value: DynamicTypeSize
        }

        private let canvasWidth: CGFloat = 960
        private let textSizes = [
            TextSize(name: "xsmall", value: .xSmall),
            TextSize(name: "large", value: .large),
            TextSize(name: "xxxlarge", value: .xxxLarge),
            TextSize(name: "ax1", value: .accessibility1),
            TextSize(name: "ax3", value: .accessibility3),
            TextSize(name: "ax5", value: .accessibility5),
        ]

        @Test func surfacesMatchRecordedPixels() async throws {
            _ = FreesideFont.registration
            let recording =
                ProcessInfo.processInfo.environment["FREESIDE_RECORD_SCREENSHOTS"] == "1"
            let overrides = try loadOverrides()
            if recording {
                try validateRecordingOperatingSystem(
                    operatingSystemKey, overrideKeys: Set(overrides.keys))
            }
            let expected = try loadManifest(overrides: overrides)
            var actual: [String: String] = [:]

            for size in textSizes {
                try await FreesideFont.$screenshotDynamicTypeSize.withValue(size.value) {
                    for surface in try makeSurfaces(at: size.value) {
                        let key = "\(surface.name)-\(size.name)"
                        let image = try render(
                            surface.view,
                            at: size.value,
                            width: surface.width ?? canvasWidth)
                        actual[key] = try digest(image)
                        if ProcessInfo.processInfo.environment["FREESIDE_DUMP_SCREENSHOTS"] == "1" {
                            _ = try dump(image, named: key)
                        }
                        if !recording, expected[key] != actual[key] {
                            let dump = try dump(image, named: key)
                            let expectedDigest = expected[key] ?? "missing"
                            let actualDigest = actual[key] ?? "missing"
                            Issue.record(
                                "Screenshot mismatch for \(key): expected \(expectedDigest), got \(actualDigest); inspect \(dump.path) and record only after review."
                            )
                        }
                        await Task.yield()
                    }
                }
            }

            if recording {
                try writeManifest(actual)
            } else {
                #expect(actual.count == expected.count)
                #expect(
                    Set(actual.values).count > 45,
                    "The six-size matrix must exercise more than the prior two-state rendering")
            }
        }

        @Test func recordingRequiresABaselineOperatingSystem() {
            #expect(throws: ScreenshotError.self) {
                try validateRecordingOperatingSystem(
                    "macOS-26.5", overrideKeys: ["macOS-26.5"])
            }
        }

        private func makeSurfaces(at dynamicTypeSize: DynamicTypeSize) throws -> [Surface] {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let inbox = AttentionFixtures.defaultInbox()
            let store = InboxStore(client: client)
            store.replaceAll(with: inbox)

            var surfaces = [
                Surface(
                    name: "inbox",
                    view: AnyView(
                        InboxView(
                            store: store,
                            selection: .constant(inbox.first?.item.id),
                            launchScope: nil,
                            launchProjectID: nil
                        ).screenshotContent()
                    ))
            ]

            for snapshot in inbox {
                let detail = DecisionDetailView(
                    store: store,
                    itemID: snapshot.item.id,
                    showsValidationProgress: false
                )
                surfaces.append(
                    Surface(
                        name: "decision-\(snapshot.item._type.rawValue)",
                        view: AnyView(
                            detail.screenshotCard(snapshot.item, at: dynamicTypeSize))))

                if snapshot.item._type == .review_diminishing_returns {
                    surfaces.append(
                        Surface(
                            name: "decision-review_diminishing_returns-phone",
                            width: 390,
                            view: AnyView(
                                detail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    compactLayout: true))))
                }
            }

            let adjudication = AttentionFixtures.fixture(type: .finding_adjudication).item
            let recommendedDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                recommendation: .init(
                    action: .accept_recommended_route,
                    reason: "This route preserves the evidence-backed finding.",
                    confidence: "High"
                ),
                showsValidationProgress: false
            )
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-recommended",
                    view: AnyView(
                        recommendedDetail.screenshotCard(adjudication, at: dynamicTypeSize))))

            let question = AttentionFixtures.fixture(type: .agent_question).item
            let destructiveRecommendation = DecisionDetailView(
                store: store,
                itemID: question.id,
                recommendation: .init(
                    action: .stop,
                    reason: "Stopping avoids applying an unreviewed answer.",
                    confidence: nil
                ),
                showsValidationProgress: false
            )
            surfaces.append(
                Surface(
                    name: "decision-agent_question-recommended-stop",
                    view: AnyView(
                        destructiveRecommendation.screenshotCard(
                            question, at: dynamicTypeSize))))

            let cache = InMemoryCacheStore()
            let runs = RunFixtures.defaultRuns()
            let schedules = RunFixtures.defaultSchedules()
            try cache.save(
                .init(
                    cursors: .init(
                        syncEpoch: "screenshot-epoch",
                        lastFullSnapshotRevision: 12,
                        highestObservedServerRevision: 12
                    ),
                    attentionItems: inbox,
                    runs: runs,
                    schedules: schedules,
                    runTimelines: RunFixtures.defaultTimelines()
                ))
            let coordinator = SyncCoordinator(client: client, cache: cache)
            guard let activeRun = runs.first(where: { $0.run.id == RunFixtures.activeRunID }) else {
                throw ScreenshotError.missingActiveRun
            }
            surfaces.append(
                Surface(
                    name: "runs-list",
                    view: AnyView(
                        RunsListView(
                            runs: runs,
                            schedules: schedules,
                            selection: .constant(activeRun.run.id)
                        ).screenshotContent()
                    )))
            guard
                let timeline = RunFixtures.defaultTimelines().first(where: {
                    $0.run_id == activeRun.run.id
                })
            else {
                throw ScreenshotError.missingActiveTimeline
            }
            surfaces.append(
                Surface(
                    name: "run-timeline",
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: activeRun)
                            .screenshotContent(timeline)
                    )))

            let pairing = PairingModel(
                client: client,
                credentials: InMemoryCredentialStore(),
                pairingCode: "483911"
            )
            pairing.displayName = "Operator's iPhone"
            surfaces.append(
                Surface(
                    name: "pairing",
                    view: AnyView(PairingView(model: pairing) { _ in }.screenshotContent())
                ))
            return surfaces
        }

        private func render(
            _ view: AnyView,
            at size: DynamicTypeSize,
            width: CGFloat
        ) throws -> CGImage {
            guard let timeZone = TimeZone(secondsFromGMT: 0) else {
                throw ScreenshotError.missingGMT
            }
            let root =
                view
                .environment(\.dynamicTypeSize, size)
                .environment(\.colorScheme, .light)
                .environment(\.locale, Locale(identifier: "en_US_POSIX"))
                .environment(\.calendar, Calendar(identifier: .gregorian))
                .environment(\.timeZone, timeZone)
                .frame(width: width, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)
                .background(Color.ground)
            let renderer = ImageRenderer(content: root)
            renderer.proposedSize = ProposedViewSize(width: width, height: nil)
            renderer.scale = 1
            guard let image = renderer.cgImage else {
                throw ScreenshotError.renderFailed
            }
            return image
        }

        private func digest(_ image: CGImage) throws -> String {
            let bytesPerRow = image.width * 4
            var pixels = Data(count: bytesPerRow * image.height)
            try pixels.withUnsafeMutableBytes { bytes in
                guard
                    let context = CGContext(
                        data: bytes.baseAddress,
                        width: image.width,
                        height: image.height,
                        bitsPerComponent: 8,
                        bytesPerRow: bytesPerRow,
                        space: CGColorSpaceCreateDeviceRGB(),
                        bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue
                    )
                else { throw ScreenshotError.bitmapContextFailed }
                context.draw(image, in: CGRect(x: 0, y: 0, width: image.width, height: image.height))
            }
            var input = Data("\(image.width)x\(image.height):\(bytesPerRow)\n".utf8)
            input.append(pixels)
            return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
        }

        private func loadManifest(
            overrides: [String: [String: String]]
        ) throws -> [String: String] {
            guard
                let url = Bundle.module.url(
                    forResource: "ScreenshotDigests", withExtension: "json")
            else { throw ScreenshotError.missingManifest }
            let baseline = try JSONDecoder().decode(
                [String: String].self, from: Data(contentsOf: url))
            return baseline.merging(overrides[operatingSystemKey] ?? [:]) { _, override in
                override
            }
        }

        private func loadOverrides() throws -> [String: [String: String]] {
            guard
                let overridesURL = Bundle.module.url(
                    forResource: "ScreenshotDigestOverrides", withExtension: "json")
            else { return [:] }
            return try JSONDecoder().decode(
                [String: [String: String]].self,
                from: Data(contentsOf: overridesURL))
        }

        private func validateRecordingOperatingSystem(
            _ key: String,
            overrideKeys: Set<String>
        ) throws {
            guard !overrideKeys.contains(key) else {
                throw ScreenshotError.recordingRequiresBaselineOperatingSystem(key)
            }
        }

        private var operatingSystemKey: String {
            let version = ProcessInfo.processInfo.operatingSystemVersion
            return "macOS-\(version.majorVersion).\(version.minorVersion)"
        }

        private func writeManifest(_ manifest: [String: String]) throws {
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
            let sourceURL = URL(fileURLWithPath: #filePath)
                .deletingLastPathComponent()
                .appendingPathComponent("Resources/ScreenshotDigests.json")
            var data = try encoder.encode(manifest)
            data.append(0x0A)
            try data.write(to: sourceURL, options: .atomic)
        }

        private func dump(_ image: CGImage, named name: String) throws -> URL {
            let directory = FileManager.default.temporaryDirectory
                .appendingPathComponent("freeside-screenshot-regressions", isDirectory: true)
            try FileManager.default.createDirectory(
                at: directory, withIntermediateDirectories: true)
            let url = directory.appendingPathComponent("\(name).png")
            let bitmap = NSBitmapImageRep(cgImage: image)
            guard let png = bitmap.representation(using: .png, properties: [:]) else {
                throw ScreenshotError.pngEncodingFailed
            }
            try png.write(to: url, options: .atomic)
            return url
        }
    }

    private enum ScreenshotError: Error {
        case bitmapContextFailed
        case missingActiveRun
        case missingActiveTimeline
        case missingGMT
        case missingManifest
        case pngEncodingFailed
        case recordingRequiresBaselineOperatingSystem(String)
        case renderFailed
    }
#endif
