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
            let colorScheme: ColorScheme
            let view: AnyView

            init(
                name: String,
                width: CGFloat? = nil,
                colorScheme: ColorScheme = .light,
                view: AnyView
            ) {
                self.name = name
                self.width = width
                self.colorScheme = colorScheme
                self.view = view
            }
        }

        private struct TextSize {
            let name: String
            let value: DynamicTypeSize
        }

        private let canvasWidth: CGFloat = 960
        private let baselineOperatingSystemKey = "macOS-26.6"
        private let screenshotNow = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
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
                    operatingSystemKey, baselineKey: baselineOperatingSystemKey)
            }
            let expected = try loadManifest(overrides: overrides)
            var actual: [String: String] = [:]

            for size in textSizes {
                try await FreesideFont.$screenshotDynamicTypeSize.withValue(size.value) {
                    for surface in try await makeSurfaces(at: size.value) {
                        let key = "\(surface.name)-\(size.name)"
                        let image = try await render(
                            surface.view,
                            at: size.value,
                            width: surface.width ?? canvasWidth,
                            colorScheme: surface.colorScheme)
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
                    ] {
                        let key = "\(supplemental.name)-\(size.name)"
                        let image = try await render(
                            supplemental.view,
                            at: size.value,
                            width: supplemental.width ?? canvasWidth,
                            colorScheme: supplemental.colorScheme)
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

            if recording {
                try writeManifest(actual)
            } else {
                #expect(actual.count == expected.count)
                #expect(
                    Set(actual.values).count > 45,
                    "The six-size matrix must exercise more than the prior two-state rendering")
            }
        }

        @Test func recordingRequiresTheDesignatedBaselineOperatingSystem() throws {
            try validateRecordingOperatingSystem(
                "macOS-26.6", baselineKey: "macOS-26.6")
            #expect(throws: ScreenshotError.self) {
                try validateRecordingOperatingSystem(
                    "macOS-26.5", baselineKey: "macOS-26.6")
            }
        }

        private func makeSurfaces(at dynamicTypeSize: DynamicTypeSize) async throws -> [Surface] {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let inbox = AttentionFixtures.defaultInbox()
            let store = InboxStore(client: client)
            store.replaceAll(with: inbox)
            store.replaceAllConversations(with: AttentionFixtures.defaultConversations())
            for snapshot in inbox {
                for digest in snapshot.item.artifact_digests {
                    await store.attachments.load(digest)
                }
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
            let feedback = DecisionFeedbackModel(
                announce: { _ in },
                schedule: { _, _ in Task {} })
            feedback.present(
                .init(
                    itemID: "item-spec_approval",
                    actionLabel: "Approve",
                    resultingStatus: .resolved,
                    at: screenshotNow),
                advancesAutomatically: false,
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
                            title: "Request changes",
                            prompt: "Describe the revision the specification needs.",
                            submitLabel: "Request changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-phone",
                    width: 390,
                    view: AnyView(
                        MessageComposerSheet(
                            title: "Request changes",
                            prompt: "Describe the revision the specification needs.",
                            submitLabel: "Request changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-dark",
                    width: 480,
                    colorScheme: .dark,
                    view: AnyView(
                        MessageComposerSheet(
                            title: "Request changes",
                            prompt: "Describe the revision the specification needs.",
                            submitLabel: "Request changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _ in true }))))
            surfaces.append(
                Surface(
                    name: "message-composer-phone-dark",
                    width: 390,
                    colorScheme: .dark,
                    view: AnyView(
                        MessageComposerSheet(
                            title: "Request changes",
                            prompt: "Describe the revision the specification needs.",
                            submitLabel: "Request changes",
                            byteLimit: 8192,
                            rendersInteractiveControls: false,
                            submit: { _ in true }))))

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
                conversationNow: screenshotNow)
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
                conversationNow: screenshotNow)
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
                conversationNow: screenshotNow)
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
                conversationNow: screenshotNow)
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

            if let selected = inbox.first?.item {
                surfaces.append(
                    Surface(
                        name: "inbox-selected-differentiate-without-color",
                        width: 560,
                        view: AnyView(
                            VStack(spacing: 8) {
                                InboxRowView(
                                    item: selected,
                                    now: screenshotNow,
                                    differentiateWithoutColorOverride: true
                                )
                                InboxRowView(
                                    item: selected,
                                    isSelected: true,
                                    now: screenshotNow,
                                    differentiateWithoutColorOverride: true
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
                                    now: screenshotNow,
                                    differentiateWithoutColorOverride: true
                                )
                                InboxRowView(
                                    item: selected,
                                    isSelected: true,
                                    now: screenshotNow,
                                    differentiateWithoutColorOverride: true
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

            for snapshot in inbox {
                let graphics = graphicPresentations(for: snapshot.item)
                let detail = DecisionDetailView(
                    store: store,
                    itemID: snapshot.item.id,
                    graphics: graphics,
                    loadsAttachments: false,
                    showsValidationProgress: false,
                    conversationNow: screenshotNow
                )
                let proposalFacts =
                    snapshot.item._type == .run_proposal
                    ? Components.Schemas.RunProposalFactsSnapshot(
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
                surfaces.append(
                    Surface(
                        name: "decision-\(snapshot.item._type.rawValue)",
                        view: AnyView(
                            detail.screenshotCard(
                                snapshot.item,
                                at: dynamicTypeSize,
                                proposalFacts: proposalFacts))))

                if snapshot.item._type == .spec_approval {
                    let phoneDetail = DecisionDetailView(
                        store: store,
                        itemID: snapshot.item.id,
                        graphics: graphics,
                        loadsAttachments: false,
                        showsValidationProgress: false,
                        conversationNow: screenshotNow)
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
                        conversationNow: screenshotNow)
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
                        conversationNow: screenshotNow)
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
                }

                if snapshot.item._type == .ready_for_final_review {
                    surfaces.append(
                        Surface(
                            name: "decision-ready_for_final_review-1200",
                            width: 1_200,
                            view: AnyView(
                                detail.screenshotCard(
                                    snapshot.item,
                                    at: dynamicTypeSize,
                                    detailWidth: 1_200))))
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

                    let preferencesSuite = "FreesideScreenshotDiminishingPreferences"
                    guard let preferencesDefaults = UserDefaults(suiteName: preferencesSuite) else {
                        throw ScreenshotError.preferencesUnavailable
                    }
                    preferencesDefaults.removePersistentDomain(forName: preferencesSuite)
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

            let adjudication = AttentionFixtures.fixture(type: .finding_adjudication).item
            let recommendedDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                recommendation: .init(
                    action: .accept_recommended_route,
                    reason: "This route preserves the evidence-backed finding.",
                    confidence: "High"
                ),
                loadsAttachments: false,
                showsValidationProgress: false
            )
            let preferencesSuite = "FreesideScreenshotInspectorPreferences"
            guard let preferencesDefaults = UserDefaults(suiteName: preferencesSuite) else {
                throw ScreenshotError.preferencesUnavailable
            }
            preferencesDefaults.removePersistentDomain(forName: preferencesSuite)
            let inspectorPreferences = DecisionSectionPreferences(defaults: preferencesDefaults)
            inspectorPreferences.claimsExpanded = true
            inspectorPreferences.evidenceExpanded = true
            inspectorPreferences.detailsExpanded = true
            let inspectorDetail = DecisionDetailView(
                store: store,
                itemID: adjudication.id,
                recommendation: .init(
                    action: .accept_recommended_route,
                    reason: "This route preserves the evidence-backed finding.",
                    confidence: "High"
                ),
                showsValidationProgress: false,
                sectionPreferences: inspectorPreferences)
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-recommended",
                    view: AnyView(
                        recommendedDetail.screenshotCard(adjudication, at: dynamicTypeSize))))
            for width in [CGFloat(900), CGFloat(1_200)] {
                surfaces.append(
                    Surface(
                        name: "decision-finding_adjudication-recommended-\(Int(width))",
                        width: width,
                        view: AnyView(
                            recommendedDetail.screenshotCard(
                                adjudication,
                                at: dynamicTypeSize,
                                detailWidth: width))))
            }
            surfaces.append(
                Surface(
                    name: "decision-finding_adjudication-inspector",
                    width: 360,
                    view: AnyView(
                        inspectorDetail.screenshotInspector(
                            adjudication,
                            at: dynamicTypeSize))))

            let question = AttentionFixtures.fixture(type: .agent_question).item
            let destructiveRecommendation = DecisionDetailView(
                store: store,
                itemID: question.id,
                recommendation: .init(
                    action: .stop,
                    reason: "Stopping avoids applying an unreviewed answer.",
                    confidence: nil
                ),
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
            surfaces.append(
                Surface(
                    name: "operational-summary",
                    width: 640,
                    view: AnyView(
                        OperationalSummaryView(
                            summary: OperationalSummary(
                                openSnapshots: store.openSnapshots,
                                runs: runs,
                                freshness: .fresh)))))
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
            await unavailableLoader.load(digest)
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
                        label: "verify_log", digest: digest, attachments: tooLargeLoader,
                        loadsAttachments: false,
                        rendersInteractiveControls: false)
                }
                .padding(14)
                .freesideCard()
                .padding())
        }

        private func makeUnavailableAttachmentSurface(
            at dynamicTypeSize: DynamicTypeSize
        ) async throws -> Surface {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let blocked = AttentionFixtures.fixture(type: .blocked)
            let store = InboxStore(client: client)
            store.replaceAll(with: [blocked])
            await store.attachments.load("sha256:img-blocked")
            #expect(store.attachments.phase(for: "sha256:img-blocked") == .unavailable)
            let detail = DecisionDetailView(
                store: store,
                itemID: blocked.item.id,
                loadsAttachments: false,
                showsValidationProgress: false)
            return Surface(
                name: "decision-blocked-unavailable",
                view: AnyView(detail.screenshotCard(blocked.item, at: dynamicTypeSize)))
        }

        private func makeImageAttachmentSurface(
            at dynamicTypeSize: DynamicTypeSize
        ) async throws -> Surface {
            let server = MockServer()
            let client = APIClientFactory.mock(server: server)
            let approval = AttentionFixtures.fixture(type: .spec_approval)
            let store = InboxStore(client: client)
            store.replaceAll(with: [approval])
            await store.attachments.load("sha256:img-spec_approval")
            guard case .image = store.attachments.phase(for: "sha256:img-spec_approval") else {
                throw ScreenshotError.missingSeededImage
            }
            let detail = DecisionDetailView(
                store: store,
                itemID: approval.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                conversationNow: screenshotNow)
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
                .run_proposal, .system_health, .blocked:
                return .init()
            }
        }

        private func render(
            _ view: AnyView,
            at size: DynamicTypeSize,
            width: CGFloat,
            colorScheme: ColorScheme
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
                .frame(width: width, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)
                .background(Color.ground)
            // ImageRenderer can transiently return an incomplete glyph raster
            // under CI load. Baselines must come from a settled frame, and an
            // unstable surface fails closed instead of blessing random pixels.
            var priorDigest: String?
            for _ in 0..<3 {
                let renderer = ImageRenderer(content: root)
                renderer.proposedSize = ProposedViewSize(width: width, height: nil)
                renderer.scale = 1
                guard let image = renderer.cgImage else {
                    throw ScreenshotError.renderFailed
                }
                let currentDigest = try digest(image)
                if currentDigest == priorDigest {
                    return image
                }
                priorDigest = currentDigest
                await Task.yield()
            }
            throw ScreenshotError.unstableRender
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
        case missingSeededImage
        case pngEncodingFailed
        case preferencesUnavailable
        case recordingRequiresBaselineOperatingSystem(expected: String, actual: String)
        case renderFailed
        case unstableRender
    }
#endif
