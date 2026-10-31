import Foundation
import FreesideAPI

/// Human-readable labels for the contract's enums. Behaviour-dispatch
/// switches omit `default` on purpose: a new enum member must be handled
/// here before the code compiles.
package enum AttentionDisplay {
    package struct BindingRow: Equatable {
        package let label: String
        package let value: String

        package init(label: String, value: String) {
            self.label = label
            self.value = value
        }
    }

    /// One Section 9 card fact: a daemon-produced value with the label the
    /// card shows it under. Identifiers and digests render monospaced so a
    /// fact that must be compared by eye reads as one.
    package struct FactRow: Equatable, Identifiable {
        /// How a card draws the value. `value` is the same reading as plain
        /// text on every form, for a summary, a test, or a stacking rule.
        package enum Form: Equatable {
            case plain
            /// Counts in the diff cuts (R28): successive measurements of one
            /// diff, earliest first.
            case diffs([DiffCounts])
            /// A system-health item's admission posture, a state: the chip
            /// sits in the value slot (R9).
            case posture(Components.Schemas.HealthPosture)
            /// Another attention item, which the value links to (R3).
            case item(id: String)
            /// A page outside the app, which the value links to (R3).
            case link(URL)
            /// A recorded instant in the short time grammar (R8), with the
            /// exact instant one gesture away.
            case instant(Date)
            /// A sentence, such as the daemon's reason for a close: it
            /// stacks under its label in the body face, since prose is not
            /// a mono value.
            case prose
        }

        package let label: String
        package let value: String
        let monospaced: Bool
        package let form: Form

        package var id: String { label }

        package init(_ label: String, _ value: String, monospaced: Bool = false) {
            self.label = label
            self.value = value
            self.monospaced = monospaced
            self.form = .plain
        }

        init(_ label: String, prose: String) {
            self.label = label
            self.value = prose
            self.monospaced = false
            self.form = .prose
        }

        init(_ label: String, posture: Components.Schemas.HealthPosture) {
            self.label = label
            self.value = AttentionDisplay.label(posture)
            self.monospaced = false
            self.form = .posture(posture)
        }

        init(_ label: String, _ value: String, linkingItem id: String) {
            self.label = label
            self.value = value
            self.monospaced = false
            self.form = .item(id: id)
        }

        /// A value that names something with a page of its own. Without a
        /// URL the value still draws, as the identifier it is.
        init(_ label: String, _ value: String, linking url: URL?) {
            self.label = label
            self.value = value
            self.monospaced = url == nil
            self.form = url.map(Form.link) ?? .plain
        }

        /// `locale` and `timeZone` come from the view's environment, so a
        /// pinned screenshot or a device in another zone prints the same
        /// instant the other task surfaces do.
        init(_ label: String, instant: Date, now: Date, locale: Locale, timeZone: TimeZone) {
            self.label = label
            self.value = FreesideFormat.shortTime(instant, now: now, locale: locale, timeZone: timeZone)
            self.monospaced = false
            self.form = .instant(instant)
        }

        init(_ label: String, diffs: [DiffCounts]) {
            self.label = label
            self.value = DiffCounts.plain(diffs)
            self.monospaced = true
            self.form = .diffs(diffs)
        }
    }

    struct SubjectLine: Equatable {
        let lead: String
        let identifier: String?
    }

    package struct RowContext: Equatable {
        package struct Segment: Equatable {
            package let value: String
            let isIdentifier: Bool
            /// An agent-proposed task name: the row labels it as the
            /// agent's claim (plan §9), as the task row does.
            package var isAgentClaim = false
        }

        package let project: Segment
        package let workUnit: Segment?
    }

    package struct CopyableSubjectReference: Equatable {
        package let label: String
        package let value: String
    }

    package static func title(_ type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .spec_approval: return "Spec Approval"
        case .execution_failure: return "Execution Failure"
        case .agent_question: return "Agent Question"
        case .review_diminishing_returns: return "Diminishing Returns"
        case .review_dispute: return "Review Dispute"
        case .review_contradiction: return "Review Contradiction"
        case .review_configuration: return "Review Configuration"
        case .finding_adjudication: return "Finding Adjudication"
        case .ready_for_final_review: return "Ready for Final Review"
        case .publish_blocked: return "Publish Blocked"
        case .task_proposal: return "Task Proposal"
        case .effect_proposal: return "Effect Proposal"
        case .system_health: return "System Health"
        case .blocked: return "Blocked"
        }
    }

    package static func title(_ item: Components.Schemas.AttentionItem) -> String {
        guard item._type == .ready_for_final_review,
            item.readiness?.value1._class == .ready_degraded
        else { return title(item._type) }
        return "Ready for Final Review (Degraded)"
    }

    static func hasExternalDisputedFindings(_ item: Components.Schemas.AttentionItem) -> Bool {
        item.requested_decision.contains(.adjudicate_findings)
            && item.review_dispute?.value1.findings?.contains(where: { $0.external != nil }) == true
    }

    package static func disputeRouteLabel(
        _ route: Components.Schemas.AdjudicationRoute, external: Bool
    ) -> String {
        guard external else { return label(route) }
        switch route {
        case .remediate: return "Fix in This Pull Request"
        case .decline: return "Decline and Reply"
        case ._defer: return "Defer and Reply"
        default: return label(route)
        }
    }

    /// What picking a route does to an external finding, drawn under the
    /// route's label (R29). Nil on a finding with no review thread: the
    /// reply these lines promise goes to that thread.
    package static func disputeRouteConsequence(
        _ route: Components.Schemas.AdjudicationRoute, external: Bool
    ) -> String? {
        guard external else { return nil }
        switch route {
        case .remediate: return "Starts remediation in this pull request."
        case .decline: return "Records the outcome and replies in the finding's thread."
        case ._defer: return "Records the outcome and replies in the finding's thread; files no follow-up issue."
        default: return nil
        }
    }

    /// Whether a route posts a reply in an external finding's thread: Decline
    /// and Reply and Defer and Reply. The daemon requires the operator's
    /// reason on exactly these and refuses one on any other route.
    package static func disputeRouteRepliesExternally(
        _ route: Components.Schemas.AdjudicationRoute, external: Bool
    ) -> Bool {
        external && (route == .decline || route == ._defer)
    }

    /// The prompt in the reply reason field of a replying route.
    package static let disputeReplyReasonPlaceholder = "Reason for the reply"

    /// The line under the reply reason field: where the reason goes, and
    /// why Apply waits while the draft is blank or too long to send.
    package static func disputeReplyReasonHint(_ draft: String) -> String {
        let bytes = draft.trimmingCharacters(in: .whitespacesAndNewlines).utf8.count
        if bytes == 0 { return "Required. Posted with the reply in the reviewer's thread." }
        return bytes > Components.Schemas.FindingRouteChoice.maxReasonBytes
            ? "Too long to post. Shorten it to send the reply."
            : "Posted with the reply in the reviewer's thread."
    }

    /// The mark on the offered route the adjudicator favored. The model's
    /// route is "favored", not "recommended": a recommendation is what an
    /// accept-all action applies, and this one fell below the dispatch
    /// threshold, so nothing preselects it. The mark informs the pick, it
    /// never makes it. `adjudication.lean` keeps its schema name.
    package static let disputedFindingFavoredMark = "Favored"

    /// The trailing summary on a disputed finding's closed Reason and
    /// Source fold: whether the model favored a route, its confidence, and
    /// the review thread, so the closed fold still says what it holds.
    package static func disputedFindingReasonSummary(_ finding: Components.Schemas.DisputedFinding) -> String {
        var parts: [String] = []
        if let adjudication = finding.adjudication {
            if adjudication.lean == nil { parts.append("no favored route") }
            parts.append("\(label(adjudication.confidence).lowercased()) confidence")
        } else {
            parts.append("no adjudication")
        }
        if let external = finding.external { parts.append(external.thread_id) }
        return parts.joined(separator: " · ")
    }

    /// Why the model's route is only favored, inside the fold. Nil when the
    /// confidence met the threshold, which says nothing about this.
    package static func disputedFindingThreshold(
        _ adjudication: Components.Schemas.DisputedFinding.adjudicationPayload
    ) -> String? {
        guard adjudication.below_threshold else { return nil }
        let confidence = label(adjudication.confidence)
        return adjudication.lean == nil
            ? "\(confidence) confidence is below the dispatch threshold, so the model favors no route."
            : "\(confidence) confidence is below the dispatch threshold, so this is a favored route, not a recommendation."
    }

    /// The favored route when the finding does not offer it, which no
    /// option can carry as a mark: the lean is a model proposal and may
    /// name a route the dispute cannot send.
    package static func disputedFindingUnofferedFavorite(_ finding: Components.Schemas.DisputedFinding) -> String? {
        guard let lean = finding.adjudication?.lean?.value1, !finding.offered_routes.contains(lean) else {
            return nil
        }
        let route = disputeRouteLabel(lean, external: finding.external != nil)
        return "The model favors \(route), which this finding does not offer."
    }

    /// Where a disputed finding is and who raised it, as one mono line on
    /// its face. Nil when it has neither a location nor a reviewer.
    package static func disputedFindingSource(_ finding: Components.Schemas.DisputedFinding) -> String? {
        let parts = [finding.location.map { findingLocation($0.value1) }, finding.external?.reviewer_login]
            .compactMap { $0 }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    package static let disputedFindingNoAdjudication = "No adjudicator view was recorded for this finding."

    /// The Apply label on a routed dispute, counting the picks (R32), so a
    /// disabled button says how many findings still need a route.
    package static func applyFindingRoutesLabel(selected: Int, of total: Int) -> String {
        guard selected > 0 else { return label(.adjudicate_findings) }
        guard selected == total else { return "Apply \(selected) of \(total) Routes" }
        return total == 1 ? "Apply 1 Route" : "Apply \(total) Routes"
    }

    package static func ask(_ item: Components.Schemas.AttentionItem) -> String {
        switch item._type {
        case .spec_approval:
            return "Approve this specification for implementation?"
        case .execution_failure:
            return "How should this failed execution continue?"
        case .agent_question:
            return "How should the agent proceed with this question?"
        case .review_diminishing_returns:
            return "How should review conclude after diminishing returns?"
        case .review_dispute:
            if hasExternalDisputedFindings(item) {
                return "How should these external review findings be handled?"
            }
            return "How should this review dispute be resolved?"
        case .review_contradiction:
            return "Recover review under the approved execution contract?"
        case .review_configuration:
            return "Adopt this reviewer configuration and resume review?"
        case .finding_adjudication:
            return "Which disposition should apply to these review findings?"
        case .ready_for_final_review:
            return "Is this change ready for final GitHub review?"
        case .publish_blocked:
            return "How should publication recover from this trust failure?"
        case .task_proposal:
            return "Start this proposed run?"
        case .effect_proposal:
            return "Approve this proposed effect?"
        case .system_health:
            return "How should this system-health condition be handled?"
        case .blocked:
            // A blocked card offers no decision, so where the daemon typed
            // the wait the card leads with it (survey frame 5.5).
            return blockedWaitSentence(item) ?? "What is keeping this run blocked?"
        }
    }

    /// What a blocked run waits on, as a sentence, where the item's typed
    /// wait says.
    static func blockedWaitSentence(_ item: Components.Schemas.AttentionItem) -> String? {
        item.blocked_on.map { "Waiting on \(phrase($0.value1.kind))." }
    }

    package static func rowSummary(_ item: Components.Schemas.AttentionItem) -> String {
        if let lead = ConclusionDisplay.leadSentence(item) { return lead }
        switch item._type {
        case .spec_approval:
            return "A specification is ready for approval."
        case .execution_failure:
            return "An execution failed and needs a recovery choice."
        case .agent_question:
            return "The agent needs an answer before it can continue."
        case .review_diminishing_returns:
            return "Review has reached diminishing returns."
        case .review_dispute:
            if hasExternalDisputedFindings(item) {
                return "Choose how to answer each external review finding."
            }
            return "A review finding needs a disposition."
        case .review_contradiction:
            return "Review contradicted the approved execution contract."
        case .review_configuration:
            return "Reviewer configuration no longer matches approved policy."
        case .finding_adjudication:
            if let round = item.finding_adjudication?.value1.round {
                return "Review round \(round) has findings to adjudicate."
            }
            return "Review findings need adjudication."
        case .ready_for_final_review:
            guard let readiness = item.readiness?.value1 else {
                return "Verification status is unavailable; final review is requested."
            }
            if readiness._class == .ready_degraded {
                return "Verification is degraded and needs final review."
            }
            return "Verification is clean and ready for final review."
        case .publish_blocked:
            return "Publication is blocked by a trust-policy check."
        case .task_proposal:
            return "A proposed run is ready to start."
        case .effect_proposal:
            return "A proposed effect is ready to review."
        case .system_health:
            guard let diagnostic = item.health_diagnostic?.value1 else {
                return "A system-health condition needs attention."
            }
            guard diagnostic.impairs != Components.Schemas.ImpairedCapability.none else {
                return "Diagnostic \(diagnostic.code) is open; no capability is impaired."
            }
            return "\(label(diagnostic.impairs)) is impaired by diagnostic \(diagnostic.code)."
        case .blocked:
            return blockedWaitSentence(item) ?? "A run is waiting on a blocker."
        }
    }

    /// The Section 9 "leads with" facts for one item type, read from the
    /// item's typed fact fields. Every value is a daemon-produced card fact;
    /// nothing here is derived from prose, logs, or event names, and a type
    /// whose lead is a claim, an artifact, or its own module contributes no
    /// row.
    package static func cardFacts(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> [FactRow] {
        switch item._type {
        case .execution_failure:
            guard let failure = item.execution_failure?.value1 else { return [] }
            return [
                .init("Outcome", label(failure.outcome)),
                failure.cause.map { .init("Cause", label($0)) },
                .init("Stage", label(failure.stage)),
                .init("Invocation", failure.invocation_id, monospaced: true),
            ].compactMap { $0 }
        case .review_diminishing_returns:
            return [
                item.billable_cost_so_far.map { .init("Cost so Far", costSoFar($0.value1)) },
                diffGrowth(item),
            ].compactMap { $0 }
        case .review_dispute:
            guard let dispute = item.review_dispute?.value1 else { return [] }
            let listed = dispute.findings?.map(\.finding_id) ?? []
            let disputed = listed.isEmpty ? dispute.finding_ids : listed
            let routed = dispute.finding_ids.filter { !disputed.contains($0) }
            var rows: [FactRow] = [
                .init("Run", dispute.run_id, monospaced: true),
                .init("Round", "\(dispute.round)"),
                .init(
                    "Disputed findings",
                    disputed.joined(separator: ", "), monospaced: true),
            ]
            if !routed.isEmpty {
                rows.append(.init("Already routed", routed.joined(separator: ", "), monospaced: true))
            }
            rows.append(.init("Completion evidence", dispute.completion_evidence, monospaced: true))
            return rows
        case .ready_for_final_review:
            // Diff alone: the checklist's "Bound to" row above carries the
            // readiness candidate's head and base, and Details keeps this
            // diff's own base and head as Diff base and Diff head, so
            // repeating them here made one datum render in three card modules
            // (#1107).
            guard let diff = item.diff_stats?.value1 else { return [] }
            return [.init(diffFactLabel, diffStats(diff))]
        case .publish_blocked:
            guard let block = item.publish_block?.value1 else { return [] }
            if let rule = block.trust_rule?.value1 {
                return [.init("Failed trust rule", label(rule))]
            }
            if let hold = block.hold_reason?.value1 {
                return [.init("Hold reason", label(hold))]
            }
            return []
        case .system_health:
            var rows: [FactRow] = []
            if let diagnostic = item.health_diagnostic?.value1 {
                rows.append(.init("Diagnostic", diagnostic.code, monospaced: true))
                rows.append(.init("Impairs", label(diagnostic.impairs)))
            }
            // Whether the finding gates unattended admission is a fact about
            // it, so it reads with the diagnostic in either posture.
            if let posture = item.posture?.value1 {
                rows.append(.init("Posture", posture: posture))
            }
            return rows
        case .blocked:
            guard let wait = item.blocked_on?.value1 else { return [] }
            var rows: [FactRow] = [
                // The wait reads as the duration the inbox row already uses.
                // Its exact start is an audit coordinate, so it stays with the
                // other technical bindings rather than leading the card as a
                // monospaced timestamp.
                .init("Waiting", relativeRowTime(wait.since, now: now))
            ]
            // The item the run waits on is one the operator can open, so the
            // row links to it; its id is a binding and stays in Details.
            if let blockingItem = wait.item_id {
                rows.append(.init("Blocked on", label(wait.kind), linkingItem: blockingItem))
            } else {
                rows.append(.init("Blocked on", label(wait.kind)))
            }
            if let pull = wait.pr_reference?.value1 {
                rows.append(
                    .init("Pull Request", "\(pull.repo)#\(pull.number)", monospaced: true))
            }
            return rows
        case .agent_question:
            // The decisions lead the card, so these two say who stopped and
            // what the run waits on, once, below them: they were a stage
            // sentence and a caption inside the question module until the
            // question took the lead (#1107).
            guard let facts = item.agent_question?.value1 else { return [] }
            var rows: [FactRow] = [.init("Stage", label(facts.stage))]
            if let kind = facts.kind?.value1 {
                rows.append(.init("Blocked on", AgentQuestionPresentation.kindLabel(kind)))
            }
            return rows
        // The ask leads a spec approval, the adjudication artifact leads
        // finding_adjudication, the authenticated proposal snapshot leads
        // task_proposal, and the recovery bindings these two recovery types
        // lead with are already their own rows.
        case .spec_approval, .finding_adjudication, .task_proposal, .effect_proposal,
            .review_contradiction, .review_configuration:
            return []
        }
    }

    /// The authenticated-proposal card facts for an effect proposal, as pure
    /// label/value pairs so a display test asserts the wording without a
    /// view. Every value is a daemon fact from the snapshot; agent-written
    /// text (a filing's title and body) is never a row here and draws in
    /// its own unverified sections (`proposedIssueText`). The rows follow
    /// `effect_kind`, never which arm happens to be present, and are empty
    /// when the kind's arm is missing or the kind has no effect card.
    static func effectProposalRows(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> [FactRow] {
        switch facts.effect_kind {
        case .source_issue_closure:
            guard let closure = facts.source_issue_closure?.value1 else { return [] }
            return sourceIssueClosureRows(closure, supersedes: facts.supersedes?.value1)
        case .follow_up_filing:
            guard let filing = facts.follow_up_filing?.value1 else { return [] }
            return followUpFilingRows(filing)
        case .image_policy_adoption:
            guard let adoption = facts.image_policy_adoption?.value1 else { return [] }
            return imagePolicyAdoptionRows(adoption)
        case .run_proposal:
            return []
        }
    }

    /// What approving an adoption adds to the repository's image policy.
    /// The recipe digest is shortened as a card row shortens a revision; the
    /// full digest and the refusal the card answers are audit coordinates,
    /// kept for Details (`imagePolicyAdoptionDetailRows`).
    private static func imagePolicyAdoptionRows(
        _ adoption: Components.Schemas.ImagePolicyAdoptionFacts
    ) -> [FactRow] {
        [
            .init(effectFactLabel, effectKindLabel(.image_policy_adoption)),
            .init("Repository", adoption.repository.repo, monospaced: true),
            .init(
                "Recipe",
                adoption.recipe_digest.map { shortDigest($0.value1) } ?? "Unchanged",
                monospaced: adoption.recipe_digest != nil),
            .init(
                "Registry Hosts",
                adoption.registry_authorities.isEmpty ? "None" : adoption.registry_authorities.joined(separator: ", "),
                monospaced: !adoption.registry_authorities.isEmpty),
        ]
    }

    /// A `sha256:` digest with its hex shortened as `shortRevision` shortens
    /// a revision, the algorithm kept so the value still reads as a digest.
    /// Any other value is returned whole.
    static func shortDigest(_ digest: String) -> String {
        let algorithm = "sha256:"
        guard digest.hasPrefix(algorithm) else { return digest }
        return algorithm + shortRevision(String(digest.dropFirst(algorithm.count)))
    }

    static let effectFactLabel = "Effect"
    static let onMergeFactLabel = "On Merge"

    static func issueName(_ issue: Components.Schemas.IssueSubjectRef) -> String {
        "\(issue.repo)#\(issue.issue_number)"
    }

    /// What approving an effect binds, as one sentence (frame 7.9). For a
    /// source-issue closure: the candidate head, the base it would merge
    /// into, and what the merge would do to the issue. For an image-policy
    /// adoption: what it adds to the repository's image policy, and why.
    /// The identifier (the head, or the registry hosts) is kept apart so the
    /// card can set it in the identifier face; it is empty when the sentence
    /// names none. Nil for an effect with nothing to state, which keeps its
    /// rows instead.
    package struct EffectBinding: Equatable {
        package let lead: String
        package let head: String
        package let outcome: String

        package var plain: String { lead + head + outcome }
    }

    package static func effectBinding(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> EffectBinding? {
        if facts.effect_kind == .image_policy_adoption {
            return facts.image_policy_adoption.flatMap { adoptionBinding($0.value1) }
        }
        guard facts.effect_kind == .source_issue_closure,
            let closure = facts.source_issue_closure?.value1
        else { return nil }
        let issue = issueName(closure.target)
        return .init(
            lead: "Merging head ",
            head: shortRevision(closure.merge.candidate_head_sha),
            outcome: closure.resolves
                ? " into \(closure.merge.base_ref) would close \(issue)."
                : " into \(closure.merge.base_ref) would leave \(issue) open.")
    }

    /// The adoption sentence: the tip's recipe, the registry hosts, or both,
    /// added to the policy so upkeep can rebuild. It names the effect, so the
    /// card drops the "Effect" row under it. Nil for facts that add nothing,
    /// which the decision model already refuses.
    private static func adoptionBinding(_ adoption: Components.Schemas.ImagePolicyAdoptionFacts) -> EffectBinding? {
        let hosts = adoption.registry_authorities
        let hostNoun = hosts.count == 1 ? "registry host " : "registry hosts "
        let lead: String
        switch (adoption.recipe_digest != nil, hosts.isEmpty) {
        case (true, false): lead = "Approving adds the tip's recipe and " + hostNoun
        case (false, false): lead = "Approving adds " + hostNoun
        case (true, true): lead = "Approving adds the tip's recipe"
        case (false, true): return nil
        }
        return .init(
            lead: lead,
            head: hosts.joined(separator: ", "),
            outcome: " to the repository's image policy, so upkeep can rebuild the image.")
    }

    /// The effect's rows as the card draws them. Where the binding
    /// statement leads, it already names the effect and what the merge
    /// does, so those two rows are not repeated under it.
    package static func effectProposalCardRows(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> [FactRow] {
        let rows = effectProposalRows(facts)
        guard effectBinding(facts) != nil else { return rows }
        return rows.filter { $0.label != effectFactLabel && $0.label != onMergeFactLabel }
    }

    /// The closure card names no PR (the facts carry none).
    private static func sourceIssueClosureRows(
        _ closure: Components.Schemas.SourceIssueClosureFacts,
        supersedes: Components.Schemas.EffectProposalRevisionFacts?
    ) -> [FactRow] {
        var rows: [FactRow] = [
            .init(effectFactLabel, effectKindLabel(.source_issue_closure)),
            .init(
                "Target Issue", issueName(closure.target),
                linking: DecisionModel.issueURL(for: closure.target)),
            .init(
                onMergeFactLabel,
                closure.resolves ? "Closes the issue" : "Doesn't close the issue"),
            .init("Reference", closureProvenanceExplanation(closure.provenance)),
            .init("Closure Flag", closureOriginLabel(closure.origin)),
            .init(
                "Bound to",
                "Head \(shortRevision(closure.merge.candidate_head_sha)) · "
                    + "Base \(closure.merge.base_ref)@\(shortRevision(closure.merge.base_sha))",
                monospaced: true),
        ]
        if let prior = supersedes {
            let priorPhrase =
                switch prior.source_issue_closure?.resolves {
                case .some(true): "was closing the issue"
                case .some(false): "was leaving the issue open"
                case nil: "differed"
                }
            // The full prior digest, as the sibling "Prior proposal" binding
            // row shows it: shortRevision only abbreviates a bare hex object
            // name, and a Digest is algorithm-prefixed, so shortening here
            // would be a no-op that only reads as if it did something.
            rows.append(
                .init(
                    "Superseded Proposal",
                    "Previously \(priorPhrase) (\(prior.proposal_digest))"))
        }
        return rows
    }

    /// What the daemon derived or checked for a filing: where the issue
    /// would be filed, what it would carry, which finding it answers, and
    /// what the title and body were screened under.
    private static func followUpFilingRows(
        _ filing: Components.Schemas.FollowUpFilingFacts
    ) -> [FactRow] {
        [
            .init(effectFactLabel, effectKindLabel(.follow_up_filing)),
            .init("Repository", filing.repository.repo, monospaced: true),
            .init("Labels", filing.labels.isEmpty ? "None" : filing.labels.joined(separator: ", ")),
            .init("Milestone", filing.milestone ?? "None"),
            .init(
                "Source",
                "Finding \(filing.source.finding_id) · \(followUpSourceKindLabel(filing.source.kind))"),
            .init("Text Screening", textScreening(title: filing.title, body: filing.body)),
        ]
    }

    /// One phrase while the title and body share a ruleset and verdict;
    /// otherwise each field names its own, so a difference is never hidden
    /// behind the other field's result.
    private static func textScreening(
        title: Components.Schemas.ScreenedIssueText,
        body: Components.Schemas.ScreenedIssueText
    ) -> String {
        func screened(_ text: Components.Schemas.ScreenedIssueText) -> String {
            "\(screeningVerdictLabel(text.verdict)) under \(issueTextRulesetLabel(text.ruleset))"
        }
        if title.ruleset == body.ruleset, title.verdict == body.verdict {
            return "Title and body: \(screened(title))"
        }
        return "Title: \(screened(title)) · Body: \(screened(body))"
    }

    static func followUpSourceKindLabel(_ kind: Components.Schemas.FollowUpSourceKind) -> String {
        switch kind {
        case .deferred_disposition: return "Deferred disposition"
        case .separate_work_verdict: return "Separate-work verdict"
        }
    }

    /// The ruleset's own versioned name: the operator compares it with the
    /// daemon's records, so it is not reworded.
    static func issueTextRulesetLabel(_ ruleset: Components.Schemas.IssueTextRuleset) -> String {
        switch ruleset {
        case .github_hyphen_issue_sol_1: return "github-issue/1"
        }
    }

    static func screeningVerdictLabel(_ verdict: Components.Schemas.ScreeningVerdict) -> String {
        switch verdict {
        case .passed: return "passed"
        case .rejected: return "rejected"
        }
    }

    /// The agent-written text a follow-up filing would publish, exactly as
    /// it would be sent.
    package struct ProposedIssueText: Equatable {
        package let title: String
        package let body: String
    }

    /// The filing's title and body, kept apart from `effectProposalRows`
    /// so agent text never shares a row or a section with a daemon fact.
    /// Nil for any other effect kind, even one that carries a stray filing
    /// arm.
    package static func proposedIssueText(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> ProposedIssueText? {
        guard facts.effect_kind == .follow_up_filing,
            let filing = facts.follow_up_filing?.value1
        else { return nil }
        return .init(title: filing.title.text, body: filing.body.text)
    }

    /// The sentence under a filing's title and body. The shared unverified
    /// sentence says the daemon did not check the text; the daemon did
    /// screen this text (the "Text screening" row), so the sentence says
    /// what that screening does not establish.
    package static let screenedIssueTextExplanation =
        "Written by the agent. The daemon screened this text; it did not check that it is true."

    /// The adoption's audit coordinates for Details, in full: the repository
    /// identity, the recipe digest the card row shortens, when the adoption
    /// names one, and the upkeep refusal the card answers.
    package static func imagePolicyAdoptionDetailRows(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> [BindingRow] {
        guard facts.effect_kind == .image_policy_adoption,
            let adoption = facts.image_policy_adoption?.value1
        else { return [] }
        var rows: [BindingRow] = [
            .init(label: "Repository ID", value: String(adoption.repository.repository_id))
        ]
        if let digest = adoption.recipe_digest?.value1 {
            rows.append(.init(label: "Recipe Digest", value: digest))
        }
        rows.append(.init(label: "Upkeep Refusal", value: adoption.refusal_record_id))
        return rows
    }

    /// The filing's audit coordinates for Details, in full: the card's rows
    /// name the repository and finding for reading, and these are the
    /// forge identity and digest an operator copies to compare.
    package static func followUpFilingDetailRows(
        _ facts: Components.Schemas.EffectProposalFactsSnapshot
    ) -> [BindingRow] {
        guard facts.effect_kind == .follow_up_filing,
            let filing = facts.follow_up_filing?.value1
        else { return [] }
        return [
            .init(label: "Repository ID", value: String(filing.repository.repository_id)),
            .init(label: "Finding", value: filing.source.finding_id),
            .init(label: "Adjudication Digest", value: filing.source.adjudication_digest),
        ]
    }

    /// The effect kind, named neutrally: the "On merge" row carries whether
    /// approving closes the issue, so this row must not assert the closing
    /// action, or a resolve-false proposal (every daemon_fallback, and any
    /// resolve-false inference-site one) would contradict it.
    static func effectKindLabel(_ kind: Components.Schemas.EffectKind) -> String {
        switch kind {
        case .run_proposal: return "Run proposal"
        case .source_issue_closure: return "Source issue closure"
        case .follow_up_filing: return "Follow-up issue filing"
        case .image_policy_adoption: return "Image policy adoption"
        }
    }

    static func closureProvenanceExplanation(
        _ provenance: Components.Schemas.ClosureProvenance
    ) -> String {
        switch provenance {
        case .verified: return "The daemon bound this issue reference at intake."
        case .recommended:
            return "From the submission's source URL; approving confirms this reference."
        }
    }

    /// The mechanism that produced the closure flag (schema ClosureFlagOrigin,
    /// plan §5.13): the inference site that authored the publication emitted
    /// it, or the daemon minted a resolve-false fallback because that site or
    /// its admission failed. Not the human work proposal.
    static func closureOriginLabel(_ origin: Components.Schemas.ClosureFlagOrigin) -> String {
        switch origin {
        case .propose_site: return "Emitted by the inference site"
        case .daemon_fallback: return "Daemon resolve-false fallback"
        }
    }

    static func costSoFar(_ cost: Components.Schemas.CostSoFar) -> String {
        let invocations =
            cost.invocations == 1 ? "1 invocation" : "\(cost.invocations) invocations"
        return "\(cost.currency) \(cost.amount) across \(invocations)"
            + (cost.complete ? "" : ", still accruing")
    }

    /// The change's cumulative diff at round 1 beside the latest round the
    /// daemon measured, both from `yield_history` (plan §9), as counts the
    /// card draws in the diff cuts (R28). A round without `diff_metrics` is a
    /// gap, and the label names the round wherever a gap would otherwise hide
    /// it: with no round-1 measurement there is nothing to grow from, so the
    /// row is the latest measured round's size alone; growth that stops short
    /// of the last round says where it stops; and with no measurement at all
    /// there is no row.
    private static func diffGrowth(_ item: Components.Schemas.AttentionItem) -> FactRow? {
        let rounds = item.yield_history?.value1.rounds ?? []
        guard let latest = rounds.last(where: { $0.diff_metrics != nil }),
            let latestMetrics = latest.diff_metrics
        else { return nil }
        let latestSize = diffCounts(latestMetrics.cumulative)
        guard latest.round != 1,
            let first = rounds.first(where: { $0.round == 1 })?.diff_metrics
        else {
            return .init("Diff Size at Round \(latest.round)", diffs: [latestSize])
        }
        return .init(
            latest.round == rounds.last?.round
                ? "Diff Growth" : "Diff Growth to Round \(latest.round)",
            diffs: [diffCounts(first.cumulative), latestSize])
    }

    /// The label of the fact that carries a candidate's whole diff.
    package static let diffFactLabel = "Diff"

    package static func fileCount(_ count: Int) -> String {
        count == 1 ? "1 file" : "\(count) files"
    }

    package static func diffCounts(_ diff: Components.Schemas.DiffStats) -> DiffCounts {
        .init(added: diff.additions, removed: diff.deletions)
    }

    /// A whole diff as one plain line, in the order the Change row draws it.
    private static func diffStats(_ diff: Components.Schemas.DiffStats) -> String {
        "\(diffCounts(diff).plain) · \(fileCount(diff.files_changed))"
    }

    package static func label(_ action: Components.Schemas.Action) -> String {
        switch action {
        case .approve: return "Approve"
        case .request_changes: return "Request Changes"
        case .discuss: return "Discuss"
        case .stop: return "Stop"
        case .finish_now: return "Finish Now"
        case .apply_then_finish: return "Apply Then Finish"
        case .continue_under_policy: return "Continue Under Policy"
        case .convert_to_policy: return "Convert to Policy"
        case .retry: return "Retry"
        case .retry_with_capabilities: return "Retry With Profile"
        case .answer_and_retry: return "Answer and Retry"
        case .answer_without_retry: return "Answer Without Retry"
        case .rerun_trust_evaluation: return "Rerun Trust Evaluation"
        case .inspect_trust_failure: return "Inspect Trust Failure"
        case .open_pr: return "View PR"
        case .return_to_agent: return "Return to Agent"
        case .mark_seen: return "Mark Seen"
        case .dismiss: return "Dismiss"
        case .start: return "Start"
        case .start_with_changes: return "Start With Changes"
        case .approve_with_changes: return "Approve With Changes"
        case .decline: return "Decline"
        case .snooze: return "Snooze"
        case .acknowledge: return "Acknowledge"
        case .run_doctor: return "Run Doctor"
        case .stop_unattended: return "Stop Unattended"
        case .resume_unattended: return "Resume Unattended"
        case .recover_review: return "Recover Review"
        case .adopt_review_configuration: return "Adopt Review Configuration"
        case .resolve_reenrollment: return "Resolve Re-enrollment"
        case .accept_recommended_route: return "Accept Recommended Route"
        case .choose_alternative_route: return "Choose Another Route"
        case .adjudicate_findings: return "Apply Finding Routes"
        }
    }

    /// An action's label where its item is known. On an item that binds
    /// several findings, `accept_recommended_route` applies every proposed
    /// route at once, so the label says so (visual audit D09), and past two
    /// findings it carries their count: the findings stack above the
    /// action, and the count says how many of them the one press settles.
    /// The action and what it submits are the same either way.
    package static func label(
        _ action: Components.Schemas.Action,
        for item: Components.Schemas.AttentionItem?
    ) -> String {
        guard action == .accept_recommended_route,
            let findingCount = item?.finding_adjudication?.value1.proposals.count, findingCount > 1
        else { return label(action) }
        return findingCount > 2
            ? "Accept All \(findingCount) Dispositions" : "Accept All Dispositions"
    }

    package static func systemImage(_ action: Components.Schemas.Action) -> String? {
        switch action {
        case .open_pr: return "arrow.up.right.square"
        case .retry: return "arrow.clockwise"
        case .snooze: return "clock"
        case .stop, .stop_unattended: return "stop.fill"
        case .return_to_agent, .approve, .request_changes, .discuss, .finish_now, .apply_then_finish,
            .continue_under_policy, .convert_to_policy,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation,
            .inspect_trust_failure, .mark_seen, .dismiss, .start,
            .start_with_changes, .approve_with_changes, .decline, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route, .adjudicate_findings:
            return nil
        }
    }

    package static func confirmationConsequence(
        _ action: Components.Schemas.Action,
        for item: Components.Schemas.AttentionItem
    ) -> String? {
        switch action {
        case .stop:
            switch item._type {
            case .finding_adjudication:
                return "The run stays parked without accepting or choosing an adjudication route."
            case .review_configuration:
                return "The run concludes as a configuration failure; no replacement review configuration is adopted."
            case .spec_approval, .review_diminishing_returns, .review_dispute,
                .review_contradiction, .execution_failure, .agent_question,
                .publish_blocked, .ready_for_final_review, .task_proposal,
                .effect_proposal, .system_health, .blocked:
                break
            }
            return "The current invocation is discarded. Work already exported stays; the round in flight does not."
        case .stop_unattended:
            return "New unattended work will not start until unattended operation is resumed."
        case .decline:
            // An effect proposal has no run to start: declining a closure
            // proposal only drops the proposed effect, so the shared task
            // wording would misdescribe it.
            switch item._type {
            case .effect_proposal:
                return "The proposal is dismissed and the effect is not applied."
            case .spec_approval, .execution_failure, .agent_question,
                .review_diminishing_returns, .review_dispute, .review_contradiction,
                .review_configuration, .finding_adjudication, .ready_for_final_review,
                .publish_blocked, .task_proposal, .system_health, .blocked:
                return "The proposal is dismissed and no run starts."
            }
        case .dismiss:
            return "The item closes without taking the requested action."
        case .approve, .request_changes, .discuss, .finish_now, .apply_then_finish,
            .continue_under_policy, .convert_to_policy, .retry,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation,
            .inspect_trust_failure, .open_pr, .return_to_agent, .mark_seen,
            .start, .start_with_changes, .approve_with_changes, .snooze, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route, .adjudicate_findings:
            return nil
        }
    }

    static func label(_ outcome: Components.Schemas.ExecutionOutcomeStatus) -> String {
        switch outcome {
        case .failed: return "Failed"
        case .canceled: return "Canceled"
        case .lost: return "Lost"
        case .blocked: return "Blocked"
        }
    }

    static func label(_ cause: Components.Schemas.FailureCause) -> String {
        switch cause {
        case .quota: return "Quota exhausted"
        case .credential_expired: return "Credential expired"
        case .credential_revoked: return "Credential revoked"
        case .service_capacity: return "Service at capacity"
        case .provider_unavailable: return "Provider unavailable"
        case .invalid_configuration: return "Invalid configuration"
        case .contract_contradiction: return "Contract contradiction"
        case .unknown: return "Unknown"
        }
    }

    package static func label(_ stage: Components.Schemas.StageName) -> String {
        switch stage {
        case .specification: return "Specification"
        case .implementation: return "Implementation"
        case .review: return "Review"
        case .verification: return "Verification"
        }
    }

    static func label(_ rule: Components.Schemas.TrustRule) -> String {
        switch rule {
        case .recipe_unapproved: return "Verification recipe not approved"
        case .verification_failed: return "Verification failed"
        case .trust_profile_drift: return "Trust profile drifted"
        case .target_base_advanced: return "Target base advanced"
        }
    }

    static func label(_ reason: Components.Schemas.RunHoldReason) -> String {
        RunDisplay.label(reason)
    }

    static func label(_ capability: Components.Schemas.ImpairedCapability) -> String {
        switch capability {
        case .unattended_admission: return "Unattended admission"
        case .run_visibility: return "Run visibility"
        case .agent_credential: return "Agent credential"
        case .none: return "No capability"
        }
    }

    static func label(_ kind: Components.Schemas.BlockedWaitKind) -> String {
        switch kind {
        case .spec_approval: return "Specification approval"
        case .pr_checks: return "PR checks"
        case .external_review: return "External review"
        }
    }

    /// The same wait, mid-sentence: "PR checks" must not be lowercased into
    /// "pr checks" by a caller.
    private static func phrase(_ kind: Components.Schemas.BlockedWaitKind) -> String {
        switch kind {
        case .spec_approval: return "specification approval"
        case .pr_checks: return "PR checks"
        case .external_review: return "external review"
        }
    }

    package static func label(_ priority: Components.Schemas.Priority) -> String {
        switch priority {
        case .low: return "Low"
        case .normal: return "Normal"
        case .high: return "High"
        case .urgent: return "Urgent"
        }
    }

    package static func label(_ status: Components.Schemas.ItemStatus) -> String {
        switch status {
        case .open: return "Open"
        case .resolved: return "Resolved"
        case .superseded: return "Superseded"
        case .dismissed: return "Dismissed"
        case .expired: return "Expired"
        }
    }

    package static func label(_ posture: Components.Schemas.HealthPosture) -> String {
        switch posture {
        case .blocking: return "Blocking"
        case .advisory: return "Advisory"
        }
    }

    package static func label(_ notice: Components.Schemas.CommitPlanNoticeReason) -> String {
        switch notice {
        case .absent: return "No plan provided"
        case .structural: return "Plan rejected (structure)"
        case .screening: return "Plan rejected (message screening)"
        case .present_but_not_honored: return "Plan present, not honored"
        }
    }

    static func subject(_ item: Components.Schemas.AttentionItem) -> SubjectLine {
        switch item.subject {
        case .run(let run), .proposal_batch(let run):
            return SubjectLine(lead: item.project_id, identifier: run.subject_id)
        case .project(let unscoped), .system(let unscoped):
            return SubjectLine(lead: unscoped.subject_id, identifier: nil)
        case .task(let task):
            return SubjectLine(lead: item.project_id, identifier: task.subject_id)
        }
    }

    /// Row context comes from the daemon's own labels (`display_names`), which
    /// carry whether each one is a chosen name or an identifier fallback. A
    /// legacy item without them falls back to its raw identifiers.
    package static func rowContext(_ item: Components.Schemas.AttentionItem) -> RowContext {
        let names = item.display_names?.value1
        let project = RowContext.Segment(
            value: nonempty(names?.project.text) ?? item.project_id,
            isIdentifier: names.map { $0.project.source == .identifier } ?? true
        )
        let workUnit: RowContext.Segment?
        switch item.subject {
        case .run(let run), .proposal_batch(let run):
            workUnit = .init(
                value: nonempty(names?.task.text) ?? run.subject_id,
                isIdentifier: names.map { $0.task.source == .identifier } ?? true,
                isAgentClaim: names?.task.source == .agent
            )
        case .project, .system:
            workUnit = nil
        case .task(let task):
            workUnit = .init(
                value: nonempty(names?.task.text) ?? task.task_id,
                isIdentifier: names.map { $0.task.source == .identifier } ?? true,
                isAgentClaim: names?.task.source == .agent)
        }
        return .init(project: project, workUnit: workUnit)
    }

    package static func copyableSubjectReference(
        _ item: Components.Schemas.AttentionItem
    ) -> CopyableSubjectReference? {
        switch item.subject {
        case .run(let run):
            return .init(label: "Copy run reference", value: run.subject_id)
        case .proposal_batch(let batch):
            return .init(label: "Copy proposal batch reference", value: batch.subject_id)
        case .task(let task):
            return .init(label: "Copy task reference", value: task.task_id)
        case .project, .system:
            return nil
        }
    }

    package static func relativeRowTime(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> String? {
        if item.status == .open, let due = item.expires_when {
            let remaining = due.timeIntervalSince(now)
            if abs(remaining) < 60 {
                return "due now"
            }
            if remaining > 0 {
                return "due \(relativeDuration(remaining))"
            }
            return "overdue \(relativeDuration(-remaining))"
        }
        guard let created = item.created_at else { return nil }
        return relativeDuration(max(0, now.timeIntervalSince(created)))
    }

    package static func relativeRowTime(_ date: Date, now: Date) -> String {
        relativeDuration(max(0, now.timeIntervalSince(date)))
    }

    package static func exactRowTimestamp(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> String? {
        if item.status == .open, let due = item.expires_when {
            return due.formatted(.iso8601)
        }
        return item.created_at?.formatted(.iso8601)
    }

    package static func showsLifecycleBadge(_ status: Components.Schemas.ItemStatus) -> Bool {
        status != .open
    }

    package static func showsDegradedBadge(_ item: Components.Schemas.AttentionItem) -> Bool {
        item.readiness?.value1._class == .ready_degraded
    }

    package static func uniqueEvidenceDigests(
        _ item: Components.Schemas.AttentionItem
    ) -> [String] {
        var seen: Set<String> = []
        return item.evidence_snapshot.compactMap { artifact in
            seen.insert(artifact.digest).inserted ? artifact.digest : nil
        }
    }

    static func attachmentDigestRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        var rows: [BindingRow] = []
        var representedDigests: Set<String> = []
        var seenEvidenceDigests: Set<String> = []
        var seenClaimDigests: Set<String> = []

        func append(_ label: String, _ digest: String, seen: inout Set<String>) {
            guard seen.insert(digest).inserted else { return }
            rows.append(.init(label: label, value: digest))
            representedDigests.insert(digest)
        }

        for artifact in item.evidence_snapshot {
            append("Evidence Digest", artifact.digest, seen: &seenEvidenceDigests)
        }
        for claim in item.agent_claims {
            // The specification's daemon-bound digest is the one a reader
            // pastes to verify the approval, so it names its own channel; every
            // other claim keeps the generic label. The row's value is unchanged.
            let label =
                claim.label == AgentClaimLabels.specification
                ? "Specification Digest" : "Claim Digest"
            append(label, claim.digest, seen: &seenClaimDigests)
        }
        for digest in item.artifact_digests {
            guard representedDigests.insert(digest).inserted else { continue }
            rows.append(.init(label: "Artifact Digest", value: digest))
        }
        return rows
    }

    /// Section 9's audit record for actions this client cannot faithfully
    /// collect and execute: they are omitted from the action surface and
    /// listed in the drill-down, so an audit still shows what the daemon
    /// asked for.
    package static func unavailableActionRows(
        _ actions: [Components.Schemas.Action]
    ) -> [BindingRow] {
        actions.map { .init(label: "Requested, Not Available Here", value: label($0)) }
    }

    package static func detailBindingRows(
        _ item: Components.Schemas.AttentionItem,
        priorProposalDigest: String? = nil,
        proposalDigest: String? = nil
    ) -> [BindingRow] {
        var rows: [BindingRow] = []
        if let posture = item.posture?.value1 {
            rows.append(.init(label: "Posture", value: label(posture)))
        }
        if let created = item.created_at {
            rows.append(.init(label: "Created", value: created.formatted(.iso8601)))
        }
        if let due = item.expires_when {
            rows.append(.init(label: "Due", value: due.formatted(.iso8601)))
        }
        switch item.subject {
        case .project(let unscoped), .system(let unscoped):
            rows.append(.init(label: "Subject", value: unscoped.subject_id))
        case .run, .proposal_batch, .task:
            break
        }
        if let wait = item.blocked_on?.value1 {
            rows.append(
                .init(label: "Waiting Since", value: wait.since.formatted(.iso8601)))
            if let blockingItem = wait.item_id {
                rows.append(.init(label: "Blocking Item", value: blockingItem))
            }
        }
        if let hold = item.publish_block?.value1.hold_reason?.value1 {
            rows.append(.init(label: "Hold Code", value: hold.rawValue))
        }
        rows.append(.init(label: "Item Version", value: "\(item.item_version)"))
        if !item.pr_head_sha.isEmpty {
            rows.append(.init(label: "PR Head", value: item.pr_head_sha))
        }
        // The diff's own base and head, which no other layer is guaranteed to
        // render: the checklist's "Bound to" row reads readiness_detail, which
        // a legacy or fake-mode item can omit, and it shows the readiness
        // candidate's coordinates rather than these. Audit coordinates belong
        // in the technical bindings, so the card can stop repeating them
        // without losing them (#1107).
        if let diff = item.diff_stats?.value1 {
            rows.append(.init(label: "Diff Base", value: diff.base_sha))
            rows.append(.init(label: "Diff Head", value: diff.head_sha))
        }
        rows.append(contentsOf: attachmentDigestRows(item))
        if let priorProposalDigest {
            rows.append(.init(label: "Prior Proposal", value: priorProposalDigest))
        }
        if let proposalDigest {
            rows.append(.init(label: "Proposal", value: proposalDigest))
        }
        rows.append(contentsOf: reviewRecoveryBindingRows(item))
        rows.append(contentsOf: reviewConfigurationRecoveryRows(item))
        rows.append(contentsOf: codexReenrollmentRecoveryRows(item))
        rows.append(contentsOf: findingAdjudicationRows(item))
        rows.append(contentsOf: readinessSummaryRows(item))
        rows.append(contentsOf: reviewYieldRows(item))
        if let drift = item.review_diminishing?.value1.drift_audit?.value1 {
            rows.append(.init(label: "Drift Audit", value: drift.audit_digest))
        }
        return rows
    }

    private static func nonempty(_ value: String?) -> String? {
        guard let value, !value.isEmpty else { return nil }
        return value
    }

    private static func relativeDuration(_ interval: TimeInterval) -> String {
        if interval < 3_600 {
            return "\(max(1, Int(interval / 60)))m"
        }
        if interval < 86_400 {
            return "\(Int(interval / 3_600))h"
        }
        return "\(Int(interval / 86_400))d"
    }

    static func reviewYieldRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let history = item.yield_history?.value1 else { return [] }
        var rows = history.rounds.map { round in
            BindingRow(
                label: "Review Round \(round.round)",
                value: "\(round.findings_ingested) findings · \(round.new_findings) new · "
                    + "\(round.recurring_findings) recurring · \(round.fixed) fixed · "
                    + "\(round.declined) declined · \(round.deferred) deferred · "
                    + reviewOutcomeLabel(round.outcome)
            )
        }
        rows.append(
            .init(
                label: "Terminal Review",
                value: reviewOutcomeLabel(history.terminal_outcome)))
        return rows
    }

    private static func reviewOutcomeLabel(
        _ outcome: Components.Schemas.ReviewOutcome
    ) -> String {
        switch outcome {
        case .clean: return "Clean"
        case .findings: return "Findings"
        }
    }

    static func readinessSummaryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let readiness = item.readiness?.value1 else { return [] }
        let verdict: String
        switch readiness._class {
        case .ready_clean: verdict = "Clean"
        case .ready_degraded: verdict = "Degraded"
        }
        var rows = [
            BindingRow(label: "Readiness", value: verdict),
            BindingRow(label: "Evaluation Set", value: readiness.evaluation_set_digest),
        ]
        guard let detail = item.readiness_detail?.value1 else { return rows }
        rows.append(BindingRow(label: "Bound Head", value: detail.candidate_head))
        rows.append(
            BindingRow(label: "Bound Base", value: "\(detail.base.base_ref)@\(detail.base.base_sha)"))
        for requirement in detail.requirements {
            var value = [
                label(requirement.check_class), label(requirement.kind), label(requirement.state),
            ].joined(separator: ", ")
            if let proof = requirement.proof_recipe_digest?.value1 {
                value += ", proof \(proof)"
            }
            rows.append(BindingRow(label: "Requirement \(requirement.requirement_key)", value: value))
            if let waiver = requirement.waiver?.value1 {
                rows.append(
                    BindingRow(
                        label: "Waiver \(waiver.id)",
                        value:
                            "\(waiver.dimension), \(label(waiver.authority)), "
                            + waiver.granted_at.formatted(.iso8601)))
            }
        }
        return rows
    }

    /// A revision shortened for a card row to the eight characters every
    /// binding line uses; Technical Bindings keeps the full daemon value. Only a
    /// hex object name is abbreviated. The other
    /// coordinates a readiness invalidation carries on its axis, a base ref
    /// and a "repository_id#pr_number" identity, put their meaning in the
    /// tail, so truncating them can render two different values identically
    /// and hide the very change the row exists to name.
    package static func shortRevision(_ value: String) -> String {
        guard value.count > 8, value.allSatisfy(\.isHexDigit) else { return value }
        return String(value.prefix(8))
    }

    package static func label(_ state: Components.Schemas.ReadinessRequirementState) -> String {
        switch state {
        case .passed: return "Passed"
        case .failed: return "Failed"
        case .not_run: return "Not run"
        case .not_applicable: return "Not applicable"
        }
    }

    static func label(_ checkClass: Components.Schemas.VerificationCheckClass) -> String {
        switch checkClass {
        case .clean_verification: return "Clean verification"
        case .independent_review: return "Independent review"
        case .repo_change_policy: return "Repo change policy"
        }
    }

    static func label(_ kind: Components.Schemas.RequirementKind) -> String {
        switch kind {
        case .required: return "Required"
        case .optional: return "Optional"
        }
    }

    package static func label(_ authority: Components.Schemas.WaiverGrantingAuthority) -> String {
        switch authority {
        case .explicit_human_approval: return "Explicit human approval"
        case .daemon_trusted_configuration: return "Daemon trusted configuration"
        }
    }

    package static func label(_ reason: Components.Schemas.ReadinessInvalidationReason) -> String {
        switch reason {
        case .head_changed: return "Head changed"
        case .base_advanced: return "Base advanced"
        case .retargeted: return "Retargeted"
        case .identity_changed: return "Identity changed"
        }
    }

    static func findingAdjudicationRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.finding_adjudication?.value1 else { return [] }
        return [
            BindingRow(label: "Adjudication Digest", value: binding.adjudication_digest),
            BindingRow(label: "Adjudication Run", value: binding.run_id),
            BindingRow(label: "Adjudication Round", value: "\(binding.round)"),
        ]
    }

    /// A route named by the outcome accepting it produces (#1551). Every
    /// route that parks the run starts with "Park". The daemon writes the
    /// same labels into the card's reason (`domain.AdjudicationRouteLabel`
    /// in daemon/internal/domain/finding_adjudication.go); change both.
    package static func label(_ route: Components.Schemas.AdjudicationRoute) -> String {
        switch route {
        case .remediate: return "Fix in this PR"
        case .park_revision: return "Park: revise the work unit"
        case .park_separate_work: return "Park: needs separate work"
        case .attention_human_decision: return "Park: needs a human decision"
        case .park_unknown: return "Park: can't tell where it belongs"
        case ._defer: return "Defer to later work"
        case .decline: return "Decline the finding"
        case .dispute: return "Park: dispute the finding"
        case .attention_unclear: return "Park: unclear if the goal needs it"
        }
    }

    static func label(_ site: Components.Schemas.JudgmentSite) -> String {
        switch site {
        case .finding_adjudicator: return "Finding adjudicator"
        case .task_namer: return "Task namer"
        }
    }

    static func label(_ relationship: Components.Schemas.GoalRelationship) -> String {
        switch relationship {
        case .required: return "Required"
        case .adjacent: return "Adjacent"
        case .contradictory: return "Contradictory"
        case .unclear: return "Unclear"
        }
    }

    package static func label(_ confidence: Components.Schemas.AdjudicationConfidence) -> String {
        switch confidence {
        case .low: return "Low"
        case .medium: return "Medium"
        case .high: return "High"
        }
    }

    /// Renders a finding location the way the daemon's canonical location string
    /// does: "path" for a whole-file location, "path:line" for a single line, and
    /// "path:start-end" for a range.
    package static func findingLocation(_ location: Components.Schemas.FindingLocation) -> String {
        if location.start_line == 0 && location.end_line == 0 {
            return location.path
        }
        if location.start_line == location.end_line {
            return "\(location.path):\(location.start_line)"
        }
        return "\(location.path):\(location.start_line)-\(location.end_line)"
    }

    /// `unverifiedKeyword` is the label without its "(unverified)" word, for
    /// a producer whose label carries one, so a card can draw the word
    /// through `UnverifiedLabel` with its explanation (visual audit D03).
    package static func adjudicationProducerPresentation(
        _ producer: Components.Schemas.AdjudicationProducer
    ) -> (label: String, modelBacked: Bool, unverifiedKeyword: String?) {
        switch producer {
        case .engine: return ("Daemon recommendation", false, nil)
        case .human: return ("Human decision", false, nil)
        case .model: return ("Model proposal (unverified)", true, "Model proposal")
        case .engine_model:
            return ("Model judgment with engine-authorized remediation", true, nil)
        }
    }

    static func label(_ compatibility: Components.Schemas.WorkUnitCompatibility?) -> String {
        guard let compatibility else { return "Not assessed" }
        switch compatibility {
        case .allowed: return "Allowed"
        case .work_unit_revision_required: return "Work-unit revision required"
        case .separate_work_required: return "Separate work required"
        case .human_decision_required: return "Human decision required"
        case .unknown: return "Unknown"
        }
    }

    static func reviewRecoveryBindingRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.review_recovery_binding?.value1 else { return [] }
        return [
            BindingRow(label: "Recovery Run", value: binding.run_id),
            BindingRow(label: "Invocation", value: binding.invocation_id),
            BindingRow(label: "Round", value: "\(binding.round)"),
            BindingRow(label: "Base", value: binding.base_sha),
            BindingRow(label: "Head", value: binding.head_sha),
            BindingRow(label: "Failure Digest", value: binding.failure_digest),
        ]
    }

    static func reviewConfigurationRecoveryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.review_configuration_recovery?.value1 else { return [] }
        return [
            BindingRow(label: "Recovery Run", value: binding.run_id),
            BindingRow(label: "Invocation", value: binding.invocation_id),
            BindingRow(label: "Round", value: "\(binding.round)"),
            BindingRow(label: "Base", value: binding.base_sha),
            BindingRow(label: "Head", value: binding.head_sha),
            BindingRow(label: "Failure Digest", value: binding.failure_digest),
            BindingRow(label: "Repository", value: binding.repo),
            BindingRow(label: "Superseded Profile", value: binding.superseded_profile_digest),
        ]
    }

    static func codexReenrollmentRecoveryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.codex_reenrollment_recovery_binding?.value1 else { return [] }
        return [
            BindingRow(label: "Auth Identity", value: binding.auth_identity_id),
            BindingRow(label: "Lease Fence", value: "\(binding.lease_fence)"),
            BindingRow(label: "Auth Store Digest", value: binding.auth_store_digest),
            BindingRow(
                label: "Token Expires",
                value: binding.access_token_expires_at.formatted(.iso8601)
            ),
        ]
    }
}
