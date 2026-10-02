import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionStopCausePresentationTests {
    private typealias Cause = Components.Schemas.ReviewDiminishingCause
    private typealias Verdict = Components.Schemas.DriftVerdict

    private func drift(
        _ verdict: Verdict = .over_hardened,
        simplificationOnContinue: Bool = false,
        longReversalList: Bool = false
    ) -> Components.Schemas.AttentionItem {
        AttentionFixtures.reviewDiminishing(
            cause: .drift_audit, verdict: verdict,
            simplificationOnContinue: simplificationOnContinue,
            longReversalList: longReversalList
        ).item
    }

    /// The cause is read from the typed field: a `reason` that names another
    /// cause changes nothing.
    @Test func everyCauseHasItsOwnPlainSentenceFromTheTypedField() throws {
        let expected: [Cause: String] = [
            .low_value_streak:
                "Review stopped because the latest rounds raised only low-value findings.",
            .fixed_recurrence:
                "Review stopped because a finding that was already fixed came back.",
            .final_review_findings:
                "Review stopped because the last allowed round still had findings.",
            .growth_without_blockers:
                "Review stopped because the change kept growing while no round raised a blocker.",
            .drift_audit:
                "Review stopped because a drift audit judged the change over-hardened.",
        ]
        #expect(Set(expected.keys) == Set(Cause.allCases))
        for (cause, sentence) in expected {
            var item = AttentionFixtures.reviewDiminishing(cause: cause).item
            item.reason = "A finding recurred after a fixed disposition."
            let presentation = try #require(DecisionStopCausePresentation(item))

            #expect(presentation.cause == sentence)
            #expect((presentation.audit != nil) == (cause == .drift_audit))
        }
    }

    @Test func driftAuditCauseNamesEachVerdict() throws {
        let expected: [Verdict: (label: String, sentence: String)] = [
            .converged: (
                "Converged", "Review stopped on a drift audit that judged the change converged."
            ),
            .over_hardened: (
                "Over-hardened",
                "Review stopped because a drift audit judged the change over-hardened."
            ),
            .stuck: (
                "Stuck", "Review stopped because a drift audit judged the review rounds stuck."
            ),
        ]
        #expect(Set(expected.keys) == Set(Verdict.allCases))
        for (verdict, words) in expected {
            let item = drift(verdict)
            let presentation = try #require(DecisionStopCausePresentation(item))
            let audit = try #require(presentation.audit)
            let facts = try #require(item.review_diminishing?.value1.drift_audit?.value1)

            #expect(presentation.cause == words.sentence)
            #expect(audit.verdict == words.label)
            #expect(audit.confidence == "High")
            #expect(audit.explanation == facts.explanation)
            #expect(audit.reversals.map(\.findingID) == facts.reversals.map(\.finding_id))
            #expect(audit.reversals.map(\.undo) == facts.reversals.map(\.undo))
            #expect(audit.reversals.map(\.rationale) == facts.reversals.map(\.rationale))
        }
    }

    @Test func continuingRunsTheSimplificationRoundOnlyWhenTheDaemonPromisesIt() throws {
        #expect(
            try #require(DecisionStopCausePresentation(drift(simplificationOnContinue: true)))
                .continuation
                == "Continue under policy runs one simplification round that undoes the listed fixes.")
        #expect(
            try #require(DecisionStopCausePresentation(drift())).continuation
                == "Continue under policy runs an ordinary review round and will not undo the listed fixes."
        )
    }

    @Test(arguments: [Components.Schemas.DriftVerdict.stuck, .converged])
    func continuingRunsAnOrdinaryRoundForAVerdictWithoutReversals(verdict: Components.Schemas.DriftVerdict) throws {
        var item = drift(verdict)
        #expect(
            try #require(DecisionStopCausePresentation(item)).continuation
                == "Continue under policy runs an ordinary review round.")

        // Narrowed to finish_now there is no continuing to describe and no
        // listed fix to warn about.
        item.requested_decision = [.finish_now]
        #expect(try #require(DecisionStopCausePresentation(item)).continuation == nil)
    }

    /// The daemon narrows the card to finish_now at the round limit. The card
    /// then offers nothing that undoes the listed fixes, whatever the
    /// simplification flag says.
    @Test(arguments: [false, true])
    func anOverHardenedCardWithoutContinueSaysNoActionUndoesTheFixes(simplificationOnContinue: Bool) throws {
        var item = drift(simplificationOnContinue: simplificationOnContinue)
        #expect(try #require(DecisionStopCausePresentation(item)).continuationTitle == "If you continue")
        item.requested_decision = [.finish_now]
        let narrowed = try #require(DecisionStopCausePresentation(item))

        #expect(narrowed.continuation == "No action on this card will undo the listed fixes.")
        #expect(narrowed.continuationTitle == "Undoing the fixes")
    }

    @Test(arguments: [
        Components.Schemas.ReviewDiminishingCause.low_value_streak, .fixed_recurrence,
        .final_review_findings, .growth_without_blockers,
    ])
    func aCauseWithoutAnAuditSaysNothingAboutContinuing(
        cause: Components.Schemas.ReviewDiminishingCause
    ) throws {
        let presentation = try #require(
            DecisionStopCausePresentation(AttentionFixtures.reviewDiminishing(cause: cause).item))

        #expect(presentation.audit == nil)
        #expect(presentation.continuation == nil)
    }

    @Test func aLongReversalListCollapsesButKeepsItsCountAndEveryEntry() throws {
        let long = try #require(
            DecisionStopCausePresentation(drift(longReversalList: true))?.audit)

        #expect(long.collapses)
        #expect(long.reversalCount == "8 fixes to undo")
        #expect(
            long.visibleReversals(showingAll: false).map(\.findingID) == [
                "finding-1", "finding-2", "finding-3",
            ])
        #expect(long.visibleReversals(showingAll: true) == long.reversals)
        #expect(long.reversals.count == 8)

        let short = try #require(DecisionStopCausePresentation(drift())?.audit)
        #expect(!short.collapses)
        #expect(short.visibleReversals(showingAll: false) == short.reversals)
    }

    @Test func anItemWithoutFactsHasNoStopCause() {
        var item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        item.review_diminishing = nil

        #expect(DecisionStopCausePresentation(item) == nil)
    }
}
