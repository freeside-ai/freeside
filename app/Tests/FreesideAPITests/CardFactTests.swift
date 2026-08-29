import Foundation
import Testing

@testable import FreesideAPI

@Suite struct CardFactTests {
    @Test func fixturesCarryEveryCardFactAndRoundTripNullablePayloads() throws {
        let fixtures = AttentionFixtures.defaultInbox().map(\.item)

        #expect(fixtures.allSatisfy { $0.display_names != nil })
        #expect(
            AttentionFixtures.fixture(type: .review_diminishing_returns).item
                .billable_cost_so_far?.value1.amount == "42.75")
        #expect(
            AttentionFixtures.fixture(type: .execution_failure).item
                .execution_failure?.value1.stage == .implementation)
        #expect(
            AttentionFixtures.fixture(type: .publish_blocked).item
                .publish_block?.value1.trust_rule?.value1 == .trust_profile_drift)
        #expect(
            AttentionFixtures.fixture(type: .ready_for_final_review).item
                .diff_stats?.value1.head_sha == "cafebabe")
        #expect(
            AttentionFixtures.fixture(type: .blocked).item
                .blocked_on?.value1.kind == .spec_approval)
        #expect(
            AttentionFixtures.fixture(type: .system_health).item
                .health_diagnostic?.value1.impairs == .run_visibility)
        #expect(
            AttentionFixtures.fixture(type: .review_dispute).item
                .review_dispute?.value1.finding_ids == ["finding-1", "finding-2"])

        var legacy = AttentionFixtures.fixture(type: .execution_failure).item
        legacy.display_names = nil
        legacy.execution_failure = nil

        let encoder = JSONEncoder()
        let decoder = JSONDecoder()
        let decoded = try decoder.decode(
            Components.Schemas.AttentionItem.self, from: encoder.encode(legacy))

        #expect(decoded.display_names == nil)
        #expect(decoded.execution_failure == nil)
    }

    @Test func validationMirrorsCardFactTypeAndValueRules() {
        var emptyName = AttentionFixtures.fixture(type: .spec_approval).item
        emptyName.display_names?.value1.project.text = ""
        #expect(MockContractValidation.itemValidityBreach(emptyName) == "empty display name")

        var invalidCost = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        invalidCost.billable_cost_so_far?.value1.amount = "01.00"
        #expect(
            MockContractValidation.itemValidityBreach(invalidCost)
                == "invalid billable_cost_so_far")

        var emptyInvocation = AttentionFixtures.fixture(type: .execution_failure).item
        emptyInvocation.execution_failure?.value1.invocation_id = ""
        #expect(
            MockContractValidation.itemValidityBreach(emptyInvocation)
                == "empty execution_failure invocation_id")

        var ambiguousBlock = AttentionFixtures.fixture(type: .publish_blocked).item
        ambiguousBlock.publish_block?.value1.hold_reason = .init(value1: .trust_blocked)
        #expect(
            MockContractValidation.itemValidityBreach(ambiguousBlock)
                == "publish_block does not have exactly one variant")

        var negativeDiff = AttentionFixtures.fixture(type: .ready_for_final_review).item
        negativeDiff.diff_stats?.value1.additions = -1
        #expect(MockContractValidation.itemValidityBreach(negativeDiff) == "invalid diff_stats")

        var mismatchedWait = AttentionFixtures.fixture(type: .blocked).item
        mismatchedWait.blocked_on?.value1.kind = .pr_checks
        #expect(
            MockContractValidation.itemValidityBreach(mismatchedWait)
                == "blocked_on reference disagrees with its kind")

        var invalidDiagnostic = AttentionFixtures.fixture(type: .system_health).item
        invalidDiagnostic.health_diagnostic?.value1.code = "Run Projection"
        #expect(
            MockContractValidation.itemValidityBreach(invalidDiagnostic)
                == "invalid health_diagnostic code")

        var duplicateFinding = AttentionFixtures.fixture(type: .review_dispute).item
        duplicateFinding.review_dispute?.value1.finding_ids = ["finding-1", "finding-1"]
        #expect(
            MockContractValidation.itemValidityBreach(duplicateFinding)
                == "invalid review_dispute binding")
    }

    @Test func validationMirrorsCardFactTypeGatesAndCrossChecks() {
        var wrongCostType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongCostType.billable_cost_so_far =
            AttentionFixtures.fixture(type: .review_diminishing_returns).item.billable_cost_so_far
        #expect(
            MockContractValidation.itemValidityBreach(wrongCostType)
                == "billable_cost_so_far on a different item type")

        var wrongFailureType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongFailureType.execution_failure =
            AttentionFixtures.fixture(type: .execution_failure).item.execution_failure
        #expect(
            MockContractValidation.itemValidityBreach(wrongFailureType)
                == "execution_failure facts on a different item type")

        var missingRunID = AttentionFixtures.fixture(type: .execution_failure).item
        if case .run(var subject) = missingRunID.subject {
            subject.run_id = nil
            missingRunID.subject = .run(subject)
        }
        #expect(
            MockContractValidation.itemValidityBreach(missingRunID)
                == "execution_failure facts on a non-run subject")

        var wrongBlockType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongBlockType.publish_block =
            AttentionFixtures.fixture(type: .publish_blocked).item.publish_block
        #expect(
            MockContractValidation.itemValidityBreach(wrongBlockType)
                == "publish_block facts on a different item type")

        var emptyBlock = AttentionFixtures.fixture(type: .publish_blocked).item
        emptyBlock.publish_block?.value1.trust_rule = nil
        #expect(
            MockContractValidation.itemValidityBreach(emptyBlock)
                == "publish_block does not have exactly one variant")

        var wrongDiffType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongDiffType.diff_stats =
            AttentionFixtures.fixture(type: .ready_for_final_review).item.diff_stats
        #expect(
            MockContractValidation.itemValidityBreach(wrongDiffType)
                == "diff_stats on a different item type")

        var wrongDiffHead = AttentionFixtures.fixture(type: .ready_for_final_review).item
        wrongDiffHead.diff_stats?.value1.head_sha = "other-head"
        #expect(
            MockContractValidation.itemValidityBreach(wrongDiffHead)
                == "diff_stats head disagrees with item head")

        var wrongWaitType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongWaitType.blocked_on = AttentionFixtures.fixture(type: .blocked).item.blocked_on
        #expect(
            MockContractValidation.itemValidityBreach(wrongWaitType)
                == "blocked_on facts on a different item type")

        var zeroWait = AttentionFixtures.fixture(type: .blocked).item
        zeroWait.blocked_on?.value1.since = daemonZeroInstant
        #expect(
            MockContractValidation.itemValidityBreach(zeroWait)
                == "blocked_on has an unset since")

        var futureWait = AttentionFixtures.fixture(type: .blocked).item
        futureWait.blocked_on?.value1.since = AttentionFixtures.createdInstant.addingTimeInterval(1)
        #expect(
            MockContractValidation.itemValidityBreach(futureWait)
                == "blocked_on starts after item creation")

        var wrongHealthType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongHealthType.health_diagnostic =
            AttentionFixtures.fixture(type: .system_health).item.health_diagnostic
        #expect(
            MockContractValidation.itemValidityBreach(wrongHealthType)
                == "health_diagnostic on a different item type")

        var wrongDisputeType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongDisputeType.review_dispute =
            AttentionFixtures.fixture(type: .review_dispute).item.review_dispute
        #expect(
            MockContractValidation.itemValidityBreach(wrongDisputeType)
                == "review_dispute binding on a different item type")

        var wrongDisputeRun = AttentionFixtures.fixture(type: .review_dispute).item
        wrongDisputeRun.review_dispute?.value1.run_id = "run-other"
        #expect(
            MockContractValidation.itemValidityBreach(wrongDisputeRun)
                == "review_dispute binding disagrees with item subject")
    }

    @Test func runSnapshotRejectsEmptyDisplayNames() {
        var snapshot = RunFixtures.defaultRuns().first {
            $0.run.id == RunFixtures.activeRunID
        }!
        snapshot.run.display_names = .init(
            value1: .init(
                project: .init(text: "owner/repo", source: .name),
                work_unit: .init(text: "", source: .identifier)))

        #expect(
            MockContractValidation.runSnapshotBreach(snapshot, serverRevision: 12)
                == "empty display name")
    }

    // swift-format-ignore: NeverForceUnwrap
    private var daemonZeroInstant: Date {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }
}
