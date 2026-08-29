import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct AttentionDisplayTests {
    @Test func everyAttentionTypeHasAOneSentenceQuestionAsk() {
        for type in AttentionFixtures.phase1Types {
            let ask = AttentionDisplay.ask(AttentionFixtures.fixture(type: type).item)
            #expect(!ask.isEmpty)
            #expect(ask.hasSuffix("?"))
            #expect(ask.dropLast().contains("?") == false)
        }
    }

    @Test func askNeverUsesTheItemsFreeTextReason() {
        var item = AttentionFixtures.fixture(type: .execution_failure).item
        item.reason = "free text that must not become the ask"

        #expect(AttentionDisplay.ask(item) == "How should this failed execution continue?")
    }

    @Test func rowSummariesAreSentenceCasedTypeSpecificAndNotFreeTextReasons() {
        for type in AttentionFixtures.phase1Types {
            let item = AttentionFixtures.fixture(type: type).item
            let summary = AttentionDisplay.rowSummary(item)

            #expect(!summary.isEmpty)
            #expect(summary.first?.isUppercase == true)
            #expect(summary.hasSuffix("."))
            #expect(summary != item.reason)
        }
        #expect(
            AttentionDisplay.rowSummary(
                AttentionFixtures.fixture(type: .finding_adjudication).item)
                == "Review round 3 has findings to adjudicate.")
        #expect(
            AttentionDisplay.rowSummary(AttentionFixtures.degradedReady().item)
                == "Verification is degraded and needs final review.")

        var legacyReady = AttentionFixtures.fixture(type: .ready_for_final_review).item
        legacyReady.readiness = nil
        #expect(
            AttentionDisplay.rowSummary(legacyReady)
                == "Verification status is unavailable; final review is requested.")
    }

    @Test func mechanicalRowSummariesPreserveDistinctDaemonFacts() {
        var systemHealth = AttentionFixtures.fixture(type: .system_health).item
        systemHealth.reason = "Doctor check backup_age is unhealthy: checkpoint is stale"
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Doctor check backup_age is unhealthy: checkpoint is stale.")

        systemHealth.reason = "credential integrity probe failed."
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Credential integrity probe failed.")

        var blocked = AttentionFixtures.fixture(type: .blocked).item
        blocked.reason = "waiting on external reviewer"
        #expect(AttentionDisplay.rowSummary(blocked) == "Waiting on external reviewer.")

        blocked.reason = "the run has waited 18h on an external reviewer"
        let firstSummary = AttentionDisplay.rowSummary(blocked)
        blocked.reason = "the run has waited 2d on an external reviewer"
        #expect(firstSummary == "The run is waiting on an external reviewer.")
        #expect(AttentionDisplay.rowSummary(blocked) == firstSummary)
    }

    @Test func concludedRowSummariesAreNeutralAcrossEveryTypeAndStatus() {
        let statuses: [Components.Schemas.ItemStatus] = [
            .resolved, .superseded, .dismissed, .expired,
        ]
        for type in AttentionFixtures.phase1Types {
            guard type != .system_health else { continue }
            for status in statuses {
                var item = AttentionFixtures.fixture(type: type).item
                item.status = status

                #expect(
                    AttentionDisplay.rowSummary(item)
                        == "This \(AttentionDisplay.title(type).lowercased()) item is "
                        + "\(AttentionDisplay.label(status).lowercased()).")
            }
        }

        var firstHealth = AttentionFixtures.fixture(type: .system_health).item
        firstHealth.status = .resolved
        firstHealth.reason = "Doctor check backup_age is unhealthy: checkpoint is stale"
        var secondHealth = firstHealth
        secondHealth.reason = "Credential integrity probe failed."
        #expect(
            AttentionDisplay.rowSummary(firstHealth)
                == "Resolved: Doctor check backup_age is unhealthy: checkpoint is stale.")
        #expect(
            AttentionDisplay.rowSummary(secondHealth)
                == "Resolved: Credential integrity probe failed.")
    }

    @Test func relativeRowTimesUseCoarseUnitsBlockedWordingAndDeadlinePrecedence() {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        var ordinary = AttentionFixtures.fixture(type: .spec_approval).item
        ordinary.created_at = now.addingTimeInterval(-59 * 60)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "59m")

        ordinary.created_at = now.addingTimeInterval(-18 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "18h")

        ordinary.created_at = now.addingTimeInterval(-3 * 86_400)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "3d")

        var blocked = AttentionFixtures.fixture(type: .blocked).item
        blocked.created_at = now.addingTimeInterval(-18 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "blocked 18h")

        let actualWaitStart = now.addingTimeInterval(-2 * 86_400)
        blocked.created_at = now.addingTimeInterval(-60)
        blocked.reason =
            "Specification approval has been waiting since "
            + "\(actualWaitStart.formatted(.iso8601))."
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "blocked 2d")
        #expect(
            AttentionDisplay.exactRowTimestamp(blocked, now: now)
                == actualWaitStart.formatted(.iso8601))

        blocked.expires_when = now.addingTimeInterval(2 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "due in 2h")
        #expect(
            AttentionDisplay.exactRowTimestamp(blocked, now: now)
                == blocked.expires_when?.formatted(.iso8601))

        blocked.expires_when = now.addingTimeInterval(30)
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "due now")

        blocked.expires_when = now.addingTimeInterval(-30)
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "due now")

        blocked.expires_when = now.addingTimeInterval(-2 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "overdue 2h")
        #expect(
            AttentionDisplay.exactRowTimestamp(blocked, now: now)
                == blocked.expires_when?.formatted(.iso8601))

        blocked.created_at = now.addingTimeInterval(-18 * 3_600)
        blocked.status = .resolved
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "18h")
        #expect(
            AttentionDisplay.exactRowTimestamp(blocked, now: now)
                == blocked.created_at?.formatted(.iso8601))

        ordinary.created_at = nil
        ordinary.expires_when = nil
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == nil)
    }

    @Test func relativeRowTimeFormatsBareDatesWithTheSharedUnits() {
        let now = Date(timeIntervalSince1970: 200_000)
        #expect(
            AttentionDisplay.relativeRowTime(
                now.addingTimeInterval(-86_400), now: now) == "1d")
    }

    @Test func rowContextPrefersNamesAndFallsBackToIdentifiers() {
        let run = AttentionFixtures.fixture(type: .execution_failure).item

        #expect(
            AttentionDisplay.rowContext(run)
                == .init(
                    project: .init(value: "proj-1", isIdentifier: true),
                    workUnit: .init(value: "run-execution_failure", isIdentifier: true)
                ))
        #expect(
            AttentionDisplay.rowContext(
                run, projectName: "Freeside", workUnitName: "Inbox scanning")
                == .init(
                    project: .init(value: "Freeside", isIdentifier: false),
                    workUnit: .init(value: "Inbox scanning", isIdentifier: false)
                ))

        let system = AttentionFixtures.fixture(type: .system_health).item
        #expect(AttentionDisplay.rowContext(system).workUnit == nil)
    }

    @Test func copyableSubjectReferencesMatchTheirContractSubject() {
        let run = AttentionFixtures.fixture(type: .execution_failure).item
        #expect(
            AttentionDisplay.copyableSubjectReference(run)
                == .init(label: "Copy run reference", value: "run-execution_failure"))

        let proposal = AttentionFixtures.fixture(type: .run_proposal).item
        #expect(
            AttentionDisplay.copyableSubjectReference(proposal)
                == .init(label: "Copy proposal batch reference", value: "batch-run_proposal"))

        let system = AttentionFixtures.fixture(type: .system_health).item
        #expect(AttentionDisplay.copyableSubjectReference(system) == nil)
    }

    @Test func rowBadgePolicyKeepsOnlyExceptionalStates() {
        #expect(AttentionDisplay.showsPriorityBadge(.urgent))
        #expect(AttentionDisplay.showsPriorityBadge(.high))
        #expect(!AttentionDisplay.showsPriorityBadge(.normal))
        #expect(!AttentionDisplay.showsPriorityBadge(.low))

        #expect(!AttentionDisplay.showsLifecycleBadge(.open))
        #expect(AttentionDisplay.showsLifecycleBadge(.resolved))
        #expect(AttentionDisplay.showsLifecycleBadge(.superseded))
        #expect(AttentionDisplay.showsLifecycleBadge(.dismissed))
        #expect(AttentionDisplay.showsLifecycleBadge(.expired))

        #expect(
            !AttentionDisplay.showsDegradedBadge(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))
        let degraded = AttentionFixtures.degradedReady().item
        #expect(AttentionDisplay.showsDegradedBadge(degraded))
        #expect(AttentionDisplay.title(degraded._type) == "Ready for final review")
    }

    @Test func existingPullRequestActionUsesViewLanguage() {
        #expect(AttentionDisplay.label(Components.Schemas.Action.open_pr) == "View PR")
    }

    @Test func stopConfirmationsDescribeTheItemsActualOutcome() {
        let finding = AttentionFixtures.fixture(type: .finding_adjudication).item
        let configuration = AttentionFixtures.fixture(type: .review_configuration).item
        let failure = AttentionFixtures.fixture(type: .execution_failure).item

        #expect(
            AttentionDisplay.confirmationConsequence(.stop, for: finding)
                == "The run stays parked without accepting or choosing an adjudication route.")
        #expect(
            AttentionDisplay.confirmationConsequence(.stop, for: configuration)
                == "The run concludes as a configuration failure; no replacement review configuration is adopted.")
        #expect(
            AttentionDisplay.confirmationConsequence(.stop, for: failure)
                == "The current invocation is discarded. Work already exported stays; the round in flight does not.")
    }

    @Test func healthPostureLabelsAreExplicit() {
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.blocking) == "Blocking")
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.advisory) == "Advisory")
    }

    @Test func adjudicationConfidenceLabelsAreExplicit() {
        #expect(AttentionDisplay.label(Components.Schemas.AdjudicationConfidence.low) == "Low")
        #expect(AttentionDisplay.label(Components.Schemas.AdjudicationConfidence.medium) == "Medium")
        #expect(AttentionDisplay.label(Components.Schemas.AdjudicationConfidence.high) == "High")
    }

    @Test func findingLocationRendersLikeTheDaemonCanonicalString() {
        #expect(
            AttentionDisplay.findingLocation(.init(path: "daemon/a.go", start_line: 0, end_line: 0))
                == "daemon/a.go")
        #expect(
            AttentionDisplay.findingLocation(.init(path: "daemon/a.go", start_line: 12, end_line: 12))
                == "daemon/a.go:12")
        #expect(
            AttentionDisplay.findingLocation(.init(path: "daemon/a.go", start_line: 12, end_line: 18))
                == "daemon/a.go:12-18")
    }

    @Test func attachmentDigestsKeepTheirEvidenceAndClaimContext() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item

        let rows = AttentionDisplay.attachmentDigestRows(item)

        #expect(rows.count == item.artifact_digests.count)
        #expect(rows.first?.label == "Evidence digest")
        #expect(rows.dropFirst().allSatisfy { $0.label == "Claim digest" })
        #expect(Set(rows.map(\.value)) == Set(item.artifact_digests))
    }

    @Test func sharedAttachmentDigestKeepsBothTrustChannelLabels() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let digest = item.evidence_snapshot[0].digest
        item.agent_claims[0].digest = digest
        item.artifact_digests = Array(Set(item.agent_claims.map(\.digest) + [digest])).sorted()

        let rows = AttentionDisplay.attachmentDigestRows(item)

        #expect(rows.contains(.init(label: "Evidence digest", value: digest)))
        #expect(rows.contains(.init(label: "Claim digest", value: digest)))
    }

    @Test func contextMenuEvidenceDigestsAreUniqueAndStable() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let first = item.evidence_snapshot[0]
        item.evidence_snapshot.append(first)

        let digests = AttentionDisplay.uniqueEvidenceDigests(item)

        #expect(digests == item.evidence_snapshot.dropLast().map(\.digest))
    }

    @Test func detailBindingsKeepDistinctLabelsThatShareAValue() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        let rows = AttentionDisplay.detailBindingRows(item)

        #expect(rows.contains(.init(label: "PR head", value: "cafebabe")))
        #expect(rows.contains(.init(label: "Head", value: "cafebabe")))
    }

    @Test func detailBindingsExposeExactCreatedAndDueTimestamps() {
        var item = AttentionFixtures.fixture(type: .blocked).item
        item.expires_when = item.created_at?.addingTimeInterval(7_200)

        let rows = AttentionDisplay.detailBindingRows(item)

        #expect(rows.contains(.init(label: "Created", value: "2026-01-02T03:04:05Z")))
        #expect(rows.contains(.init(label: "Due", value: "2026-01-02T05:04:05Z")))
    }

    @Test func detailBindingsPreserveUnscopedSubjectIdentifiers() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        let rows = AttentionDisplay.detailBindingRows(item)

        #expect(rows.contains(.init(label: "Subject", value: "system")))
    }

    @Test func proposalBindingSurvivesMatchingAttachmentDigest() {
        let item = AttentionFixtures.fixture(type: .run_proposal).item
        let digest = item.artifact_digests[0]

        let rows = AttentionDisplay.detailBindingRows(item, proposalDigest: digest)

        #expect(rows.contains { $0.value == digest && $0.label.hasSuffix("digest") })
        #expect(rows.contains(.init(label: "Proposal", value: digest)))
    }

    @Test func runSubjectLeadsWithProjectAndDemotesTheRunID() {
        let item = AttentionFixtures.fixture(type: .execution_failure).item

        #expect(
            AttentionDisplay.subject(item)
                == .init(lead: "proj-1", identifier: "run-execution_failure"))
    }

    @Test func readableUnscopedSubjectRendersOnlyOnce() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        #expect(AttentionDisplay.subject(item) == .init(lead: "system", identifier: nil))
    }

    @Test func reviewRecoveryBindingRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(
            AttentionDisplay.reviewRecoveryBindingRows(item) == [
                .init(label: "Recovery run", value: "run-review_contradiction"),
                .init(label: "Invocation", value: "review-run-review_contradiction-1"),
                .init(label: "Round", value: "1"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure digest",
                    value: "sha256:failure-review_contradiction"
                ),
            ])
    }

    @Test func ordinaryItemsHaveNoReviewRecoveryBindingRows() {
        let item = AttentionFixtures.fixture(type: .review_dispute).item

        #expect(AttentionDisplay.reviewRecoveryBindingRows(item).isEmpty)
    }

    @Test func reviewConfigurationRecoveryRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .review_configuration).item

        #expect(
            AttentionDisplay.reviewConfigurationRecoveryRows(item) == [
                .init(label: "Recovery run", value: "run-review_configuration"),
                .init(label: "Invocation", value: "review-run-review_configuration-2"),
                .init(label: "Round", value: "2"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure digest",
                    value: "sha256:failure-review_configuration"
                ),
                .init(label: "Repository", value: "owner/repo"),
                .init(
                    label: "Superseded profile",
                    value: "sha256:profile-review_configuration"
                ),
            ])
    }

    @Test func ordinaryItemsHaveNoReviewConfigurationRecoveryRows() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(AttentionDisplay.reviewConfigurationRecoveryRows(item).isEmpty)
    }

    @Test func codexReenrollmentRecoveryRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        #expect(
            AttentionDisplay.codexReenrollmentRecoveryRows(item) == [
                .init(label: "Auth identity", value: "codex-primary"),
                .init(label: "Lease fence", value: "4"),
                .init(label: "Auth store digest", value: "sha256:replacement-store"),
                .init(label: "Token expires", value: "2026-08-12T02:44:05Z"),
            ])
    }

    @Test func ordinaryItemsHaveNoCodexReenrollmentRecoveryRows() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(AttentionDisplay.codexReenrollmentRecoveryRows(item).isEmpty)
    }

    @Test func findingAdjudicationRowsExposeAuthorityCoordinates() {
        let item = AttentionFixtures.fixture(type: .finding_adjudication).item

        #expect(
            AttentionDisplay.findingAdjudicationRows(item) == [
                .init(
                    label: "Adjudication digest",
                    value: "sha256:adjudication-finding_adjudication"),
                .init(label: "Adjudication run", value: "run-finding_adjudication"),
                .init(label: "Adjudication round", value: "3"),
            ])
    }

    @Test(arguments: [
        (Components.Schemas.AdjudicationProducer.engine, "Daemon recommendation", false),
        (Components.Schemas.AdjudicationProducer.model, "Model proposal (unverified)", true),
        (
            Components.Schemas.AdjudicationProducer.engine_model,
            "Model judgment with engine-authorized remediation",
            true
        ),
    ])
    func findingAdjudicationProducerLabelsDistinguishProvenance(
        producer: Components.Schemas.AdjudicationProducer,
        label: String,
        modelBacked: Bool
    ) {
        let presentation = AttentionDisplay.adjudicationProducerPresentation(producer)

        #expect(presentation.label == label)
        #expect(presentation.modelBacked == modelBacked)
    }

    @Test func readyClassesRenderDistinctlyWithTheirEvaluationSet() {
        let clean = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let degraded = AttentionFixtures.degradedReady().item

        #expect(AttentionDisplay.title(clean) == "Ready for final review")
        #expect(AttentionDisplay.title(degraded) == "Ready for final review (degraded)")
        #expect(
            AttentionDisplay.readinessSummaryRows(clean) == [
                .init(label: "Readiness", value: "Clean"),
                .init(label: "Evaluation set", value: "sha256:evaluation-clean"),
            ])
        #expect(
            AttentionDisplay.readinessSummaryRows(degraded) == [
                .init(label: "Readiness", value: "Degraded"),
                .init(label: "Evaluation set", value: "sha256:evaluation-degraded"),
            ])
    }

    @Test func readyReviewYieldRendersEveryRoundAndTerminalOutcome() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item

        #expect(
            AttentionDisplay.reviewYieldRows(item) == [
                .init(
                    label: "Review round 1",
                    value: "2 findings · 2 new · 0 recurring · 1 fixed · 0 declined · 1 deferred · Findings"),
                .init(
                    label: "Review round 2",
                    value: "2 findings · 1 new · 1 recurring · 1 fixed · 1 declined · 0 deferred · Findings"),
                .init(
                    label: "Review round 3",
                    value: "0 findings · 0 new · 0 recurring · 0 fixed · 0 declined · 0 deferred · Clean"),
                .init(label: "Terminal review", value: "Clean"),
            ])
    }

    @Test func diminishingReviewYieldRendersEveryRoundAndTerminalOutcome() {
        let item = AttentionFixtures.fixture(type: .review_diminishing_returns).item

        #expect(
            AttentionDisplay.reviewYieldRows(item) == [
                .init(
                    label: "Review round 1",
                    value: "4 findings · 4 new · 0 recurring · 2 fixed · 1 declined · 1 deferred · Findings"),
                .init(
                    label: "Review round 2",
                    value: "3 findings · 1 new · 2 recurring · 1 fixed · 1 declined · 1 deferred · Findings"),
                .init(
                    label: "Review round 3",
                    value: "3 findings · 0 new · 3 recurring · 0 fixed · 2 declined · 1 deferred · Findings"),
                .init(label: "Terminal review", value: "Findings"),
            ])
    }

    @Test func legacyReadyItemOmitsReviewYieldRows() {
        var item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        item.yield_history = nil

        #expect(AttentionDisplay.reviewYieldRows(item).isEmpty)
    }
}
