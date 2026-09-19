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
        #expect(
            ReviewRoundPresentation.priorSummary([two[1]]) == "Round 1 · clean at its bound head · historical")
        #expect(
            ReviewRoundPresentation.priorSummary([RunFixtures.reviewRound(.failed, round: 1)])
                == "Round 1 · failed · historical")
        #expect(
            ReviewRoundPresentation.priorSummary([RunFixtures.reviewRound(.completed, round: 1, findings: true)])
                == "Round 1 · findings · historical")
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
}
