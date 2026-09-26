import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct ReviewRoundPresentationTests {
    @Test func verdictReadsStateThenOutcomeThenTheOpenCount() {
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.pending)) == "Pending")
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.running)) == "Running")
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.failed)) == "Failed")
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.completed)) == "Clean")

        var findings = RunFixtures.reviewRound(.completed, findings: true)
        #expect(ReviewRoundPresentation.verdict(findings) == "Findings · 1 open")
        // Without dispositions the chip falls back to the findings count,
        // and without that to the word alone; it never invents a number.
        findings.dispositions = nil
        #expect(ReviewRoundPresentation.verdict(findings) == "Findings · 3")
        findings.findings_count = nil
        #expect(ReviewRoundPresentation.verdict(findings) == "Findings")
        findings.outcome = nil
        #expect(ReviewRoundPresentation.verdict(findings) == "Completed")
    }

    @Test func cutIsFaintForPriorRoundsAndAttentionOnlyForOpenAdjudication() {
        let findings = RunFixtures.reviewRound(.completed, findings: true)
        let clean = RunFixtures.reviewRound(.completed)
        #expect(ReviewRoundPresentation.cut(findings, isCurrent: true, hasOpenAdjudication: true) == .attention)
        #expect(ReviewRoundPresentation.cut(findings, isCurrent: true, hasOpenAdjudication: false) == .ink)
        // An adjudication item never turns a clean or a prior round accent.
        #expect(ReviewRoundPresentation.cut(clean, isCurrent: true, hasOpenAdjudication: true) == .ink)
        #expect(ReviewRoundPresentation.cut(findings, isCurrent: false, hasOpenAdjudication: true) == .faint)
    }

    @Test func openAdjudicationIsBoundToTheRun() throws {
        var snapshot = try #require(
            AttentionFixtures.defaultInbox().first { $0.item._type == .finding_adjudication })
        guard case .run(let subject) = snapshot.item.subject else {
            Issue.record("the finding adjudication fixture has a run subject")
            return
        }
        let runID = try #require(subject.run_id)
        snapshot.item.status = .open
        #expect(ReviewRoundPresentation.hasOpenAdjudication(runID: runID, in: [snapshot]))
        #expect(!ReviewRoundPresentation.hasOpenAdjudication(runID: "run-other", in: [snapshot]))
        snapshot.item.status = .resolved
        #expect(!ReviewRoundPresentation.hasOpenAdjudication(runID: runID, in: [snapshot]))
    }

    @Test func factsSummaryIsTheBindingLineAndTheShortSource() {
        var round = RunFixtures.reviewRound(.completed)
        #expect(
            ReviewRoundPresentation.factsSummary(round) == "Head bbbbbbbb · Base aaaaaaaa · Freeside-invoked")
        round.source = .init(kind: "github")
        #expect(ReviewRoundPresentation.factsSummary(round).hasSuffix("· External · github"))
        round.source = .init(kind: "future-source")
        #expect(ReviewRoundPresentation.factsSummary(round).hasSuffix("· Unknown source · future-source"))
        #expect(ReviewRoundPresentation.bindingLine(head: "abc", base: "") == "Head abc · Base ")
    }

    @Test func priorSummaryNamesTheRoundsNewestFirst() {
        let two = [RunFixtures.reviewRound(.completed, round: 2), RunFixtures.reviewRound(.completed, round: 1)]
        #expect(
            ReviewRoundPresentation.priorSummary(two) == "Rounds 2 and 1 · clean at their bound heads · historical")
        // A single folded round leads with its title; without a proven
        // subject that title names only the head.
        #expect(
            ReviewRoundPresentation.priorSummary([two[1]])
                == "Review 1 (head bbbbbbbb) · clean at its bound head · historical")
        #expect(
            ReviewRoundPresentation.priorSummary([RunFixtures.reviewRound(.failed, round: 1)])
                == "Review 1 (head bbbbbbbb) · failed · historical")
        #expect(
            ReviewRoundPresentation.priorSummary([RemediationFixtures.findingsRound()])
                == "Review 1: implementation (head 91b3bf9c) · findings · historical")
        let mixed = [
            RunFixtures.reviewRound(.failed, round: 3),
            RunFixtures.reviewRound(.completed, round: 2, findings: true),
            RunFixtures.reviewRound(.completed, round: 1),
        ]
        #expect(
            ReviewRoundPresentation.priorSummary(mixed)
                == "Rounds 3, 2, and 1 · 1 with findings, 1 clean, 1 failed · historical")
    }

    @Test func aRoundsTimeIsItsCompletionElseItsRequest() {
        let completed = RunFixtures.reviewRound(.completed)
        #expect(ReviewRoundPresentation.time(completed) == completed.completed_at)
        let running = RunFixtures.reviewRound(.running)
        #expect(ReviewRoundPresentation.time(running) == running.requested_at)
    }

    @Test func titleSaysWhatEachRoundReviewed() {
        let rounds = [RemediationFixtures.findingsRound(), RemediationFixtures.reReview(.running)]
        #expect(
            ReviewRoundPresentation.title(rounds[0], in: rounds) == "Review 1: implementation (head 91b3bf9c)")
        #expect(
            ReviewRoundPresentation.title(rounds[1], in: rounds)
                == "Review 2: re-review after remediation of 1 finding from Review 1 (head af66c692)")
        var feedback = rounds[1]
        feedback.subject = .init(value1: .init(kind: .operator_feedback, invocation_id: "inv-feedback"))
        #expect(ReviewRoundPresentation.title(feedback, in: rounds) == "Review 2: operator feedback (head af66c692)")
        // An unproven producer makes no claim about what produced the head.
        var unproven = rounds[1]
        unproven.subject = nil
        #expect(ReviewRoundPresentation.title(unproven, in: rounds) == "Review 2 (head af66c692)")
        // Without the source round's facts the count is left out, not guessed.
        #expect(
            ReviewRoundPresentation.title(rounds[1], in: [rounds[1]])
                == "Review 2: re-review after remediation from Review 1 (head af66c692)")
    }

    @Test func reReviewLinksToTheSourceRound() {
        let rounds = [RemediationFixtures.findingsRound(), RemediationFixtures.reReview(.completed)]
        #expect(ReviewRoundPresentation.sourceRound(of: rounds[1], in: rounds)?.round == 1)
        #expect(ReviewRoundPresentation.sourceRound(of: rounds[0], in: rounds) == nil)
    }

    @Test func remediationLineCountsTheRoutedFindings() {
        #expect(
            ReviewRoundPresentation.remediationLine(RemediationFixtures.findingsRound())
                == "Sent 1 finding to remediation")
        #expect(ReviewRoundPresentation.remediationLine(RemediationFixtures.reReview(.running)) == nil)
    }

    @Test func evidenceNoteSaysNotProducedYetUntilTheRoundEnds() {
        #expect(
            ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.pending))
                == "Reviewer output not produced yet")
        #expect(
            ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.running))
                == "Reviewer output not produced yet")
        #expect(ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.completed)) == "Evidence unavailable")
        #expect(ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.failed)) == "Evidence unavailable")
        #expect(
            ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.running, availability: .unknown))
                == "Evidence unknown")
        #expect(
            ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.completed, availability: .available))
                == nil)
    }
}

/// A findings round whose adjudication started a remediation, and the
/// re-review of the remediated head, as the daemon reports them.
enum RemediationFixtures {
    static let implementer = "inv-impl"
    static let remediator = "inv-remediate-1-run"
    /// A minute after the fixture round completes, so rendered facts read
    /// in order.
    static let decidedAt = (RunFixtures.reviewRound(.completed).completed_at ?? .distantPast)
        .addingTimeInterval(60)

    static func findingsRound() -> Components.Schemas.RunReviewRound {
        var round = RunFixtures.reviewRound(.completed, round: 1, findings: true, availability: .available)
        round.head_sha = "91b3bf9c" + String(repeating: "0", count: 32)
        round.subject = .init(value1: .init(kind: .implementation, invocation_id: implementer))
        round.remediation = .init(
            value1: .init(invocation_id: remediator, finding_ids: ["finding-note"], decided_at: decidedAt))
        return round
    }

    static func reReview(_ state: Components.Schemas.ReviewProgressState) -> Components.Schemas.RunReviewRound {
        var round = RunFixtures.reviewRound(state, round: 2)
        round.head_sha = "af66c692" + String(repeating: "0", count: 32)
        round.subject = .init(value1: .init(kind: .remediation, invocation_id: remediator, remediates_round: 1))
        return round
    }

    /// Oldest first: the implementer's milestones, then the remediator's.
    static func milestones(remediatorStarted: Bool) -> [Components.Schemas.RunMilestone] {
        func milestone(
            _ kind: Components.Schemas.RunMilestoneKind, _ invocation: String, _ offset: TimeInterval
        ) -> Components.Schemas.RunMilestone {
            .init(
                run_id: "run", kind: kind, invocation_id: invocation,
                recorded_at: decidedAt.addingTimeInterval(offset))
        }
        let implementation = [
            milestone(.invocation_admitted, implementer, -600),
            milestone(.invocation_started, implementer, -540),
            milestone(.execution_export_recorded, implementer, -120),
        ]
        guard remediatorStarted else { return implementation }
        return implementation + [
            milestone(.invocation_admitted, remediator, 60),
            milestone(.invocation_started, remediator, 120),
        ]
    }
}
