import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct ReviewRoundPresentationTests {
    /// A clock to place a round's request against.
    private static let now = Date(timeIntervalSince1970: 1_790_000_000)

    private static func requested(
        _ state: Components.Schemas.ReviewProgressState, minutesAgo: Double
    ) -> Components.Schemas.RunReviewRound {
        RunFixtures.reviewRound(state, requestedAt: now.addingTimeInterval(-minutesAgo * 60))
    }

    @Test func verdictReadsStateThenOutcomeThenTheOpenCount() {
        let now = Self.now
        #expect(ReviewRoundPresentation.verdict(Self.requested(.pending, minutesAgo: 12), now: now) == "Pending")
        #expect(ReviewRoundPresentation.verdict(Self.requested(.running, minutesAgo: 12), now: now) == "Running")
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.failed), now: now) == "Failed")
        #expect(ReviewRoundPresentation.verdict(RunFixtures.reviewRound(.completed), now: now) == "Clean")

        var findings = RunFixtures.reviewRound(.completed, findings: true)
        #expect(ReviewRoundPresentation.verdict(findings, now: now) == "Findings · 1 open")
        // Without dispositions the chip falls back to the findings count,
        // and without that to the word alone; it never invents a number.
        findings.dispositions = nil
        #expect(ReviewRoundPresentation.verdict(findings, now: now) == "Findings · 3")
        findings.findings_count = nil
        #expect(ReviewRoundPresentation.verdict(findings, now: now) == "Findings")
        findings.outcome = nil
        #expect(ReviewRoundPresentation.verdict(findings, now: now) == "Completed")
    }

    @Test func onlyAPendingOrRunningRoundIsInProgress() {
        #expect(ReviewRoundPresentation.isInProgress(RunFixtures.reviewRound(.pending)))
        #expect(ReviewRoundPresentation.isInProgress(RunFixtures.reviewRound(.running)))
        #expect(!ReviewRoundPresentation.isInProgress(RunFixtures.reviewRound(.completed)))
        #expect(!ReviewRoundPresentation.isInProgress(RunFixtures.reviewRound(.failed)))
    }

    @Test func aRoundInProgressShowsTheTimeSinceItsRequest() {
        let now = Self.now
        for state in [Components.Schemas.ReviewProgressState.pending, .running] {
            let round = Self.requested(state, minutesAgo: 12)
            #expect(ReviewRoundPresentation.elapsed(round, now: now) == "12m")
            // The text moves with the clock, which is what a minute's tick
            // redraws.
            #expect(ReviewRoundPresentation.elapsed(round, now: now.addingTimeInterval(60)) == "13m")
            #expect(ReviewRoundPresentation.elapsed(Self.requested(state, minutesAgo: 125), now: now) == "2h")
            // A request time the daemon did not record leaves nothing to
            // count from.
            var unrequested = round
            unrequested.requested_at = nil
            #expect(ReviewRoundPresentation.elapsed(unrequested, now: now) == nil)
            #expect(!ReviewRoundPresentation.isRunningLong(unrequested, now: now))
        }
        // A round with a result keeps its recorded instant.
        for state in [Components.Schemas.ReviewProgressState.completed, .failed] {
            #expect(ReviewRoundPresentation.elapsed(Self.requested(state, minutesAgo: 12), now: now) == nil)
        }
    }

    @Test func aRoundInProgressPastThirtyMinutesSaysSoWithoutChangingItsCut() {
        let now = Self.now
        #expect(ReviewRoundPresentation.longRunningAfter == 30 * 60)
        for (state, word) in [(Components.Schemas.ReviewProgressState.pending, "Pending"), (.running, "Running")] {
            let fresh = Self.requested(state, minutesAgo: 12)
            let atTheBound = Self.requested(state, minutesAgo: 30)
            let long = Self.requested(state, minutesAgo: 47)
            #expect(!ReviewRoundPresentation.isRunningLong(fresh, now: now))
            #expect(!ReviewRoundPresentation.isRunningLong(atTheBound, now: now))
            #expect(ReviewRoundPresentation.isRunningLong(atTheBound, now: now.addingTimeInterval(1)))
            #expect(ReviewRoundPresentation.isRunningLong(long, now: now))
            #expect(ReviewRoundPresentation.verdict(fresh, now: now) == word)
            #expect(ReviewRoundPresentation.verdict(atTheBound, now: now) == word)
            #expect(ReviewRoundPresentation.verdict(long, now: now) == "\(word) · over 30m")
            // A long round raises no Inbox item, so the chip keeps the ink
            // cut even while another item is open for the run.
            for round in [fresh, long] {
                #expect(ReviewRoundPresentation.cut(round, isCurrent: true, hasOpenAdjudication: false) == .ink)
                #expect(ReviewRoundPresentation.cut(round, isCurrent: true, hasOpenAdjudication: true) == .ink)
            }
        }
        // Only a round in progress runs long, however old its request.
        for state in [Components.Schemas.ReviewProgressState.completed, .failed] {
            #expect(!ReviewRoundPresentation.isRunningLong(Self.requested(state, minutesAgo: 47), now: now))
        }
    }

    @Test func aRoundInProgressNamesOnlyTheIdentityTheDaemonSent() {
        for state in [Components.Schemas.ReviewProgressState.pending, .running] {
            #expect(ReviewRoundPresentation.identity(RunFixtures.reviewRound(state)) == nil)
            #expect(
                ReviewRoundPresentation.identity(
                    RunFixtures.reviewRound(state, provider: "openai", modelConfiguration: "codex/high"))
                    == "openai · codex/high")
            #expect(ReviewRoundPresentation.identity(RunFixtures.reviewRound(state, provider: "openai")) == "openai")
            #expect(
                ReviewRoundPresentation.identity(RunFixtures.reviewRound(state, modelConfiguration: "codex/high"))
                    == "codex/high")
        }
        // A round with a result keeps the line, where a missing identity is
        // unknown.
        #expect(ReviewRoundPresentation.identity(RunFixtures.reviewRound(.completed)) == "openai · codex/high")
        #expect(
            ReviewRoundPresentation.identity(RunFixtures.reviewRound(.failed)) == "Reviewer unknown · Model unknown")
        var unidentified = RunFixtures.reviewRound(.completed)
        unidentified.provider = nil
        unidentified.model_configuration = nil
        #expect(ReviewRoundPresentation.identity(unidentified) == "Reviewer unknown · Model unknown")
        unidentified.provider = "openai"
        #expect(ReviewRoundPresentation.identity(unidentified) == "openai · Model unknown")
    }

    @Test func aRoundInProgressListsNoResultLines() {
        for state in [Components.Schemas.ReviewProgressState.pending, .running] {
            let round = RunFixtures.reviewRound(state)
            #expect(ReviewRoundPresentation.resultLines(round).isEmpty)
            #expect(ReviewRoundPresentation.missingCompletionLine(round) == nil)
        }
        // A round with a result keeps both lines, and says which value is
        // missing.
        #expect(
            ReviewRoundPresentation.resultLines(RunFixtures.reviewRound(.completed)) == [
                "Outcome: Clean", "0 findings",
            ])
        #expect(
            ReviewRoundPresentation.resultLines(RunFixtures.reviewRound(.completed, findings: true))
                == ["Outcome: Findings", "3 findings"])
        #expect(
            ReviewRoundPresentation.resultLines(RunFixtures.reviewRound(.failed))
                == ["Outcome unavailable", "Findings count unavailable"])
        for state in [Components.Schemas.ReviewProgressState.completed, .failed] {
            #expect(
                ReviewRoundPresentation.missingCompletionLine(RunFixtures.reviewRound(state))
                    == "Completion time unavailable")
        }
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
            ReviewRoundPresentation.priorSummary(two) == "Rounds 2 and 1 · clean at their bound heads")
        // A single folded round leads with its title, which names the
        // attempt it reviewed only when the producer is proven.
        #expect(
            ReviewRoundPresentation.priorSummary([two[1]])
                == "Review 1 · clean at its bound head")
        #expect(
            ReviewRoundPresentation.priorSummary([RunFixtures.reviewRound(.failed, round: 1)])
                == "Review 1 · failed")
        #expect(
            ReviewRoundPresentation.priorSummary(
                [RemediationFixtures.findingsRound()], stages: RemediationFixtures.stages)
                == "Review 1: Implementation · Pass 1 · Round 2 · findings")
        let mixed = [
            RunFixtures.reviewRound(.failed, round: 3),
            RunFixtures.reviewRound(.completed, round: 2, findings: true),
            RunFixtures.reviewRound(.completed, round: 1),
        ]
        #expect(
            ReviewRoundPresentation.priorSummary(mixed)
                == "Rounds 3, 2, and 1 · 1 with findings, 1 clean, 1 failed")
    }

    @Test func aRoundsTimeIsItsCompletionElseItsRequest() {
        let completed = RunFixtures.reviewRound(.completed)
        #expect(ReviewRoundPresentation.time(completed) == completed.completed_at)
        let running = RunFixtures.reviewRound(.running)
        #expect(ReviewRoundPresentation.time(running) == running.requested_at)
    }

    @Test func titleNamesTheAttemptEachRoundReviewed() {
        let rounds = [RemediationFixtures.findingsRound(), RemediationFixtures.reReview(.running)]
        let stages = RemediationFixtures.stages
        // The same labels the run rail gives these attempts' milestones.
        #expect(
            ReviewRoundPresentation.title(rounds[0], in: rounds, stages: stages)
                == "Review 1: Implementation · Pass 1 · Round 2")
        #expect(
            ReviewRoundPresentation.title(rounds[1], in: rounds, stages: stages)
                == "Review 2: Remediation for Review 1")
        // Without the run's stages the attempt is named by its kind.
        #expect(ReviewRoundPresentation.title(rounds[0], in: rounds) == "Review 1: Implementation")
        var feedback = rounds[1]
        feedback.subject = .init(value1: .init(kind: .operator_feedback, invocation_id: "inv-feedback"))
        #expect(ReviewRoundPresentation.title(feedback, in: rounds) == "Review 2: Operator feedback")
        // An unproven producer makes no claim about what was reviewed.
        var unproven = rounds[1]
        unproven.subject = nil
        #expect(ReviewRoundPresentation.title(unproven, in: rounds, stages: stages) == "Review 2")
        // Without the source round's facts the subject still names the round
        // it answers.
        #expect(ReviewRoundPresentation.title(rounds[1], in: [rounds[1]]) == "Review 2: Remediation for Review 1")
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

    @Test func evidenceNoteExplainsMissingOutputOnlyOnceTheRoundEnds() {
        // A round in progress has produced no output yet, so nothing is
        // missing and there is nothing to explain.
        #expect(ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.pending)) == nil)
        #expect(ReviewRoundPresentation.evidenceNote(RunFixtures.reviewRound(.running)) == nil)
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

    /// The implementer is the second attempt of the run's first
    /// `implement` stage; the remediator runs in a second one.
    static let stages: [Components.Schemas.Stage] = [
        .init(
            id: "stage-impl", run_id: "run", name: "implement",
            attempts: [
                .init(id: "attempt-1", stage_id: "stage-impl", number: 1, invocation_id: "inv-impl-first"),
                .init(id: "attempt-2", stage_id: "stage-impl", number: 2, invocation_id: implementer),
            ]),
        .init(
            id: "stage-remediate", run_id: "run", name: "implement",
            attempts: [.init(id: "attempt-3", stage_id: "stage-remediate", number: 1, invocation_id: remediator)]),
    ]

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
