import FreesideAPI
import SwiftUI

/// Why the review loop stopped, for a `review_diminishing_returns` card (plan
/// §7 "Routing"). Every sentence is chosen from the item's typed
/// `review_diminishing` facts and its action list, never parsed from `reason`.
/// The audit's own words (its explanation and each reversal's text) are kept
/// apart in `Audit`, so the view can label them as the audit model's judgment
/// rather than as facts the daemon computed.
struct DecisionStopCausePresentation: Equatable {
    struct Reversal: Equatable, Identifiable {
        let findingID: String
        let undo: String
        let rationale: String

        var id: String { findingID }
    }

    struct Audit: Equatable {
        let verdict: String
        let confidence: String
        let explanation: String
        let reversals: [Reversal]

        /// A list longer than `collapsedReversalCount` leads with that many
        /// entries and opens the rest on request.
        var collapses: Bool {
            reversals.count > DecisionStopCausePresentation.collapsedReversalCount
        }

        var reversalCount: String {
            reversals.count == 1 ? "1 fix to undo" : "\(reversals.count) fixes to undo"
        }

        /// The keyword over the audit's own words. With no fix to undo the
        /// section holds only its explanation.
        var keyword: String { reversals.isEmpty ? "Audit Explanation" : "Fixes to Undo" }

        /// The entries past the first `collapsedReversalCount`, which open
        /// on request under `moreFixesLabel`.
        var foldedReversals: [Reversal] {
            collapses
                ? Array(reversals.dropFirst(DecisionStopCausePresentation.collapsedReversalCount))
                : []
        }

        var moreFixesLabel: String {
            foldedReversals.count == 1 ? "1 More Fix" : "\(foldedReversals.count) More Fixes"
        }

        var totalFixes: String { "\(reversals.count) in total" }

        func visibleReversals(showingAll: Bool) -> [Reversal] {
            collapses && !showingAll
                ? Array(reversals.prefix(DecisionStopCausePresentation.collapsedReversalCount))
                : reversals
        }
    }

    /// How many reversals the card shows before the rest collapse, so a long
    /// list cannot push the actions out of reach on a phone.
    static let collapsedReversalCount = 3

    let cause: String
    let audit: Audit?
    /// What `continue_under_policy` will do, or that no offered action undoes
    /// the listed fixes. Nil where the typed facts say nothing about it.
    let continuation: String?

    init?(_ item: Components.Schemas.AttentionItem) {
        guard let facts = item.review_diminishing?.value1 else { return nil }
        let drift = facts.drift_audit?.value1
        cause = Self.causeSentence(facts.cause, verdict: drift?.verdict)
        audit = drift.map { drift in
            .init(
                verdict: Self.label(drift.verdict),
                confidence: AttentionDisplay.label(drift.confidence),
                explanation: drift.explanation,
                reversals: drift.reversals.map {
                    .init(findingID: $0.finding_id, undo: $0.undo, rationale: $0.rationale)
                })
        }
        let offered = item.requested_decision.contains(.continue_under_policy)
        continuation = drift.flatMap { Self.continuationSentence($0, offered: offered) }
    }

    static func label(_ verdict: Components.Schemas.DriftVerdict) -> String {
        switch verdict {
        case .converged: return "Converged"
        case .over_hardened: return "Over-hardened"
        case .stuck: return "Stuck"
        }
    }

    private static func causeSentence(
        _ cause: Components.Schemas.ReviewDiminishingCause,
        verdict: Components.Schemas.DriftVerdict?
    ) -> String {
        switch cause {
        case .low_value_streak:
            return "Review stopped because the latest rounds raised only low-value findings."
        case .fixed_recurrence:
            return "Review stopped because a finding that was already fixed came back."
        case .final_review_findings:
            return "Review stopped because the last allowed round still had findings."
        case .growth_without_blockers:
            return "Review stopped because the change kept growing while no round raised a blocker."
        case .drift_audit:
            // The contract pairs this cause with drift facts; a payload that
            // breaks the pairing still gets a true sentence, without a verdict.
            guard let verdict else { return "Review stopped on a drift audit." }
            switch verdict {
            case .converged:
                return "Review stopped on a drift audit that judged the change converged."
            case .over_hardened:
                return "Review stopped because a drift audit judged the change over-hardened."
            case .stuck:
                return "Review stopped because a drift audit judged the review rounds stuck."
            }
        }
    }

    /// The daemon narrows the card to `finish_now` at the round limit, so a
    /// promise to simplify counts only while the action that keeps it is
    /// offered; otherwise the card would offer work the daemon refuses to run.
    private static func continuationSentence(
        _ drift: Components.Schemas.DriftAuditFacts,
        offered: Bool
    ) -> String? {
        let action = AttentionDisplay.label(Components.Schemas.Action.continue_under_policy)
        switch drift.verdict {
        case .over_hardened:
            guard offered else { return "No action on this card will undo the listed fixes." }
            return drift.simplification_on_continue
                ? "\(action) runs one simplification round that undoes the listed fixes."
                : "\(action) runs an ordinary review round and will not undo the listed fixes."
        case .converged, .stuck:
            return offered ? "\(action) runs an ordinary review round." : nil
        }
    }
}

struct DecisionStopCauseModuleView: View {
    @State private var showsEveryReversal = false
    let presentation: DecisionStopCausePresentation
    /// Whether the audit's keyword draws the card's one explanation control.
    var carriesInfo = false
    var rendersInteractiveControls = true

    private var scale: DecisionCardComposition.Scale { .refined }

    var body: some View {
        VStack(alignment: .leading, spacing: scale.sectionGap) {
            Text(presentation.cause)
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            if let audit = presentation.audit {
                VStack(alignment: .leading, spacing: scale.moduleGap) {
                    KeywordLabel(text: "Drift Audit")
                    FactRow(label: "Verdict", value: audit.verdict)
                    FactRow(label: "Confidence", value: audit.confidence)
                }
                // Quoted, like every model-written section on the card: the
                // explanation and the reversal text are the audit model's
                // words, not values the daemon computed.
                VStack(alignment: .leading, spacing: scale.moduleGap) {
                    UnverifiedLabel(
                        text: audit.keyword, carriesInfo: carriesInfo,
                        rendersInteractiveControls: rendersInteractiveControls)
                    QuoteBlock {
                        Text(audit.explanation)
                            .font(FreesideFont.message)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    ForEach(audit.visibleReversals(showingAll: false)) { reversal in
                        fix(reversal)
                    }
                    if audit.collapses {
                        SentenceDisclosure(
                            label: audit.moreFixesLabel, summary: audit.totalFixes,
                            isExpanded: $showsEveryReversal
                        ) {
                            VStack(alignment: .leading, spacing: scale.moduleGap) {
                                ForEach(audit.foldedReversals) { reversal in
                                    fix(reversal)
                                }
                            }
                        }
                    }
                }
            }
            if let continuation = presentation.continuation {
                Text(continuation)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// One fix to undo: what to undo in the serif, then why and which
    /// finding it answers in dim beneath (frame 5.2).
    private func fix(_ reversal: DecisionStopCausePresentation.Reversal) -> some View {
        QuoteBlock {
            Text(reversal.undo)
                .font(FreesideFont.optionLabel)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            Text(reversal.rationale)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            Text(reversal.findingID)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .combine)
    }
}
