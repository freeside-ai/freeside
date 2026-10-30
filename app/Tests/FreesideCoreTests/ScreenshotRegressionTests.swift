#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    @Suite(.serialized) @MainActor struct ScreenshotRegressionTests {
        private struct Surface {
            let name: String
            let width: CGFloat?
            let colorScheme: ColorScheme
            let contrast: LaunchInputs.Contrast?
            let nativeAppearance: Bool
            let view: AnyView

            init(
                name: String,
                width: CGFloat? = nil,
                colorScheme: ColorScheme = .light,
                contrast: LaunchInputs.Contrast? = nil,
                nativeAppearance: Bool = false,
                view: AnyView
            ) {
                self.name = name
                self.width = width
                self.colorScheme = colorScheme
                self.contrast = contrast
                self.nativeAppearance = nativeAppearance
                self.view = view
            }
        }

        private struct TextSize {
            let name: String
            let value: DynamicTypeSize
        }

        private let canvasWidth: CGFloat = 960
        /// A detail column just wide enough for the one-column card at its
        /// cap (R18): the card and the pane's 24pt margins.
        private static let detailColumnWidth = DecisionCardComposition.Scale.cardWidth + 48
        private let baselineOperatingSystemKey = "macOS-26.7"
        private let screenshotNow = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        private let screenshotTimeZone = TimeZone(identifier: "UTC") ?? .current
        private let textSizes = [
            TextSize(name: "xsmall", value: .xSmall),
            TextSize(name: "large", value: .large),
            TextSize(name: "xxxlarge", value: .xxxLarge),
            TextSize(name: "ax1", value: .accessibility1),
            TextSize(name: "ax3", value: .accessibility3),
            TextSize(name: "ax5", value: .accessibility5),
        ]
        /// The sizes `surfacesMatchRecordedPixels` compares a light surface
        /// at. Every explicit Dynamic Type check in `app/Sources` branches at
        /// `.xxxLarge` or at the accessibility sizes, so these four reach
        /// each of those branches; `xsmall` and `ax3` are given up, along
        /// with any `ViewThatFits` fallback only they would select. The probe
        /// and `render-surfaces.sh` still take all six `textSizes` (#2250).
        private static let lightMatrixSizes: Set<String> = ["large", "xxxlarge", "ax1", "ax5"]
        /// Dark surfaces that also compare at `ax5`. Dark mode changes colour,
        /// not layout, so a dark surface compares at `large` alone; these keep
        /// one wrapped-text case per family, where a contrast bug could show
        /// only at a large size.
        private static let darkWrappedTextSpecimens: Set<String> = [
            "inbox-sidebar-dark",
            "discuss-composer-phone-dark",
            "new-task-sheet-phone-filled-dark",
            "run-review-findings-390-dark",
            "decision-ready-summary-wrapped-collapsed-390-dark",
            "task-history-390-dark",
        ]

        /// The text sizes the comparison matrix renders a surface at. Every
        /// dark size is also a light size, so `lightMatrixSizes` bounds the
        /// sizes the matrix visits.
        private static func matrixSizeNames(for name: String, colorScheme: ColorScheme) -> Set<String> {
            guard colorScheme == .dark else { return lightMatrixSizes }
            return darkWrappedTextSpecimens.contains(name) ? ["large", "ax5"] : ["large"]
        }

        /// One slice of the comparison matrix, so CI can render the matrix as
        /// concurrent processes (#2292). Read from
        /// `FREESIDE_SCREENSHOT_SHARD=<index>/<count>`, with a zero-based
        /// index; unset means the whole matrix.
        private struct ScreenshotShard: Equatable {
            let index: Int
            let count: Int

            static func parse(_ selector: String?) throws -> ScreenshotShard? {
                guard let selector else { return nil }
                let parts = selector.split(separator: "/", omittingEmptySubsequences: false)
                guard parts.count == 2, let index = Int(parts[0]), let count = Int(parts[1]),
                    count >= 1, (0..<count).contains(index)
                else { throw ScreenshotError.invalidShardSelection(selector) }
                return ScreenshotShard(index: index, count: count)
            }
        }

        /// The manifest keys slice `index` of `count` owns: the keys whose
        /// position in the sorted key list, modulo `count`, is `index`. The
        /// slices are disjoint, cover the manifest, and differ in size by at
        /// most one.
        private static func shardKeys(
            _ keys: some Collection<String>, index: Int, count: Int
        ) -> Set<String> {
            Set(keys.sorted().enumerated().filter { $0.offset % count == index }.map(\.element))
        }

        @Test func screenshotMetricsUseTheirOwnTextStyleBaseline() {
            FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
                #expect(screenshotMetricBase(28, relativeTo: .title) == 28)
                #expect(screenshotMetricBase(4, relativeTo: .callout) == 4)
            }
            FreesideFont.$screenshotDynamicTypeSize.withValue(.accessibility5) {
                #expect(screenshotMetricBase(28, relativeTo: .title) == 58)
                #expect(screenshotMetricBase(4, relativeTo: .callout) == 9)
            }
        }

        @Test func surfacesMatchRecordedPixels() async throws {
            _ = FreesideFont.registration
            let recording =
                ProcessInfo.processInfo.environment["FREESIDE_RECORD_SCREENSHOTS"] == "1"
            let shard = try ScreenshotShard.parse(
                ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_SHARD"])
            let overrides = try loadOverrides()
            if recording {
                // The manifest is written from what was rendered, so it
                // must come from the whole matrix.
                guard shard == nil else { throw ScreenshotError.shardedRecordingRefused }
                try validateRecordingHost(hosted: isHostedRunner)
                try validateRecordingOperatingSystem(
                    operatingSystemKey, baselineKey: baselineOperatingSystemKey)
            }
            let expected = try loadManifest(overrides: overrides)
            if recording {
                let sourceURL = URL(fileURLWithPath: #filePath)
                    .deletingLastPathComponent()
                    .appendingPathComponent("Resources/ScreenshotDigests.json")
                try await ScreenshotCapture.record(to: sourceURL) {
                    try await captureSurfaces(expected: expected, recording: true)
                }
            } else if let shard {
                let slice = Self.shardKeys(expected.keys, index: shard.index, count: shard.count)
                let actual = try await captureSurfaces(expected: expected, recording: false) { key in
                    // A key the manifest lacks renders in slice 0 alone, so
                    // a new surface fails exactly one slice as `missing`.
                    expected[key] == nil ? shard.index == 0 : slice.contains(key)
                }
                #expect(Set(actual.keys).intersection(expected.keys) == slice)
                // When every slice passes, each manifest key rendered its
                // recorded digest and no other key rendered, so the
                // renders equal the manifest and slice 0 checks it whole.
                if shard.index == 0 {
                    expectWholeMatrix(expected)
                }
            } else {
                let actual = try await captureSurfaces(expected: expected, recording: false)
                #expect(actual.count == expected.count)
                expectWholeMatrix(actual)
            }
        }

        private func expectWholeMatrix(_ digests: [String: String]) {
            #expect(
                Set(digests.values).count > 45,
                "The size matrix must exercise more than the prior two-state rendering")
            for specimen in Self.darkWrappedTextSpecimens {
                // A light surface also renders at ax5, so a dark one is
                // the specimen that renders there but not at ax1.
                #expect(
                    digests["\(specimen)-ax5"] != nil && digests["\(specimen)-ax1"] == nil,
                    "\(specimen) names no dark surface")
            }
        }

        private func captureSurfaces(
            expected: [String: String],
            recording: Bool,
            rendersKey: (String) -> Bool = { _ in true }
        ) async throws -> [String: String] {
            var actual: [String: String] = [:]

            for size in textSizes where Self.lightMatrixSizes.contains(size.name) {
                try await FreesideFont.$screenshotDynamicTypeSize.withValue(size.value) {
                    for surface in try await makeSurfaces(at: size.value)
                    where Self.matrixSizeNames(for: surface.name, colorScheme: surface.colorScheme)
                        .contains(size.name)
                    {
                        let key = "\(surface.name)-\(size.name)"
                        guard rendersKey(key) else { continue }
                        let image = try await render(
                            surface.view,
                            at: size.value,
                            width: surface.width ?? canvasWidth,
                            colorScheme: surface.colorScheme,
                            contrast: surface.contrast,
                            nativeAppearance: surface.nativeAppearance, key: key)
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

                    for supplemental in [
                        try await makeUnavailableAttachmentSurface(at: size.value),
                        try await makeImageAttachmentSurface(at: size.value),
                    ]
                    where Self.matrixSizeNames(for: supplemental.name, colorScheme: supplemental.colorScheme)
                        .contains(size.name)
                    {
                        let key = "\(supplemental.name)-\(size.name)"
                        guard rendersKey(key) else { continue }
                        let image = try await render(
                            supplemental.view,
                            at: size.value,
                            width: supplemental.width ?? canvasWidth,
                            colorScheme: supplemental.colorScheme, key: key)
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
                    }
                }
            }

            // Exercise the mounted countdown after the complete matrix so
            // this check also covers rendering state left by other surfaces.
            // A slice runs it after its own renders only, half the state at
            // CI's two slices.
            guard
                let pairingRepetitions = Int(
                    ProcessInfo.processInfo.environment["FREESIDE_PAIRING_PROBE_REPETITIONS"] ?? "1"),
                pairingRepetitions > 0
            else { throw ScreenshotError.invalidDiagnosticSelection }
            for repetition in 0..<pairingRepetitions {
                if pairingRepetitions > 1 { print("PAIRING_PROBE repetition=\(repetition + 1)") }
                for remaining in [30.0, 270.0, 330.0, 870.0] {
                    try mountedTimelineMatchesWallClock(remaining: remaining)
                }
            }

            return actual
        }

        /// Diagnostic captures never enter the recording path. The selectors
        /// name existing matrix keys, so a subset cannot replace the manifest.
        @Test func probeScreenshotDeterminism() async throws {
            let environment = ProcessInfo.processInfo.environment
            guard let selection = environment["FREESIDE_SCREENSHOT_PROBE_KEYS"] else { return }
            guard environment["FREESIDE_RECORD_SCREENSHOTS"] != "1" else {
                throw ScreenshotError.diagnosticRecordingRefused
            }
            _ = FreesideFont.registration
            let keys = Set(selection.split(separator: ",").map(String.init))
            var found: Set<String> = []
            let mode = environment["FREESIDE_SCREENSHOT_PROBE_MODE"] ?? "selected"
            let probeContrast = environment["FREESIDE_SCREENSHOT_PROBE_CONTRAST"].flatMap(
                LaunchInputs.Contrast.init(rawValue:))
            if environment["FREESIDE_SCREENSHOT_PROBE_CONTRAST"] != nil, probeContrast == nil {
                throw ScreenshotError.invalidDiagnosticSelection
            }
            guard let repetitions = Int(environment["FREESIDE_SCREENSHOT_PROBE_REPETITIONS"] ?? "1"), repetitions > 0
            else {
                throw ScreenshotError.invalidDiagnosticSelection
            }
            guard ["selected", "reverse", "construct", "render", "prefix", "contrast"].contains(mode) else {
                throw ScreenshotError.invalidDiagnosticSelection
            }
            for size in textSizes where keys.contains(where: { $0.hasSuffix("-\(size.name)") }) {
                try await FreesideFont.$screenshotDynamicTypeSize.withValue(size.value) {
                    let surfaces = try await makeSurfaces(at: size.value)
                    // Constructing extra fixtures and rendering them are
                    // distinct probes: their effects need not have one cause.
                    let extras: [Surface]
                    if mode == "prefix" {
                        extras = Array(surfaces.prefix { !keys.contains("\($0.name)-\(size.name)") })
                    } else {
                        extras = mode == "construct" || mode == "render" ? try await makeSurfaces(at: size.value) : []
                    }
                    if mode == "render" || mode == "prefix" {
                        for surface in extras {
                            _ = try await render(
                                surface.view, at: size.value, width: surface.width ?? canvasWidth,
                                colorScheme: surface.colorScheme, contrast: surface.contrast,
                                nativeAppearance: surface.nativeAppearance,
                                key: "extra-\(surface.name)-\(size.name)")
                        }
                    }
                    let selected = surfaces.filter { keys.contains("\($0.name)-\(size.name)") }
                    for surface in mode == "reverse" ? Array(selected.reversed()) : selected {
                        let key = "\(surface.name)-\(size.name)"
                        var firstDigest: String?
                        for repetition in 0..<repetitions {
                            if mode == "contrast" {
                                _ = try await render(
                                    surface.view, at: size.value, width: surface.width ?? canvasWidth,
                                    colorScheme: surface.colorScheme, contrast: .increased,
                                    nativeAppearance: surface.nativeAppearance, key: "increased-\(key)")
                            }
                            let image = try await render(
                                surface.view, at: size.value, width: surface.width ?? canvasWidth,
                                colorScheme: surface.colorScheme, contrast: probeContrast ?? surface.contrast,
                                nativeAppearance: surface.nativeAppearance, key: key)
                            let actual = try digest(image)
                            print("SCREENSHOT_RESULT \(key) \(image.width)x\(image.height) \(actual)")
                            if let firstDigest {
                                #expect(actual == firstDigest, "\(key), repetition \(repetition + 1)")
                            } else {
                                firstDigest = actual
                            }
                            _ = try dump(image, named: repetitions == 1 ? key : "\(key)-repeat-\(repetition + 1)")
                        }
                        found.insert(key)
                    }
                    withExtendedLifetime(extras) {}
                }
            }
            guard found == keys, !found.isEmpty else {
                throw ScreenshotError.invalidDiagnosticSelection
            }
        }

        @Test func taskHistoryEvidence() async throws {
            _ = FreesideFont.registration
            let history = TaskHistoryFixtures.history(.published)
            let coordinator = try TaskHistoryFixtures.coordinator(history)
            for size in [DynamicTypeSize.large, .accessibility1] {
                for width in [CGFloat(820), CGFloat(390)] {
                    for scheme in [ColorScheme.light, .dark] {
                        let view = TaskTimelineView(
                            coordinator: coordinator, snapshot: TaskHistoryFixtures.snapshot(history),
                            onOpenRun: { _ in }
                        ).screenshotContent(history)
                        let image = try await FreesideFont.$screenshotDynamicTypeSize.withValue(size) {
                            try await render(
                                AnyView(view), at: size, width: width, colorScheme: scheme, nativeAppearance: true)
                        }
                        #expect(image.width == Int(width))
                        if ProcessInfo.processInfo.environment["FREESIDE_DUMP_SCREENSHOTS"] == "1" {
                            _ = try dump(image, named: "task-history-evidence-\(Int(width))-\(scheme)-\(size)")
                        }
                    }
                }
            }
        }

        @Test func publishedHandoffSurfaces() async throws {
            _ = FreesideFont.registration
            let runs = RunFixtures.defaultRuns()
            let tasks = TaskFixtures.defaultTasks()
            let published = try #require(runs.first { $0.run.id == RunFixtures.readyRunID })
            let task = try #require(tasks.first { $0.task.id == published.run.task_id })
            var digests: [String: String] = [:]
            for state in ["clean", "degraded", "offline", "verification"] {
                let items =
                    state == "verification" ? [] : [AttentionFixtures.publishedTaskReady(degraded: state == "degraded")]
                let server = MockServer(items: items)
                let cache = InMemoryCacheStore()
                let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
                await coordinator.refresh()
                let display =
                    state == "offline"
                    ? SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache) : coordinator
                if state == "offline" { display.store.freshness = .unreachable }
                for width in [CGFloat(960), 390] {
                    for scheme in [ColorScheme.light, .dark] {
                        let view = VStack(alignment: .leading, spacing: 16) {
                            FreshnessBanner(freshness: display.store.freshness)
                            TaskRowView(
                                task: task.task,
                                position: TaskDisplay.position(
                                    task.task, runs: runs, attentionItems: display.store.orderedSnapshots),
                                isSelected: false, now: RunFixtures.screenshotInstant)
                            Divider()
                            RunTimelineView(coordinator: display, snapshot: published).header
                        }
                        .padding(16)
                        .foregroundStyle(Color.ink)
                        .background(Color.ground)
                        let image = try await FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
                            try await render(AnyView(view), at: .large, width: width, colorScheme: scheme)
                        }
                        #expect(image.width == Int(width))
                        digests["\(state)-\(Int(width))-\(scheme)"] = try digest(image)
                        if ProcessInfo.processInfo.environment["FREESIDE_DUMP_SCREENSHOTS"] == "1" {
                            _ = try dump(image, named: "handoff-\(state)-\(Int(width))-\(scheme)")
                        }
                    }
                }
            }
            for width in [960, 390] {
                for scheme in [ColorScheme.light, .dark] {
                    let suffix = "\(width)-\(scheme)"
                    #expect(digests["clean-\(suffix)"] != digests["verification-\(suffix)"])
                    #expect(digests["degraded-\(suffix)"] != digests["clean-\(suffix)"])
                    #expect(digests["offline-\(suffix)"] != digests["clean-\(suffix)"])
                }
            }
        }

        @Test func recordingIsRefusedOnAHostedRunner() throws {
            try validateRecordingHost(hosted: false)
            #expect(throws: ScreenshotError.self) {
                try validateRecordingHost(hosted: true)
            }
        }

        @Test func explicitContrastReachesCaptureWithoutPersistentWrites() async throws {
            let prior = UserDefaults.standard.object(forKey: "FreesideContrast") as? String
            for nativeAppearance in [false, true] {
                let view = AnyView(Color.rule.frame(height: 8))
                let standard = try await render(
                    view, at: .large, width: 8, colorScheme: .light,
                    contrast: .standard, nativeAppearance: nativeAppearance)
                let increased = try await render(
                    view, at: .large, width: 8, colorScheme: .light,
                    contrast: .increased, nativeAppearance: nativeAppearance)
                #expect(try digest(standard) != digest(increased))
                await #expect(throws: ScreenshotCapture.Failure.self) {
                    _ = try await render(
                        view, at: .large, width: 0, colorScheme: .light,
                        contrast: .increased, nativeAppearance: nativeAppearance)
                }
                #expect(LaunchInputs.screenshotIncreasedContrast == nil)
                #expect(UserDefaults.standard.object(forKey: "FreesideContrast") as? String == prior)
            }
        }

        @Test func recordingRequiresTheDesignatedBaselineOperatingSystem() throws {
            try validateRecordingOperatingSystem(
                "macOS-26.7", baselineKey: "macOS-26.7")
            #expect(throws: ScreenshotError.self) {
                try validateRecordingOperatingSystem(
                    "macOS-26.6", baselineKey: "macOS-26.7")
            }
        }

        @Test func matrixSizesCoverEveryLayoutBreakpointButRepeatNone() {
            #expect(Self.matrixSizeNames(for: "inbox-sidebar", colorScheme: .light) == Self.lightMatrixSizes)
            #expect(Self.lightMatrixSizes == ["large", "xxxlarge", "ax1", "ax5"])
            #expect(Self.matrixSizeNames(for: "inbox-dark", colorScheme: .dark) == ["large"])
            for specimen in Self.darkWrappedTextSpecimens {
                #expect(Self.matrixSizeNames(for: specimen, colorScheme: .dark) == ["large", "ax5"])
            }
            let sizeNames = Set(textSizes.map(\.name))
            #expect(Self.lightMatrixSizes.isSubset(of: sizeNames))
            #expect(Self.lightMatrixSizes.isDisjoint(with: ["xsmall", "ax3"]))
        }

        @Test func digestFilesResolveAndAMissingOneFailsLoudly() throws {
            #expect(!(try loadManifest(overrides: try loadOverrides()).isEmpty))
            #expect(throws: ScreenshotError.self) {
                try digestFileURL(named: "NoSuchDigestFile")
            }
        }

        @Test func shardsPartitionTheManifest() throws {
            let keys = Set(try loadManifest(overrides: [:]).keys)
            for count in [2, 3] {
                let slices = (0..<count).map { Self.shardKeys(keys, index: $0, count: count) }
                #expect(slices.reduce(into: Set<String>()) { $0.formUnion($1) } == keys)
                for first in slices.indices {
                    for second in slices.indices where second > first {
                        #expect(slices[first].isDisjoint(with: slices[second]))
                    }
                }
                let sizes = slices.map(\.count)
                #expect(try #require(sizes.max()) - (try #require(sizes.min())) <= 1)
            }
            for malformed in ["3/3", "a/b", "0/0", "1", "-1/3", "0/1/2", "/3", ""] {
                #expect(throws: ScreenshotError.self) { try ScreenshotShard.parse(malformed) }
            }
            #expect(try ScreenshotShard.parse(nil) == nil)
            #expect(try ScreenshotShard.parse("0/1") == ScreenshotShard(index: 0, count: 1))
            #expect(try ScreenshotShard.parse("2/3") == ScreenshotShard(index: 2, count: 3))
        }

        /// Mirrors the mock's read-time availability projection so the store's
        /// pre-loaded phases match what production renders after sync: an image
        /// or referenced claim is available only when its digest's bytes are
        /// held, and an inline text claim renders in-band (no load needed).
        private func preloadReferences(
            _ item: Components.Schemas.AttentionItem,
            into store: InboxStore,
            withoutBytesFor absent: Set<String> = []
        ) async {
            let attachments = AttentionFixtures.defaultAttachments().filter { !absent.contains($0.key) }
            func reference(
                _ metadata: Components.Schemas.EvidenceMetadata,
                digest: String
            ) -> AttachmentReference {
                var metadata = metadata
                metadata.availability = attachments[digest] != nil ? .available : .bytes_absent
                return AttachmentReference(metadata)
            }
            for artifact in item.evidence_snapshot {
                await store.attachments.load(
                    artifact.digest,
                    reference: reference(artifact.metadata, digest: artifact.digest))
            }
            for claim in item.agent_claims where claim.text == nil {
                await store.attachments.load(
                    claim.digest, reference: reference(claim.metadata, digest: claim.digest))
            }
        }

        private func makeSurfaces(at dynamicTypeSize: DynamicTypeSize) async throws -> [Surface] {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let inbox = AttentionFixtures.defaultInbox()
            let store = InboxStore(client: client, now: { screenshotNow })
            store.replaceAll(with: inbox)
            store.replaceAllConversations(with: AttentionFixtures.defaultConversations())
            for snapshot in inbox {
                await preloadReferences(snapshot.item, into: store)
            }

            var surfaces = [
                Surface(
                    name: "inbox",
                    view: AnyView(
                        InboxView(
                            store: store,
                            selection: .constant(inbox.first?.item.id),
                            launchScope: nil,
                            launchProjectID: nil
                        ).screenshotContent(now: screenshotNow)
                    ))
            ]
            // The macOS sidebar chrome in the design language: the section
            // switcher and scope control as Freeside segments, the project
            // trigger, and the first rows, by day and by dusk (the size loop
            // covers the stacked layout from xxxLarge up). The switcher
            // carries both counts, so the task fixtures supply the second.
            let sidebarTaskCount = TasksListView.activeCount(in: TaskFixtures.defaultTasks(), loaded: true)
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "inbox-sidebar" + (colorScheme == .dark ? "-dark" : ""),
                        width: 320,
                        colorScheme: colorScheme,
                        view: AnyView(
                            InboxView(
                                store: store,
                                selection: .constant(inbox.first?.item.id),
                                launchScope: nil,
                                launchProjectID: nil
                            ).screenshotSidebar(now: screenshotNow, activeTaskCount: sidebarTaskCount))))
            }
            let feedback = DecisionFeedbackModel(
                announce: { _ in },
                schedule: { _, _ in Task {} })
            feedback.present(
                .init(
                    itemID: "item-spec_approval",
                    actionLabel: "Approve",
                    resultingStatus: .resolved,
                    at: screenshotNow),
                advance: {})
            surfaces.append(
                Surface(
                    name: "decision-feedback",
                    width: 640,
                    view: AnyView(
                        DecisionFeedbackBanner(feedback: feedback, onView: { _ in })
                            .padding())))
            surfaces.append(
                Surface(
                    name: "decision-feedback-dark",
                    width: 640,
                    colorScheme: .dark,
                    view: AnyView(
                        DecisionFeedbackBanner(feedback: feedback, onView: { _ in })
                            .padding())))
            surfaces.append(
                Surface(name: "attachment-states", width: 480, view: await attachmentStates()))
            surfaces.append(
                Surface(
                    name: "message-composer",
                    width: 480,
                    view: AnyView(
                        MessageComposerSheet(
                            eyebrow: "Request Changes",
                            ask: "What should the specification change?",
                            submitLabel: "Request Changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _, _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-phone",
                    width: 390,
                    view: AnyView(
                        MessageComposerSheet(
                            eyebrow: "Request Changes",
                            ask: "What should the specification change?",
                            submitLabel: "Request Changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _, _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-dark",
                    width: 480,
                    colorScheme: .dark,
                    view: AnyView(
                        MessageComposerSheet(
                            eyebrow: "Request Changes",
                            ask: "What should the specification change?",
                            submitLabel: "Request Changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _, _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        MessageComposerSheet(
                            eyebrow: "Request Changes",
                            ask: "What should the specification change?",
                            submitLabel: "Request Changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _, _ in true }))))

            // The Discuss composer (R11): the item's thread above the field
            // on Mac and phone, and a first message, which has no thread
            // section to draw.
            let discussThread = AttentionFixtures.defaultConversations()[0]
            for (name, width, scheme, drawsThread) in [
                ("discuss-composer", CGFloat(480), ColorScheme.light, true),
                ("discuss-composer-phone", CGFloat(390), ColorScheme.light, true),
                ("discuss-composer-dark", CGFloat(480), ColorScheme.dark, true),
                ("discuss-composer-phone-dark", CGFloat(390), ColorScheme.dark, true),
                ("discuss-composer-first", CGFloat(480), ColorScheme.light, false),
            ] {
                surfaces.append(
                    Surface(
                        name: name,
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            MessageComposerSheet(
                                eyebrow: "Discuss",
                                ask: "What do you want to ask the agent?",
                                consequence: "The item stays open while the agent replies.",
                                submitLabel: "Send",
                                rendersInteractiveControls: false,
                                thread: drawsThread
                                    ? .init(
                                        snapshot: discussThread, attachments: store.attachments,
                                        loadsAttachments: false, now: screenshotNow)
                                    : nil,
                                submit: { _, _ in true }))))
            }

            // The New Task composer (#1330): the project picker trigger, the
            // source field, and the optional name, on Mac and phone. Empty
            // and filled are each drawn by day and by dusk, so the fill rule
            // shows in both appearances: Submit takes the disabled recipe on
            // the empty form and its fill on the valid one.
            // ImageRenderer draws the static stand-ins, not the live
            // TextEditor or Picker; the toolbar action and open sheet are in
            // the PR's real-app screenshots.
            let newTaskProjects = TaskDisplay.knownProjects(in: TaskFixtures.defaultTasks())
            // The composer only opens on a settled sync, so the goldens depict
            // that state: a fresh coordinator keeps the filled surface's Submit
            // enabled (the action is gated on `.fresh`).
            let newTaskCoordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
            newTaskCoordinator.store.freshness = .fresh
            let newTaskModel = TaskSubmissionModel(coordinator: newTaskCoordinator)
            let filledSource =
                "Add a /health endpoint returning the daemon version and uptime, with a test."
            for (name, width, scheme, draftSource, draftName) in [
                ("new-task-sheet", CGFloat(380), ColorScheme.light, "", ""),
                ("new-task-sheet-filled-dark", CGFloat(380), ColorScheme.dark, filledSource, "Health endpoint"),
                ("new-task-sheet-phone", CGFloat(390), ColorScheme.light, "", ""),
                ("new-task-sheet-phone-filled-dark", CGFloat(390), ColorScheme.dark, filledSource, "Health endpoint"),
                ("new-task-sheet-dark", CGFloat(380), ColorScheme.dark, "", ""),
                ("new-task-sheet-filled", CGFloat(380), ColorScheme.light, filledSource, "Health endpoint"),
                ("new-task-sheet-phone-dark", CGFloat(390), ColorScheme.dark, "", ""),
                ("new-task-sheet-phone-filled", CGFloat(390), ColorScheme.light, filledSource, "Health endpoint"),
            ] {
                surfaces.append(
                    Surface(
                        name: name,
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            NewTaskSheet(
                                projects: newTaskProjects,
                                model: newTaskModel,
                                onSubmitted: { _ in },
                                rendersInteractiveControls: false,
                                initialSource: draftSource,
                                initialName: draftName))))
            }

            let recoveryModel = try TaskSubmissionRecoveryScreenshots.model(client: client)
            for (name, width, scheme) in [
                ("new-task-recovery", CGFloat(380), ColorScheme.light),
                ("new-task-recovery-dark", CGFloat(380), ColorScheme.dark),
                ("new-task-recovery-phone", CGFloat(390), ColorScheme.light),
                ("new-task-recovery-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: name, width: width, colorScheme: scheme,
                        view: AnyView(
                            TaskSubmissionRecoverySheet(
                                model: recoveryModel, onRecovered: { _ in },
                                rendersInteractiveControls: false))))
            }

            // The Devices sheet (#981) with every state it renders: this
            // device, another active one, and a revoked one never seen.
            // The phone's own card also shows how to set its notifications
            // up (#1924): here on a server that is not ntfy's own, after a
            // notification was opened.
            let devicesModel = await DevicesScreenshots.model()
            let phoneDevicesModel = await DevicesScreenshots.model(
                notificationSubscription: .mock, openedAt: screenshotNow.addingTimeInterval(-3600))
            for (name, width, scheme, model) in [
                ("devices", CGFloat(380), ColorScheme.light, devicesModel),
                ("devices-dark", CGFloat(380), ColorScheme.dark, devicesModel),
                ("devices-phone", CGFloat(390), ColorScheme.light, phoneDevicesModel),
                ("devices-phone-dark", CGFloat(390), ColorScheme.dark, phoneDevicesModel),
            ] {
                surfaces.append(
                    Surface(
                        name: name, width: width, colorScheme: scheme,
                        view: AnyView(DevicesView(model: model, rendersInteractiveControls: false))))
            }

            // The notification setup the iPhone offers after pairing: on
            // ntfy's own server, so only the topic is asked for, before any
            // notification was opened. The Devices card above draws the same
            // steps for another server, after one was.
            let hostedSubscription = try #require(
                DeviceNtfySubscription(serverURL: "https://ntfy.sh", topic: DeviceNtfySubscription.mock.topic))
            for (name, scheme) in [
                ("notification-setup-phone", ColorScheme.light),
                ("notification-setup-phone-dark", ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: name, width: 390, colorScheme: scheme,
                        view: AnyView(
                            PhoneNotificationSetupSheet(
                                subscription: hostedSubscription, opens: NotificationOpenLog(defaults: nil),
                                deviceID: DeviceIdentity.mock.deviceID, rendersInteractiveControls: false))))
            }

            // The Answer-and-retry composer with the route picker (#1083): the
            // operator answers and chooses whether to retry the implementer or
            // revise the specification. Captured on Mac and phone, light and dark.
            let routedComposer: [Components.Schemas.AnswerRoute] = [
                .retry_implementation, .revise_specification,
            ]
            for (name, width, scheme) in [
                ("answer-retry-composer", CGFloat(480), ColorScheme.light),
                ("answer-retry-composer-phone", CGFloat(390), ColorScheme.light),
                ("answer-retry-composer-dark", CGFloat(480), ColorScheme.dark),
                ("answer-retry-composer-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: name,
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            MessageComposerSheet(
                                eyebrow: "Answer and Retry",
                                ask: "What is your answer?",
                                submitLabel: "Answer and Retry",
                                byteLimit: 8192,
                                rendersInteractiveControls: false,
                                routeOptions: routedComposer,
                                submit: { _, _ in true }))))
            }

            let awaitingStore = InboxStore(client: client)
            let awaitingItem = AttentionFixtures.fixture(type: .spec_approval)
            var awaitingConversation = AttentionFixtures.defaultConversations()[0]
            awaitingConversation.conversation.status = .awaiting_agent
            awaitingStore.replaceAll(with: [awaitingItem])
            awaitingStore.replaceAllConversations(with: [awaitingConversation])
            let awaitingDetail = DecisionDetailView(
                store: awaitingStore,
                itemID: awaitingItem.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-awaiting",
                    view: AnyView(
                        awaitingDetail.screenshotCard(
                            awaitingItem.item, at: dynamicTypeSize))))
            let awaitingPhoneDetail = DecisionDetailView(
                store: awaitingStore,
                itemID: awaitingItem.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-awaiting-phone",
                    width: 390,
                    view: AnyView(
                        awaitingPhoneDetail.screenshotCard(
                            awaitingItem.item, at: dynamicTypeSize))))
            let awaitingDarkDetail = DecisionDetailView(
                store: awaitingStore,
                itemID: awaitingItem.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-awaiting-dark",
                    colorScheme: .dark,
                    view: AnyView(
                        awaitingDarkDetail.screenshotCard(
                            awaitingItem.item, at: dynamicTypeSize))))
            let awaitingPhoneDarkDetail = DecisionDetailView(
                store: awaitingStore,
                itemID: awaitingItem.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-awaiting-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        awaitingPhoneDarkDetail.screenshotCard(
                            awaitingItem.item, at: dynamicTypeSize))))

            let replacementStore = InboxStore(client: client)
            var superseded = AttentionFixtures.fixture(type: .spec_approval)
            superseded.item.status = .superseded
            superseded.item.decided_at = screenshotNow
            var replacement = AttentionFixtures.fixture(type: .spec_approval)
            replacement.item.id = "item-spec_approval-revision-2"
            replacement.item.item_version = 2
            replacement.item.conversation_id = nil
            replacementStore.replaceAll(with: [superseded, replacement])
            let replacementDetail = DecisionDetailView(
                store: replacementStore,
                itemID: superseded.item.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-superseded-link",
                    view: AnyView(
                        replacementDetail.screenshotCard(
                            superseded.item, at: dynamicTypeSize))))
            let replacementPhoneDetail = DecisionDetailView(
                store: replacementStore,
                itemID: superseded.item.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-superseded-link-phone",
                    width: 390,
                    view: AnyView(
                        replacementPhoneDetail.screenshotCard(
                            superseded.item, at: dynamicTypeSize))))
            let replacementDarkDetail = DecisionDetailView(
                store: replacementStore,
                itemID: superseded.item.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-superseded-link-dark",
                    colorScheme: .dark,
                    view: AnyView(
                        replacementDarkDetail.screenshotCard(
                            superseded.item, at: dynamicTypeSize))))
            let replacementPhoneDarkDetail = DecisionDetailView(
                store: replacementStore,
                itemID: superseded.item.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-superseded-link-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        replacementPhoneDarkDetail.screenshotCard(
                            superseded.item, at: dynamicTypeSize))))

            // Closed cards (#2097): the lead sentence, the Outcome facts, the
            // timeline link, and the inert route list with the applied route
            // outlined. The closed inbox draws one card per cause; the matrix
            // adds a resolved, an expired, and a superseded card for the
            // dispute, the specification, and the health notice. The tasks
            // hold each subject's task so the link draws.
            let dispute = AttentionFixtures.routedReviewDispute()
            let specification = AttentionFixtures.fixture(type: .spec_approval)
            let health = AttentionFixtures.fixture(type: .system_health)
            let closedMatrix: [(name: String, snapshot: Components.Schemas.AttentionItemSnapshot)] = [
                ("review_dispute-resolved", AttentionFixtures.decidedReviewDispute()),
                (
                    "review_dispute-expired",
                    AttentionFixtures.closedByDaemon(
                        dispute, id: "production-review-run-review_dispute-3", cause: .expired, after: 5400,
                        reason: "Expired when the task's workflow was closed. The operator stopped the task.")
                ),
                (
                    "review_dispute-superseded",
                    AttentionFixtures.closedByDaemon(
                        dispute, id: "production-review-run-review_dispute-4", cause: .superseded, after: 2400,
                        reason: "Superseded when the pull request head changed during the dispute.",
                        successor: dispute.item.id)
                ),
                (
                    "spec_approval-resolved",
                    AttentionFixtures.closedByDecision(
                        specification, id: "item-spec_approval-resolved", status: .resolved, after: 1800,
                        commandID: "cmd-approve-decided", action: .approve,
                        deviceID: DeviceFixtures.currentDeviceID, deviceName: "Studio Mac")
                ),
                (
                    "spec_approval-expired",
                    AttentionFixtures.closedByDaemon(
                        specification, id: "item-spec_approval-expired", cause: .expired, after: 5400,
                        reason: "Expired when the task's workflow was closed. The operator stopped the task.")
                ),
                (
                    "spec_approval-superseded",
                    AttentionFixtures.closedByDaemon(
                        specification, id: "item-spec_approval-superseded", cause: .superseded, after: 2400,
                        reason: "Superseded by revision 2 of the specification.", successor: specification.item.id)
                ),
                ("system_health-resolved", AttentionFixtures.resolvedSystemHealth()),
                (
                    "system_health-expired",
                    AttentionFixtures.closedByDaemon(
                        health, id: "item-system_health-expired", cause: .expired, after: 5400,
                        reason: "Expired when the daemon restored from a backup.")
                ),
                (
                    "system_health-superseded",
                    AttentionFixtures.closedByDaemon(
                        health, id: "item-system_health-superseded", cause: .superseded, after: 2400,
                        reason: "Superseded by a notice for the same diagnostic.", successor: health.item.id)
                ),
                ("publish_blocked-expired", AttentionFixtures.expiredPublishBlocked()),
                ("effect_proposal-superseded", AttentionFixtures.supersededEffectProposal()),
            ]
            let closedStore = InboxStore(client: client, now: { screenshotNow })
            closedStore.replaceAll(with: closedMatrix.map(\.snapshot) + [dispute, specification, health])
            let closedTasks = closedMatrix.compactMap { entry -> Components.Schemas.TaskSnapshot? in
                guard case .run(let run) = entry.snapshot.item.subject, let taskID = run.task_id,
                    var task = TaskFixtures.defaultTasks().first
                else { return nil }
                task.task.id = taskID
                return task
            }
            for entry in closedMatrix {
                // The adjudicated dispute, the densest card, also draws on
                // the phone and by dusk.
                let variants: [(suffix: String, width: CGFloat?, colorScheme: ColorScheme)] =
                    entry.name == "review_dispute-resolved"
                    ? [("", nil, .light), ("-phone", 390, .light), ("-dark", nil, .dark)]
                    : [("", nil, .light)]
                for variant in variants {
                    let detail = DecisionDetailView(
                        store: closedStore,
                        itemID: entry.snapshot.item.id,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow,
                        tasks: closedTasks)
                    surfaces.append(
                        Surface(
                            name: "decision-\(entry.name)\(variant.suffix)",
                            width: variant.width,
                            colorScheme: variant.colorScheme,
                            view: AnyView(detail.screenshotCard(entry.snapshot.item, at: dynamicTypeSize))))
                }
            }

            // Selection draws one way whatever Differentiate Without Color
            // says: the bar and the dropped border are geometry, so the pair
            // below pins the cue that setting relies on.
            if let selected = inbox.first?.item {
                surfaces.append(
                    Surface(
                        name: "inbox-selected-differentiate-without-color",
                        width: 560,
                        view: AnyView(
                            VStack(spacing: 8) {
                                InboxRowView(
                                    item: selected,
                                    now: screenshotNow
                                )
                                InboxRowView(
                                    item: selected,
                                    isSelected: true,
                                    now: screenshotNow
                                )
                            }
                            .padding()
                        )))
                surfaces.append(
                    Surface(
                        name: "inbox-selected-differentiate-without-color-dark",
                        width: 560,
                        colorScheme: .dark,
                        view: AnyView(
                            VStack(spacing: 8) {
                                InboxRowView(
                                    item: selected,
                                    now: screenshotNow
                                )
                                InboxRowView(
                                    item: selected,
                                    isSelected: true,
                                    now: screenshotNow
                                )
                            }
                            .padding()
                        )))
            }

            var concludedDegraded = AttentionFixtures.degradedReady().item
            concludedDegraded.status = .resolved
            surfaces.append(
                Surface(
                    name: "inbox-concluded-degraded",
                    width: 320,
                    view: AnyView(
                        InboxRowView(item: concludedDegraded, now: screenshotNow)
                            .padding()
                    )))

            for snapshot in [AttentionFixtures.scopeConflictQuestion(), AttentionFixtures.scopeKeptReady()] {
                let detail = DecisionDetailView(
                    store: store, itemID: snapshot.item.id,
                    loadsAttachments: false, showsValidationProgress: false, now: screenshotNow)
                for (device, width) in [("mac", CGFloat(960)), ("iphone", CGFloat(390))] {
                    for (theme, scheme) in [("light", ColorScheme.light), ("dark", ColorScheme.dark)] {
                        surfaces.append(
                            Surface(
                                name: "\(snapshot.item.id)-\(device)-\(theme)", width: width, colorScheme: scheme,
                                view: AnyView(detail.screenshotCard(snapshot.item, at: dynamicTypeSize))))
                    }
                }
            }

            // One open card per type; the closed matrix below draws the
            // closed fixtures under their own names.
            for snapshot in inbox where snapshot.item.status == .open {
                let graphics = graphicPresentations(for: snapshot.item)
                let detail = DecisionDetailView(
                    store: store,
                    itemID: snapshot.item.id,
                    graphics: graphics,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow
                )
                let proposalFacts =
                    snapshot.item._type == .task_proposal
                    ? Components.Schemas.TaskProposalFactsSnapshot(
                        as_of_revision: snapshot.as_of_revision,
                        entity_version: snapshot.entity_version,
                        item_version: snapshot.item.item_version,
                        proposal_digest: snapshot.item.evidence_snapshot.first?.digest ?? "",
                        supersedes: nil,
                        intent: .implement_subject,
                        expected_cost_units: 12,
                        scope: .init(
                            component_count: 1,
                            declared_path_count: 3,
                            touches_control_plane: false))
                    : nil
                let effectProposalFacts = AttentionFixtures.effectProposalFacts(for: snapshot)
                surfaces.append(
                    Surface(
                        name: "decision-\(snapshot.item._type.rawValue)",
                        view: AnyView(
                            detail.screenshotCard(
                                snapshot.item,
                                at: dynamicTypeSize,
                                proposalFacts: proposalFacts,
                                effectProposalFacts: effectProposalFacts))))

                if [.agent_question, .review_dispute, .ready_for_final_review].contains(
                    snapshot.item._type)
                {
                    // The decision-first cards fold their run coordinates,
                    // the recorded reason, and the final review's yield; this
                    // pins what the folds hold.
                    let openDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        // The summary's report is its own fold (#1461).
                        expandsSummaryReports: true,
                        expandedDisclosures: Set(
                            [.runDetails, .recordedContext, .reviewYield]
                                + snapshot.item.agent_claims.map { .claimSource($0) }),
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-\(snapshot.item._type.rawValue)-disclosures-open",
                            view: AnyView(
                                openDetail.screenshotCard(snapshot.item, at: dynamicTypeSize))))
                }

                if snapshot.item._type == .agent_question {
                    // Visual audit D06: the question card on both platforms
                    // and themes, and a long question with more than one
                    // decision. Its inspector is appended last, below.
                    let name = "decision-\(snapshot.item._type.rawValue)"
                    for (suffix, width, scheme) in [
                        ("phone", CGFloat(390), ColorScheme.light),
                        ("dark", canvasWidth, ColorScheme.dark),
                        ("phone-dark", CGFloat(390), ColorScheme.dark),
                    ] {
                        surfaces.append(
                            Surface(
                                name: "\(name)-\(suffix)",
                                width: width,
                                colorScheme: scheme,
                                view: AnyView(
                                    detail.screenshotCard(
                                        snapshot.item,
                                        at: dynamicTypeSize,
                                        compactLayout: width < canvasWidth))))
                    }
                    let long = AttentionFixtures.longQuestion().item
                    for (suffix, width) in [("long", canvasWidth), ("long-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "\(name)-\(suffix)",
                                width: width,
                                view: AnyView(
                                    detail.screenshotCard(
                                        long,
                                        at: dynamicTypeSize,
                                        compactLayout: width < canvasWidth))))
                    }
                }

                if snapshot.item._type == .execution_failure {
                    surfaces.append(
                        Surface(
                            name: "decision-execution_failure-phone",
                            width: 390,
                            view: AnyView(
                                detail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    proposalFacts: proposalFacts))))
                }

                if snapshot.item._type == .effect_proposal {
                    // The card gains facts, so it is pinned on both platforms
                    // and both themes, as the other proposal cards are.
                    for (suffix, width, scheme) in [
                        ("phone", CGFloat(390), ColorScheme.light),
                        ("dark", canvasWidth, ColorScheme.dark),
                        ("phone-dark", CGFloat(390), ColorScheme.dark),
                    ] {
                        surfaces.append(
                            Surface(
                                name: "decision-effect_proposal-\(suffix)",
                                width: width,
                                colorScheme: scheme,
                                view: AnyView(
                                    detail.screenshotCard(
                                        snapshot.item,
                                        at: dynamicTypeSize,
                                        effectProposalFacts: effectProposalFacts))))
                    }
                }

                if snapshot.item._type == .spec_approval {
                    let phoneDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-phone",
                            width: 390,
                            view: AnyView(
                                phoneDetail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    proposalFacts: proposalFacts))))
                    let darkDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-dark",
                            colorScheme: .dark,
                            view: AnyView(
                                darkDetail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    proposalFacts: proposalFacts))))
                    let phoneDarkDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-phone-dark",
                            width: 390,
                            colorScheme: .dark,
                            view: AnyView(
                                phoneDarkDetail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    proposalFacts: proposalFacts))))

                    // The approval card's summary (#1461): the observed
                    // one-paragraph shape and the specifier's headings, by
                    // day and by dusk at both widths, closed and opened.
                    for (name, content) in [
                        ("legacy", DecisionSummaryFixtures.specificationLegacy),
                        ("structured", DecisionSummaryFixtures.specificationStructured),
                    ] {
                        let summarySnapshot = DecisionSummaryFixtures.snapshot(
                            content: content, type: .spec_approval)
                        for expanded in [false, true] {
                            let summaryDetail = DecisionDetailView(
                                store: store, itemID: summarySnapshot.item.id,
                                expandsSummaryReports: expanded, loadsAttachments: false,
                                showsValidationProgress: false, now: screenshotNow)
                            for width: CGFloat in [720, 390] {
                                for theme in [ColorScheme.light, .dark] {
                                    surfaces.append(
                                        Surface(
                                            name:
                                                "decision-spec_approval-summary-\(name)-\(expanded ? "expanded-tail" : "collapsed")-\(Int(width))-\(theme)",
                                            width: width, colorScheme: theme,
                                            view: AnyView(
                                                summaryDetail.screenshotCard(
                                                    summarySnapshot.item, at: dynamicTypeSize,
                                                    proposalFacts: proposalFacts,
                                                    compactLayout: width == 390
                                                )
                                                .fixedSize(horizontal: false, vertical: true)
                                                // The same bounded tail as the ready card's
                                                // expanded report below.
                                                .frame(height: expanded ? 1_600 : nil, alignment: .bottom)
                                                .clipped())))
                                }
                            }
                        }
                        if name == "structured" {
                            // Details carry the approval's reason, which is
                            // the same summary, in full and drawn the same.
                            let reasonInspector = DecisionDetailView(
                                store: store, itemID: summarySnapshot.item.id, detailsExpanded: true,
                                loadsAttachments: false, showsValidationProgress: false)
                            surfaces.append(
                                Surface(
                                    name: "decision-spec_approval-summary-structured-details",
                                    width: 360,
                                    view: AnyView(
                                        reasonInspector.screenshotInspector(
                                            summarySnapshot.item, at: dynamicTypeSize))))
                        }
                    }

                    let revised = AttentionFixtures.revisedSpecification()
                    let revisedStore = InboxStore(client: client)
                    revisedStore.replaceAll(with: [revised])
                    revisedStore.replaceAllConversations(
                        with: AttentionFixtures.defaultConversations())
                    let revisedDetail = DecisionDetailView(
                        store: revisedStore,
                        itemID: revised.item.id,
                        graphics: .init(),
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-revised",
                            view: AnyView(
                                revisedDetail.screenshotCard(
                                    revised.item, at: dynamicTypeSize))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-revised-phone",
                            width: 390,
                            view: AnyView(
                                revisedDetail.screenshotCard(
                                    revised.item, at: dynamicTypeSize))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-revised-dark",
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotCard(
                                    revised.item, at: dynamicTypeSize))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-revised-phone-dark",
                            width: 390,
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotCard(
                                    revised.item, at: dynamicTypeSize))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-specification-reader",
                            width: 720,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .specification, item: revised.item))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-specification-reader-phone",
                            width: 390,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .specification, item: revised.item, asSheet: true))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-specification-reader-dark",
                            width: 720,
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .specification, item: revised.item))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-specification-reader-phone-dark",
                            width: 390,
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .specification, item: revised.item, asSheet: true))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-specification-reader-details",
                            width: 720,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .specification, item: revised.item,
                                    expandsTechnicalDetails: true))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-diff-reader",
                            width: 720,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .diff, item: revised.item))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-diff-reader-phone",
                            width: 390,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .diff, item: revised.item, asSheet: true))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-diff-reader-dark",
                            width: 720,
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .diff, item: revised.item))))
                    surfaces.append(
                        Surface(
                            name: "decision-spec_approval-diff-reader-phone-dark",
                            width: 390,
                            colorScheme: .dark,
                            view: AnyView(
                                revisedDetail.screenshotSpecApprovalReader(
                                    .diff, item: revised.item, asSheet: true))))
                }

                if snapshot.item._type == .finding_adjudication {
                    // One finding open: its reason, daemon facts, and
                    // alternatives expand inside its own card, above the
                    // action region, while the other finding stays closed
                    // (#1107, visual audit D09).
                    let expandedDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        expandedFindings: ["review-finding-17"],
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow)
                    surfaces.append(
                        Surface(
                            name: "decision-finding_adjudication-expanded",
                            view: AnyView(
                                expandedDetail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize))))
                }

                if snapshot.item._type == .ready_for_final_review {
                    for (name, content) in [
                        ("structured", Optional(DecisionSummaryFixtures.structured)),
                        ("legacy", Optional(DecisionSummaryFixtures.legacy)),
                        ("short", Optional(DecisionSummaryFixtures.change)),
                        ("wrapped", Optional(DecisionSummaryFixtures.wrapped)),
                        ("artifact-only", nil),
                    ] {
                        let summarySnapshot = DecisionSummaryFixtures.snapshot(content: content)
                        // The wrapped report is short, so its closed card
                        // already shows all of it.
                        for expanded in [false, true]
                        where !expanded || (content != nil && name != "wrapped") {
                            let summaryDetail = DecisionDetailView(
                                store: store, itemID: summarySnapshot.item.id,
                                expandsSummaryReports: expanded, loadsAttachments: false,
                                showsValidationProgress: false, now: screenshotNow)
                            for width: CGFloat in [720, 390] {
                                for theme in [ColorScheme.light, .dark] {
                                    surfaces.append(
                                        Surface(
                                            name:
                                                "decision-ready-summary-\(name)-\(expanded ? "expanded-tail" : "collapsed")-\(Int(width))-\(theme)",
                                            width: width, colorScheme: theme,
                                            view: AnyView(
                                                summaryDetail.screenshotCard(
                                                    summarySnapshot.item, at: dynamicTypeSize,
                                                    compactLayout: width == 390
                                                )
                                                .fixedSize(horizontal: false, vertical: true)
                                                // Bound the expanded scroll viewport: a full ax5
                                                // report exceeds the platform PNG height limit.
                                                // The report opens in place under the summary, so
                                                // the tail pins its final concern and digest with
                                                // the actions below them; collapsed captures keep
                                                // the whole card.
                                                .frame(height: expanded ? 1_600 : nil, alignment: .bottom)
                                                .clipped())))
                                }
                            }
                        }
                    }
                    // The card carries this item's commit-plan fact, so the
                    // inspector beside it must not repeat a Facts section.
                    let readyPreferencesSuite = "FreesideScreenshotReadyPreferences-\(UUID().uuidString)"
                    guard let readyDefaults = UserDefaults(suiteName: readyPreferencesSuite)
                    else {
                        throw ScreenshotError.preferencesUnavailable
                    }
                    defer { readyDefaults.removePersistentDomain(forName: readyPreferencesSuite) }
                    let readyPreferences = DecisionSectionPreferences(defaults: readyDefaults)
                    readyPreferences.claimsExpanded = true
                    readyPreferences.evidenceExpanded = true
                    readyPreferences.detailsExpanded = true
                    let readyInspector = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        sectionPreferences: readyPreferences)
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-inspector",
                            width: 360,
                            view: AnyView(
                                readyInspector.screenshotInspector(
                                    snapshot.item,
                                    at: dynamicTypeSize))))
                    // The card with the inspector open and the inspector's
                    // Evidence disclosure open with it, which is the state
                    // the pointer row exists for: the packet is being drawn
                    // beside the card, so the card's own Evidence module
                    // stands down to "N attachments → inspector" (#1107).
                    // Only the evidence preference is raised here, so this
                    // surface differs from decision-ready_for_final_review
                    // in exactly the module under test; with the default
                    // preferences the
                    // inspector shows nothing and the card keeps its rows, so
                    // recording that state would pin a card identical to the
                    // one above and catch no pointer regression at all.
                    let openEvidenceSuite = "FreesideScreenshotReadyInspectorOpenPreferences-\(UUID().uuidString)"
                    guard let openEvidenceDefaults = UserDefaults(suiteName: openEvidenceSuite)
                    else {
                        throw ScreenshotError.preferencesUnavailable
                    }
                    defer { openEvidenceDefaults.removePersistentDomain(forName: openEvidenceSuite) }
                    let openEvidencePreferences = DecisionSectionPreferences(
                        defaults: openEvidenceDefaults)
                    openEvidencePreferences.evidenceExpanded = true
                    let readyInspectorOpen = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow,
                        sectionPreferences: openEvidencePreferences)
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-inspector-open",
                            view: AnyView(
                                readyInspectorOpen.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    inspectorPresented: true))))
                    // A verdict the daemon invalidated (issue #982), rendered
                    // stale beside the bound and observed heads (frame 7.3),
                    // and the other staleness axis (the base watch) on the
                    // phone. The degraded verdict is the 4b surfaces'.
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-stale",
                            view: AnyView(
                                detail.screenshotCard(
                                    AttentionFixtures.staleReady().item, at: dynamicTypeSize))))
                    var baseAdvanced = snapshot.item
                    baseAdvanced.base_freshness = .init(
                        value1: .init(
                            base_ref: "main", admitted_base_sha: "deadbeef",
                            observed_base_sha: "0badf00d", advanced: true,
                            observed_at: AttentionFixtures.createdInstant))
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-stale-base-phone",
                            width: 390,
                            view: AnyView(
                                detail.screenshotCard(baseAdvanced, at: dynamicTypeSize))))
                }

                if snapshot.item._type == .publish_blocked {
                    // A hold raised after its run's pull request existed
                    // (#1643): the card offers viewing that pull request
                    // beside inspecting the trust failure.
                    let published = AttentionFixtures.publishBlockedAfterPublication().item
                    surfaces.append(
                        Surface(
                            name: "decision-publish_blocked-after-publication",
                            view: AnyView(
                                detail.screenshotCard(published, at: dynamicTypeSize))))
                    surfaces.append(
                        Surface(
                            name: "decision-publish_blocked-after-publication-phone",
                            width: 390,
                            view: AnyView(
                                detail.screenshotCard(
                                    published,
                                    at: dynamicTypeSize,
                                    compactLayout: true))))
                }

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

                    // The drift-audit shapes of the card (#1052): the
                    // over_hardened verdict with and without the daemon's
                    // promise to simplify on continue, the stuck verdict, and
                    // a reversal list long enough to collapse, each at phone
                    // width and at the Mac card's.
                    let driftVariants = [
                        ("over_hardened", AttentionFixtures.reviewDiminishing(cause: .drift_audit)),
                        (
                            "over_hardened-simplify",
                            AttentionFixtures.reviewDiminishing(
                                cause: .drift_audit, simplificationOnContinue: true)
                        ),
                        (
                            "stuck",
                            AttentionFixtures.reviewDiminishing(cause: .drift_audit, verdict: .stuck)
                        ),
                        (
                            "long-reversals",
                            AttentionFixtures.reviewDiminishing(
                                cause: .drift_audit, longReversalList: true)
                        ),
                    ]
                    for (variant, drift) in driftVariants {
                        surfaces.append(
                            Surface(
                                name: "decision-review_diminishing_returns-drift-\(variant)-phone",
                                width: 390,
                                view: AnyView(
                                    detail.screenshotCard(
                                        drift.item,
                                        at: dynamicTypeSize,
                                        compactLayout: true))))
                        surfaces.append(
                            Surface(
                                name: "decision-review_diminishing_returns-drift-\(variant)",
                                view: AnyView(
                                    detail.screenshotCard(drift.item, at: dynamicTypeSize))))
                    }
                    surfaces.append(
                        Surface(
                            name: "decision-review_diminishing_returns-drift-over_hardened-phone-dark",
                            width: 390,
                            colorScheme: .dark,
                            view: AnyView(
                                detail.screenshotCard(
                                    driftVariants[0].1.item,
                                    at: dynamicTypeSize,
                                    compactLayout: true))))

                    let preferencesSuite = "FreesideScreenshotDiminishingPreferences-\(UUID().uuidString)"
                    guard let preferencesDefaults = UserDefaults(suiteName: preferencesSuite) else {
                        throw ScreenshotError.preferencesUnavailable
                    }
                    defer { preferencesDefaults.removePersistentDomain(forName: preferencesSuite) }
                    let inspectorPreferences = DecisionSectionPreferences(
                        defaults: preferencesDefaults)
                    inspectorPreferences.detailsExpanded = true
                    let inspectorDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        sectionPreferences: inspectorPreferences)
                    surfaces.append(
                        Surface(
                            name: "decision-review_diminishing_returns-inspector",
                            width: 360,
                            view: AnyView(
                                inspectorDetail.screenshotInspector(
                                    snapshot.item,
                                    at: dynamicTypeSize))))
                }
            }

            // The fixture carries the contract recommendation, so the card
            // renders the item's own authoritative lead rather than an
            // injected presentation.
            let adjudication = AttentionFixtures.fixture(type: .finding_adjudication).item
            let recommendedDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                loadsAttachments: false,
                showsValidationProgress: false
            )
            let preferencesSuite = "FreesideScreenshotInspectorPreferences-\(UUID().uuidString)"
            guard let preferencesDefaults = UserDefaults(suiteName: preferencesSuite) else {
                throw ScreenshotError.preferencesUnavailable
            }
            defer { preferencesDefaults.removePersistentDomain(forName: preferencesSuite) }
            let inspectorPreferences = DecisionSectionPreferences(defaults: preferencesDefaults)
            inspectorPreferences.claimsExpanded = true
            inspectorPreferences.evidenceExpanded = true
            inspectorPreferences.detailsExpanded = true
            let inspectorDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                showsValidationProgress: false,
                sectionPreferences: inspectorPreferences)
            // The same surface without the item's recommendation: every
            // requested action is offered with equal weight, and offer order
            // carries no endorsement.
            var equallyWeighted = adjudication
            equallyWeighted.recommendation = nil
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-equally-weighted",
                    view: AnyView(
                        recommendedDetail.screenshotCard(
                            equallyWeighted, at: dynamicTypeSize))))
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-recommended-phone",
                    width: 390,
                    view: AnyView(
                        recommendedDetail.screenshotCard(
                            adjudication, at: dynamicTypeSize, compactLayout: true))))
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-equally-weighted-phone",
                    width: 390,
                    view: AnyView(
                        recommendedDetail.screenshotCard(
                            equallyWeighted, at: dynamicTypeSize, compactLayout: true))))
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-recommended",
                    view: AnyView(
                        recommendedDetail.screenshotCard(adjudication, at: dynamicTypeSize))))
            // One finding per route, carrying the daemon's consequence-led
            // reason and recommendation: the card says what accepting does
            // before any route vocabulary (#1551).
            for route in [
                Components.Schemas.AdjudicationRoute.remediate, .park_revision, .park_separate_work,
            ] {
                let single = AttentionFixtures.findingAdjudicationFixture(route: route).item
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-\(route.rawValue)",
                        view: AnyView(
                            recommendedDetail.screenshotCard(single, at: dynamicTypeSize))))
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-\(route.rawValue)-phone",
                        width: 390,
                        view: AnyView(
                            recommendedDetail.screenshotCard(
                                single, at: dynamicTypeSize, compactLayout: true))))
            }
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-inspector",
                    width: 360,
                    view: AnyView(
                        inspectorDetail.screenshotInspector(
                            adjudication,
                            at: dynamicTypeSize))))

            // A daemon-policy recommendation whose action is destructive: the
            // card must lead with it in the fact register and still demand the
            // destructive confirmation.
            var question = AttentionFixtures.fixture(type: .agent_question).item
            question.recommendation = .init(
                value1: .init(
                    action: .stop,
                    reason: "Stopping avoids applying an unreviewed answer.",
                    source: .daemon_policy,
                    provenance: .init(
                        daemon_policy: .init(
                            value1: .init(
                                rule_digest: "sha256:rule-agent-question-stop",
                                input_digest: "sha256:input-agent-question-stop"))),
                    confidence: nil))
            let destructiveRecommendation = DecisionDetailView(
                store: store,
                itemID: question.id,
                loadsAttachments: false,
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
            try cache.save(
                .init(
                    cursors: .init(
                        syncEpoch: "screenshot-epoch",
                        lastFullSnapshotRevision: 12,
                        highestObservedServerRevision: 12
                    ),
                    attentionItems: inbox,
                    runs: runs,
                    // The task timeline lists a task's armed schedules.
                    schedules: RunFixtures.defaultSchedules(),
                    // The refreshed-history timeline is cached (not added to
                    // defaultTimelines) so its own run-timeline surface resolves
                    // review labels the way the app does, without churning the
                    // runs-list digests that defaultTimelines feeds.
                    runTimelines: RunFixtures.defaultTimelines() + [RunFixtures.refreshedHistoryTimeline()],
                    tasks: TaskFixtures.defaultTasks()
                ))
            let coordinator = SyncCoordinator(client: client, cache: cache)
            guard let activeRun = runs.first(where: { $0.run.id == RunFixtures.activeRunID }) else {
                throw ScreenshotError.missingActiveRun
            }
            let tasks = TaskFixtures.defaultTasks()
            guard let retryTask = tasks.first(where: { $0.task.id == TaskFixtures.retryTaskID }) else {
                throw ScreenshotError.missingRetryTask
            }
            surfaces.append(
                Surface(
                    name: "tasks-list",
                    view: AnyView(
                        TasksListView(
                            tasks: tasks,
                            runs: runs,
                            selection: .constant(retryTask.task.id)
                        ).screenshotContent(now: RunFixtures.screenshotInstant)
                    )))
            // The empty scope at the sidebar's width, by day and by dusk:
            // the empty state the list centers under its filters.
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "tasks-list-empty" + (colorScheme == .dark ? "-dark" : ""),
                        width: 320,
                        colorScheme: colorScheme,
                        view: AnyView(
                            TasksListView(
                                tasks: [],
                                runs: [],
                                selection: .constant(nil)
                            ).screenshotContent(now: RunFixtures.screenshotInstant)
                                .padding(.top, 32)
                                .background(Color.sidebarGround)
                        )))
            }
            for scope in [TaskListFilter.Scope.finished, .all] {
                surfaces.append(
                    Surface(
                        name: "tasks-list-\(scope.rawValue)",
                        view: AnyView(
                            TasksListView(
                                tasks: tasks,
                                runs: runs,
                                selection: .constant(nil),
                                initialScope: scope
                            ).screenshotContent(now: RunFixtures.screenshotInstant)
                        )))
            }
            // The three name sources side by side: an operator name, an
            // agent-proposed name with its claim mark, and the identifier
            // fallback, in every scheme and contrast.
            let namedRows = ["task-campaign-freeside-ready", TaskFixtures.retryTaskID, TaskFixtures.legacyTaskID]
                .compactMap { id in tasks.first { $0.task.id == id } }
            for colorScheme in [ColorScheme.light, .dark] {
                for contrast in [LaunchInputs.Contrast.standard, .increased] {
                    surfaces.append(
                        Surface(
                            name: "tasks-names-\(colorScheme)-\(contrast)",
                            width: 640,
                            colorScheme: colorScheme,
                            contrast: contrast,
                            view: AnyView(
                                VStack(spacing: 12) {
                                    ForEach(namedRows, id: \.task.id) { snapshot in
                                        TaskRowView(
                                            task: snapshot.task,
                                            position: TaskDisplay.position(snapshot.task, runs: runs),
                                            isSelected: false,
                                            now: RunFixtures.screenshotInstant)
                                    }
                                }
                                .padding()
                            )))
                }
            }
            // Two tasks with the same name (#1379): the row appends a short
            // task id so the rows do not read alike. Across the size matrix,
            // the disambiguator must survive the accessibility sizes.
            var secondEqualName = retryTask
            secondEqualName.task.id = "task-\(RunFixtures.readyRunID)"
            let equalNameTasks = [retryTask, secondEqualName]
            let equalNameAmbiguous = TaskDisplay.ambiguousTaskIDs(equalNameTasks)
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "tasks-equal-names" + (colorScheme == .dark ? "-dark" : ""),
                        width: 640,
                        colorScheme: colorScheme,
                        view: AnyView(
                            VStack(spacing: 12) {
                                ForEach(equalNameTasks, id: \.task.id) { snapshot in
                                    TaskRowView(
                                        task: snapshot.task,
                                        position: TaskDisplay.position(snapshot.task, runs: runs),
                                        isSelected: false,
                                        now: RunFixtures.screenshotInstant,
                                        showsIdentifier: equalNameAmbiguous.contains(snapshot.task.id))
                                }
                            }
                            .padding()
                        )))
            }
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "tasks-selected-differentiate-without-color"
                            + (colorScheme == .dark ? "-dark" : ""),
                        width: 560,
                        colorScheme: colorScheme,
                        view: AnyView(
                            VStack(spacing: 8) {
                                ForEach([false, true], id: \.self) { isSelected in
                                    TaskRowView(
                                        task: retryTask.task,
                                        position: TaskDisplay.position(retryTask.task, runs: runs),
                                        isSelected: isSelected,
                                        now: RunFixtures.screenshotInstant)
                                }
                            }
                            .padding()
                        )))
            }
            surfaces.append(
                Surface(
                    name: "tasks-row-sidebar",
                    width: 320,
                    view: AnyView(
                        TaskRowView(
                            task: retryTask.task,
                            position: TaskDisplay.position(retryTask.task, runs: runs),
                            isSelected: true,
                            now: RunFixtures.screenshotInstant
                        )
                        .padding()
                    )))
            guard
                let retryTimeline = TaskFixtures.defaultTimelines().first(where: {
                    $0.task_id == TaskFixtures.retryTaskID
                })
            else {
                throw ScreenshotError.missingRetryTask
            }
            let approvalFixture = TaskFixtures.approvedCampaign()
            for scheme in [ColorScheme.light, .dark] {
                let dispute = TaskProgressFixtures.make("Review dispute")
                surfaces.append(
                    Surface(
                        name: "task-review-dispute-\(scheme)", width: 390,
                        colorScheme: scheme, nativeAppearance: true,
                        view: AnyView(
                            TaskRowView(
                                task: dispute.task, position: dispute.position,
                                now: RunFixtures.screenshotInstant
                            ).padding())))
            }
            for (group, names) in TaskProgressFixtures.groups.enumerated() {
                for width in [CGFloat(320), CGFloat(390)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "task-progress-\(group)-\(Int(width))-\(scheme)",
                                width: width, colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    VStack(alignment: .leading, spacing: 16) {
                                        ForEach(names, id: \.self) { name in
                                            let fixture = TaskProgressFixtures.make(name)
                                            Text(name).font(FreesideFont.subheadline)
                                            TaskRowView(
                                                task: fixture.task, position: fixture.position,
                                                now: RunFixtures.screenshotInstant)
                                        }
                                    }.padding())))
                    }
                }
            }
            for (group, names) in HoldPresentationFixtures.groups.enumerated() {
                for width in [CGFloat(320), CGFloat(390), CGFloat(820)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "task-holds-\(group)-\(Int(width))-\(scheme)",
                                width: width, colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    VStack(alignment: .leading, spacing: 16) {
                                        ForEach(names, id: \.self) { name in
                                            let fixture = HoldPresentationFixtures.make(name)
                                            Text(name).font(FreesideFont.subheadline)
                                            TaskRowView(
                                                task: fixture.task, position: fixture.position,
                                                now: RunFixtures.screenshotInstant)
                                        }
                                    }.padding())))
                    }
                }
            }
            // The task timeline's hold callout (visual audit D02) in the
            // header it belongs to: a capacity wait beside its no-action
            // sentence, a hold the operator has to clear, and a hold under a
            // cancellation fence, whose Stop state takes its own row.
            let holdCalloutNames = ["Capacity", "Input unavailable", "Stop requested beside capacity"]
            let holdCalloutHeaders = try holdCalloutNames.map { name in
                let fixture = HoldPresentationFixtures.make(name)
                var task = approvalFixture.task
                task.task = fixture.task
                let cache = InMemoryCacheStore()
                try cache.save(
                    .init(
                        cursors: .init(
                            syncEpoch: "hold-epoch", lastFullSnapshotRevision: task.as_of_revision,
                            highestObservedServerRevision: task.as_of_revision),
                        attentionItems: fixture.items, runs: fixture.runs, tasks: [task],
                        taskTimelines: [fixture.history].compactMap { $0 }))
                let coordinator = SyncCoordinator(
                    client: APIClientFactory.mock(server: MockServer()), cache: cache)
                return (
                    name,
                    AnyView(
                        TaskTimelineView(coordinator: coordinator, snapshot: task, onOpenRun: { _ in })
                            .header(fixture.history))
                )
            }
            // A ready card an external review withdrew, before round 2's
            // request: the task row and the timeline header name the stage
            // and round the re-entry opened (#2350).
            var reentry = TaskStageFixtures.page(TaskHistoryFixtures.externallyReviewed(requested: false))
            reentry.runs[0].run.outcome = .pending
            reentry.runs[0].run.latest_milestone = .init(value1: .publication_ready)
            var reentryTask = approvalFixture.task
            reentryTask.task = reentry.task
            let reentryCache = InMemoryCacheStore()
            try reentryCache.save(
                .init(
                    cursors: .init(
                        syncEpoch: "reentry-epoch", lastFullSnapshotRevision: reentryTask.as_of_revision,
                        highestObservedServerRevision: reentryTask.as_of_revision),
                    attentionItems: reentry.items, runs: reentry.runs, tasks: [reentryTask],
                    taskTimelines: [reentry.history].compactMap { $0 }))
            let reentryCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: reentryCache)
            let reentryPosition = TaskDisplay.position(
                reentry.task, runs: reentry.runs, attentionItems: reentry.items, history: reentry.history,
                stage: TaskDisplay.currentStage(reentry.task, runs: reentry.runs, history: reentry.history))
            for scheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "task-external-review-stage-390-\(scheme)", width: 390, colorScheme: scheme,
                        nativeAppearance: true,
                        view: AnyView(
                            VStack(alignment: .leading, spacing: 28) {
                                TaskRowView(
                                    task: reentry.task, position: reentryPosition, now: RunFixtures.screenshotInstant)
                                TaskTimelineView(
                                    coordinator: reentryCoordinator, snapshot: reentryTask, onOpenRun: { _ in }
                                ).header(reentry.history)
                            }
                            .padding(24)
                            .foregroundStyle(Color.ink)
                            .background(Color.ground))))
            }
            for width in [CGFloat(820), CGFloat(390)] {
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-hold-callout-\(Int(width))-\(scheme)",
                            width: width, colorScheme: scheme, nativeAppearance: true,
                            view: AnyView(
                                VStack(alignment: .leading, spacing: 28) {
                                    ForEach(holdCalloutHeaders, id: \.0) { _, header in
                                        header
                                    }
                                }
                                .padding(24)
                                .foregroundStyle(Color.ink)
                                .background(Color.ground))))
                }
            }
            // Bounded conversation messages (visual audit D04): a short
            // message, long prose, code-like lines, and a long message with
            // an attachment, each cut to six lines, then all in full. The
            // widths are the decision card's conversation column and a phone.
            let longConversation = AttentionFixtures.longConversation()
            let longMessageIDs: Set<String> = [
                AttentionFixtures.longPlainMessageID, AttentionFixtures.longCodeMessageID,
                AttentionFixtures.longAttachmentMessageID,
            ]
            for (state, expanded) in [("collapsed", Set<String>()), ("expanded", longMessageIDs)] {
                for width in [CGFloat(560), CGFloat(390)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "conversation-long-\(state)-\(Int(width))-\(scheme)",
                                width: width, colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    ConversationView(
                                        snapshot: longConversation,
                                        attachments: store.attachments,
                                        loadsAttachments: false,
                                        now: screenshotNow,
                                        rendersInteractiveControls: false,
                                        initiallyExpandedMessageIDs: expanded
                                    )
                                    .padding(16)
                                    .background(Color.ground))))
                    }
                }
            }
            // The three authors in one thread (R5): the operator bordered on
            // the right, the daemon bordered on the left under its producer
            // label, and the agent quoted on the left.
            var authorsConversation = longConversation
            authorsConversation.conversation.messages = [
                .init(
                    id: "msg-authors-user", conversation_id: longConversation.conversation.id, sequence: 1,
                    author: .user, body: "Can the revised spec preserve the existing migration order?",
                    attachments: [], created_at: screenshotNow.addingTimeInterval(-180)),
                .init(
                    id: "msg-authors-daemon", conversation_id: longConversation.conversation.id, sequence: 2,
                    author: .daemon, body: "The agent's reply was not delivered. The question was sent again.",
                    attachments: [], created_at: screenshotNow.addingTimeInterval(-120)),
                .init(
                    id: "msg-authors-agent", conversation_id: longConversation.conversation.id, sequence: 3,
                    author: .agent, body: "Yes. The revision keeps the order and narrows the rollback step.",
                    attachments: [], created_at: screenshotNow.addingTimeInterval(-60)),
            ]
            for scheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "conversation-authors-\(scheme)",
                        width: 560, colorScheme: scheme, nativeAppearance: true,
                        view: AnyView(
                            ConversationView(
                                snapshot: authorsConversation,
                                attachments: store.attachments,
                                loadsAttachments: false,
                                now: screenshotNow,
                                rendersInteractiveControls: false
                            )
                            .padding(16)
                            .background(Color.ground))))
            }
            var failedImplementation = approvalFixture.runs[0]
            failedImplementation.run.outcome = .failed
            failedImplementation.run.lifecycle = .finished
            failedImplementation.run.latest_milestone = .init(value1: .terminal_recorded)
            for width in [CGFloat(820), CGFloat(390)] {
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-approval-\(Int(width))-\(scheme)", width: width, colorScheme: scheme,
                            nativeAppearance: true,
                            view: AnyView(
                                VStack(alignment: .leading, spacing: 16) {
                                    ForEach([false, true], id: \.self) { failed in
                                        Text(
                                            failed
                                                ? "Approved specification, failed implementation"
                                                : "Approved specification"
                                        )
                                        .font(FreesideFont.subheadline)
                                        TaskRowView(
                                            task: approvalFixture.task.task,
                                            position: TaskDisplay.position(
                                                approvalFixture.task.task,
                                                runs: failed ? [failedImplementation] : approvalFixture.runs,
                                                history: approvalFixture.history),
                                            now: RunFixtures.screenshotInstant)
                                    }
                                }.padding())))
                }
            }
            let publishedRun = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.completedRunID })
            let completeHistory = TaskHistoryFixtures.history(.published)
            let historyCoordinator = try TaskHistoryFixtures.coordinator(completeHistory)
            for width in [CGFloat(820), CGFloat(390)] {
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-history-\(Int(width))-\(scheme)", width: width, colorScheme: scheme,
                            nativeAppearance: true,
                            view: AnyView(
                                TaskTimelineView(
                                    coordinator: historyCoordinator,
                                    snapshot: TaskHistoryFixtures.snapshot(completeHistory),
                                    onOpenRun: { _ in }
                                ).screenshotContent(completeHistory))))
                }
            }
            let emptyHistory = TaskHistoryFixtures.history(.creation)
            let emptyTask = TaskHistoryFixtures.snapshot(emptyHistory)
            let emptyServer = MockServer(runs: [], tasks: [emptyTask])
            let emptyCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: emptyServer), cache: InMemoryCacheStore())
            await emptyCoordinator.bootstrap()
            await emptyCoordinator.refreshTaskTimeline(for: emptyTask.task.id)
            let loadedEmptyHistory = try #require(emptyCoordinator.taskTimelinesByTaskID[emptyTask.task.id])
            #expect(emptyCoordinator.taskTimelineLoadStates[emptyTask.task.id] == .loaded)
            let failedServer = MockServer()
            let failedCoordinator = try TaskHistoryFixtures.coordinator(completeHistory, server: failedServer)
            await failedServer.setBeforeRespond { _ in throw MockServer.ForcedStatus(500) }
            await failedCoordinator.refreshTaskTimeline(for: completeHistory.task_id)
            #expect(failedCoordinator.taskTimelineLoadStates[completeHistory.task_id] == .unavailable)
            let revisedHistory = TaskHistoryFixtures.history(.revised)
            let revisedCoordinator = try TaskHistoryFixtures.coordinator(revisedHistory)
            // The ready card an external review withdrew, under its cause.
            let externalHistory = TaskHistoryFixtures.externallyReviewed(requested: false)
            let externalCoordinator = try TaskHistoryFixtures.coordinator(externalHistory)
            for (name, history, display) in [
                ("empty", loadedEmptyHistory, emptyCoordinator),
                ("failed", completeHistory, failedCoordinator),
                ("revised", revisedHistory, revisedCoordinator),
                ("external-review", externalHistory, externalCoordinator),
            ] {
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-history-\(name)-390-\(scheme)", width: 390, colorScheme: scheme,
                            nativeAppearance: true,
                            view: AnyView(
                                TaskTimelineView(
                                    coordinator: display, snapshot: TaskHistoryFixtures.snapshot(history),
                                    onOpenRun: { _ in }
                                ).screenshotContent(history))))
                }
            }
            // The two-campaign history with folds open: the current campaign
            // alone, then every fold (each campaign, each prior run, and each
            // run's details), so the folded content is covered too.
            let currentOpen = TaskHistoryFixtures.currentCampaignOpen(revisedHistory)
            let everythingOpen = TaskTimelineDisclosurePreferences(defaults: nil)
            for section in revisedHistory.sections {
                everythingOpen.setExpanded(true, .campaign(section.campaign_id), taskID: revisedHistory.task_id)
                for run in section.runs {
                    everythingOpen.setExpanded(true, .run(run.run_id), taskID: revisedHistory.task_id)
                    everythingOpen.setExpanded(true, .runDetails(run.run_id), taskID: revisedHistory.task_id)
                }
            }
            // A current task state with a round behind it: Stop is the bare
            // button, as on the cache-restored surfaces above.
            let freshCoordinator = try TaskHistoryFixtures.coordinator(revisedHistory)
            freshCoordinator.store.freshness = .fresh
            #expect(freshCoordinator.taskStop.unavailableReason == nil)
            for (width, scheme) in [(CGFloat(820), ColorScheme.light), (390, .dark)] {
                surfaces.append(
                    Surface(
                        name: "task-history-revised-fresh-\(Int(width))-\(scheme)", width: width,
                        colorScheme: scheme, nativeAppearance: true,
                        view: AnyView(
                            TaskTimelineView(
                                coordinator: freshCoordinator,
                                snapshot: TaskHistoryFixtures.snapshot(revisedHistory), onOpenRun: { _ in }
                            ).screenshotContent(revisedHistory))))
            }
            for (name, disclosures) in [("current-open", currentOpen), ("all-open", everythingOpen)] {
                surfaces.append(
                    Surface(
                        name: "task-history-revised-\(name)-820-light", width: 820, nativeAppearance: true,
                        view: AnyView(
                            TaskTimelineView(
                                coordinator: revisedCoordinator,
                                snapshot: TaskHistoryFixtures.snapshot(revisedHistory), onOpenRun: { _ in }
                            ).screenshotContent(revisedHistory, disclosures: disclosures))))
            }
            // A task in each stage the page names (#1923): the stage on the
            // chip, on the hold callout at Verification, and at the newest
            // start point of the rail.
            for (name, scenario) in [
                ("specification", TaskStageFixtures.Scenario.specification), ("implementation", .implementation),
                ("review", .review), ("verification-held", .verification),
            ] {
                let page = TaskStageFixtures.page(scenario)
                let history = try #require(page.history)
                let stage = try TaskStageFixtures.coordinator(page)
                for width in [CGFloat(820), CGFloat(390)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "task-stage-\(name)-\(Int(width))-\(scheme)", width: width,
                                colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    TaskTimelineView(
                                        coordinator: stage.coordinator, snapshot: stage.task, onOpenRun: { _ in }
                                    ).screenshotContent(history))))
                    }
                }
            }
            // The rail's stage-start and failure markers in each scheme and
            // contrast, on a history with a failed review round. Every
            // surface draws at every text size, the largest included.
            let markersPage = TaskStageFixtures.page(.returned)
            let markersHistory = try #require(markersPage.history)
            let markers = try TaskStageFixtures.coordinator(markersPage)
            for scheme in [ColorScheme.light, .dark] {
                for contrast in [LaunchInputs.Contrast.standard, .increased] {
                    surfaces.append(
                        Surface(
                            name: "task-stage-markers-\(scheme)-\(contrast)", width: 390, colorScheme: scheme,
                            contrast: contrast, nativeAppearance: true,
                            view: AnyView(
                                TaskTimelineView(
                                    coordinator: markers.coordinator, snapshot: markers.task, onOpenRun: { _ in }
                                ).screenshotContent(markersHistory))))
                }
            }
            let publishedTask = try #require(tasks.first { $0.task.id == publishedRun.run.task_id })
            var publishedTimeline = try #require(
                RunFixtures.defaultTimelines().first { $0.run_id == publishedRun.run.id })
            var publishedRound = RunFixtures.reviewRound(.completed, availability: .available)
            publishedRound.invocation_id = "review-published-1"
            publishedTimeline.review = .init(value1: .init(rounds: [publishedRound]))
            let publishedCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer(timelines: [publishedTimeline])),
                cache: InMemoryCacheStore())
            await publishedCoordinator.bootstrap()
            await publishedCoordinator.refreshTaskTimeline(for: publishedTask.task.id)
            await publishedCoordinator.refreshTimeline(for: publishedRun.run.id)
            let publishedHistory = try #require(publishedCoordinator.taskTimelinesByTaskID[publishedTask.task.id])
            for width in [CGFloat(820), CGFloat(390)] {
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-published-review-\(Int(width))-\(scheme == .dark ? "dark" : "light")",
                            width: width, colorScheme: scheme, nativeAppearance: true,
                            view: AnyView(
                                TaskTimelineView(
                                    coordinator: publishedCoordinator, snapshot: publishedTask, onOpenRun: { _ in }
                                )
                                .screenshotContent(
                                    publishedHistory,
                                    disclosures: TaskHistoryFixtures.currentCampaignOpen(publishedHistory)))))
                }
            }
            let legacyTask = try #require(tasks.first { $0.task.id == TaskFixtures.legacyTaskID })
            var legacyTimeline = try #require(
                RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.legacyRunID })
            legacyTimeline.review = .init(value1: .init(rounds: [publishedRound]))
            let legacyCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer(timelines: [legacyTimeline])),
                cache: InMemoryCacheStore())
            await legacyCoordinator.bootstrap()
            await legacyCoordinator.refreshTaskTimeline(for: legacyTask.task.id)
            await legacyCoordinator.refreshTaskReviews(for: legacyTask.task.id, revision: legacyTask.as_of_revision)
            let legacyHistory = try #require(legacyCoordinator.taskTimelinesByTaskID[legacyTask.task.id])
            for scheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "task-legacy-review-390-\(scheme == .dark ? "dark" : "light")",
                        width: 390, colorScheme: scheme, nativeAppearance: true,
                        view: AnyView(
                            TaskTimelineView(
                                coordinator: legacyCoordinator, snapshot: legacyTask, onOpenRun: { _ in }
                            )
                            .screenshotContent(
                                legacyHistory, disclosures: TaskHistoryFixtures.currentCampaignOpen(legacyHistory)))))
            }
            surfaces.append(
                Surface(
                    name: "task-timeline",
                    view: AnyView(
                        TaskTimelineView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(retryTimeline)
                    )))
            surfaces.append(
                Surface(
                    name: "task-timeline-390",
                    width: 390,
                    view: AnyView(
                        TaskTimelineView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(retryTimeline)
                    )))
            // The page's Identifiers footer expanded (#1379, #2117): the
            // exact task, campaign, and run ids grouped by owner, with their
            // copy controls, and the current campaign open over it.
            surfaces.append(
                Surface(
                    name: "task-timeline-details",
                    view: AnyView(
                        TaskTimelineView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(
                                retryTimeline,
                                disclosures: TaskHistoryFixtures.currentCampaignOpen(
                                    retryTimeline, identifiersOpen: true))
                    )))
            surfaces.append(
                Surface(
                    name: "task-timeline-details-390",
                    width: 390,
                    view: AnyView(
                        TaskTimelineView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(
                                retryTimeline,
                                disclosures: TaskHistoryFixtures.currentCampaignOpen(
                                    retryTimeline, identifiersOpen: true))
                    )))
            // A remediation under way (#2100): the task's events return to
            // Implementation at the remediator's start, under the decision
            // that sent the findings there, and the open run card draws the
            // same decision in its own Milestones rail.
            let remediationScript = TaskStageFixtures.script(.remediation)
            let remediation = try TaskStageFixtures.coordinator(
                TaskStageFixtures.page(.remediation), runTimelines: remediationScript.runTimelines)
            for (name, width) in [("task-timeline-remediation", CGFloat?.none), ("task-timeline-remediation-390", 390)]
            {
                surfaces.append(
                    Surface(
                        name: name,
                        width: width,
                        view: AnyView(
                            TaskTimelineView(
                                coordinator: remediation.coordinator, snapshot: remediation.task, onOpenRun: { _ in }
                            )
                            .screenshotContent(
                                remediationScript.timeline,
                                disclosures: TaskHistoryFixtures.currentCampaignOpen(remediationScript.timeline))
                        )))
            }
            surfaces.append(
                Surface(
                    name: "operational-summary",
                    width: Self.detailColumnWidth,
                    view: AnyView(
                        OperationalSummaryView(
                            summary: OperationalSummary(
                                openSnapshots: store.openSnapshots,
                                tasks: tasks,
                                freshness: .fresh),
                            onSelectItem: { _ in },
                            onShowTasks: {},
                            now: screenshotNow))))
            // The summary by dusk, on its own ground, with the trailing
            // values the fact row now right-aligns.
            surfaces.append(
                Surface(
                    name: "operational-summary-dark",
                    width: Self.detailColumnWidth,
                    colorScheme: .dark,
                    view: AnyView(
                        OperationalSummaryView(
                            summary: OperationalSummary(
                                openSnapshots: store.openSnapshots,
                                tasks: tasks,
                                freshness: .fresh),
                            onSelectItem: { _ in },
                            onShowTasks: {},
                            now: screenshotNow
                        )
                        .background(Color.ground))))
            // The contract-mismatch state renders distinctly from fresh and
            // the other failure states (#1265).
            surfaces.append(
                Surface(
                    name: "operational-summary-contract-mismatch",
                    width: Self.detailColumnWidth,
                    view: AnyView(
                        OperationalSummaryView(
                            summary: OperationalSummary(
                                openSnapshots: store.openSnapshots,
                                tasks: tasks,
                                freshness: .contractMismatch(
                                    daemonContract: "sha256:" + String(repeating: "a", count: 64))),
                            onSelectItem: { _ in },
                            onShowTasks: {},
                            now: screenshotNow))))
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
            surfaces.append(
                Surface(
                    name: "run-timeline-390",
                    width: 390,
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: activeRun)
                            .screenshotContent(timeline)
                    )))
            // The page's Identifiers footer expanded (#1379): the exact run,
            // task, campaign, and parent ids and the specification digest.
            surfaces.append(
                Surface(
                    name: "run-timeline-details",
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: activeRun, expandsTechnicalDetails: true)
                            .screenshotContent(timeline)
                    )))
            surfaces.append(
                Surface(
                    name: "run-timeline-details-390",
                    width: 390,
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: activeRun, expandsTechnicalDetails: true)
                            .screenshotContent(timeline)
                    )))
            // Attempt-ordered history (#1263): two early attempts re-observed
            // after a later completed attempt, plus a review round, so the
            // ordering does not follow observation time.
            let historyRun = RunFixtures.refreshedHistoryRun()
            surfaces.append(
                Surface(
                    name: "run-timeline-history",
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: historyRun)
                            .screenshotContent(RunFixtures.refreshedHistoryTimeline())
                    )))
            surfaces.append(
                Surface(
                    name: "run-timeline-history-390",
                    width: 390,
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: historyRun)
                            .screenshotContent(RunFixtures.refreshedHistoryTimeline())
                    )))

            // What each round reviewed (#1552): the implementation's findings
            // sent to remediation, the adjudication on the rail just before
            // the remediator's milestones, and the re-review of its head.
            surfaces.append(
                Surface(
                    name: "run-timeline-remediation",
                    view: AnyView(
                        RunTimelineView(coordinator: coordinator, snapshot: historyRun)
                            .screenshotContent(try remediationHistoryTimeline())
                    )))

            for colorScheme in [ColorScheme.light, .dark] {
                for width in [CGFloat(820), 390] {
                    for (name, round) in [
                        ("clean", RunFixtures.reviewRound(.completed, availability: .available)),
                        ("findings", RunFixtures.reviewRound(.completed, findings: true, availability: .available)),
                        ("diagnostic", RunFixtures.reviewRound(.failed, availability: .available)),
                    ] {
                        let evidence = RunFixtures.reviewEvidence(runID: activeRun.run.id, round: round)
                        let presentation = ReviewEvidencePresentation(
                            events: Array(evidence.events?.data ?? []),
                            result: evidence.result.map { Array($0.data) }, exitStatus: evidence.exit_status)
                        surfaces.append(
                            Surface(
                                name:
                                    "review-evidence-\(name)-\(Int(width))-\(colorScheme == .dark ? "dark" : "light")",
                                width: width, colorScheme: colorScheme, nativeAppearance: true,
                                view: AnyView(
                                    VStack(alignment: .leading, spacing: 0) {
                                        ReviewEvidenceContent.sheetHeader(for: round)
                                        ReviewEvidenceContent(
                                            round: round, evidence: evidence, presentation: presentation
                                        )
                                        .padding(.horizontal, 16)
                                        .padding(.bottom, 16)
                                    }.background(Color.ground2))))
                    }
                    surfaces.append(
                        Surface(
                            name: "review-output-invalid-utf8-\(Int(width))-\(colorScheme == .dark ? "dark" : "light")",
                            width: width, colorScheme: colorScheme, nativeAppearance: true,
                            view: AnyView(
                                ReviewOutputText(bytes: Array("Retained reviewer output: ".utf8) + [0xff, 0xfe])
                                    .font(FreesideFont.trailingSummary)
                                    .foregroundStyle(Color.ink)
                                    .padding(24))))
                    var unknownRound = RunFixtures.reviewRound(.completed, availability: .unknown)
                    unknownRound.requested_at = nil
                    unknownRound.completed_at = nil
                    unknownRound.provider = nil
                    unknownRound.model_configuration = nil
                    unknownRound.outcome = nil
                    unknownRound.findings_count = nil
                    unknownRound.dispositions = nil
                    unknownRound.source = .init(kind: "future-source", status: "quarantined")
                    var externalRound = RunFixtures.reviewRound(.failed, round: 2)
                    externalRound.retry_pending = true
                    externalRound.source = .init(kind: "github")
                    // Oldest first, as the daemon sends them: two clean rounds at
                    // earlier heads, then the current round with findings.
                    let threeRounds = [
                        RunFixtures.reviewRound(.completed, round: 1),
                        RunFixtures.reviewRound(.completed, round: 2),
                        RunFixtures.reviewRound(.completed, round: 3, findings: true, availability: .available),
                    ]
                    for (name, rounds) in [
                        ("unknown-external", [unknownRound, externalRound]),
                        ("pending", [RunFixtures.reviewRound(.pending)]),
                        ("running", [RunFixtures.reviewRound(.running)]),
                        ("findings", [RunFixtures.reviewRound(.completed, findings: true, availability: .available)]),
                        (
                            "rereview",
                            [
                                RunFixtures.reviewRound(.completed, findings: true),
                                RunFixtures.reviewRound(.running, round: 2),
                            ]
                        ),
                        ("failed", [RunFixtures.reviewRound(.failed)]),
                        ("clean", [RunFixtures.reviewRound(.completed, availability: .available)]),
                        ("three-rounds", threeRounds),
                    ] {
                        surfaces.append(
                            Surface(
                                name: "run-review-\(name)-\(Int(width))-\(colorScheme == .dark ? "dark" : "light")",
                                width: width, colorScheme: colorScheme, nativeAppearance: true,
                                view: AnyView(
                                    RunReviewSection(
                                        coordinator: coordinator, runID: activeRun.run.id, facts: .init(rounds: rounds)
                                    ).padding(24))))
                    }
                    surfaces.append(
                        Surface(
                            name: "run-review-remediation-open-\(Int(width))-"
                                + (colorScheme == .dark ? "dark" : "light"),
                            width: width, colorScheme: colorScheme, nativeAppearance: true,
                            view: AnyView(
                                RunReviewSection(
                                    coordinator: coordinator, runID: activeRun.run.id,
                                    facts: .init(rounds: [
                                        RemediationFixtures.findingsRound(),
                                        RemediationFixtures.reReview(.running),
                                    ]),
                                    stages: RemediationFixtures.stages,
                                    startsExpanded: true
                                ).padding(24))))
                    // Every fold open: the newest round's facts and the prior
                    // rounds as verdict rows with their own facts.
                    surfaces.append(
                        Surface(
                            name: "run-review-three-rounds-open-\(Int(width))-"
                                + (colorScheme == .dark ? "dark" : "light"),
                            width: width, colorScheme: colorScheme, nativeAppearance: true,
                            view: AnyView(
                                RunReviewSection(
                                    coordinator: coordinator, runID: activeRun.run.id,
                                    facts: .init(rounds: threeRounds), startsExpanded: true
                                ).padding(24))))
                    // A lone round with its facts open, which no surface above
                    // shows: a round in progress before and after it has run
                    // long, one whose reviewer the daemon named, and a
                    // completed round to read them against. The request times
                    // are set against the pinned clock, because the fixture's
                    // own is later than it.
                    for (name, round) in [
                        (
                            "running-open",
                            RunFixtures.reviewRound(.running, requestedAt: screenshotNow.addingTimeInterval(-12 * 60))
                        ),
                        (
                            "running-long-open",
                            RunFixtures.reviewRound(.running, requestedAt: screenshotNow.addingTimeInterval(-47 * 60))
                        ),
                        (
                            "running-identified-open",
                            RunFixtures.reviewRound(
                                .running, requestedAt: screenshotNow.addingTimeInterval(-12 * 60),
                                provider: "openai", modelConfiguration: "codex/high")
                        ),
                        ("clean-open", RunFixtures.reviewRound(.completed, availability: .available)),
                    ] {
                        surfaces.append(
                            Surface(
                                name: "run-review-\(name)-\(Int(width))-\(colorScheme == .dark ? "dark" : "light")",
                                width: width, colorScheme: colorScheme, nativeAppearance: true,
                                view: AnyView(
                                    RunReviewSection(
                                        coordinator: coordinator, runID: activeRun.run.id,
                                        facts: .init(rounds: [round]), startsExpanded: true
                                    ).padding(24))))
                    }
                }
            }

            await server.seedPairingCode("483911")
            let pairing = PairingModel(
                client: client,
                credentials: InMemoryCredentialStore(),
                pairingCode: "483911"
            )
            pairing.displayName = "Operator's iPhone"
            await pairing.refreshFacts()
            let pairingFacts = try #require(pairing.facts)
            surfaces.append(
                Surface(
                    name: "pairing",
                    view: AnyView(
                        PairingView(model: pairing) { _ in }.screenshotContent(
                            now: pairingFacts.code_expires_at.addingTimeInterval(-14 * 60)))
                ))
            // The same surface before the daemon has described the code:
            // no facts, and Pair drawn in the disabled recipe.
            let awaitingFacts = PairingModel(
                client: client,
                credentials: InMemoryCredentialStore(),
                pairingCode: "000000"
            )
            awaitingFacts.displayName = "Operator's iPhone"
            await awaitingFacts.refreshFacts()
            surfaces.append(
                Surface(
                    name: "pairing-awaiting-facts",
                    view: AnyView(
                        PairingView(model: awaitingFacts) { _ in }.screenshotContent(now: screenshotNow))
                ))
            let taskProposalFacts = Components.Schemas.TaskProposalFactsSnapshot(
                as_of_revision: 12,
                entity_version: 12,
                item_version: 3,
                proposal_digest: "sha256:task-proposal-screenshot",
                supersedes: nil,
                intent: .implement_subject,
                expected_cost_units: 12,
                scope: .init(
                    component_count: 1,
                    declared_path_count: 3,
                    touches_control_plane: false))
            surfaces.append(
                Surface(
                    name: "task-proposal-revision-sheet",
                    width: 480,
                    view: AnyView(
                        TaskProposalRevisionSheet(facts: taskProposalFacts) { _ in }
                            .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-revision-sheet-phone",
                    width: 390,
                    view: AnyView(
                        TaskProposalRevisionSheet(facts: taskProposalFacts) { _ in }
                            .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-revision-sheet-dark",
                    width: 480,
                    colorScheme: .dark,
                    view: AnyView(
                        TaskProposalRevisionSheet(facts: taskProposalFacts) { _ in }
                            .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-revision-sheet-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        TaskProposalRevisionSheet(facts: taskProposalFacts) { _ in }
                            .screenshotContent())))

            // The effect-proposal edit sheet: one control for whether the PR
            // closes the issue. Pinned in the same four variants as the task
            // revision sheet.
            let effectProposalFacts = try #require(
                AttentionFixtures.effectProposalFacts(
                    for: AttentionFixtures.fixture(type: .effect_proposal)))
            for (suffix, width, scheme) in [
                ("", CGFloat(480), ColorScheme.light),
                ("-phone", CGFloat(390), ColorScheme.light),
                ("-dark", CGFloat(480), ColorScheme.dark),
                ("-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: "effect-proposal-revision-sheet\(suffix)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            EffectProposalRevisionSheet(facts: effectProposalFacts) { _ in }
                                .screenshotContent())))
            }

            // The recommended effect-proposal card is not in the default
            // inbox, so it is rendered from its own store here, in the same
            // four variants as the verified card in the inbox loop above.
            let recommendedEffect = AttentionFixtures.recommendedEffectProposal()
            let recommendedEffectStore = InboxStore(client: client)
            recommendedEffectStore.replaceAll(with: [recommendedEffect])
            let recommendedEffectDetail = DecisionDetailView(
                store: recommendedEffectStore,
                itemID: recommendedEffect.item.id,
                graphics: .init(),
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            let recommendedEffectFacts = AttentionFixtures.effectProposalFacts(for: recommendedEffect)
            for (suffix, width, scheme) in [
                ("", canvasWidth, ColorScheme.light),
                ("-phone", CGFloat(390), ColorScheme.light),
                ("-dark", canvasWidth, ColorScheme.dark),
                ("-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: "decision-effect_proposal-recommended\(suffix)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            recommendedEffectDetail.screenshotCard(
                                recommendedEffect.item,
                                at: dynamicTypeSize,
                                effectProposalFacts: recommendedEffectFacts))))
            }

            // The follow-up filing card is not in the default inbox either.
            // Its facts carry a filing arm instead of a closure, so these
            // pin the filing rows and the proposed title and body in their
            // unverified sections, at Mac and iPhone width in both schemes.
            let filingEffect = AttentionFixtures.followUpFilingEffectProposal()
            let filingEffectStore = InboxStore(client: client)
            filingEffectStore.replaceAll(with: [filingEffect])
            let filingEffectDetail = DecisionDetailView(
                store: filingEffectStore,
                itemID: filingEffect.item.id,
                graphics: .init(),
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            let filingEffectFacts = AttentionFixtures.effectProposalFacts(for: filingEffect)
            for (suffix, width, scheme) in [
                ("", canvasWidth, ColorScheme.light),
                ("-phone", CGFloat(390), ColorScheme.light),
                ("-dark", canvasWidth, ColorScheme.dark),
                ("-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: "decision-effect_proposal-filing\(suffix)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            filingEffectDetail.screenshotCard(
                                filingEffect.item,
                                at: dynamicTypeSize,
                                effectProposalFacts: filingEffectFacts))))
            }
            // The image policy adoption card is not in the default inbox
            // either. Its facts carry the digest and hosts one upkeep refusal
            // needs, so these pin its rows and its approve and decline at
            // Mac and iPhone width in both schemes.
            let adoptionEffect = AttentionFixtures.imagePolicyAdoptionEffectProposal()
            let adoptionEffectStore = InboxStore(client: client)
            adoptionEffectStore.replaceAll(with: [adoptionEffect])
            let adoptionEffectDetail = DecisionDetailView(
                store: adoptionEffectStore,
                itemID: adoptionEffect.item.id,
                graphics: .init(),
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            let adoptionEffectFacts = AttentionFixtures.effectProposalFacts(for: adoptionEffect)
            for (suffix, width, scheme) in [
                ("", canvasWidth, ColorScheme.light),
                ("-phone", CGFloat(390), ColorScheme.light),
                ("-dark", canvasWidth, ColorScheme.dark),
                ("-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: "decision-effect_proposal-adoption\(suffix)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            adoptionEffectDetail.screenshotCard(
                                adoptionEffect.item,
                                at: dynamicTypeSize,
                                effectProposalFacts: adoptionEffectFacts))))
            }
            surfaces.append(
                Surface(
                    name: "task-proposal-snooze-sheet",
                    width: 480,
                    view: AnyView(
                        TaskProposalSnoozeSheet(
                            now: screenshotNow,
                            screenshotTimeZone: screenshotTimeZone
                        ) { _ in }
                        .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-snooze-sheet-phone",
                    width: 390,
                    view: AnyView(
                        TaskProposalSnoozeSheet(
                            now: screenshotNow,
                            screenshotTimeZone: screenshotTimeZone
                        ) { _ in }
                        .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-snooze-sheet-dark",
                    width: 480,
                    colorScheme: .dark,
                    view: AnyView(
                        TaskProposalSnoozeSheet(
                            now: screenshotNow,
                            screenshotTimeZone: screenshotTimeZone
                        ) { _ in }
                        .screenshotContent())))
            surfaces.append(
                Surface(
                    name: "task-proposal-snooze-sheet-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        TaskProposalSnoozeSheet(
                            now: screenshotNow,
                            screenshotTimeZone: screenshotTimeZone
                        ) { _ in }
                        .screenshotContent())))
            if let selected = inbox.first?.item {
                let bannerDetail = DecisionDetailView(
                    store: store,
                    itemID: selected.id,
                    loadsAttachments: false,
                    showsValidationProgress: false)
                surfaces.append(
                    Surface(
                        name: "decision-banner",
                        width: 480,
                        view: AnyView(bannerDetail.screenshotBanner())))
                surfaces.append(
                    Surface(
                        name: "decision-retryable-receipt",
                        width: 480,
                        view: AnyView(
                            bannerDetail.screenshotRetryableReceipt(expanded: false))))
                surfaces.append(
                    Surface(
                        name: "decision-retryable-receipt-open",
                        width: 480,
                        view: AnyView(
                            bannerDetail.screenshotRetryableReceipt(expanded: true))))
                surfaces.append(
                    Surface(
                        name: "decision-retryable-receipt-phone-dark",
                        width: 390,
                        colorScheme: .dark,
                        view: AnyView(
                            bannerDetail.screenshotRetryableReceipt(expanded: false))))
            }
            surfaces += try await makeRecordedAcknowledgeSurfaces(at: dynamicTypeSize)
            surfaces.append(
                Surface(
                    name: "unavailable-state",
                    width: 480,
                    view: AnyView(
                        UnavailableStateView(
                            glyph: .inbox, title: "No decision selected",
                            description: "Select an item to inspect its facts."))))

            // Freshness banner over fresh vs. genuinely stale data (#1130).
            // The banner compares `lastUpdatedAt` against the wall clock at
            // render time, so each surface's timestamp carries a margin large
            // enough to survive the suite's own runtime: makeSurfaces builds
            // the whole array before any surface renders, and these are
            // appended last, so a slow or loaded runner can render them well
            // after `Date()` was read. The fresh surface's update sits an
            // hour ahead, so no plausible render delay can age it past the
            // threshold; it always draws the empty (fresh) banner. The stale
            // surface's update sits just past the threshold in the past and
            // only ages further, so it always draws the Stale row. The fresh
            // empty-banner image is the pre-fix regression witness (the old
            // banner drew the Stale row for fresh data), and the trailing
            // label keeps it non-empty so `render` does not fail on a nil
            // raster.
            func freshnessSurface(name: String, lastUpdatedAt: Date) -> Surface {
                Surface(
                    name: name,
                    width: 640,
                    view: AnyView(
                        VStack(spacing: 0) {
                            FreshnessBanner(freshness: .fresh, lastUpdatedAt: lastUpdatedAt)
                            Text("Inbox").padding()
                        }))
            }
            surfaces.append(
                freshnessSurface(
                    name: "freshness-banner-fresh",
                    lastUpdatedAt: Date().addingTimeInterval(3_600)))
            surfaces.append(
                freshnessSurface(
                    name: "freshness-banner-stale",
                    lastUpdatedAt: Date().addingTimeInterval(
                        -(SyncCoordinator.stalenessThreshold + 1))))
            // The contract-mismatch banner shows both short digests and the
            // fix, distinct from the sync-failing and unreachable banners
            // (#1265). The daemon digest is a fixed fixture value.
            surfaces.append(
                Surface(
                    name: "freshness-banner-contract-mismatch",
                    width: 640,
                    view: AnyView(
                        VStack(spacing: 0) {
                            FreshnessBanner(
                                freshness: .contractMismatch(
                                    daemonContract: "sha256:" + String(repeating: "a", count: 64)))
                            Text("Inbox").padding()
                        })))

            // The standing stopped indicator (#980) under the freshness
            // banner, where the root view places it: at the top of the Mac's
            // detail column, at the card's x and width, and in the slot
            // above the list at phone width.
            // It reads the same wall clock the banner does, so the fresh and
            // stale timestamps carry the margins described above. Fresh, it
            // states the stop as current; stale, it sits under the Stale row
            // and reports only the last known state. The finding surface
            // pins the wording for a stop no operator made.
            let operatorStopped = Components.Schemas.UnattendedOperationSnapshot(
                admission: .stopped,
                stops: [
                    .init(
                        kind: .operator_stop,
                        item_id: "system-health-unattended-stopped-cmd-stop",
                        command_id: "cmd-stop", since: AttentionFixtures.createdInstant)
                ])
            let findingStopped = Components.Schemas.UnattendedOperationSnapshot(
                admission: .stopped,
                stops: [
                    .init(
                        kind: .blocking_system_health,
                        item_id: "system-health-daemon-durable-stop-7",
                        command_id: nil, since: AttentionFixtures.createdInstant)
                ])
            func stoppedSurface(
                name: String, width: CGFloat,
                freshness: InboxStore.Freshness = .fresh,
                operation: Components.Schemas.UnattendedOperationSnapshot,
                lastUpdatedAt: Date,
                reason: String = "The daemon stopped unattended operation after repeated restarts"
            ) -> Surface {
                let inDetailColumn = width > 390
                let notices = StandingNotices(
                    saveWarning: nil, onDismissSaveWarning: {},
                    freshness: freshness, lastUpdatedAt: lastUpdatedAt, onRePair: nil,
                    operation: operation,
                    reason: { _ in reason },
                    onOpenItem: { _ in },
                    placement: inDetailColumn ? .detailColumn : .window)
                return Surface(
                    name: name,
                    width: width,
                    view: inDetailColumn
                        ? AnyView(
                            StandingDetailColumn(notices: notices) { _ in Text("Inbox").padding() })
                        : AnyView(
                            VStack(spacing: 0) {
                                notices
                                Text("Inbox").padding()
                            }))
            }
            let stoppedFresh = Date().addingTimeInterval(3_600)
            let stoppedStale = Date().addingTimeInterval(
                -(SyncCoordinator.stalenessThreshold + 1))
            for (suffix, width) in [("", Self.detailColumnWidth), ("-phone", CGFloat(390))] {
                surfaces.append(
                    stoppedSurface(
                        name: "unattended-stopped-fresh" + suffix, width: width,
                        operation: operatorStopped, lastUpdatedAt: stoppedFresh))
                surfaces.append(
                    stoppedSurface(
                        name: "unattended-stopped-stale" + suffix, width: width,
                        operation: operatorStopped, lastUpdatedAt: stoppedStale))
            }
            surfaces.append(
                stoppedSurface(
                    name: "unattended-stopped-finding", width: Self.detailColumnWidth,
                    operation: findingStopped, lastUpdatedAt: stoppedFresh))
            // The exit run's phone state (#2313): the daemon unreachable over
            // a cached finding stop whose reason is a ten-line Go error
            // chain, the shape that once pushed the inbox below the fold.
            let errorChainReason = (1...10).map { line in
                "production_publication: reconcile production publications: task "
                    + "\"production-publication/run-\(line)\": load publication disposition "
                    + "history: child record's parent key does not match"
            }.joined(separator: "\n")
            surfaces.append(
                stoppedSurface(
                    name: "unattended-stopped-unreachable-finding-phone", width: 390,
                    freshness: .unreachable, operation: findingStopped,
                    lastUpdatedAt: stoppedFresh, reason: errorChainReason))

            // The non-image attachment reader as a sheet: inline serif title,
            // the mono text preview laid out in place of its scroll view,
            // and the single Done pill.
            let readerPreview = DecisionDetailView.NonImagePreview(
                bytes: Data("verify: 3 checks passed\nlint: clean\nbuild: ok\n".utf8))
            surfaces.append(
                Surface(
                    name: "attachment-reader-sheet",
                    width: 480,
                    view: AnyView(
                        DecisionDetailView.NonImageAttachmentSheet(
                            label: "verify_log", preview: readerPreview,
                            rendersScrollableContent: false))))
            surfaces.append(
                Surface(
                    name: "attachment-reader-sheet-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        DecisionDetailView.NonImageAttachmentSheet(
                            label: "verify_log", preview: readerPreview,
                            rendersScrollableContent: false))))
            // A claim label the contract admits at any length: the header
            // clamps the title to two lines so the preview and Done pill
            // stay on the sheet.
            surfaces.append(
                Surface(
                    name: "attachment-reader-sheet-long-title",
                    width: 480,
                    view: AnyView(
                        DecisionDetailView.NonImageAttachmentSheet(
                            label: String(
                                repeating: "verify_log for the release candidate build\n", count: 6),
                            preview: readerPreview,
                            rendersScrollableContent: false))))

            // The consequential confirmation sheet: Stop on a run-scoped item
            // as the macOS sheet by day, Decline on a proposal as the iPhone
            // bottom sheet by dusk. The binding line reads the fixture's
            // subject id and item version.
            let stopItem = AttentionFixtures.fixture(type: .execution_failure).item
            let declineItem = AttentionFixtures.fixture(type: .task_proposal).item
            surfaces.append(
                Surface(
                    name: "consequence-sheet-stop",
                    width: 380,
                    view: AnyView(
                        ConsequenceSheet(action: .stop, item: stopItem, submit: {}, cancel: {}))))
            surfaces.append(
                Surface(
                    name: "consequence-sheet-decline-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        ConsequenceSheet(action: .decline, item: declineItem, submit: {}, cancel: {}))))

            // The menu-bar panel in the states the chrome handoff pins:
            // healthy by day with the inbox row under the pointer, a
            // contract mismatch after an observed restart by dusk, stopped
            // with Start as the one primary, unreachable with a failed stop,
            // and an ephemeral app's no-supervised-daemon state with no
            // control. The start instant is the fixture's fixed creation time.
            let panelHealth = DaemonHealth(version: "1.0.0", startedAt: AttentionFixtures.createdInstant)
            let panelMismatch = DaemonHealth(
                version: "1.0.0", startedAt: AttentionFixtures.createdInstant,
                contractDigest: "sha256:" + String(repeating: "a", count: 64))
            let panelActions = DaemonMenuPanel.Actions(
                openApp: {}, showInbox: {}, start: {}, stop: {}, openApprovalSettings: {}, quit: {})
            surfaces.append(
                Surface(
                    name: "menu-panel-running",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelHealth, restartObserved: false),
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 2),
                            actions: panelActions
                        )
                        .screenshotHovering(.showInbox))))
            // A healthy daemon whose unattended operation is stopped (#980):
            // the Running block stands and the wax card says what is stopped.
            surfaces.append(
                Surface(
                    name: "menu-panel-unattended-stopped",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelHealth, restartObserved: false),
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 2),
                            unattendedStopped:
                                "Unattended operation is stopped by operator decision.",
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-mismatch-restart-dark",
                    width: 320,
                    colorScheme: .dark,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelMismatch, restartObserved: true),
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 2),
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-stopped",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .stopped,
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 0),
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-unsupervised",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .unsupervised,
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 0),
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-unreachable-error-dark",
                    width: 320,
                    colorScheme: .dark,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .unreachable,
                            actionError: "The daemon could not be stopped: launchctl exited 3.",
                            inbox: .init(open: 6, urgent: 0),
                            actions: panelActions))))

            var due = AttentionFixtures.degradedReady().item
            due.expires_when = screenshotNow.addingTimeInterval(2 * 3_600)
            let age = AttentionFixtures.fixture(type: .spec_approval).item
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "inbox-time-grammar" + (colorScheme == .dark ? "-dark" : ""),
                        width: 480,
                        colorScheme: colorScheme,
                        view: AnyView(
                            VStack(spacing: 8) {
                                InboxRowView(item: due, now: screenshotNow)
                                InboxRowView(item: age, now: screenshotNow)
                            }.padding().background(Color.ground))))
            }
            for surface in surfaces
            where [
                "pairing", "freshness-banner-stale", "freshness-banner-contract-mismatch",
                "operational-summary-contract-mismatch",
            ].contains(surface.name) {
                surfaces.append(
                    Surface(
                        name: surface.name + "-dark",
                        width: surface.width,
                        colorScheme: .dark,
                        view: AnyView(
                            surface.view
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .background(Color.ground))))
            }

            surfaces.append(contentsOf: try taskStopSurfaces())
            surfaces.append(contentsOf: taskStyleSurfaces())
            surfaces.append(contentsOf: cardStyleSurfaces())
            surfaces.append(contentsOf: choiceListSurfaces())
            surfaces.append(contentsOf: finalReviewFrameSurfaces(store: store, at: dynamicTypeSize))

            // Visual audit D08: the dispute card on both platforms and
            // themes with the positions the screenshot graphics supply, then
            // the shape production sends, which has no positions: one claim
            // with its text, and one that arrived as an attachment. These
            // render after the surfaces above for the reason the inspectors
            // below do.
            if let snapshot = inbox.first(where: { $0.item._type == .review_dispute }) {
                let detail = DecisionDetailView(
                    store: store,
                    itemID: snapshot.item.id,
                    graphics: graphicPresentations(for: snapshot.item),
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow)
                let name = "decision-\(snapshot.item._type.rawValue)"
                for (suffix, width, scheme) in [
                    ("phone", CGFloat(390), ColorScheme.light),
                    ("dark", canvasWidth, ColorScheme.dark),
                    ("phone-dark", CGFloat(390), ColorScheme.dark),
                ] {
                    surfaces.append(
                        Surface(
                            name: "\(name)-\(suffix)",
                            width: width,
                            colorScheme: scheme,
                            view: AnyView(
                                detail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    compactLayout: width < canvasWidth))))
                }
                let oneClaimDetail = DecisionDetailView(
                    store: store,
                    itemID: snapshot.item.id,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow)
                for (variant, item) in [
                    ("one-claim", snapshot.item),
                    ("no-inline-text", AttentionFixtures.disputeWithoutInlineText().item),
                ] {
                    for (suffix, width) in [("", canvasWidth), ("-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "\(name)-\(variant)\(suffix)",
                                width: width,
                                view: AnyView(
                                    oneClaimDetail.screenshotCard(
                                        item,
                                        at: dynamicTypeSize,
                                        compactLayout: width < canvasWidth))))
                    }
                }
            }

            for (variant, routed) in [
                ("routed", AttentionFixtures.routedReviewDispute()),
                ("subset", AttentionFixtures.subsetReviewDispute()),
            ] {
                let routedClient = APIClientFactory.mock(server: MockServer(items: [routed]))
                let routedStore = InboxStore(client: routedClient, now: { screenshotNow })
                routedStore.replaceAll(with: [routed])
                await preloadReferences(routed.item, into: routedStore)
                let routedModel = DecisionModel(store: routedStore, itemID: routed.item.id)
                await routedModel.validate()
                await routedModel.refreshActionSurface()
                #expect(routedModel.validation == .validated)
                let routedDetail = DecisionDetailView(
                    store: routedStore, itemID: routed.item.id,
                    loadsAttachments: false, showsValidationProgress: false,
                    now: screenshotNow, model: routedModel)
                let expandedRoutedDetail = DecisionDetailView(
                    store: routedStore, itemID: routed.item.id, expandedDisclosures: [.runDetails],
                    loadsAttachments: false, showsValidationProgress: false,
                    now: screenshotNow, model: routedModel)
                let selectedRoutedDetail = DecisionDetailView(
                    store: routedStore, itemID: routed.item.id,
                    alternativeSelections: ["finding-1": .remediate, "finding-2": ._defer],
                    loadsAttachments: false, showsValidationProgress: false,
                    now: screenshotNow, model: routedModel)
                // One route picked and its finding's Reason and Source fold
                // open: the fold's contents, and Apply counting a partial pick.
                let reasonRoutedDetail = DecisionDetailView(
                    store: routedStore, itemID: routed.item.id, expandedFindings: ["finding-1"],
                    alternativeSelections: ["finding-1": .remediate],
                    loadsAttachments: false, showsValidationProgress: false,
                    now: screenshotNow, model: routedModel)
                // Decline on the external finding: the reply it posts needs
                // the operator's reason, so the card asks for one.
                let declineRoutedDetail = DecisionDetailView(
                    store: routedStore, itemID: routed.item.id,
                    alternativeSelections: ["finding-1": .decline, "finding-2": ._defer],
                    loadsAttachments: false, showsValidationProgress: false,
                    now: screenshotNow, model: routedModel)
                for (suffix, width, scheme) in [
                    ("", canvasWidth, ColorScheme.light),
                    ("-dark", canvasWidth, ColorScheme.dark),
                    ("-phone", CGFloat(390), ColorScheme.light),
                    ("-phone-dark", CGFloat(390), ColorScheme.dark),
                ] {
                    surfaces.append(
                        Surface(
                            name: "decision-review_dispute-\(variant)\(suffix)", width: width, colorScheme: scheme,
                            view: AnyView(
                                routedDetail.screenshotCard(
                                    routed.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                    surfaces.append(
                        Surface(
                            name: "decision-review_dispute-\(variant)-selected\(suffix)", width: width,
                            colorScheme: scheme,
                            view: AnyView(
                                selectedRoutedDetail.screenshotCard(
                                    routed.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                    if variant == "routed" {
                        surfaces.append(
                            Surface(
                                name: "decision-review_dispute-routed-decline\(suffix)", width: width,
                                colorScheme: scheme,
                                view: AnyView(
                                    declineRoutedDetail.screenshotCard(
                                        routed.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                        surfaces.append(
                            Surface(
                                name: "decision-review_dispute-routed-reason\(suffix)", width: width,
                                colorScheme: scheme,
                                view: AnyView(
                                    reasonRoutedDetail.screenshotCard(
                                        routed.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                    }
                    if variant == "subset" {
                        surfaces.append(
                            Surface(
                                name: "decision-review_dispute-subset-details\(suffix)", width: width,
                                colorScheme: scheme,
                                view: AnyView(
                                    expandedRoutedDetail.screenshotCard(
                                        routed.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                    }
                }
            }

            // Visual audit D06: a question that enumerates no options keeps
            // the generic ask with its reason under it.
            if var untyped = inbox.first(where: { $0.item._type == .agent_question })?.item {
                untyped.agent_question = nil
                let detail = DecisionDetailView(
                    store: store,
                    itemID: untyped.id,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow)
                surfaces.append(
                    Surface(
                        name: "decision-agent_question-untyped-phone",
                        width: 390,
                        view: AnyView(
                            detail.screenshotCard(
                                untyped, at: dynamicTypeSize, compactLayout: true))))
            }

            // The question and dispute cards' inspectors (visual audit D06,
            // D08), which keep their layout and change only their label. They
            // render after every other surface: a glyph's antialiasing
            // depends on what was rasterized before it (#1698), and drawing
            // one ahead of the final-review inspector moved one of that
            // surface's pixels.
            for type in [Components.Schemas.AttentionType.agent_question, .review_dispute] {
                guard let item = inbox.first(where: { $0.item._type == type })?.item else {
                    continue
                }
                let suite = "FreesideScreenshotCardInspectorPreferences-\(UUID().uuidString)"
                guard let defaults = UserDefaults(suiteName: suite) else {
                    throw ScreenshotError.preferencesUnavailable
                }
                defer { defaults.removePersistentDomain(forName: suite) }
                let preferences = DecisionSectionPreferences(defaults: defaults)
                preferences.claimsExpanded = true
                preferences.evidenceExpanded = true
                preferences.detailsExpanded = true
                let inspector = DecisionDetailView(
                    store: store,
                    itemID: item.id,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    sectionPreferences: preferences)
                surfaces.append(
                    Surface(
                        name: "decision-\(type.rawValue)-inspector",
                        width: 360,
                        view: AnyView(
                            inspector.screenshotInspector(item, at: dynamicTypeSize))))
            }

            // The per-finding card (visual audit D09) with messages of the
            // length a reviewer writes, on both platforms and in both
            // appearances, and the phone card with one finding open. Each
            // card shows its whole message, so the card is pinned with text
            // that wraps (#1033, #1141). Appended last for the same reason
            // as the inspectors above.
            let realisticFindings = AttentionFixtures.findingAdjudicationRealisticMessages().item
            let findingsDetail = DecisionDetailView(
                store: store,
                itemID: realisticFindings.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            for (suffix, colorScheme) in [("", ColorScheme.light), ("-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-realistic-messages\(suffix)",
                        colorScheme: colorScheme,
                        view: AnyView(
                            findingsDetail.screenshotCard(
                                realisticFindings, at: dynamicTypeSize))))
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-realistic-messages-phone\(suffix)",
                        width: 390,
                        colorScheme: colorScheme,
                        view: AnyView(
                            findingsDetail.screenshotCard(
                                realisticFindings, at: dynamicTypeSize, compactLayout: true))))
            }
            let expandedFindingsDetail = DecisionDetailView(
                store: store,
                itemID: realisticFindings.id,
                expandedFindings: ["review-finding-17"],
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-expanded-phone",
                    width: 390,
                    view: AnyView(
                        expandedFindingsDetail.screenshotCard(
                            realisticFindings, at: dynamicTypeSize, compactLayout: true))))
            // A held alternative on a closed card: the picker is inside the
            // disclosure, so the face is the only place the choice shows.
            let selectedAlternativeDetail = DecisionDetailView(
                store: store,
                itemID: realisticFindings.id,
                alternativeSelections: ["review-finding-17": .dispute],
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-alternative-selected",
                    view: AnyView(
                        selectedAlternativeDetail.screenshotCard(
                            realisticFindings, at: dynamicTypeSize))))
            // The Open scope under a project filter that leaves nothing open
            // (R13): the glyph, the statement, and the line that names the
            // filter. The project's one item is resolved, so the filter
            // survives and the scope is empty. Drawn last: ahead of another
            // surface it moves that surface's glyph antialiasing (#1698).
            var settled = try #require(inbox.first)
            settled.item.project_id = "freeside-docs"
            settled.item.status = .resolved
            let emptyStore = InboxStore(client: client, now: { screenshotNow })
            emptyStore.replaceAll(with: [settled])
            emptyStore.selectProjectFilter("freeside-docs")
            for colorScheme in [ColorScheme.light, .dark] {
                surfaces.append(
                    Surface(
                        name: "inbox-empty-scope" + (colorScheme == .dark ? "-dark" : ""),
                        width: 320,
                        colorScheme: colorScheme,
                        view: AnyView(
                            InboxView(
                                store: emptyStore,
                                selection: .constant(nil),
                                launchScope: nil,
                                launchProjectID: nil
                            ).screenshotSidebar(now: screenshotNow, activeTaskCount: sidebarTaskCount))))
            }
            // Frame 6.9 (R18): the finding card with the inspector open, and
            // that inspector, by day and by dusk. The item carries the frame's three attachments and
            // two attachment claims, so the pointer row and both counts have
            // something to count, and the inspector opens Evidence and its
            // bindings and leaves Claims closed, as the frame draws it.
            // Drawn last, for the reason given above (#1698).
            var framed = adjudication
            var report = try #require(framed.evidence_snapshot.first)
            report.id = "art-report-finding_adjudication"
            report._type = .verification_report
            report.digest = "sha256:report-finding_adjudication"
            report.metadata.media_type = .application_sol_json
            report.metadata.size_bytes = 8_192
            var transcript = report
            transcript.id = "art-transcript-finding_adjudication"
            transcript._type = .command_transcript
            transcript.digest = "sha256:transcript-finding_adjudication"
            transcript.metadata.media_type = .text_sol_plain
            transcript.metadata.size_bytes = 2_048
            framed.evidence_snapshot += [report, transcript]
            var reviewerLog = try #require(framed.agent_claims.first { $0.text == nil })
            reviewerLog.label = "reviewer log"
            reviewerLog.artifact_id = "art-reviewer-log-finding_adjudication"
            reviewerLog.digest = "sha256:reviewer-log-finding_adjudication"
            framed.agent_claims.append(reviewerLog)
            let framedSuite = "FreesideScreenshotFramedInspectorPreferences-\(UUID().uuidString)"
            guard let framedDefaults = UserDefaults(suiteName: framedSuite) else {
                throw ScreenshotError.preferencesUnavailable
            }
            defer { framedDefaults.removePersistentDomain(forName: framedSuite) }
            let framedPreferences = DecisionSectionPreferences(defaults: framedDefaults)
            framedPreferences.evidenceExpanded = true
            framedPreferences.detailsExpanded = true
            let framedDetail = DecisionDetailView(
                store: store,
                itemID: framed.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                sectionPreferences: framedPreferences)
            for (suffix, colorScheme) in [("", ColorScheme.light), ("-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-inspector-open\(suffix)",
                        colorScheme: colorScheme,
                        view: AnyView(
                            framedDetail.screenshotCard(
                                framed, at: dynamicTypeSize, inspectorPresented: true))))
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-frame-inspector\(suffix)",
                        width: 360,
                        colorScheme: colorScheme,
                        view: AnyView(
                            framedDetail.screenshotInspector(framed, at: dynamicTypeSize))))
            }
            // The two pointer states no other surface can draw: the first
            // control with a segment hovered, the second with one pressed.
            let segments: [FreesideSegmentedControl<Int>.Segment] = [
                .init(value: 0, label: "Open", count: 6),
                .init(value: 1, label: "Snoozed", count: 2),
                .init(value: 2, label: "Done"),
            ]
            for (suffix, colorScheme) in [("", ColorScheme.light), ("-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: "segmented-control-pointer-states\(suffix)",
                        width: 280,
                        colorScheme: colorScheme,
                        view: AnyView(
                            VStack(spacing: 12) {
                                FreesideSegmentedControl(
                                    accessibilityLabel: "Scope", segments: segments,
                                    selection: .constant(0), screenshotHovered: 1)
                                FreesideSegmentedControl(
                                    accessibilityLabel: "Scope", segments: segments,
                                    selection: .constant(0), screenshotPressed: 1)
                            }
                            .padding(12)
                            .background(Color.sidebarGround))))
            }

            // The standing-surface frames no earlier surface drew (#1803).
            // The revoked banner with its recovery action, then the notices
            // stacked: the three the root view can hold at once over a card
            // in the Mac's detail column, and four in the phone's slot.
            let mismatch = InboxStore.Freshness.contractMismatch(
                daemonContract: "sha256:" + String(repeating: "a", count: 64))
            surfaces.append(
                Surface(
                    name: "freshness-banner-revoked",
                    width: 640,
                    view: AnyView(
                        VStack(spacing: 0) {
                            FreshnessBanner(freshness: .unauthenticated, onRePair: {})
                            Text("Inbox").padding()
                        })))
            // The Mac's revoked state: the banner only states, and the
            // detail pane empties around Pair Again. Drawn as the detail
            // pane of a 1,180pt window, where the banner and the empty state
            // share the card's column instead of the pane's width.
            for (suffix, scheme) in [("", ColorScheme.light), ("-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: "revoked-pane" + suffix,
                        width: 860,
                        colorScheme: scheme,
                        view: AnyView(
                            StandingDetailColumn(
                                notices: StandingNotices(
                                    saveWarning: nil, onDismissSaveWarning: {},
                                    freshness: .unauthenticated, lastUpdatedAt: nil, onRePair: nil,
                                    operation: nil, reason: { _ in nil }, onOpenItem: nil,
                                    placement: .detailColumn)
                            ) { _ in
                                RevokedPane(onRePair: {}).frame(height: 320)
                            }
                            .background(Color.ground))))
            }
            // The 9 Oct 2026 wide-pane frame, option 1a: the detail pane of
            // a 1,620pt window, with every notice the root view can hold at
            // once stacked over a card, each at the card's x and width.
            surfaces.append(
                Surface(
                    name: "detail-column-standing-notices",
                    width: 1_300,
                    view: AnyView(
                        StandingDetailColumn(
                            notices: StandingNotices(
                                saveWarning:
                                    "The task was accepted, but its prompt history couldn't be saved.",
                                onDismissSaveWarning: {},
                                freshness: .fresh, lastUpdatedAt: stoppedStale, onRePair: nil,
                                operation: operatorStopped,
                                reason: { _ in
                                    "The daemon stopped unattended operation after repeated restarts"
                                },
                                onOpenItem: { _ in },
                                placement: .detailColumn)
                        ) { topMargin in
                            recommendedDetail.screenshotCard(
                                adjudication, at: dynamicTypeSize, topMargin: topMargin)
                        }
                        .background(Color.ground))))
            surfaces.append(
                Surface(
                    name: "standing-notices-stack-phone",
                    width: 390,
                    view: AnyView(
                        VStack(spacing: 0) {
                            FreshnessBanner(freshness: .fresh, lastUpdatedAt: stoppedStale)
                            UnattendedStoppedIndicator(
                                operation: operatorStopped, freshness: .fresh,
                                lastUpdatedAt: stoppedStale,
                                reason: { _ in
                                    "The daemon stopped unattended operation after repeated restarts"
                                },
                                onOpenItem: { _ in })
                            FreshnessBanner(freshness: .unauthenticated, onRePair: {})
                            FreshnessBanner(freshness: mismatch)
                            Text("Inbox").padding()
                        })))
            // Pairing at phone width, and with under a minute left, where
            // the countdown keeps the accent.
            surfaces.append(
                Surface(
                    name: "pairing-phone",
                    width: 390,
                    view: AnyView(
                        PairingView(model: pairing) { _ in }.screenshotContent(
                            now: pairingFacts.code_expires_at.addingTimeInterval(-14 * 60)))))
            surfaces.append(
                Surface(
                    name: "pairing-expiring-phone",
                    width: 390,
                    view: AnyView(
                        PairingView(model: pairing) { _ in }.screenshotContent(
                            now: pairingFacts.code_expires_at.addingTimeInterval(-45)))))
            // The readers at the inspector's two narrow widths, where the
            // header row shortens its keyword, and the diff with its later
            // hunks open.
            let paneRevised = AttentionFixtures.revisedSpecification()
            let paneStore = InboxStore(client: client)
            paneStore.replaceAll(with: [paneRevised])
            let paneDetail = DecisionDetailView(
                store: paneStore,
                itemID: paneRevised.item.id,
                graphics: .init(),
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            for width in [CGFloat(320), 480] {
                surfaces.append(
                    Surface(
                        name: "decision-spec_approval-specification-reader-\(Int(width))",
                        width: width,
                        view: AnyView(
                            paneDetail.screenshotSpecApprovalReader(.specification, item: paneRevised.item))))
                surfaces.append(
                    Surface(
                        name: "decision-spec_approval-diff-reader-\(Int(width))",
                        width: width,
                        view: AnyView(
                            paneDetail.screenshotSpecApprovalReader(.diff, item: paneRevised.item))))
            }
            surfaces.append(
                Surface(
                    name: "decision-spec_approval-diff-reader-later-hunks",
                    width: 720,
                    view: AnyView(
                        paneDetail.screenshotSpecApprovalReader(
                            .diff, item: paneRevised.item, expandsLaterHunks: true))))
            // The panel with the pointer on one row and the keyboard
            // highlight on the daemon control.
            surfaces.append(
                Surface(
                    name: "menu-panel-keyboard-control",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelHealth, restartObserved: false),
                            actionError: nil,
                            inbox: .init(open: 6, urgent: 2),
                            actions: panelActions
                        )
                        .screenshotHovering(.showInbox)
                        .screenshotHighlighting(.control))))
            surfaces.append(contentsOf: try pendingStopsSurfaces())
            // A finding whose message carries Markdown, on the dispute card
            // and the adjudication card: the title, then a code span and a
            // list as formatted blocks (#2060). Drawn last, for the reason
            // given above (#1698).
            let markdownDispute = AttentionFixtures.markdownReviewDispute()
            let markdownDisputeStore = InboxStore(
                client: APIClientFactory.mock(server: MockServer(items: [markdownDispute])),
                now: { screenshotNow })
            markdownDisputeStore.replaceAll(with: [markdownDispute])
            await preloadReferences(markdownDispute.item, into: markdownDisputeStore)
            let markdownDisputeModel = DecisionModel(store: markdownDisputeStore, itemID: markdownDispute.item.id)
            await markdownDisputeModel.validate()
            await markdownDisputeModel.refreshActionSurface()
            #expect(markdownDisputeModel.validation == .validated)
            let markdownDisputeDetail = DecisionDetailView(
                store: markdownDisputeStore, itemID: markdownDispute.item.id,
                loadsAttachments: false, showsValidationProgress: false,
                now: screenshotNow, model: markdownDisputeModel)
            let markdownFindings = AttentionFixtures.findingAdjudicationMarkdownMessages().item
            let markdownFindingsDetail = DecisionDetailView(
                store: store,
                itemID: markdownFindings.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            for (suffix, width) in [("", canvasWidth), ("-phone", CGFloat(390))] {
                surfaces.append(
                    Surface(
                        name: "decision-review_dispute-markdown\(suffix)", width: width,
                        view: AnyView(
                            markdownDisputeDetail.screenshotCard(
                                markdownDispute.item, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-markdown\(suffix)", width: width,
                        view: AnyView(
                            markdownFindingsDetail.screenshotCard(
                                markdownFindings, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
            }
            return surfaces
        }

        /// The pieces the task surfaces share, by day and by dusk: the three
        /// status-chip cuts (one long enough to wrap), a sentence disclosure
        /// closed and open, the navigation link, and a newest-first rail with
        /// its filled current marker over a hollow prior ring and a stage
        /// start.
        private func taskStyleSurfaces() -> [Surface] {
            let rail: [TimelineRail.Row] = [
                .init(kind: .event, label: "Review round 3 · Findings", recordedAt: screenshotNow, isNewest: true),
                .init(kind: .event, label: "Implementation completed · Pass 2", recordedAt: nil),
                .init(kind: .stageStart(nil), label: "Run submitted", recordedAt: nil),
            ]
            return [ColorScheme.light, .dark].map { scheme in
                Surface(
                    name: "task-style-\(scheme)", width: 390, colorScheme: scheme,
                    view: AnyView(
                        VStack(alignment: .leading, spacing: 12) {
                            StateChip(label: "Specification Approval Required", cut: .attention)
                            StateChip(label: "Stopping", cut: .ink)
                            StateChip(label: "Superseded Run · Historical", cut: .faint)
                            StateChip(
                                label: "Failed to Stop · Execution May Continue · Inspect the Recorded Outcome",
                                cut: .ink)
                            SentenceDisclosure(
                                label: "Campaign", summary: "current · 2 attempts · from Sep 11",
                                isExpanded: .constant(false)
                            ) { EmptyView() }
                            SentenceDisclosure(
                                label: "Run Details", summary: "Implementation · retry of attempt 1",
                                isExpanded: .constant(true)
                            ) {
                                Text("Reason: retry").font(FreesideFont.callout)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                            }
                            FreesideLink(title: "Open run history")
                            TimelineRail(rows: rail)
                        }.padding(24).foregroundStyle(Color.ink).background(Color.ground)))
            }
        }

        /// The pieces a decision card composes, by day and by dusk: the type
        /// eyebrow with its chip and with the unverified register, the agent
        /// quote with and without its producer label, the daemon's callout,
        /// the three notice tones (one with an action), a compact mark, and
        /// a fact row whose value is a chip.
        /// Survey Part 2 card 4b: the final review at the frame's 560pt card,
        /// day and dusk, on the clean fixture and on the degraded one the
        /// frame draws. No test graphics: a production card carries none, so
        /// View PR and Return to agent sit in one control group as the frame
        /// has them.
        private func finalReviewFrameSurfaces(
            store: InboxStore, at dynamicTypeSize: DynamicTypeSize
        ) -> [Surface] {
            let clean = AttentionFixtures.fixture(type: .ready_for_final_review).item
            let detail = DecisionDetailView(
                store: store,
                itemID: clean.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            let width = Self.detailColumnWidth
            return [("clean", clean), ("degraded", AttentionFixtures.degradedReady().item)].flatMap {
                name, item in
                [("light", ColorScheme.light), ("dark", ColorScheme.dark)].map { theme, scheme in
                    Surface(
                        name: "decision-ready-4b-\(name)-\(theme)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            detail.screenshotCard(item, at: dynamicTypeSize)))
                }
            }
        }

        private func cardStyleSurfaces() -> [Surface] {
            [ColorScheme.light, .dark].map { scheme in
                Surface(
                    name: "card-style-\(scheme)", width: 390, colorScheme: scheme,
                    view: AnyView(
                        VStack(alignment: .leading, spacing: 12) {
                            CardEyebrow(
                                keyword: "Ready for Final Review",
                                chip: StateChip(label: "High", cut: .attention))
                            CardEyebrow(
                                keyword: "Agent Question", carriesInfo: true,
                                rendersInteractiveControls: false)
                            QuoteBlock(
                                producer: "Agent summary", carriesInfo: true,
                                rendersInteractiveControls: false
                            ) {
                                Text("Both review rounds converged; one concern remains open.")
                                    .font(FreesideFont.statement)
                            }
                            QuoteBlock {
                                Text("The retry reuses the approved specification.")
                                    .font(FreesideFont.cardBody)
                            }
                            SystemCallout {
                                Text("Accepting publishes the pull request.")
                                    .font(FreesideFont.cardBody)
                            }
                            Notice(
                                tone: .neutral, keyword: "Stale",
                                sentence: "The base advanced after this card was raised.")
                            Notice(
                                tone: .accent, keyword: "Revised",
                                sentence: "A revised specification is ready.",
                                action: .init(label: "Open", handler: {}))
                            Notice(
                                tone: .wax, keyword: "Failed",
                                sentence: "The daemon could not confirm the submission.")
                            HStack(alignment: .firstTextBaseline, spacing: 8) {
                                Text("Store first, then API").font(FreesideFont.cardBody)
                                CompactMark(text: "Agent recommends")
                            }
                            FactRow(label: "Checks", chip: StateChip(label: "Degraded", cut: .attention))
                            FactRow(label: "Pull request", value: "owner/repo#7")
                        }.padding(24).foregroundStyle(Color.ink).background(Color.ground)))
            }
        }

        /// The choice list (R29) in both registers, by day and by dusk: an
        /// agent's routes as quotes with a consequence line and `PROPOSED`
        /// on the default, the pick on the other one, and the daemon's
        /// routes as bordered items with nothing but their labels.
        private func choiceListSurfaces() -> [Surface] {
            [ColorScheme.light, .dark].map { scheme in
                Surface(
                    name: "choice-list-\(scheme)", width: 390, colorScheme: scheme,
                    view: AnyView(
                        VStack(alignment: .leading, spacing: 22) {
                            VStack(alignment: .leading, spacing: 10) {
                                KeywordLabel(text: "Route")
                                ChoiceList(
                                    accessibilityLabel: "Route for finding 1",
                                    options: [
                                        .init(
                                            value: "decline", label: "Decline the finding",
                                            consequence:
                                                "Nothing changes in the PR; the finding is recorded as declined.",
                                            mark: "Proposed", register: .quote),
                                        .init(
                                            value: "fix", label: "Fix in this PR",
                                            consequence:
                                                "Starts a remediator limited to the run's allowed paths and re-reviews the PR.",
                                            register: .quote),
                                    ],
                                    selection: .constant("fix"))
                            }
                            VStack(alignment: .leading, spacing: 10) {
                                KeywordLabel(text: "Route")
                                ChoiceList(
                                    accessibilityLabel: "What to do with the answer",
                                    options: [
                                        .init(value: "retry", label: "Retry implementation", register: .item),
                                        .init(value: "revise", label: "Revise specification", register: .item),
                                    ],
                                    selection: .constant("retry"))
                            }
                        }.padding(24).foregroundStyle(Color.ink).background(Color.ground2)))
            }
        }

        /// The Pending Stops sheet holding two unconfirmed requests.
        private func pendingStopsSurfaces() throws -> [Surface] {
            let snapshots = Array(TaskFixtures.defaultTasks().prefix(2))
            try #require(snapshots.count == 2)
            let revision = try #require(snapshots.map(\.as_of_revision).max())
            let cache = InMemoryCacheStore()
            try cache.save(
                .init(
                    cursors: .init(
                        syncEpoch: "fixture-epoch", lastFullSnapshotRevision: revision,
                        highestObservedServerRevision: revision), attentionItems: [],
                    tasks: snapshots))
            let coordinator = SyncCoordinator(client: APIClientFactory.mock(), cache: cache)
            coordinator.store.freshness = .fresh
            for (index, snapshot) in snapshots.enumerated() {
                let prepared = try #require(coordinator.taskStop.prepare(taskID: snapshot.task.id))
                // The sheet lists its entries by command id and a prepared
                // Stop takes a fresh UUID, so pin the ids or the two items
                // trade places between runs.
                var command = prepared.entry.command
                command.command_id = "cmd-stop-\(index)"
                #expect(
                    coordinator.retainTaskStop(
                        PendingTaskStop(
                            command: command, taskName: prepared.entry.taskName,
                            projectName: prepared.entry.projectName)))
            }
            return [ColorScheme.light, .dark].map { scheme in
                Surface(
                    name: "task-stop-pending-\(scheme)", width: 390, colorScheme: scheme,
                    view: AnyView(TaskStopRecoveryView(coordinator: coordinator).content))
            }
        }

        private func taskStopSurfaces() throws -> [Surface] {
            var surfaces: [Surface] = []
            let original = try #require(TaskFixtures.defaultTasks().first)
            for state in [
                "queued", "specification", "implementation", "uncertain", "accepted", "failed", "confirmed", "offline",
                "finished", "abandoned",
            ] {
                var snapshot = original
                snapshot.task.display_names.task = .init(
                    text: "Update the release notes and verify the download links", source: ._operator)
                if state == "queued" {
                    snapshot.task.run_ids = []
                    snapshot.task.campaign_ids = []
                    snapshot.task.current_position = nil
                    snapshot.task.wip = false
                }
                if state == "specification" { snapshot.task.current_position?.value1.stage = "specification" }
                if state == "finished" { snapshot.task.lifecycle = .finished }
                if state == "abandoned" { snapshot = TaskFixtures.explicitlyAbandoned(snapshot) }
                if ["accepted", "failed", "confirmed"].contains(state) {
                    snapshot = TaskFixtures.confirmedStopped(snapshot)
                    if state != "confirmed" {
                        snapshot.task.lifecycle = original.task.lifecycle
                        snapshot.task.wip = original.task.wip
                        snapshot.task.cancellation?.value1.state = state == "failed" ? .failed_to_stop : .requested
                        if state == "failed" {
                            snapshot.task.cancellation?.value1.acknowledgement?.value1.state = .failed_to_stop
                        } else {
                            snapshot.task.cancellation?.value1.acknowledgement = nil
                        }
                    }
                }
                let cache = InMemoryCacheStore()
                try cache.save(
                    .init(
                        cursors: .init(
                            syncEpoch: "fixture-epoch", lastFullSnapshotRevision: snapshot.as_of_revision,
                            highestObservedServerRevision: snapshot.as_of_revision), attentionItems: [],
                        tasks: [snapshot]))
                let coordinator = SyncCoordinator(client: APIClientFactory.mock(), cache: cache)
                coordinator.store.freshness = state == "offline" ? .unreachable : .fresh
                if state == "uncertain", let prepared = coordinator.taskStop.prepare(taskID: snapshot.task.id) {
                    #expect(coordinator.retainTaskStop(prepared.entry))
                }
                for scheme in [ColorScheme.light, .dark] {
                    surfaces.append(
                        Surface(
                            name: "task-stop-\(state)-\(scheme)", width: 390, colorScheme: scheme,
                            view: AnyView(
                                VStack(alignment: .leading, spacing: 16) {
                                    Text(snapshot.task.display_names.task.text).font(FreesideFont.title)
                                    Text(TaskDisplay.projectName(snapshot.task)).font(FreesideFont.callout)
                                    TaskStopView(coordinator: coordinator, taskID: snapshot.task.id)
                                }.padding(24).foregroundStyle(Color.ink).background(Color.ground))))
                    if state == "queued", let confirmation = coordinator.taskStop.prepare(taskID: snapshot.task.id) {
                        surfaces.append(
                            Surface(
                                name: "task-stop-confirmation-\(scheme)", width: 390, colorScheme: scheme,
                                view: AnyView(
                                    TaskStopConfirmationView(entry: confirmation.entry, onConfirm: {}).content)))
                    }
                }
            }
            return surfaces
        }

        private func attachmentStates() async -> AnyView {
            let digest = "sha256:attachment-state-fixture"
            let imageLoader = AttachmentLoader(
                client: APIClientFactory.mock(
                    server: MockServer(
                        attachments: [digest: AttentionFixtures.fixtureImagePNG])))
            let notImageLoader = AttachmentLoader(
                client: APIClientFactory.mock(
                    server: MockServer(
                        attachments: [digest: Data("fixture verification log\n".utf8)])))
            let unavailableLoader = AttachmentLoader(
                client: APIClientFactory.mock(server: MockServer(attachments: [:])))
            let fetchFailedLoader = AttachmentLoader(
                client: APIClientFactory.mock(server: MockServer(attachments: [:])))
            let tooLargeLoader = AttachmentLoader(
                client: APIClientFactory.mock(
                    server: MockServer(
                        attachments: [digest: Data(repeating: 0x41, count: 64)])),
                maxBytes: 16)
            let loadingLoader = AttachmentLoader(
                client: APIClientFactory.mock(
                    server: MockServer(
                        attachments: [digest: AttentionFixtures.fixtureImagePNG])))

            await imageLoader.load(digest)
            await notImageLoader.load(digest)
            // A bytes_absent reference is the daemon-reported no-bytes state
            // (decided with no fetch); the metadata-free failed fetch below is
            // the distinct transient fetch failure.
            await unavailableLoader.load(
                digest,
                reference: AttachmentReference(
                    mediaType: "image/png", sizeBytes: 256, availability: .bytesAbsent))
            await fetchFailedLoader.load(digest)
            await tooLargeLoader.load(digest)

            return AnyView(
                VStack(alignment: .leading, spacing: 18) {
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: loadingLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: imageLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: notImageLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: unavailableLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: fetchFailedLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                    DecisionDetailView.AttachmentRow(
                        label: "verify_log", digest: digest, attachments: tooLargeLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                }
                .padding(14)
                .freesideCard()
                .padding())
        }

        /// The refreshed history with review facts: round 1 reviewed the
        /// implementation and sent its findings to the fixture's remediator,
        /// and round 2 re-reviews the remediator's head.
        private func remediationHistoryTimeline() throws -> Components.Schemas.RunTimeline {
            var timeline = RunFixtures.refreshedHistoryTimeline()
            let original = try #require(timeline.review?.value1.rounds.first)
            let remediator = try #require(
                timeline.milestones.first { $0.invocation_id?.contains("remediation") == true })
            let remediatorID = try #require(remediator.invocation_id)
            var findings = original
            findings.outcome = .init(value1: .findings)
            findings.findings_count = 2
            findings.requested_at = remediator.recorded_at.addingTimeInterval(-600)
            findings.completed_at = remediator.recorded_at.addingTimeInterval(-300)
            // Round 1 reviewed the first stage's second attempt.
            findings.subject = .init(
                value1: .init(kind: .implementation, invocation_id: "inv-\(timeline.run_id)-2"))
            findings.remediation = .init(
                value1: .init(
                    invocation_id: remediatorID, finding_ids: ["finding-1", "finding-2"],
                    decided_at: remediator.recorded_at.addingTimeInterval(-60)))
            var reReview = original
            reReview.round = 2
            reReview.invocation_id = original.invocation_id + "-2"
            reReview.head_sha = String(repeating: "c", count: 40)
            reReview.subject = .init(
                value1: .init(kind: .remediation, invocation_id: remediatorID, remediates_round: 1))
            timeline.review = .init(value1: .init(rounds: [findings, reReview]))
            return timeline
        }

        /// A stall notice's card after an accepted Acknowledge, and after a
        /// second one (#1978). The item is shaped like the daemon's
        /// `invocation_stalled` notice, and each model reaches its state by
        /// submitting to the mock server, so the receipt drawn is the one an
        /// accepted command leaves.
        private func makeRecordedAcknowledgeSurfaces(
            at dynamicTypeSize: DynamicTypeSize
        ) async throws -> [Surface] {
            var stall = AttentionFixtures.fixture(type: .system_health)
            stall.item.requested_decision = [.acknowledge]
            stall.item.posture = .init(value1: .advisory)
            stall.item.health_diagnostic = .init(
                value1: .init(code: "invocation_stalled", impairs: .none))
            stall.item.codex_reenrollment_recovery_binding = nil
            stall.item.reason =
                "Invocation inv-7 has had no provider response for 5m0s. The writer is still running, "
                + "possibly a long tool command, and its hard budget is unchanged. This notice resolves "
                + "itself when provider responses resume or the writer stops."
            let server = MockServer(items: [stall])
            let store = InboxStore(client: APIClientFactory.mock(server: server), now: { screenshotNow })
            store.replaceAll(with: [stall])
            await preloadReferences(stall.item, into: store)

            var surfaces: [Surface] = []
            for (name, presses) in [("decision-stall-acknowledged", 1), ("decision-stall-acknowledged-twice", 2)] {
                let model = DecisionModel(store: store, itemID: stall.item.id)
                await model.validate()
                for _ in 0..<presses {
                    await model.submit(.acknowledge)
                }
                #expect(model.appliedRecordRepeat == presses)
                #expect(model.phase == .idle)
                let item = try #require(model.snapshot?.item)
                #expect(item.status == .open && item.requested_decision == [.acknowledge])
                let detail = DecisionDetailView(
                    store: store,
                    itemID: item.id,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow,
                    model: model)
                surfaces.append(
                    Surface(
                        name: name,
                        view: AnyView(detail.screenshotCard(item, at: dynamicTypeSize))))
                surfaces.append(
                    Surface(
                        name: "\(name)-phone",
                        width: 390,
                        view: AnyView(
                            detail.screenshotCard(item, at: dynamicTypeSize, compactLayout: true))))
            }
            return surfaces
        }

        private func makeUnavailableAttachmentSurface(
            at dynamicTypeSize: DynamicTypeSize
        ) async throws -> Surface {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let health = AttentionFixtures.fixture(type: .system_health)
            let store = InboxStore(client: client)
            store.replaceAll(with: [health])
            // With the screenshot claim's bytes not held, the daemon projects
            // bytes_absent; the card shows the no-bytes state, decided from
            // metadata with no fetch.
            let unheld = "sha256:img-system_health"
            await preloadReferences(health.item, into: store, withoutBytesFor: [unheld])
            #expect(store.attachments.phase(for: unheld) == .unavailable)
            let detail = DecisionDetailView(
                store: store,
                itemID: health.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            return Surface(
                name: "decision-system_health-unavailable",
                view: AnyView(detail.screenshotCard(health.item, at: dynamicTypeSize)))
        }

        private func makeImageAttachmentSurface(
            at dynamicTypeSize: DynamicTypeSize
        ) async throws -> Surface {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let approval = AttentionFixtures.fixture(type: .spec_approval)
            let store = InboxStore(client: client)
            store.replaceAll(with: [approval])
            await store.attachments.load(
                "sha256:img-spec_approval",
                reference: AttachmentReference(
                    mediaType: "image/png",
                    sizeBytes: AttentionFixtures.fixtureImagePNG.count,
                    availability: .available))
            guard case .image = store.attachments.phase(for: "sha256:img-spec_approval") else {
                throw ScreenshotError.missingSeededImage
            }
            let detail = DecisionDetailView(
                store: store,
                itemID: approval.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            return Surface(
                name: "decision-spec_approval-image",
                view: AnyView(detail.screenshotCard(approval.item, at: dynamicTypeSize)))
        }

        private func graphicPresentations(
            for item: Components.Schemas.AttentionItem
        ) -> DecisionGraphicPresentations {
            switch item._type {
            case .ready_for_final_review:
                return .init(
                    changeSummary: .init(
                        text: "Adds shared card compositions and accessible graphic summaries."))
            case .execution_failure:
                return .init(
                    stageRail: DecisionStageRailPresentation.failure(
                        stages: ["Import", "Build", "Verify", "Publish"],
                        failedStageIndex: 1),
                    attemptTimings: .init(
                        title: "Attempt timings",
                        facts: [
                            .init(label: "Attempt 1", value: "1m 42s"),
                            .init(label: "Attempt 2", value: "1m 38s"),
                        ]),
                    prominentClaimIndex: item.agent_claims.firstIndex {
                        $0.label == "Likely cause (unverified)"
                    })
            case .review_dispute:
                return .init(
                    comparison: .init(
                        positions: [
                            .init(
                                title: "Reviewer",
                                text: "The reconstruction boundary needs a second guard."),
                            .init(
                                title: "Agent",
                                text: "The existing trusted gate makes that state unreachable."),
                        ],
                        verifiableFacts: [
                            .init(label: "Caller", value: "Store reconstruction"),
                            .init(label: "Current gate", value: "Approved recipe set"),
                        ]))
            case .review_diminishing_returns:
                return .init(
                    diminishingYield: .init(
                        rounds: [
                            .init(number: 1, newFindings: 4, recurringFindings: 0),
                            .init(number: 2, newFindings: 1, recurringFindings: 2),
                            .init(number: 3, newFindings: 0, recurringFindings: 3),
                        ]))
            case .spec_approval, .review_contradiction, .review_configuration,
                .finding_adjudication, .agent_question, .publish_blocked,
                .task_proposal, .effect_proposal, .system_health, .blocked:
                return .init()
            }
        }

        private func render(
            _ view: AnyView,
            at size: DynamicTypeSize,
            width: CGFloat,
            colorScheme: ColorScheme,
            contrast: LaunchInputs.Contrast? = nil,
            nativeAppearance: Bool = false,
            key: String = "supplemental"
        ) async throws -> CGImage {
            guard let timeZone = TimeZone(secondsFromGMT: 0) else {
                throw ScreenshotError.missingGMT
            }
            let root =
                view
                .environment(\.dynamicTypeSize, size)
                .environment(\.colorScheme, colorScheme)
                .environment(\.locale, Locale(identifier: "en_US_POSIX"))
                .environment(\.calendar, Calendar(identifier: .gregorian))
                .environment(\.timeZone, timeZone)
                .environment(\.pinnedNow, screenshotNow)
                // A surface at phone width is the phone's: the timelines
                // draw on the page's ground there and in the detail
                // column's card everywhere else.
                .transformEnvironment(\.horizontalSizeClass) { if width <= 430 { $0 = .compact } }
                .frame(width: width, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)
                .background(Color.ground)
                .transformEnvironment(\.colorScheme) { if nativeAppearance { $0 = colorScheme } }
            return try await ScreenshotCapture.settled(key: key) {
                let tracing = ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_TRACE"] == "1"
                if tracing {
                    print(
                        "SCREENSHOT_INPUT \(key) explicit=\(contrast?.rawValue ?? "none") fallback=\(LaunchInputs.accessibilityContrastOverride()?.rawValue ?? "host") hostIncreased=\(NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast)"
                    )
                }
                let renderer = ImageRenderer(content: root)
                renderer.proposedSize = ProposedViewSize(width: width, height: nil)
                renderer.scale = 1
                let started = UInt64(Date().timeIntervalSince1970 * 1_000_000_000)
                defer {
                    if tracing {
                        let ended = UInt64(Date().timeIntervalSince1970 * 1_000_000_000)
                        print("SCREENSHOT_CAPTURE \(key) \(started) \(ended)")
                    }
                }
                return try LaunchInputs.$screenshotIncreasedContrast.withValue(contrast.map { $0 == .increased }) {
                    if nativeAppearance {
                        var result: Result<CGImage, Error>?
                        NSAppearance(named: colorScheme == .dark ? .darkAqua : .aqua)?
                            .performAsCurrentDrawingAppearance {
                                result = Result { try ScreenshotCapture.bitmap(renderer) }
                            }
                        guard let result else { throw ScreenshotError.renderFailed }
                        return try result.get()
                    }
                    return try ScreenshotCapture.bitmap(renderer)
                }
            }
        }

        private func digest(_ image: CGImage) throws -> String {
            try ScreenshotCapture.digest(image)
        }

        /// Optional hosted-runner overrides layer on the universal baseline.
        /// The current macOS 26.6 runner passes without one; a local run
        /// always uses the universal baseline.
        private func loadManifest(
            overrides: [String: [String: String]]
        ) throws -> [String: String] {
            let url = try digestFileURL(named: "ScreenshotDigests")
            let baseline = try JSONDecoder().decode(
                [String: String].self, from: Data(contentsOf: url))
            let applicable = isHostedRunner ? overrides[operatingSystemKey] ?? [:] : [:]
            return baseline.merging(applicable) { _, override in
                override
            }
        }

        private var isHostedRunner: Bool {
            ProcessInfo.processInfo.environment["GITHUB_ACTIONS"] == "true"
        }

        /// Recording stays refused on hosted runners even when no override
        /// applies, so runner pixels cannot replace the universal baseline.
        private func validateRecordingHost(hosted: Bool) throws {
            guard !hosted else { throw ScreenshotError.recordingRefusedOnHostedRunner }
        }

        private func loadOverrides() throws -> [String: [String: String]] {
            let overridesURL = try digestFileURL(named: "ScreenshotDigestOverrides")
            return try JSONDecoder().decode(
                [String: [String: String]].self,
                from: Data(contentsOf: overridesURL))
        }

        /// Both digest files always ship, even with no overrides to apply,
        /// so a failed lookup means the bundle is wrong. Reading it as "no
        /// overrides" would compare a hosted runner to the wrong digests.
        private func digestFileURL(named name: String) throws -> URL {
            guard let url = Bundle.module.url(forResource: name, withExtension: "json") else {
                throw ScreenshotError.missingDigestFile(name)
            }
            return url
        }

        private func validateRecordingOperatingSystem(
            _ key: String,
            baselineKey: String
        ) throws {
            guard key == baselineKey else {
                throw ScreenshotError.recordingRequiresBaselineOperatingSystem(
                    expected: baselineKey, actual: key)
            }
        }

        private var operatingSystemKey: String {
            let version = ProcessInfo.processInfo.operatingSystemVersion
            return "macOS-\(version.majorVersion).\(version.minorVersion)"
        }

        private func dump(_ image: CGImage, named name: String) throws -> URL {
            // Separate evidence from concurrent worktrees without changing
            // which pixels are compared or accepted.
            let directory =
                ProcessInfo.processInfo.environment["FREESIDE_SCREENSHOT_OUTPUT"].map {
                    URL(fileURLWithPath: $0, isDirectory: true)
                }
                ?? FileManager.default.temporaryDirectory
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
        case diagnosticRecordingRefused
        case invalidDiagnosticSelection
        case invalidShardSelection(String)
        case missingActiveRun
        case missingActiveTimeline
        case missingDigestFile(String)
        case missingRetryTask
        case missingGMT
        case missingSeededImage
        case pngEncodingFailed
        case preferencesUnavailable
        case recordingRefusedOnHostedRunner
        case recordingRequiresBaselineOperatingSystem(expected: String, actual: String)
        case renderFailed
        case shardedRecordingRefused
    }
#endif
