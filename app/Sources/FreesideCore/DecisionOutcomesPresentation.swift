import FreesideAPI
import SwiftUI

/// What a run did around its pull request, for the ready card's Recorded
/// Context (plan §9, `ready_for_final_review`'s "Below" column): each
/// follow-up issue or issue comment it filed, posted, or proposed, where
/// each stands, and each finding it declined or deferred. Read from the run
/// timeline's `outcomes`, which decides nothing; the list carries no verdict
/// on the merge, so it folds under the actions.
///
/// A post's row is the daemon's record. A finding's message and reason are
/// the reviewer's and the agent's words, kept apart in `Finding` so the view
/// labels them as claims.
struct DecisionOutcomesPresentation: Equatable {
    struct Post: Equatable {
        /// The effect kind, as the card's other effect rows name it.
        let kind: String
        /// Where the post stands, with its reference when it has one:
        /// `owner/repo#N`, and the comment ID for a posted comment.
        let value: String
        /// Why a refused, capped, or ambiguous post ended as it did: the
        /// notice's reason, else the ledger's refusal reason.
        let reason: String?
    }

    struct Finding: Equatable {
        /// The disposition, round, and origin, in the daemon's words.
        let heading: String
        let message: String
        let reason: String
    }

    let posts: [Post]
    let findings: [Finding]
    /// The daemon could not read part of the list; what it could is here.
    let complete: Bool

    /// Nil when there is nothing to draw: no list, as from a daemon that
    /// serves none or a timeline not yet loaded, or an empty, complete one.
    /// An empty list the daemon could not read in full still draws, as its
    /// notice alone, so it never reads as a run that posted nothing.
    init?(_ outcomes: Components.Schemas.RunOutcomes?) {
        guard let outcomes,
            !(outcomes.posts.isEmpty && outcomes.findings.isEmpty && outcomes.complete)
        else { return nil }
        posts = outcomes.posts.map(Self.post)
        findings = outcomes.findings.map(Self.finding)
        complete = outcomes.complete
    }

    static let incompleteNotice = "Freeside could not read part of this list, so it may be missing entries."

    private static func post(_ post: Components.Schemas.RunOutcomePost) -> Post {
        let reference = reference(post)
        let state = stateLabel(post.state)
        return Post(
            kind: AttentionDisplay.effectKindLabel(post.effect_kind),
            value: [state, reference].compactMap { $0 }.joined(separator: " · "),
            reason: reason(post))
    }

    /// `owner/repo#N` for an issue, the repository alone before a filing
    /// has one, and nothing for an unreadable post, which has no repository.
    private static func reference(_ post: Components.Schemas.RunOutcomePost) -> String? {
        guard let repo = post.repository?.value1.repo else { return nil }
        let issue = post.issue_number.map { "\(repo)#\($0)" } ?? repo
        guard let comment = post.comment_id else { return issue }
        return "\(issue) · comment \(comment)"
    }

    private static func stateLabel(_ state: Components.Schemas.RunOutcomePostState) -> String {
        switch state {
        case .pending: "Pending"
        case .card_declined: "Declined on its card"
        case .withdrawn: "Withdrawn"
        case .completed: "Completed"
        case .refused: "Refused"
        case .capped: "Capped"
        case .ambiguous: "May have posted"
        case .unreadable: "Unreadable"
        case .unknown: "Unrecognized state"
        }
    }

    private static func reason(_ post: Components.Schemas.RunOutcomePost) -> String? {
        switch post.state {
        case .refused, .capped:
            return noticeReason(post) ?? post.refusal_reason.flatMap(refusalLabel)
        case .ambiguous:
            return noticeReason(post)
        case .pending, .card_declined, .withdrawn, .completed, .unreadable, .unknown:
            return nil
        }
    }

    /// The schema lets a notice's reason be empty; that explains nothing,
    /// so the ledger's reason stands in.
    private static func noticeReason(_ post: Components.Schemas.RunOutcomePost) -> String? {
        guard let reason = post.notice?.value1.reason, !reason.isEmpty else { return nil }
        return reason
    }

    private static func refusalLabel(
        _ reason: Components.Schemas.RunOutcomePost.refusal_reasonPayload
    ) -> String? {
        switch reason {
        case .definite_rejection: "GitHub rejected the request."
        case .retry_bound_spent: "The retries ran out."
        case .precondition_failed: "A precondition for the post failed."
        case .unknown: "The daemon gave a reason this app doesn't recognize."
        case ._empty: nil
        }
    }

    private static func finding(_ finding: Components.Schemas.RunOutcomeFinding) -> Finding {
        let disposition =
            switch finding.disposition {
            case .declined: "Declined"
            case .deferred: "Deferred"
            case .unknown: "Set aside"
            }
        let origin = finding.external ? " · external" : ""
        return Finding(
            heading: "\(disposition) in round \(finding.round)\(origin) · \(finding.finding_id)",
            message: finding.message,
            reason: finding.reason)
    }
}

/// The outcome list inside Recorded Context: one fact row per post, then the
/// set-aside findings under an unverified keyword, each quoted.
struct DecisionOutcomesModuleView: View {
    let presentation: DecisionOutcomesPresentation
    /// Whether the findings' keyword draws the card's one explanation
    /// control. Inside Recorded Context it never does.
    var carriesInfo = false
    var rendersInteractiveControls = true

    static let title = "Outcomes"
    static let findingsTitle = "Findings Set Aside"

    var body: some View {
        VStack(alignment: .leading, spacing: DecisionCardComposition.Scale.moduleGap) {
            KeywordLabel(text: Self.title)
            if !presentation.posts.isEmpty {
                FactGroup(spacing: DecisionCardComposition.Scale.moduleGap) {
                    ForEach(Array(presentation.posts.enumerated()), id: \.offset) { _, post in
                        postRow(post)
                    }
                }
            }
            if !presentation.findings.isEmpty {
                UnverifiedLabel(
                    text: Self.findingsTitle, carriesInfo: carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls)
                ForEach(Array(presentation.findings.enumerated()), id: \.offset) { _, finding in
                    findingQuote(finding)
                }
            }
            if !presentation.complete {
                Text(DecisionOutcomesPresentation.incompleteNotice)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .textSelection(.enabled)
    }

    @ViewBuilder
    private func postRow(_ post: DecisionOutcomesPresentation.Post) -> some View {
        if let reason = post.reason {
            VStack(alignment: .leading, spacing: 3) {
                FactRow(label: post.kind, value: post.value)
                Text(reason)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .accessibilityElement(children: .combine)
        } else {
            FactRow(label: post.kind, value: post.value)
        }
    }

    /// The finding's message in ink, the reason it was set aside in dim
    /// beneath, and the daemon's coordinates last, as a drift audit's fix
    /// reads.
    private func findingQuote(_ finding: DecisionOutcomesPresentation.Finding) -> some View {
        QuoteBlock {
            Text(finding.message)
                .font(FreesideFont.optionLabel)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            Text(finding.reason)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            Text(finding.heading)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .combine)
    }
}
