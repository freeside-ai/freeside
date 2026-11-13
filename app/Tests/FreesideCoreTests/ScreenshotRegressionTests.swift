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
        private static let detailColumnWidth = DecisionCardComposition.Scale.columnCap + 48
        private let baselineOperatingSystemKey = "macOS-27.2"
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
            "unified-list-dark",
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

        /// Every light surface, inserted while an assistive client reads
        /// (#2993; `AccessibilityLabelRecursionTests` explains the crash).
        /// A dark surface has the same accessibility tree as its light
        /// twin, so the pass skips it.
        @Test func insertingASurfaceUnderAnAssistiveClientDoesNotRecurse() async throws {
            guard let timeZone = TimeZone(secondsFromGMT: 0) else { throw ScreenshotError.missingGMT }
            var recursing: [String: [String]] = [:]
            for surface in try await makeSurfaces(at: .large) where surface.colorScheme == .light {
                let width = surface.width ?? canvasWidth
                let view = surface.view
                    .environment(\.locale, Locale(identifier: "en_US_POSIX"))
                    .environment(\.calendar, Calendar(identifier: .gregorian))
                    .environment(\.timeZone, timeZone)
                    .environment(\.pinnedNow, screenshotNow)
                    .transformEnvironment(\.horizontalSizeClass) { if width <= 430 { $0 = .compact } }
                let elements = try AccessibilityInsertionProbe.recursingElements(in: view, width: width)
                if !elements.isEmpty { recursing[surface.name] = elements }
            }
            #expect(recursing.isEmpty, "label lookups recursed on: \(recursing)")
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
                        let view = TaskPageView(
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
                "macOS-27.2", baselineKey: "macOS-27.2")
            #expect(throws: ScreenshotError.self) {
                try validateRecordingOperatingSystem(
                    "macOS-26.7", baselineKey: "macOS-27.2")
            }
        }

        @Test func matrixSizesCoverEveryLayoutBreakpointButRepeatNone() {
            #expect(Self.matrixSizeNames(for: "unified-list", colorScheme: .light) == Self.lightMatrixSizes)
            #expect(Self.lightMatrixSizes == ["large", "xxxlarge", "ax1", "ax5"])
            #expect(Self.matrixSizeNames(for: "unified-list-phone-dark", colorScheme: .dark) == ["large"])
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

        private func screenshotNavigation(selecting itemID: String?) -> NavigationModel {
            let navigation = NavigationModel(
                launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
            if let itemID { navigation.route(to: .attentionItem(itemID)) }
            return navigation
        }

        /// A memory-only draft holding what a composer golden draws.
        private func screenshotDraft(
            source: String, choices: [NewTaskSheet.Role: AgentChoiceDisplay.Selection] = [:],
            agentsOpen: Bool = false, attachments: TaskAttachmentDraft = TaskAttachmentDraft()
        ) -> NewTaskDraftStore {
            let store = NewTaskDraftStore(deploymentID: "mock", deviceID: "screenshot")
            store.draft.source = source
            store.draft.choices = choices
            store.draft.agentsOpen = agentsOpen
            store.draft.attachments = attachments
            return store
        }

        /// A draft loaded from a file that recorded attachments, as a
        /// relaunch restores one.
        private func restoredScreenshotDraft(
            source: String, choices: [NewTaskSheet.Role: AgentChoiceDisplay.Selection]
        ) throws -> NewTaskDraftStore {
            let directory = FileManager.default.temporaryDirectory
                .appendingPathComponent("FreesideScreenshotDraft-\(UUID().uuidString)")
            let writer = NewTaskDraftStore(deploymentID: "mock", deviceID: "screenshot", directory: directory)
            writer.draft.source = source
            writer.draft.choices = choices
            _ = writer.draft.attachments.add(
                name: "Layout bug.png", claimedMediaType: "image/png", bytes: AttentionFixtures.fixtureImagePNG)
            let restored = NewTaskDraftStore(deploymentID: "mock", deviceID: "screenshot", directory: directory)
            try #require(restored.attachmentsDroppedAtRelaunch)
            return restored
        }

        private func screenshotListPreferences(expanded: Bool = false) throws -> ListSectionPreferences {
            let defaults = try #require(UserDefaults(suiteName: "FreesideScreenshotList-\(UUID().uuidString)"))
            let preferences = ListSectionPreferences(defaults: defaults)
            preferences.workingExpanded = expanded
            preferences.finishedExpanded = expanded
            return preferences
        }

        /// The list with its sections held open or shut, one task standing in
        /// Needs you among the scoped items. The canvas frames these match
        /// are named in the unit's decision note.
        private func unifiedListSurfaces(
            inbox: [Components.Schemas.AttentionItemSnapshot],
            tasks: [Components.Schemas.TaskSnapshot],
            runs: [Components.Schemas.RunSnapshot]
        ) throws -> [Surface] {
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(
                with: inbox.map { snapshot in
                    var snapshot = snapshot
                    if snapshot.item.id == "item-spec_approval" {
                        snapshot.item.subject = .task(
                            .init(
                                subject_type: .task, subject_id: TaskFixtures.retryTaskID,
                                task_id: TaskFixtures.retryTaskID))
                    }
                    return snapshot
                })
            func list(
                _ store: InboxStore, expanded: Bool = false, draftStore: NewTaskDraftStore? = nil
            ) throws -> AnyView {
                try AnyView(
                    UnifiedListView(
                        store: store, tasks: tasks, runs: runs,
                        navigation: screenshotNavigation(selecting: "item-spec_approval"),
                        preferences: screenshotListPreferences(expanded: expanded),
                        draftStore: draftStore
                    ).screenshotContent(now: screenshotNow))
            }
            // An unsent New Task draft (#2935) above Needs You.
            let draft = screenshotDraft(
                source: "Add a /health endpoint returning the daemon version and uptime, with a test.\nKeep it cheap.")
            draft.draft.projectID = "freeside"
            var surfaces: [Surface] = []
            for colorScheme in [ColorScheme.light, .dark] {
                let suffix = colorScheme == .dark ? "-dark" : ""
                surfaces += try [
                    Surface(
                        name: "unified-list" + suffix, width: 320, colorScheme: colorScheme, view: list(store)),
                    Surface(
                        name: "unified-list-working-expanded" + suffix, width: 320, colorScheme: colorScheme,
                        view: list(store, expanded: true)),
                    Surface(
                        name: "unified-list-phone" + suffix, width: 390, colorScheme: colorScheme,
                        view: list(store, expanded: true)),
                    Surface(
                        name: "unified-list-draft" + suffix, width: 320, colorScheme: colorScheme,
                        view: list(store, draftStore: draft)),
                    Surface(
                        name: "unified-list-phone-draft" + suffix, width: 390, colorScheme: colorScheme,
                        view: list(store, draftStore: draft)),
                    Surface(
                        name: "project-filter-popover" + suffix, width: 288, colorScheme: colorScheme,
                        view: AnyView(
                            ProjectFilterPopover(
                                projects: ["freeside", "freeside-ai/oriole"], offersNoProject: true,
                                selection: "freeside", onSelect: { _ in },
                                rendersInteractiveControls: false
                            ).padding(10))),
                ]
            }
            let filtered = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            filtered.replaceAll(with: store.orderedSnapshots)
            filtered.selectProjectFilter("freeside")
            try surfaces.append(
                Surface(name: "unified-list-project-filter", width: 320, view: list(filtered, expanded: true)))
            // Finished opened on a short page in the Mac sidebar, its Show
            // More line under the rows.
            let clear = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            clear.replaceAll(with: [])
            let finishedOnly = try screenshotListPreferences()
            finishedOnly.finishedExpanded = true
            surfaces.append(
                Surface(
                    name: "unified-list-finished-page", width: 320,
                    view: AnyView(
                        UnifiedListView(
                            store: clear, tasks: tasks, runs: runs,
                            navigation: screenshotNavigation(selecting: nil), preferences: finishedOnly
                        ).screenshotContent(now: screenshotNow, sidebarHeight: 640, finishedLimit: 2))))
            return surfaces
        }

        private func makeSurfaces(at dynamicTypeSize: DynamicTypeSize) async throws -> [Surface] {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let inbox = AttentionFixtures.defaultInbox()
            let store = InboxStore(client: client)
            store.replaceAll(with: inbox)
            store.replaceAllConversations(with: AttentionFixtures.defaultConversations())
            for snapshot in inbox {
                await preloadReferences(snapshot.item, into: store)
            }

            var surfaces: [Surface] = []
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

            // A spec approval's Discuss composer with attachments (#2995):
            // an image chip with its thumbnail and a file chip after the
            // field, then the Attach chip, on Mac and phone, by day and by
            // dusk.
            var discussAttachments = TaskAttachmentDraft(for: .message)
            _ = discussAttachments.add(
                name: "Layout bug.png", claimedMediaType: "image/png", bytes: AttentionFixtures.fixtureImagePNG)
            _ = discussAttachments.add(
                name: "crash.log", claimedMediaType: "text/plain", bytes: Data(repeating: 0x61, count: 48_213))
            for (name, width, scheme) in [
                ("discuss-composer-attachments", CGFloat(480), ColorScheme.light),
                ("discuss-composer-attachments-phone", CGFloat(390), ColorScheme.light),
                ("discuss-composer-attachments-dark", CGFloat(480), ColorScheme.dark),
                ("discuss-composer-attachments-phone-dark", CGFloat(390), ColorScheme.dark),
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
                                thread: .init(
                                    snapshot: discussThread, attachments: store.attachments,
                                    loadsAttachments: false, now: screenshotNow),
                                attachments: .constant(discussAttachments),
                                submit: { _, _ in true }))))
            }

            // The New Task composer (#1330, #2581): the text area over the
            // project chip and the Agents chip (#1602), on Mac and phone,
            // empty and filled, each by day and by dusk.
            // ImageRenderer draws the static stand-ins, not the live
            // TextEditor, Menu, or the iPhone's navigation bar; the toolbar
            // action and open sheet are in the PR's real-app screenshots.
            let newTaskProjects = TaskDisplay.knownProjects(in: TaskFixtures.defaultTasks())
            // The composer only opens on a settled sync, so the goldens depict
            // that state: a fresh coordinator keeps the filled surface's Submit
            // enabled (the action is gated on `.fresh`).
            // A synced coordinator carries the mock's agent catalog and
            // routing policy, which the agent choices list and label. Its
            // own server keeps the sync round off the one the other
            // surfaces share.
            let newTaskCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            await newTaskCoordinator.refresh()
            newTaskCoordinator.store.freshness = .fresh
            let newTaskModel = TaskSubmissionModel(coordinator: newTaskCoordinator)
            let noTreeCoordinator = SyncCoordinator(
                client: APIClientFactory.mock(
                    server: MockServer(routingPolicy: nil, agentCatalog: UsageFixtures.treelessAgentCatalog())),
                cache: InMemoryCacheStore())
            await noTreeCoordinator.refresh()
            noTreeCoordinator.store.freshness = .fresh
            let noTreeModel = TaskSubmissionModel(coordinator: noTreeCoordinator)
            let filledSource =
                "Add a /health endpoint returning the daemon version and uptime, with a test."
            for (name, width, scheme, draftSource) in [
                ("new-task-sheet", CGFloat(380), ColorScheme.light, ""),
                ("new-task-sheet-filled-dark", CGFloat(380), ColorScheme.dark, filledSource),
                ("new-task-sheet-phone", CGFloat(390), ColorScheme.light, ""),
                ("new-task-sheet-phone-filled-dark", CGFloat(390), ColorScheme.dark, filledSource),
                ("new-task-sheet-dark", CGFloat(380), ColorScheme.dark, ""),
                ("new-task-sheet-filled", CGFloat(380), ColorScheme.light, filledSource),
                ("new-task-sheet-phone-dark", CGFloat(390), ColorScheme.dark, ""),
                ("new-task-sheet-phone-filled", CGFloat(390), ColorScheme.light, filledSource),
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
                                draftStore: screenshotDraft(source: draftSource),
                                onSubmitted: { _ in },
                                rendersInteractiveControls: false))))
            }
            // The agent choices: one writer and a reviewer chosen, so the
            // open composer draws the four role chips; a daemon with no
            // agent tree, so the Agents chip is disabled; an implementer
            // whose model and a reviewer whose effort the catalog dropped,
            // which block Submit; and, closed, a summary that names one
            // choice by its enrollment alone and another in full, at the
            // live sheet's 560pt and at the phone's width.
            let writerAndReviewer: [NewTaskSheet.Role: AgentChoiceDisplay.Selection] = [
                .specifier: UsageFixtures.agentSelection("claude-secondary"),
                .implementer: UsageFixtures.agentSelection("claude-secondary"),
                .remediator: UsageFixtures.agentSelection("claude-secondary"),
                .reviewer: UsageFixtures.agentSelection("claude-primary"),
            ]
            let staleImplementer: [NewTaskSheet.Role: AgentChoiceDisplay.Selection] = [
                .implementer: UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-retired", "high"),
                .reviewer: UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "max"),
            ]
            let mixed: [NewTaskSheet.Role: AgentChoiceDisplay.Selection] = [
                .remediator: UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "max"),
                .reviewer: UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-mini", "harness_default"),
            ]
            for (name, width, scheme, model, choices, open) in [
                ("new-task-sheet-agents", CGFloat(380), ColorScheme.light, newTaskModel, writerAndReviewer, true),
                ("new-task-sheet-agents-dark", CGFloat(380), ColorScheme.dark, newTaskModel, writerAndReviewer, true),
                ("new-task-sheet-phone-agents", CGFloat(390), ColorScheme.light, newTaskModel, writerAndReviewer, true),
                (
                    "new-task-sheet-phone-agents-dark", CGFloat(390), ColorScheme.dark, newTaskModel, writerAndReviewer,
                    true
                ),
                ("new-task-sheet-no-catalog", CGFloat(380), ColorScheme.light, noTreeModel, [:], false),
                ("new-task-sheet-no-catalog-dark", CGFloat(380), ColorScheme.dark, noTreeModel, [:], false),
                ("new-task-sheet-phone-no-catalog", CGFloat(390), ColorScheme.light, noTreeModel, [:], false),
                ("new-task-sheet-phone-no-catalog-dark", CGFloat(390), ColorScheme.dark, noTreeModel, [:], false),
                ("new-task-sheet-agent-missing", CGFloat(380), ColorScheme.light, newTaskModel, staleImplementer, true),
                ("new-task-sheet-agents-mixed", CGFloat(560), ColorScheme.light, newTaskModel, mixed, false),
                ("new-task-sheet-agents-mixed-dark", CGFloat(560), ColorScheme.dark, newTaskModel, mixed, false),
                ("new-task-sheet-phone-agents-mixed", CGFloat(390), ColorScheme.light, newTaskModel, mixed, false),
            ]
                as [(
                    String, CGFloat, ColorScheme, TaskSubmissionModel,
                    [NewTaskSheet.Role: AgentChoiceDisplay.Selection], Bool
                )]
            {
                surfaces.append(
                    Surface(
                        name: name, width: width, colorScheme: scheme,
                        view: AnyView(
                            NewTaskSheet(
                                projects: newTaskProjects, model: model,
                                draftStore: screenshotDraft(source: filledSource, choices: choices, agentsOpen: open),
                                onSubmitted: { _ in }, rendersInteractiveControls: false))))
            }
            surfaces += rolePickerSurfaces()
            // A long draft grows the text area to its cap; the foot stays
            // under it, and the closed Agents chip reads the choices.
            let grownSource =
                "Make the acceptance rig recover a stale lease an older-schema build left, and cover it with the "
                + "convergence suite. Keep the lease's schema version in the record so a future build can tell its "
                + "own leases from the older ones, and make the recovery idempotent: a second pass over an "
                + "already-recovered lease is a no-op with a log line. Add the two convergence cases from #2451 and "
                + "the one from the 30 Oct exit run.\n\nThe rig should also say which build left the lease when it "
                + "recovers one, so the exit run's report can name it, and the recovery should never touch a lease "
                + "a live build still holds: check the holder's heartbeat first and leave a fresh one alone. When "
                + "the heartbeat is stale but the build is still running, log both and recover nothing. Note the "
                + "recovery in the task's timeline so the operator can see it happened."
            for (name, scheme) in [("new-task-sheet-grown", ColorScheme.light), ("new-task-sheet-grown-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: name, width: 380, colorScheme: scheme,
                        view: AnyView(
                            NewTaskSheet(
                                projects: newTaskProjects, model: newTaskModel,
                                draftStore: screenshotDraft(source: grownSource, choices: writerAndReviewer),
                                onSubmitted: { _ in }, rendersInteractiveControls: false))))
            }

            // Attachments in the composer (#2590): an image chip with its
            // thumbnail, a numbered repeat, a file chip, and a folder chip
            // (#2591); one surface also shows the refusal notice for an image
            // over its limit, another the sentence for a folder's left-out
            // links.
            var composerAttachments = TaskAttachmentDraft()
            for (name, claim, bytes) in [
                ("Layout bug.png", "image/png", AttentionFixtures.fixtureImagePNG),
                ("Layout bug.png", "image/png", AttentionFixtures.fixtureImagePNG + Data([0])),
                ("crash.log", "text/plain", Data(repeating: 0x61, count: 48_213)),
            ] {
                _ = composerAttachments.add(name: name, claimedMediaType: claim, bytes: bytes)
            }
            _ = composerAttachments.addFolder(
                name: "design handoff",
                files: (0..<14).map { index in
                    FolderFile(
                        path: "screens/screen-\(index).png", claimedMediaType: "image/png",
                        bytes: AttentionFixtures.fixtureImagePNG + Data(repeating: UInt8(index), count: 180_000))
                })
            let imageRefusal = "Screen recording.png is 12.4 MiB; an image can be at most 10 MiB."
            let linksSentence = "design handoff: 2 symbolic links weren't attached, such as assets/logo.svg."
            for (name, width, scheme, refusals) in [
                ("new-task-sheet-attachments", CGFloat(380), ColorScheme.light, [imageRefusal]),
                ("new-task-sheet-attachments-dark", CGFloat(380), ColorScheme.dark, [linksSentence]),
                ("new-task-sheet-phone-attachments", CGFloat(390), ColorScheme.light, []),
                ("new-task-sheet-phone-attachments-dark", CGFloat(390), ColorScheme.dark, [imageRefusal]),
            ] as [(String, CGFloat, ColorScheme, [String])] {
                surfaces.append(
                    Surface(
                        name: name, width: width, colorScheme: scheme,
                        view: AnyView(
                            NewTaskSheet(
                                projects: newTaskProjects, model: newTaskModel,
                                draftStore: screenshotDraft(source: filledSource, attachments: composerAttachments),
                                onSubmitted: { _ in }, rendersInteractiveControls: false,
                                initialRefusals: refusals))))
            }

            // A draft restored after a relaunch (#2935): its text and agent
            // choices came back, its attachments didn't, and the composer
            // says so under the box.
            for (name, width, scheme) in [
                ("new-task-sheet-restored", CGFloat(380), ColorScheme.light),
                ("new-task-sheet-restored-dark", CGFloat(380), ColorScheme.dark),
                ("new-task-sheet-phone-restored", CGFloat(390), ColorScheme.light),
                ("new-task-sheet-phone-restored-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: name, width: width, colorScheme: scheme,
                        view: AnyView(
                            NewTaskSheet(
                                projects: newTaskProjects, model: newTaskModel,
                                draftStore: try restoredScreenshotDraft(
                                    source: filledSource, choices: writerAndReviewer),
                                onSubmitted: { _ in }, rendersInteractiveControls: false))))
            }

            let recoveryModel = try await TaskSubmissionRecoveryScreenshots.model(client: client)
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

            // Settings (#2580), as canvas band 1 of the 4 Nov 2026 handoff
            // draws it: the Mac window at 760 with each pane selected, and
            // the iPhone's list and pushed panes at 390, by day and by dusk.
            // Devices shows this device with notifications off, another
            // active one with them on, and a revoked one never seen (#981).
            // The Mac's Notifications pane shows this device off; the
            // phone's has it on, so it shows how to set notifications up
            // (#1924), here on a server that is not ntfy's own, after a
            // notification was opened.
            let devicesModel = await DevicesScreenshots.model()
            let phoneDevicesModel = await DevicesScreenshots.model(
                notificationSubscription: .mock, openedAt: screenshotNow.addingTimeInterval(-3600))
            let phoneNotificationsModel = await DevicesScreenshots.model(
                notificationSubscription: .mock, openedAt: screenshotNow.addingTimeInterval(-3600),
                notificationsOn: true)
            let routing = await RoutingPolicyScreenshots.model(.policy)
            let workflow = await WorkflowPolicyScreenshots.model(.layers)
            let usage = await AccountUsageScreenshots.model(.usage)
            let settingsDefaults = try #require(
                UserDefaults(suiteName: "FreesideScreenshotSettings-\(UUID().uuidString)"))
            let flowPreferences = DecisionFlowPreferences(defaults: settingsDefaults)
            flowPreferences.advancesToNextItem = true
            let settingsDaemon = DaemonPane.MacDaemon(
                state: .running(
                    DaemonHealth(version: "1.0.0", startedAt: AttentionFixtures.createdInstant),
                    restartObserved: false),
                actionError: nil, start: {}, stop: {}, openApprovalSettings: {})
            // The System Health pane (#2920) reads the default inbox's
            // findings: an advisory and a blocking one open, a resolved one
            // closed, with the gate the blocking one closes. Its empty state
            // has no findings and an open gate.
            let blockingFinding = AttentionFixtures.blockingSystemHealth().item
            let healthStopped = Components.Schemas.UnattendedOperationSnapshot(
                admission: .stopped,
                stops: [
                    .init(
                        kind: .blocking_system_health, item_id: blockingFinding.id, command_id: nil,
                        since: blockingFinding.created_at)
                ])
            let noFindings = InboxStore(client: client)
            // The pane's findings add held work (#3026), a task finding whose
            // row names its task and reason, beside the default inbox's.
            let heldFinding = AttentionFixtures.heldWorkSystemHealth()
            let healthStore = InboxStore(client: client)
            healthStore.replaceAll(with: inbox + [heldFinding])
            for snapshot in inbox + [heldFinding] {
                await preloadReferences(snapshot.item, into: healthStore)
            }
            func settingsSync(
                _ devices: DevicesModel, routing: RoutingPolicyModel = routing,
                workflow: WorkflowPolicyModel = workflow, usage: AccountUsageModel = usage,
                store: InboxStore = store,
                unattendedOperation: Components.Schemas.UnattendedOperationSnapshot? = healthStopped,
                age: TimeInterval = 120
            ) -> SettingsSync {
                SettingsSync(
                    devices: devices, routing: routing, workflow: workflow, usage: usage, freshness: .fresh,
                    lastUpdatedAt: screenshotNow.addingTimeInterval(-age), revision: 4812,
                    projects: ["freeside-ai/freeside", "freeside-ai/teloleo", "free-skills"],
                    deviceID: DeviceIdentity.mock.deviceID, unresolvedActionCount: 0, store: store,
                    unattendedOperation: unattendedOperation)
            }
            let historyDefaults = try #require(
                UserDefaults(suiteName: "FreesideScreenshotHistory-\(UUID().uuidString)"))
            ListSectionPreferences(defaults: historyDefaults).systemHealthHistoryExpanded = true
            func settingsWindow(
                _ page: NavigationModel.SettingsPage, sync: SettingsSync, listDefaults: UserDefaults = settingsDefaults,
                finding: String? = nil, tasks: [Components.Schemas.TaskSnapshot] = []
            ) -> AnyView {
                let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
                navigation.selectSettingsPane(page)
                if let finding { navigation.openFinding(finding) }
                return AnyView(
                    SettingsWindowContent(
                        navigation: navigation, sync: sync, flowPreferences: flowPreferences,
                        host: "studio.local", daemon: settingsDaemon, tasks: tasks, listDefaults: listDefaults,
                        rendersInteractiveControls: false))
            }
            func phonePane(
                _ page: NavigationModel.SettingsPage, sync: SettingsSync, listDefaults: UserDefaults = settingsDefaults
            ) -> AnyView {
                AnyView(
                    SettingsPaneView(
                        page: page, sync: sync, flowPreferences: flowPreferences, host: "studio.local",
                        client: .iPhone, listDefaults: listDefaults, rendersInteractiveControls: false
                    )
                    .background(Color.ground))
            }
            let macSync = settingsSync(devicesModel)
            let phoneSync = settingsSync(phoneDevicesModel)
            for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                for (surface, page) in [
                    ("settings", NavigationModel.SettingsPage.general), ("devices", .devices),
                    ("notifications", .notifications), ("daemon", .daemon),
                ] {
                    surfaces.append(
                        Surface(
                            name: surface + suffix, width: 760, colorScheme: scheme,
                            view: settingsWindow(page, sync: macSync)))
                }
                surfaces.append(
                    Surface(
                        name: "settings-phone" + suffix, width: 390, colorScheme: scheme,
                        view: AnyView(
                            SettingsList(
                                navigation: NavigationModel(
                                    launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil)),
                                sync: phoneSync, flowPreferences: flowPreferences, host: "studio.local",
                                client: .iPhone, rendersInteractiveControls: false))))
                for (surface, page) in [
                    ("general-phone", NavigationModel.SettingsPage.general), ("devices-phone", .devices),
                    ("daemon-phone", .daemon),
                ] {
                    surfaces.append(
                        Surface(
                            name: surface + suffix, width: 390, colorScheme: scheme,
                            view: phonePane(page, sync: phoneSync)))
                }
                surfaces.append(
                    Surface(
                        name: "notifications-phone" + suffix, width: 390, colorScheme: scheme,
                        view: phonePane(.notifications, sync: settingsSync(phoneNotificationsModel))))
                // Inside the staleness threshold, so the gate reads current.
                let healthSync = settingsSync(devicesModel, store: healthStore, age: 30)
                let phoneHealthSync = settingsSync(phoneDevicesModel, store: healthStore, age: 30)
                let emptySync = settingsSync(
                    devicesModel, store: noFindings, unattendedOperation: .init(admission: .open, stops: []),
                    age: 30)
                surfaces += [
                    Surface(
                        name: "system-health" + suffix, width: 760, colorScheme: scheme,
                        view: settingsWindow(.systemHealth, sync: healthSync)),
                    Surface(
                        name: "system-health-empty" + suffix, width: 760, colorScheme: scheme,
                        view: settingsWindow(.systemHealth, sync: emptySync)),
                    Surface(
                        name: "system-health-history" + suffix, width: 760, colorScheme: scheme,
                        view: settingsWindow(.systemHealth, sync: healthSync, listDefaults: historyDefaults)),
                    Surface(
                        name: "system-health-seat" + suffix, width: 760, colorScheme: scheme,
                        view: settingsWindow(.systemHealth, sync: healthSync, finding: blockingFinding.id)),
                    // Held work's card names its task as a link to the page.
                    Surface(
                        name: "system-health-held-seat" + suffix, width: 760, colorScheme: scheme,
                        view: settingsWindow(
                            .systemHealth, sync: healthSync, finding: heldFinding.item.id,
                            tasks: TaskFixtures.defaultTasks())),
                    Surface(
                        name: "system-health-phone" + suffix, width: 390, colorScheme: scheme,
                        view: phonePane(.systemHealth, sync: phoneHealthSync)),
                    Surface(
                        name: "system-health-empty-phone" + suffix, width: 390, colorScheme: scheme,
                        view: phonePane(.systemHealth, sync: emptySync)),
                    Surface(
                        name: "system-health-history-phone" + suffix, width: 390, colorScheme: scheme,
                        view: phonePane(.systemHealth, sync: phoneHealthSync, listDefaults: historyDefaults)),
                ]
            }

            // The Agents pane (#2582), the routing policy (#2394, #2528) as a
            // popup per role over the agent tree: read with the canvas's
            // policy, with the default one whose writer roles differ, with a
            // drain-first account, with neither a policy nor an agent tree,
            // after an unanswered change, and after a stale change the daemon
            // refused. An edit of the orders and the account draws in the
            // Change order sheet, which the suite can't present, so those
            // states render the editor alone at a sheet's width.
            for state in RoutingPolicyScreenshots.State.allCases {
                let stateModel = await RoutingPolicyScreenshots.model(state)
                for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                    if state.isEditing {
                        for (platform, width) in [("", CGFloat(480)), ("-phone", 390)] {
                            surfaces.append(
                                Surface(
                                    name: state.surface + platform + suffix, width: width, colorScheme: scheme,
                                    view: AnyView(RoutingPolicyEditor(model: stateModel).background(Color.ground))))
                        }
                        continue
                    }
                    surfaces.append(
                        Surface(
                            name: state.surface + suffix, width: 760, colorScheme: scheme,
                            view: settingsWindow(.agents, sync: settingsSync(devicesModel, routing: stateModel))))
                    surfaces.append(
                        Surface(
                            name: state.surface + "-phone" + suffix, width: 390, colorScheme: scheme,
                            view: phonePane(.agents, sync: settingsSync(devicesModel, routing: stateModel))))
                }
            }

            // The Workflow pane (#3145): the general layer and the served
            // project's, each row's winning value and origin, with a
            // project row offering its reset to general; then an unanswered
            // change and a change refused because another device changed
            // the policy first.
            for state in WorkflowPolicyScreenshots.State.allCases {
                let stateModel = await WorkflowPolicyScreenshots.model(state)
                for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                    surfaces.append(
                        Surface(
                            name: state.surface + suffix, width: 760, colorScheme: scheme,
                            view: settingsWindow(.workflow, sync: settingsSync(devicesModel, workflow: stateModel))))
                    surfaces.append(
                        Surface(
                            name: state.surface + "-phone" + suffix, width: 390, colorScheme: scheme,
                            view: phonePane(.workflow, sync: settingsSync(devicesModel, workflow: stateModel))))
                }
            }

            // The Usage pane, which hosts account usage (#1721) drawn as
            // capacity cards (#2579): the default pools (readings and a
            // used-up window, an unsupported collector, and a pool with no
            // reading), the same pools once their readings go stale, the
            // #2389 Claude shape, a pool at R32's low line, an unanswered
            // refresh, a refused reassignment, and a confirmed one. Each
            // state pins its own clock against the usage fixtures' instants.
            for state in AccountUsageScreenshots.State.allCases {
                let stateModel = await AccountUsageScreenshots.model(state)
                for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                    surfaces.append(
                        Surface(
                            name: state.surface + suffix, width: 760, colorScheme: scheme,
                            view: AnyView(
                                settingsWindow(.usage, sync: settingsSync(devicesModel, usage: stateModel))
                                    .environment(\.pinnedNow, state.now))))
                    surfaces.append(
                        Surface(
                            name: state.surface + "-phone" + suffix, width: 390, colorScheme: scheme,
                            view: AnyView(
                                phonePane(.usage, sync: settingsSync(devicesModel, usage: stateModel))
                                    .environment(\.pinnedNow, state.now))))
                }
            }

            // The phone's Notifications pane just after its switch is turned
            // on: on ntfy's own server, so only the topic is asked for,
            // before any notification was opened. The pane above draws the
            // same steps for another server, after one was.
            let hostedSubscription = try #require(
                DeviceNtfySubscription(serverURL: "https://ntfy.sh", topic: DeviceNtfySubscription.mock.topic))
            let hostedNotificationsModel = await DevicesScreenshots.model(
                notificationSubscription: hostedSubscription, notificationsOn: true)
            for (name, scheme) in [
                ("notifications-phone-hosted", ColorScheme.light),
                ("notifications-phone-hosted-dark", ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: name, width: 390, colorScheme: scheme,
                        view: phonePane(.notifications, sync: settingsSync(hostedNotificationsModel))))
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

            // A type a newer daemon added: the generic card, with the
            // daemon's reason and its offered action disabled.
            let newerTypeStore = InboxStore(client: client)
            let newerTypeItem = AttentionFixtures.newerTypeFixture()
            newerTypeStore.replaceAll(with: [newerTypeItem])
            let newerTypeDetail = DecisionDetailView(
                store: newerTypeStore,
                itemID: newerTypeItem.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            surfaces.append(
                Surface(
                    name: "decision-newer_type",
                    view: AnyView(newerTypeDetail.screenshotCard(newerTypeItem.item, at: dynamicTypeSize))))

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
                        reason: "Superseded by version 2 of the specification.", successor: specification.item.id)
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
            let closedStore = InboxStore(client: client)
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

            // The unviewed mark (#2930): an open row no device has opened
            // (the top-right dot), an open row already viewed, and a closed
            // row, which never carries the mark.
            if let unviewed = inbox.first(where: {
                $0.item.status == .open && $0.item.surface_first_opened_at == nil
            })?.item,
                let viewed = inbox.first(where: {
                    $0.item.status == .open && $0.item.surface_first_opened_at != nil
                })?.item,
                let closed = inbox.first(where: { $0.item.status != .open })?.item
            {
                for variant: (suffix: String, width: CGFloat, colorScheme: ColorScheme) in [
                    ("", 560, .light), ("-dark", 560, .dark), ("-phone", 390, .light), ("-phone-dark", 390, .dark),
                ] {
                    surfaces.append(
                        Surface(
                            name: "inbox-viewed-mix\(variant.suffix)",
                            width: variant.width,
                            colorScheme: variant.colorScheme,
                            view: AnyView(
                                VStack(spacing: 8) {
                                    InboxRowView(item: unviewed, now: screenshotNow)
                                    InboxRowView(item: viewed, now: screenshotNow)
                                    InboxRowView(item: closed, now: screenshotNow)
                                }
                                .padding()
                            )))
                }
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

            // One open card per type, the first of each, since the surface
            // takes the type's name; the closed matrix below draws the
            // closed fixtures under their own names.
            var drawnTypes = Set<Components.Schemas.AttentionType>()
            for snapshot in inbox where snapshot.item.status == .open && drawnTypes.insert(snapshot.item._type).inserted
            {
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
                    // the recorded reason, and the final review's yield into
                    // Recorded Context; this pins what the fold holds.
                    let openDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        // The summary's report is its own fold (#1461).
                        expandsSummaryReports: true,
                        expandedDisclosures: Set(
                            [.runDetails, .recordedContext]
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
                    // decision. Its open foot is appended last, below.
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
                    // A quota failure whose card offers other agents (#869):
                    // the Other Agents fact and the retry action. The offers'
                    // readings are current, stale, and unsupported (#3030).
                    var quota = AttentionFixtures.quotaFailure().item
                    quota.alternate_agents = UsageFixtures.alternateAgentOffers()
                    let quotaDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow,
                        usagePools: UsageFixtures.alternatesUsagePools(now: screenshotNow)
                    )
                    for (suffix, width) in [("", canvasWidth), ("-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "decision-execution_failure-quota\(suffix)",
                                width: width,
                                view: AnyView(
                                    quotaDetail.screenshotCard(
                                        quota, at: dynamicTypeSize, compactLayout: width < canvasWidth))))
                    }
                    // An import refusal of the run's evidence (#3180): the
                    // cause names invalid evidence rather than unknown.
                    surfaces.append(
                        Surface(
                            name: "decision-execution_failure-invalid_evidence",
                            width: canvasWidth,
                            view: AnyView(
                                detail.screenshotCard(
                                    AttentionFixtures.invalidEvidenceFailure().item, at: dynamicTypeSize))))
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
                    // one-paragraph shape, the specifier's headings, and the
                    // handoff-report shape (#2614), by day and by dusk at
                    // both widths, closed and opened.
                    for (name, content) in [
                        ("legacy", DecisionSummaryFixtures.specificationLegacy),
                        ("structured", DecisionSummaryFixtures.specificationStructured),
                        ("aligned-specifier", DecisionSummaryFixtures.alignedSpecifier),
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
                            // Recorded Context carries the approval's reason,
                            // which is the same summary, in full and drawn the
                            // same.
                            let reasonDetail = DecisionDetailView(
                                store: store, itemID: summarySnapshot.item.id,
                                expandedDisclosures: [.recordedContext],
                                loadsAttachments: false, showsValidationProgress: false)
                            surfaces.append(
                                Surface(
                                    name: "decision-spec_approval-summary-structured-details",
                                    view: AnyView(
                                        reasonDetail.screenshotCard(
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
                                    .specification, item: revised.item))))
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
                                    .specification, item: revised.item))))
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
                                    .diff, item: revised.item))))
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
                                    .diff, item: revised.item))))
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
                        ("aligned-implementer", Optional(DecisionSummaryFixtures.alignedImplementer)),
                        ("artifact-only", nil),
                    ] {
                        let summarySnapshot = DecisionSummaryFixtures.snapshot(content: content)
                        // The wrapped and aligned-implementer reports are
                        // short, so their closed cards already show all of
                        // them.
                        for expanded in [false, true]
                        where !expanded || (content != nil && name != "wrapped" && name != "aligned-implementer") {
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
                    // card's foot must not repeat a Facts section.
                    let readyPreferencesSuite = "FreesideScreenshotReadyPreferences-\(UUID().uuidString)"
                    guard let readyDefaults = UserDefaults(suiteName: readyPreferencesSuite)
                    else {
                        throw ScreenshotError.preferencesUnavailable
                    }
                    defer { readyDefaults.removePersistentDomain(forName: readyPreferencesSuite) }
                    let readyPreferences = DecisionSectionPreferences(defaults: readyDefaults)
                    readyPreferences.evidenceExpanded = true
                    readyPreferences.detailsExpanded = true
                    let readyFoot = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        expandedDisclosures: [.recordedContext],
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow,
                        sectionPreferences: readyPreferences)
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-foot",
                            view: AnyView(
                                readyFoot.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize))))
                    // A verdict the daemon invalidated (issue #982), rendered
                    // stale beside the bound and observed heads (frame 7.3).
                    // The degraded verdict is the 4b surfaces'.
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-stale",
                            view: AnyView(
                                detail.screenshotCard(
                                    AttentionFixtures.staleReady().item, at: dynamicTypeSize))))
                    // An open review whose checks deadline elapsed (#2752):
                    // the elapse draws as a Checks fact after the Change row,
                    // on the Mac and on the phone.
                    let deadline = AttentionFixtures.readyWithElapsedChecksDeadline().item
                    for (suffix, width) in [("", CGFloat?.none), ("-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "decision-ready_for_final_review-deadline\(suffix)",
                                width: width,
                                view: AnyView(
                                    detail.screenshotCard(
                                        deadline, at: dynamicTypeSize, compactLayout: width != nil))))
                    }
                    // A review guide (#2751): the agent's stops, after the
                    // summary and ahead of the facts, with a resolved head
                    // excerpt that opens at its line and an unresolved base
                    // excerpt that says why, on the Mac and on the phone.
                    let guided = AttentionFixtures.readyWithReviewGuide().item
                    for (suffix, width) in [("", CGFloat?.none), ("-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "decision-ready_for_final_review-guide\(suffix)",
                                width: width,
                                view: AnyView(
                                    detail.screenshotCard(
                                        guided, at: dynamicTypeSize, compactLayout: width != nil))))
                    }
                    // A guide the daemon trimmed to its caps (#3180): the
                    // trims draw as daemon fact ahead of the stops.
                    let trimmed = AttentionFixtures.readyWithTrimmedReviewGuide().item
                    for (suffix, width) in [("", CGFloat?.none), ("-phone", CGFloat(390))] {
                        surfaces.append(
                            Surface(
                                name: "decision-ready_for_final_review-guide_trimmed\(suffix)",
                                width: width,
                                view: AnyView(
                                    detail.screenshotCard(
                                        trimmed, at: dynamicTypeSize, compactLayout: width != nil))))
                    }
                    // A guide over the read bound, left out (#3180): the
                    // module says why the card has none.
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-guide_omitted",
                            width: nil,
                            view: AnyView(
                                detail.screenshotCard(
                                    AttentionFixtures.readyWithOmittedReviewGuide().item,
                                    at: dynamicTypeSize))))
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
                    let footPreferences = DecisionSectionPreferences(
                        defaults: preferencesDefaults)
                    footPreferences.detailsExpanded = true
                    // Recorded Context open too: the reason and the review
                    // rounds read there (#2607).
                    let footDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        expandedDisclosures: [.recordedContext],
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        now: screenshotNow,
                        sectionPreferences: footPreferences)
                    surfaces.append(
                        Surface(
                            name: "decision-review_diminishing_returns-foot",
                            view: AnyView(
                                footDetail.screenshotCard(
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
            let preferencesSuite = "FreesideScreenshotFootPreferences-\(UUID().uuidString)"
            guard let preferencesDefaults = UserDefaults(suiteName: preferencesSuite) else {
                throw ScreenshotError.preferencesUnavailable
            }
            defer { preferencesDefaults.removePersistentDomain(forName: preferencesSuite) }
            let footPreferences = DecisionSectionPreferences(defaults: preferencesDefaults)
            footPreferences.evidenceExpanded = true
            footPreferences.detailsExpanded = true
            let footDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                expandedDisclosures: [.recordedContext],
                showsValidationProgress: false,
                sectionPreferences: footPreferences)
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
                    name: "decision-finding_adjudication-foot",
                    view: AnyView(
                        footDetail.screenshotCard(
                            adjudication,
                            at: dynamicTypeSize))))
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-foot-phone",
                    width: 390,
                    view: AnyView(
                        footDetail.screenshotCard(
                            adjudication,
                            at: dynamicTypeSize,
                            compactLayout: true))))

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
            surfaces += try unifiedListSurfaces(inbox: inbox, tasks: tasks, runs: runs)
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
            // Two tasks for one issue under one name (#1379, #1922), created
            // a day apart: each row ends its context line with its creation
            // time and short id so the rows do not read alike. At the Mac
            // sidebar's width and the iPhone's, across the size matrix, the
            // marker must survive the accessibility sizes.
            var secondEqualName = retryTask
            secondEqualName.task.id = "task-\(RunFixtures.readyRunID)"
            secondEqualName.task.created_at = retryTask.task.created_at.addingTimeInterval(-26 * 3600)
            let equalNameTasks = [retryTask, secondEqualName]
            let equalNameAmbiguous = TaskDisplay.ambiguousTaskIDs(equalNameTasks)
            for colorScheme in [ColorScheme.light, .dark] {
                for (width, tail) in [(CGFloat(320), ""), (390, "-390")] {
                    surfaces.append(
                        Surface(
                            name: "tasks-equal-names" + tail + (colorScheme == .dark ? "-dark" : ""),
                            width: width,
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
                        TaskPageView(coordinator: coordinator, snapshot: task, onOpenRun: { _ in })
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
                                TaskPageView(
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
            // A launch waiting for machine memory (#1598) and one waiting on
            // a full usage pool (#1596): the row's capacity wait and the
            // timeline header's callout, each guidance sentence naming its
            // cause from the recorded hold (#1599).
            // A park on an exhausted quota window that offers another agent
            // draws its move menu under the callout (#869).
            // A task line whose model its enrollment doesn't serve names the
            // refused part as the task's guidance (#2992).
            for (name, slug) in [
                ("Machine capacity", "machine-capacity"), ("Usage pool", "usage-pool"),
                ("Quota window", "quota-window"), ("Model unserved", "model-unserved"),
            ] {
                let wait = HoldPresentationFixtures.make(name)
                var waitTask = approvalFixture.task
                waitTask.task = wait.task
                let waitCache = InMemoryCacheStore()
                try waitCache.save(
                    .init(
                        cursors: .init(
                            syncEpoch: "\(slug)-epoch", lastFullSnapshotRevision: waitTask.as_of_revision,
                            highestObservedServerRevision: waitTask.as_of_revision),
                        attentionItems: wait.items, runs: wait.runs, tasks: [waitTask],
                        taskTimelines: [wait.history].compactMap { $0 }))
                let waitCoordinator = SyncCoordinator(
                    client: APIClientFactory.mock(server: MockServer()), cache: waitCache)
                // The page offers the move only once /health lists it, and
                // this coordinator never syncs.
                waitCoordinator.daemonFeatures = [.parked_task_move]
                for width in [CGFloat(390), CGFloat(820)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "task-hold-\(slug)-\(Int(width))-\(scheme)", width: width,
                                colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    VStack(alignment: .leading, spacing: 28) {
                                        TaskRowView(
                                            task: wait.task, position: wait.position,
                                            now: RunFixtures.screenshotInstant)
                                        TaskPageView(
                                            coordinator: waitCoordinator, snapshot: waitTask, onOpenRun: { _ in }
                                        ).header(wait.history)
                                    }
                                    .padding(24)
                                    .foregroundStyle(Color.ink)
                                    .background(Color.ground))))
                    }
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
                                TaskPageView(
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
                                TaskPageView(
                                    coordinator: display, snapshot: TaskHistoryFixtures.snapshot(history),
                                    onOpenRun: { _ in }
                                ).screenshotContent(history))))
                }
            }
            // The two-campaign history with every fold open (the earlier
            // campaign, each prior run, and each run's details), so the
            // folded content is covered too. The current campaign has no
            // fold, so the history's closed surface above is its open one.
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
                            TaskPageView(
                                coordinator: freshCoordinator,
                                snapshot: TaskHistoryFixtures.snapshot(revisedHistory), onOpenRun: { _ in }
                            ).screenshotContent(revisedHistory))))
            }
            surfaces.append(
                Surface(
                    name: "task-history-revised-all-open-820-light", width: 820, nativeAppearance: true,
                    view: AnyView(
                        TaskPageView(
                            coordinator: revisedCoordinator,
                            snapshot: TaskHistoryFixtures.snapshot(revisedHistory), onOpenRun: { _ in }
                        ).screenshotContent(revisedHistory, disclosures: everythingOpen))))
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
                                    TaskPageView(
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
                                TaskPageView(
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
                                TaskPageView(
                                    coordinator: publishedCoordinator, snapshot: publishedTask, onOpenRun: { _ in }
                                )
                                .screenshotContent(publishedHistory))))
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
                            TaskPageView(
                                coordinator: legacyCoordinator, snapshot: legacyTask, onOpenRun: { _ in }
                            )
                            .screenshotContent(legacyHistory))))
            }
            surfaces.append(
                Surface(
                    name: "task-page-parked",
                    view: AnyView(
                        TaskPageView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(retryTimeline)
                    )))
            surfaces.append(
                Surface(
                    name: "task-page-parked-390",
                    width: 390,
                    view: AnyView(
                        TaskPageView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(retryTimeline)
                    )))
            // A line change from the task page (#2651): the saved change the
            // daemon didn't answer, with its Retry, and a change refused
            // because the reviewer runs, naming the line it keeps. Each state
            // has its own coordinator over the same cache, so no other
            // surface draws it.
            var linedTask = retryTask
            let reviewerLine = UsageFixtures.agentSelection("codex-primary")
            linedTask.task.task_lines = [
                .init(
                    id: "sha256:" + String(repeating: "d", count: 64), task_id: retryTask.task.id, role: .reviewer,
                    agent: nil, selection: .init(value1: reviewerLine), version: 1, predecessor_id: nil,
                    source: .submit_task, set_by: "command-screenshot", set_at: RunFixtures.screenshotInstant,
                    provenance: nil)
            ]
            var linedState = try #require(cache.load())
            linedState.tasks = linedState.tasks.map { $0.task.id == retryTask.task.id ? linedTask : $0 }
            for state in ["unanswered", "refused"] {
                let lineCache = InMemoryCacheStore()
                try lineCache.save(linedState)
                let lineCoordinator = SyncCoordinator(client: client, cache: lineCache)
                if state == "unanswered" {
                    let saved = lineCoordinator.retainTaskLineChange(
                        PendingTaskLineChange(
                            command: .init(
                                command_id: "line-change-screenshot", device_id: lineCoordinator.store.device.deviceID,
                                payload: .change_task_line(
                                    .init(
                                        kind: .change_task_line, task_id: retryTask.task.id, role: .reviewer,
                                        selection: nil))),
                            syncEpoch: "screenshot-epoch"))
                    #expect(saved)
                } else {
                    lineCoordinator.taskLineChangeFailure = .init(
                        taskID: retryTask.task.id,
                        message: TaskLineChangeModel.runningMessage(role: .reviewer, task: linedTask.task))
                }
                for width in [CGFloat(820), CGFloat(390)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "task-page-line-change-\(state)-\(Int(width))-\(scheme)", width: width,
                                colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    TaskPageView(coordinator: lineCoordinator, snapshot: linedTask, onOpenRun: { _ in })
                                        .screenshotContent(retryTimeline))))
                    }
                }
            }
            // The page's Identifiers footer expanded (#1379, #2117): the
            // exact task, campaign, and run ids grouped by owner, with their
            // copy controls.
            surfaces.append(
                Surface(
                    name: "task-page-details",
                    view: AnyView(
                        TaskPageView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(
                                retryTimeline,
                                disclosures: TaskHistoryFixtures.identifiersOpen(retryTimeline))
                    )))
            surfaces.append(
                Surface(
                    name: "task-page-details-390",
                    width: 390,
                    view: AnyView(
                        TaskPageView(coordinator: coordinator, snapshot: retryTask, onOpenRun: { _ in })
                            .screenshotContent(
                                retryTimeline,
                                disclosures: TaskHistoryFixtures.identifiersOpen(retryTimeline))
                    )))
            // The task's attachments (#2590): an image renders inline and a
            // file shows its type and size, each in its own box; a folder
            // (#2591) is one folded row. A separate
            // coordinator holds the preloaded image, so no other surface's
            // loader changes.
            var attachedTask = retryTask
            attachedTask.task.attachments = [
                .init(
                    digest: "sha256:img-spec_approval", name: "Layout bug.png", media_type: .image_sol_png,
                    claimed_media_type: "image/png", size_bytes: Int64(AttentionFixtures.fixtureImagePNG.count)),
                .init(
                    digest: "sha256:log-spec_approval", name: "crash.log",
                    media_type: .application_sol_octet_hyphen_stream, claimed_media_type: "text/plain",
                    size_bytes: 48_213),
            ]
            let attachedCoordinator = SyncCoordinator(client: client, cache: cache)
            for attachment in attachedTask.task.attachments {
                await attachedCoordinator.store.attachments.load(
                    attachment.digest,
                    reference: AttachmentReference(
                        mediaType: attachment.media_type.rawValue, sizeBytes: Int(attachment.size_bytes),
                        availability: .available))
            }
            guard case .image = attachedCoordinator.store.attachments.phase(for: "sha256:img-spec_approval") else {
                throw ScreenshotError.missingSeededImage
            }
            // A folder's files (#2591) fold under one row, closed, so they
            // load nothing.
            attachedTask.task.attachments += ["brief.md", "screens/home.png", "screens/settings.png"].enumerated()
                .map { index, path in
                    .init(
                        digest: "sha256:" + String(repeating: "\(index)", count: 64), name: "design handoff/\(path)",
                        media_type: path.hasSuffix(".png") ? .image_sol_png : .application_sol_octet_hyphen_stream,
                        claimed_media_type: path.hasSuffix(".png") ? "image/png" : "text/markdown",
                        size_bytes: 412_000)
                }
            // The files sit in the page head as one fold under the
            // documents line: `task-page-attachments` draws it open,
            // `-folded` closed, as a task first shows it.
            for (base, open) in [("task-page-attachments", true), ("task-page-attachments-folded", false)] {
                let disclosures = TaskTimelineDisclosurePreferences(defaults: nil)
                disclosures.setExpanded(open, .attachments, taskID: attachedTask.task.id)
                for (tail, width) in [("", CGFloat?.none), ("-390", 390)] {
                    surfaces.append(
                        Surface(
                            name: base + tail,
                            width: width,
                            view: AnyView(
                                TaskPageView(
                                    coordinator: attachedCoordinator, snapshot: attachedTask, onOpenRun: { _ in }
                                )
                                .screenshotContent(retryTimeline, disclosures: disclosures)
                            )))
                }
            }
            // A remediation under way (#2100): the task's events return to
            // Implementation at the remediator's start, under the decision
            // that sent the findings there, and Current Run draws the review
            // round that found them.
            let remediationScript = TaskStageFixtures.script(.remediation)
            let remediation = try TaskStageFixtures.coordinator(
                TaskStageFixtures.page(.remediation), runTimelines: remediationScript.runTimelines)
            for (name, width) in [("task-page-remediation", CGFloat?.none), ("task-page-remediation-390", 390)] {
                surfaces.append(
                    Surface(
                        name: name,
                        width: width,
                        view: AnyView(
                            TaskPageView(
                                coordinator: remediation.coordinator, snapshot: remediation.task, onOpenRun: { _ in }
                            )
                            .screenshotContent(remediationScript.timeline)
                        )))
            }
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
            // The run timeline's hold callout with each capacity hold's
            // recorded fact (#1599): the full pool, the machine's memory
            // reservations, and a stored identity_parallelism hold, which
            // names no pool and so states no fact.
            // An admission hold names the selection part it refused (#2992).
            for (name, slug) in [
                ("Usage pool", "usage-pool"), ("Machine capacity", "machine-capacity"), ("Capacity", "identity"),
                ("Model unserved", "model-unserved"),
            ] {
                guard let hold = HoldPresentationFixtures.hold(name, runID: activeRun.run.id) else {
                    preconditionFailure("\(name) has no recorded hold fixture")
                }
                var heldRun = activeRun
                heldRun.run.hold_reason = .init(value1: hold.reason)
                var heldTimeline = timeline
                heldTimeline.hold = .init(value1: hold)
                for width in [CGFloat(390), CGFloat(820)] {
                    for scheme in [ColorScheme.light, .dark] {
                        surfaces.append(
                            Surface(
                                name: "run-timeline-hold-\(slug)-\(Int(width))-\(scheme)", width: width,
                                colorScheme: scheme, nativeAppearance: true,
                                view: AnyView(
                                    RunTimelineView(coordinator: coordinator, snapshot: heldRun)
                                        .screenshotContent(heldTimeline))))
                    }
                }
            }
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

            // A specification run's invocations named by why each ran
            // (#3044), with a repeated research continuation.
            let specificationHistory = RunFixtures.specificationHistoryRun()
            let specificationWidths: [(String, CGFloat?)] = [
                ("run-timeline-specification", nil),
                ("run-timeline-specification-390", 390),
            ]
            for (name, width) in specificationWidths {
                surfaces.append(
                    Surface(
                        name: name, width: width,
                        view: AnyView(
                            RunTimelineView(coordinator: coordinator, snapshot: specificationHistory)
                                .screenshotContent(RunFixtures.specificationHistoryTimeline())
                        )))
            }

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
            // The issue comment card is not in the default inbox either. Its
            // facts carry the issue the duplicate search matched, so these pin
            // its rows and the proposed comment in its unverified section, at
            // Mac and iPhone width in both schemes.
            let commentEffect = AttentionFixtures.issueCommentEffectProposal()
            let commentEffectStore = InboxStore(client: client)
            commentEffectStore.replaceAll(with: [commentEffect])
            let commentEffectDetail = DecisionDetailView(
                store: commentEffectStore,
                itemID: commentEffect.item.id,
                graphics: .init(),
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow)
            let commentEffectFacts = AttentionFixtures.effectProposalFacts(for: commentEffect)
            for (suffix, width, scheme) in [
                ("", canvasWidth, ColorScheme.light),
                ("-phone", CGFloat(390), ColorScheme.light),
                ("-dark", canvasWidth, ColorScheme.dark),
                ("-phone-dark", CGFloat(390), ColorScheme.dark),
            ] {
                surfaces.append(
                    Surface(
                        name: "decision-effect_proposal-comment\(suffix)",
                        width: width,
                        colorScheme: scheme,
                        view: AnyView(
                            commentEffectDetail.screenshotCard(
                                commentEffect.item,
                                at: dynamicTypeSize,
                                effectProposalFacts: commentEffectFacts))))
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
            // The incompatible banner names who updates (#3109), distinct
            // from the sync-failing and unreachable banners. The root view
            // draws the refusal screen instead; the banner is the standing
            // notice's form of the same words.
            surfaces.append(
                Surface(
                    name: "freshness-banner-incompatible",
                    width: 640,
                    view: AnyView(
                        VStack(spacing: 0) {
                            FreshnessBanner(
                                freshness: .incompatible(.appBelowFloor(floor: 4, daemonVersion: "1.0.0")))
                            Text("Inbox").padding()
                        })))
            // The refusal screen that replaces the whole inbox while the app
            // and daemon can't talk (#3109), for each side that needs the
            // update, on the Mac and on the iPhone. The app level is a fixed
            // fixture value so the frames hold when the spec's level rises.
            let refusals: [(String, DaemonIncompatibility)] = [
                ("app-below-floor", .appBelowFloor(floor: 4, daemonVersion: "1.0.0")),
                ("daemon-below-level", .daemonBelowLevel(level: 2, daemonVersion: "1.0.0")),
            ]
            for (name, incompatibility) in refusals {
                for (platform, width, suffix) in [
                    (CompatibilityWords.Platform.mac, CGFloat(860), ""), (.phone, 390, "-phone"),
                ] {
                    surfaces.append(
                        Surface(
                            name: "compatibility-refusal-\(name)\(suffix)",
                            width: width,
                            view: AnyView(
                                CompatibilityRefusalView(
                                    incompatibility: incompatibility, platform: platform, appLevel: 3
                                )
                                .frame(height: 480)
                                .background(Color.ground))))
                }
            }

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
                reason: String? = "The daemon stopped unattended operation after repeated restarts",
                colorScheme: ColorScheme = .light,
                paintsGround: Bool = false,
                content: AnyView = AnyView(Text("Inbox").padding())
            ) -> Surface {
                let inDetailColumn = width > 390
                let notices = StandingNotices(
                    saveWarning: nil, onDismissSaveWarning: {},
                    freshness: freshness, lastUpdatedAt: lastUpdatedAt, onRePair: nil,
                    operation: operation,
                    reason: { _ in reason },
                    onOpenSystemHealth: { _ in },
                    placement: inDetailColumn ? .detailColumn : .window)
                let view =
                    inDetailColumn
                    ? AnyView(StandingDetailColumn(notices: notices) { _ in content })
                    : AnyView(
                        VStack(spacing: 0) {
                            notices
                            content
                        })
                return Surface(
                    name: name,
                    width: width,
                    colorScheme: colorScheme,
                    view: paintsGround ? AnyView(view.background(Color.ground)) : view)
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
            // A stop whose item is not open here (#2921): the action opens
            // System Health alone, where it was withheld before.
            for (suffix, width) in [("", Self.detailColumnWidth), ("-phone", CGFloat(390))] {
                for (schemeSuffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                    surfaces.append(
                        stoppedSurface(
                            name: "unattended-stopped-system-health" + suffix + schemeSuffix, width: width,
                            operation: operatorStopped, lastUpdatedAt: stoppedFresh, reason: nil,
                            colorScheme: scheme, paintsGround: true))
                }
            }
            // The iPhone bottom bar's gear with its open-findings badge
            // (#2921), one digit and two, in the bar's tint. The suite
            // renders on the Mac, so the bar itself is drawn here by hand.
            for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                surfaces.append(
                    Surface(
                        name: "settings-gear-badge-phone" + suffix,
                        width: 390,
                        colorScheme: scheme,
                        view: AnyView(
                            HStack {
                                SettingsGearLabel(badge: "2")
                                Spacer()
                                SettingsGearLabel(badge: "12")
                                Spacer()
                                SettingsGearLabel(badge: nil)
                            }
                            .font(.title3)
                            .foregroundStyle(Color.accentText)
                            .padding(.horizontal, 20)
                            .padding(.vertical, 14)
                            .background(Color.ground))))
            }
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
            // The inbox clear while a blocking finding is open (#2920): the
            // finding is not a Needs you row, so the detail column keeps the
            // inbox-clear result and the phone's list draws no Needs you rows,
            // with the standing notice above each carrying the stop.
            let findingOnly = InboxStore(client: client)
            findingOnly.replaceAll(with: [AttentionFixtures.blockingSystemHealth()])
            let clear = UnifiedListView.emptyNeedsYou(projectID: nil)
            for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                surfaces.append(
                    stoppedSurface(
                        name: "unattended-stopped-clear" + suffix, width: Self.detailColumnWidth,
                        operation: findingStopped, lastUpdatedAt: stoppedFresh, colorScheme: scheme,
                        content: AnyView(
                            UnavailableStateView(
                                glyph: .inbox, title: clear.title, description: clear.description,
                                seat: .detailColumn))))
                try surfaces.append(
                    stoppedSurface(
                        name: "unattended-stopped-clear-phone" + suffix, width: 390,
                        operation: findingStopped, lastUpdatedAt: stoppedFresh, colorScheme: scheme,
                        content: AnyView(
                            UnifiedListView(
                                store: findingOnly, tasks: [], runs: [],
                                navigation: screenshotNavigation(selecting: nil),
                                preferences: screenshotListPreferences()
                            ).screenshotContent(now: screenshotNow))))
            }

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
            // healthy by day with the inbox row under the pointer, a daemon
            // too new for this app after an observed restart by dusk,
            // stopped with Start as the one primary, unreachable with a
            // failed stop, and an ephemeral app's no-supervised-daemon state
            // with no control. The start instant is the fixture's fixed creation time.
            let panelHealth = DaemonHealth(version: "1.0.0", startedAt: AttentionFixtures.createdInstant)
            let panelIncompatible = DaemonHealth(
                version: "1.0.0", startedAt: AttentionFixtures.createdInstant,
                compatibilityLevel: 1_000, compatibilityFloor: 1_000)
            let panelActions = DaemonMenuPanel.Actions(
                openApp: {}, showNeedsYou: {}, showSystemHealth: {}, start: {}, stop: {},
                openApprovalSettings: {}, quit: {})
            surfaces.append(
                Surface(
                    name: "menu-panel-running",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelHealth, restartObserved: false),
                            actionError: nil,
                            needsYou: .init(rows: 6),
                            actions: panelActions
                        )
                        .screenshotHovering(.showNeedsYou))))
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
                            needsYou: .init(rows: 6),
                            unattendedStopped:
                                "Unattended operation is stopped by operator decision.",
                            actions: panelActions))))
            // Open findings (#2921): the System Health line after Show Needs
            // You with the pane's open count, here beside the stop a
            // blocking finding raised.
            for (suffix, scheme) in [("", ColorScheme.light), ("-dark", ColorScheme.dark)] {
                surfaces.append(
                    Surface(
                        name: "menu-panel-system-health" + suffix,
                        width: 320,
                        colorScheme: scheme,
                        view: AnyView(
                            DaemonMenuPanel(
                                state: .running(panelHealth, restartObserved: false),
                                actionError: nil,
                                needsYou: .init(rows: 6),
                                systemHealth: .init(open: 2),
                                unattendedStopped:
                                    "Unattended operation is stopped by a system health finding.",
                                actions: panelActions))))
            }
            surfaces.append(
                Surface(
                    name: "menu-panel-incompatible-restart-dark",
                    width: 320,
                    colorScheme: .dark,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .running(panelIncompatible, restartObserved: true),
                            actionError: nil,
                            needsYou: .init(rows: 6),
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-stopped",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .stopped,
                            actionError: nil,
                            needsYou: .init(rows: 6),
                            actions: panelActions))))
            surfaces.append(
                Surface(
                    name: "menu-panel-unsupervised",
                    width: 320,
                    view: AnyView(
                        DaemonMenuPanel(
                            state: .unsupervised,
                            actionError: nil,
                            needsYou: .init(rows: 6),
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
                            needsYou: .init(rows: 6),
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
                "pairing", "freshness-banner-stale", "freshness-banner-incompatible",
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
            surfaces.append(contentsOf: try taskPageSurfaces(at: dynamicTypeSize))
            surfaces.append(contentsOf: taskSpecificationReaderSurfaces())
            surfaces.append(contentsOf: taskPromptReaderSurfaces())
            surfaces.append(contentsOf: taskStyleSurfaces())
            surfaces.append(contentsOf: cardStyleSurfaces())
            surfaces.append(contentsOf: choiceListSurfaces())
            surfaces.append(contentsOf: finalReviewFrameSurfaces(store: store, at: dynamicTypeSize))

            // Visual audit D08: the dispute card on both platforms and
            // themes with the positions the screenshot graphics supply, then
            // the shape production sends, which has no positions: one claim
            // with its text, and one that arrived as an attachment. These
            // render after the surfaces above for the reason the open feet
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
                let routedStore = InboxStore(client: routedClient)
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

            // The question and dispute cards with their foot open (visual
            // audit D06, D08). They
            // render after every other surface: a glyph's antialiasing
            // depends on what was rasterized before it (#1698), and drawing
            // one ahead of the final-review foot moved one of that surface's
            // pixels.
            for type in [Components.Schemas.AttentionType.agent_question, .review_dispute] {
                guard let item = inbox.first(where: { $0.item._type == type })?.item else {
                    continue
                }
                let suite = "FreesideScreenshotCardFootPreferences-\(UUID().uuidString)"
                guard let defaults = UserDefaults(suiteName: suite) else {
                    throw ScreenshotError.preferencesUnavailable
                }
                defer { defaults.removePersistentDomain(forName: suite) }
                let preferences = DecisionSectionPreferences(defaults: defaults)
                preferences.evidenceExpanded = true
                preferences.detailsExpanded = true
                let foot = DecisionDetailView(
                    store: store,
                    itemID: item.id,
                    expandedDisclosures: [.recordedContext],
                    graphics: graphicPresentations(for: item),
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    now: screenshotNow,
                    sectionPreferences: preferences)
                surfaces.append(
                    Surface(
                        name: "decision-\(type.rawValue)-foot",
                        view: AnyView(foot.screenshotCard(item, at: dynamicTypeSize))))
            }

            // The per-finding card (visual audit D09) with messages of the
            // length a reviewer writes, on both platforms and in both
            // appearances, and the phone card with one finding open. Each
            // card shows its whole message, so the card is pinned with text
            // that wraps (#1033, #1141). Appended last for the same reason
            // as the open feet above.
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
            // disclosure, so the face is the only place the choice shows. The
            // card is validated so its controls draw enabled: the held pick
            // fills Choose Another Route and outlines the acceptance (R6,
            // #3011), which a disabled control would not show.
            let selectedAlternativeSnapshot = AttentionFixtures.findingAdjudicationRealisticMessages()
            let selectedAlternativeStore = InboxStore(
                client: APIClientFactory.mock(server: MockServer(items: [selectedAlternativeSnapshot])))
            selectedAlternativeStore.replaceAll(with: [selectedAlternativeSnapshot])
            await preloadReferences(realisticFindings, into: selectedAlternativeStore)
            let selectedAlternativeModel = DecisionModel(
                store: selectedAlternativeStore, itemID: realisticFindings.id)
            await selectedAlternativeModel.validate()
            await selectedAlternativeModel.refreshActionSurface()
            #expect(selectedAlternativeModel.validation == .validated)
            let selectedAlternativeDetail = DecisionDetailView(
                store: selectedAlternativeStore,
                itemID: realisticFindings.id,
                alternativeSelections: ["review-finding-17": .dispute],
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow,
                model: selectedAlternativeModel)
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-alternative-selected",
                    view: AnyView(
                        selectedAlternativeDetail.screenshotCard(
                            realisticFindings, at: dynamicTypeSize))))
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-alternative-selected-phone",
                    width: 390,
                    view: AnyView(
                        selectedAlternativeDetail.screenshotCard(
                            realisticFindings, at: dynamicTypeSize, compactLayout: true))))
            // The first finding's reason expanded in place with Less, the
            // second still clamped with More (#2608), on both platforms and
            // in both appearances.
            let expandedReason = FindingReasonPreferences(defaults: nil)
            expandedReason.setExpanded(
                true, findingID: "review-finding-17", itemID: realisticFindings.id)
            let expandedReasonDetail = DecisionDetailView(
                store: store,
                itemID: realisticFindings.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: screenshotNow,
                findingReasonPreferences: expandedReason)
            for (suffix, colorScheme) in [("", ColorScheme.light), ("-dark", .dark)] {
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-reason-expanded\(suffix)",
                        colorScheme: colorScheme,
                        view: AnyView(
                            expandedReasonDetail.screenshotCard(
                                realisticFindings, at: dynamicTypeSize))))
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-reason-expanded-phone\(suffix)",
                        width: 390,
                        colorScheme: colorScheme,
                        view: AnyView(
                            expandedReasonDetail.screenshotCard(
                                realisticFindings, at: dynamicTypeSize, compactLayout: true))))
            }
            // The Mac sidebar at a window's height with nothing open: the
            // space above stays empty and Working and Finished sit pinned at
            // its foot. Drawn last: ahead of another surface it once moved
            // that surface's glyph antialiasing (#1698).
            var settled = try #require(inbox.first)
            settled.item.status = .resolved
            let emptyStore = InboxStore(client: client)
            emptyStore.replaceAll(with: [settled])
            for colorScheme in [ColorScheme.light, .dark] {
                try surfaces.append(
                    Surface(
                        name: "unified-list-empty-needs-you" + (colorScheme == .dark ? "-dark" : ""),
                        width: 320,
                        colorScheme: colorScheme,
                        view: AnyView(
                            UnifiedListView(
                                store: emptyStore, tasks: tasks,
                                navigation: screenshotNavigation(selecting: nil),
                                preferences: screenshotListPreferences()
                            ).screenshotContent(now: screenshotNow, sidebarHeight: 640))))
            }
            // The finding card's foot by day and by dusk, with three
            // attachments and two attachment claims so both counts have
            // something to count, Evidence and Identifiers open and
            // Recorded Context closed.
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
            let framedSuite = "FreesideScreenshotFramedFootPreferences-\(UUID().uuidString)"
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
                        name: "decision-finding_adjudication-foot-attachments\(suffix)",
                        colorScheme: colorScheme,
                        view: AnyView(
                            framedDetail.screenshotCard(framed, at: dynamicTypeSize))))
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
            let incompatible = InboxStore.Freshness.incompatible(
                .appBelowFloor(floor: 4, daemonVersion: "1.0.0"))
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
                                    operation: nil, reason: { _ in nil }, onOpenSystemHealth: nil,
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
                                onOpenSystemHealth: { _ in },
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
                                onOpenSystemHealth: { _ in })
                            FreshnessBanner(freshness: .unauthenticated, onRePair: {})
                            FreshnessBanner(freshness: incompatible)
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
            // The readers at two narrow widths, and the diff with its later
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
                            needsYou: .init(rows: 6),
                            actions: panelActions
                        )
                        .screenshotHovering(.showNeedsYou)
                        .screenshotHighlighting(.control))))
            surfaces.append(contentsOf: try pendingStopsSurfaces())
            // A finding whose message carries Markdown, on the dispute card
            // and the adjudication card: the title, then a code span and a
            // list as formatted blocks (#2060). Drawn last, for the reason
            // given above (#1698).
            let markdownDispute = AttentionFixtures.markdownReviewDispute()
            let markdownDisputeStore = InboxStore(
                client: APIClientFactory.mock(server: MockServer(items: [markdownDispute])))
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
                                label: "Stop Unconfirmed · No New Work · Inspect the Recorded Outcome",
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

        /// The task page in each state the 30 Oct canvas draws, at Mac and
        /// phone width and in both appearances: a card leading alone
        /// (bands 3, 4), with others under it (band 3), working and finished
        /// (band 5), and with the outline row pinned (band 3).
        private func taskPageSurfaces(at dynamicTypeSize: DynamicTypeSize) throws -> [Surface] {
            let lines = [
                TaskStageFixtures.line(.implementer, agent: "claude", pool: "A"),
                TaskStageFixtures.line(.reviewer, agent: "codex", pool: nil),
            ]
            let question = TaskStageFixtures.item(.agent_question, id: "page-question", hoursOld: 0)
            let approval = TaskStageFixtures.item(.spec_approval, id: "page-approval", hoursOld: 2)
            let adjudication = TaskStageFixtures.item(.finding_adjudication, id: "page-adjudication", hoursOld: 6)

            var open = TaskStageFixtures.page(.review)
            open.task.task_lines = lines
            open.items = [question]
            var several = open
            several.items = [question, approval, adjudication]
            // A typed task, so the documents line leads with Prompt (#2612).
            var working = TaskStageFixtures.page(.implementation)
            working.task.source = .init(
                value1: .work_item_artifact(
                    .init(
                        kind: .work_item_artifact, work_item_artifact_id: "artifact-working",
                        work_item_artifact_digest: .init(value1: "sha256:" + String(repeating: "e", count: 64)))))
            working.task.task_lines = lines
            var finished = TaskStageFixtures.page(.review)
            finished.task.task_lines = lines
            finished.task.lifecycle = .finished
            finished.task.wip = false
            finished.runs[0].run.lifecycle = .finished
            finished.runs[0].run.outcome = .published
            finished.runs[0].run.completion = .init(
                value1: .init(
                    pr_number: 2431, merge_commit_sha: "4be1d0a7",
                    recorded_at: Date(timeIntervalSince1970: 1_786_500_000)))

            // The open page beside a second task for its issue, created a
            // day earlier, as the Wave 8 exit run found (#1922). The page's
            // task is created when its timeline says, since its header
            // now prints that instant.
            var lookAlike = open
            lookAlike.task.created_at = try #require(
                open.history?.events.first { $0.kind == .task_created }?.recorded_at)
            var twin = TaskFixtures.approvedCampaign().task
            twin.task = lookAlike.task
            twin.task.id = "task-\(RunFixtures.readyRunID)"
            twin.task.created_at = lookAlike.task.created_at.addingTimeInterval(-26 * 3600)

            var surfaces: [Surface] = []
            // `task-page-open-identifiers` opens the page's Identifiers
            // footer, which holds the seated card's identifiers as a
            // Decision group after the task's (#2607). `task-page-look-alike`
            // is the open page beside its twin, so the header draws the
            // look-alike marker over the card.
            for (name, page, leading, pinned, identifiersOpen, others) in [
                ("task-page-open", open, Optional(question), false, false, []),
                ("task-page-open-identifiers", open, Optional(question), false, true, []),
                ("task-page-look-alike", lookAlike, Optional(question), false, false, [twin]),
                ("task-page-several", several, Optional(question), false, false, []),
                ("task-page-working", working, nil, false, false, []),
                ("task-page-finished", finished, nil, false, false, []),
                ("task-page-pinned", several, Optional(question), true, false, []),
            ] {
                let (coordinator, task) = try TaskStageFixtures.coordinator(
                    page, conversations: AttentionFixtures.defaultConversations(), otherTasks: others)
                let timeline = try #require(page.history)
                let disclosures = TaskTimelineDisclosurePreferences(defaults: nil)
                disclosures.setExpanded(identifiersOpen, .identifiers, taskID: timeline.task_id)
                let view = TaskPageView(coordinator: coordinator, snapshot: task, onOpenRun: { _ in })
                let card = DecisionDetailView(
                    store: coordinator.store, itemID: question.item.id, loadsAttachments: false,
                    showsValidationProgress: false)
                for scheme in [ColorScheme.light, .dark] {
                    let suffix = scheme == .dark ? "-dark" : ""
                    for (width, compact, tail) in [(CGFloat?.none, false, ""), (390, true, "-390")] {
                        surfaces.append(
                            Surface(
                                name: name + tail + suffix, width: width, colorScheme: scheme,
                                nativeAppearance: true,
                                view: AnyView(
                                    view.screenshotContent(
                                        timeline, disclosures: disclosures, leadingItemID: leading?.item.id,
                                        leadingCard: leading.map {
                                            AnyView(
                                                card.screenshotPageCard(
                                                    $0.item, at: dynamicTypeSize, compactLayout: compact))
                                        },
                                        decisionIdentifiers: leading.map {
                                            DecisionDetailView.identifierRows(
                                                $0.item, proposalFacts: nil, effectProposalFacts: nil)
                                        } ?? [],
                                        pinned: pinned))))
                    }
                }
            }
            // Earlier Conversations (#3041): a finished task with no open
            // item and two closed items' threads, folded under the
            // Timeline newest first, the newer open and the older closed.
            var approved = approval
            approved.item.status = .resolved
            var answered = TaskStageFixtures.item(.agent_question, id: "page-answered", hoursOld: 1)
            answered.item.status = .superseded
            answered.item.conversation_id = "conv-page-answered"
            var answeredThread = try #require(AttentionFixtures.defaultConversations().first)
            answeredThread.conversation.id = "conv-page-answered"
            answeredThread.conversation.messages = zip(
                answeredThread.conversation.messages,
                [
                    "Should the migration pause between projects?",
                    "Yes. Each project finishes and verifies before the next one starts.",
                ]
            ).map { message, body in
                var message = message
                message.id = "answered-" + message.id
                message.conversation_id = "conv-page-answered"
                message.body = body
                return message
            }
            var earlier = finished
            earlier.items = [answered, approved]
            let (earlierCoordinator, earlierTask) = try TaskStageFixtures.coordinator(
                earlier, conversations: AttentionFixtures.defaultConversations() + [answeredThread])
            let earlierTimeline = try #require(earlier.history)
            let earlierFolds = TaskTimelineDisclosurePreferences(defaults: nil)
            earlierFolds.setExpanded(true, .conversation("conv-page-answered"), taskID: earlierTimeline.task_id)
            let earlierView = TaskPageView(coordinator: earlierCoordinator, snapshot: earlierTask, onOpenRun: { _ in })
            for scheme in [ColorScheme.light, .dark] {
                let suffix = scheme == .dark ? "-dark" : ""
                for (width, tail) in [(CGFloat?.none, ""), (390, "-390")] {
                    surfaces.append(
                        Surface(
                            name: "task-page-earlier-conversations" + tail + suffix, width: width,
                            colorScheme: scheme, nativeAppearance: true,
                            view: AnyView(earlierView.screenshotContent(earlierTimeline, disclosures: earlierFolds))))
                }
            }
            return surfaces
        }

        /// The specification reader the task page's documents line opens:
        /// the approved bytes, and the unavailable state that names the
        /// digest when the daemon has none.
        private func taskSpecificationReaderSurfaces() -> [Surface] {
            let digest = "sha256:" + String(repeating: "c", count: 64)
            let loader = AttachmentLoader(client: APIClientFactory.mock(server: MockServer(attachments: [:])))
            let bytes = Data(
                """
                # Specification

                Store first, then API. Existing rows migrate before any client can read them.

                ## Acceptance

                - The API keeps the old shape for one release.
                """.utf8)
            var surfaces: [Surface] = []
            for (name, version, content) in [
                ("task-specification-reader", 2, TaskDocumentReaderSheet.Content.loaded(bytes)),
                ("task-specification-reader-unavailable", nil, .unavailable),
            ] {
                let sheet = TaskDocumentReaderSheet(
                    request: .init(digest: digest, document: .specification(version: version)), attachments: loader,
                    content: content)
                for scheme in [ColorScheme.light, .dark] {
                    let suffix = scheme == .dark ? "-dark" : ""
                    for (width, tail) in [(CGFloat(720), ""), (390, "-phone")] {
                        surfaces.append(
                            Surface(
                                name: name + tail + suffix, width: width, colorScheme: scheme,
                                view: AnyView(sheet.screenshotContent)))
                    }
                }
            }
            return surfaces
        }

        /// The prompt reader a typed task's documents line opens (#2612):
        /// the submitted text in the plain card, and the unavailable state
        /// that names the digest when the daemon has none.
        private func taskPromptReaderSurfaces() -> [Surface] {
            let digest = "sha256:" + String(repeating: "e", count: 64)
            let loader = AttachmentLoader(client: APIClientFactory.mock(server: MockServer(attachments: [:])))
            let bytes = Data(
                """
                Add a health endpoint to the daemon's API so the app can tell a stopped daemon from a slow one.

                Keep the old status route for one release, and say so in the README.
                """.utf8)
            var surfaces: [Surface] = []
            for (name, content) in [
                ("task-prompt-reader", TaskDocumentReaderSheet.Content.loaded(bytes)),
                ("task-prompt-reader-unavailable", .unavailable),
            ] {
                let sheet = TaskDocumentReaderSheet(
                    request: .init(digest: digest, document: .prompt), attachments: loader, content: content)
                for scheme in [ColorScheme.light, .dark] {
                    let suffix = scheme == .dark ? "-dark" : ""
                    for (width, tail) in [(CGFloat(720), ""), (390, "-phone")] {
                        surfaces.append(
                            Surface(
                                name: name + tail + suffix, width: width, colorScheme: scheme,
                                view: AnyView(sheet.screenshotContent)))
                    }
                }
            }
            return surfaces
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
                                    // The menu as the outline row draws it,
                                    // so each state shows its enablement.
                                    TaskActionsMenu(
                                        coordinator: coordinator, taskID: snapshot.task.id,
                                        confirmation: .constant(nil), lineChange: .constant(nil),
                                        rendersInteractiveControls: false)
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

        /// One role's agent picker (#3027), drawn directly: an enrollment
        /// chosen, at the phone's width and the Mac popover's, light and
        /// dark; the default chosen, its model and effort read-only; an
        /// enrollment a sync dropped while the picker was open, kept in its
        /// slot; and a running task's change with `Now` and `New`.
        private func rolePickerSurfaces() -> [Surface] {
            let catalog = UsageFixtures.defaultAgentCatalog()
            let policy = UsageFixtures.defaultRoutingPolicy()
            let pools = UsageFixtures.defaultUsagePools()
            let now = Date(timeIntervalSince1970: 1_786_502_400 + 3_000)
            let mac = UsageFixtures.readingCollectorID
            var chosen = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
            chosen.pick(.enrollment(mac), catalog: catalog)
            chosen.pick(.effort("max"), catalog: catalog)
            let lineup = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
            var dropped = catalog
            dropped.enrollments.removeAll { $0.enrollment_id == UsageFixtures.readingSecondEnrollmentID }
            var change = RolePicker.change(
                role: .specifier, current: UsageFixtures.agentSelection("claude-primary"), byName: nil,
                catalog: catalog)
            change.pick(.enrollment(UsageFixtures.unsupportedCollectorID), catalog: catalog)
            change.pick(.effort("medium"), catalog: catalog)
            let phone = CGFloat(390)
            let popover = RolePickerLayout.popoverWidth
            let cases: [(String, CGFloat, ColorScheme, RolePicker, AgentChoiceDisplay.Catalog)] = [
                ("role-picker-phone", phone, .light, chosen, catalog),
                ("role-picker-phone-dark", phone, .dark, chosen, catalog),
                ("role-picker-popover", popover, .light, chosen, catalog),
                ("role-picker-popover-dark", popover, .dark, chosen, catalog),
                ("role-picker-phone-default", phone, .light, lineup, catalog),
                ("role-picker-popover-dropped", popover, .light, chosen, dropped),
                ("role-picker-phone-change", phone, .light, change, catalog),
                ("role-picker-popover-change-dark", popover, .dark, change, catalog),
            ]
            return cases.map { name, width, scheme, picker, live in
                Surface(
                    name: name, width: width, colorScheme: scheme,
                    view: AnyView(
                        RolePickerView(
                            picker: picker, catalog: live, policy: policy, pools: pools, now: now,
                            onConfirm: { _ in }, onCancel: {}
                        ).content))
            }
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
            let store = InboxStore(client: APIClientFactory.mock(server: server))
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
                .task_proposal, .effect_proposal, .system_health, .blocked, .unknown:
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
        /// The current macOS 27 runner passes without one; a local run
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
