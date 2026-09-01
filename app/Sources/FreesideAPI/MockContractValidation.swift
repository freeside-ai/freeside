import Crypto
import Foundation

/// Pure, state-free mirror of the daemon's contract validation over the
/// generated wire shapes, extracted from `MockServer` (#205) so the actor
/// changes only for state and transitions. Each function is an input →
/// verdict predicate the actor calls during reconstruction and submission
/// (domain.AttentionItem.Validate, TimingSummary.Validate,
/// AttentionDelivery.Validate, signet's per-type action policy, and the
/// command structure Signet checks before lookup plus the action input it
/// checks after command-id replay). Nothing here reads actor state.
/// `snapshotBreach` takes the approved-recipe set as a parameter so the
/// actor's bridge can pass its own policy set. `MockContractValidationTests`
/// pins each in isolation.
enum MockContractValidation {
    static func runSnapshotBreach(
        _ snapshot: Components.Schemas.RunSnapshot, serverRevision: Int64
    ) -> String? {
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 || snapshot.as_of_revision > serverRevision {
            return "as_of_revision outside the server revision"
        }
        let run = snapshot.run
        if run.id.isEmpty || run.project_id.isEmpty { return "empty run identity" }
        if run.spec_digest.isEmpty || run.policy_digest.isEmpty { return "empty run digest" }
        if let names = run.display_names?.value1,
            let breach = displayNamesBreach(names)
        {
            return breach
        }
        switch (run.campaign_id, run.attempt_number, run.attempt_reason, run.parent_run_id) {
        case (nil, nil, nil, nil):
            break
        case (let campaign?, 1?, nil, nil) where !campaign.isEmpty:
            break
        case (let campaign?, let number?, let reason?, let parent?)
        where !campaign.isEmpty && number >= 2 && !parent.isEmpty
            && reason == reason.trimmingCharacters(in: .whitespacesAndNewlines)
            && !reason.isEmpty:
            break
        default:
            return "inconsistent production attempt lineage"
        }
        var stageIDs = Set<String>()
        var invocationIDs = Set<String>()
        for stage in run.stages {
            if stage.id.isEmpty || stage.run_id != run.id || stage.name.isEmpty {
                return "invalid stage binding"
            }
            if !stageIDs.insert(stage.id).inserted { return "duplicate stage id" }
            for (index, attempt) in stage.attempts.enumerated() {
                if attempt.id.isEmpty || attempt.stage_id != stage.id
                    || attempt.number != index + 1 || attempt.invocation_id.isEmpty
                {
                    return "invalid stage attempt"
                }
                if !invocationIDs.insert(attempt.invocation_id).inserted {
                    return "duplicate invocation id"
                }
            }
        }
        return nil
    }

    /// Field-for-field mirror of domain.AttentionItem.Validate over the
    /// generated shapes. Checks the schema already makes unrepresentable
    /// are omitted: invalid enum members, an agent producer class in
    /// evidence, mixed provenance branches, a run_id on an unscoped
    /// subject, and caller-set publish_eligible. Recipe approval is
    /// runtime policy: snapshotBreach re-runs that gate against the
    /// server's approved set, since Validate holds no policy.
    static func itemValidityBreach(
        _ item: Components.Schemas.AttentionItem
    ) -> String? {
        if item.id.isEmpty { return "empty id" }
        if item.project_id.isEmpty { return "empty project_id" }
        switch item.subject {
        case .run(let scoped), .proposal_batch(let scoped):
            if scoped.subject_id.isEmpty { return "empty subject_id" }
            if let runID = scoped.run_id, runID.isEmpty { return "empty run_id" }
        case .project(let unscoped), .system(let unscoped):
            if unscoped.subject_id.isEmpty { return "empty subject_id" }
        }
        if let conversation = item.conversation_id, conversation.isEmpty {
            return "empty conversation_id"
        }
        if let created = item.created_at, created.timeIntervalSince1970 < -62_000_000_000 {
            return "zero created_at"
        }
        if let expires = item.expires_when, expires.timeIntervalSince1970 < -62_000_000_000 {
            return "zero expires_when"
        }
        if let decided = item.decided_at, decided.timeIntervalSince1970 < -62_000_000_000 {
            return "zero decided_at"
        }
        if let reference = item.pr_reference?.value1 {
            if item._type != .ready_for_final_review {
                return "pr_reference on a non-ready_for_final_review item"
            }
            let parts = reference.repo.split(separator: "/", omittingEmptySubsequences: false)
            if parts.count != 2 || parts.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." }) {
                return "invalid pr_reference repo"
            }
            if reference.number < 1 { return "non-positive pr_reference number" }
        } else if item._type == .ready_for_final_review {
            return "ready_for_final_review item lacks pr_reference"
        }
        if let readiness = item.readiness?.value1 {
            if item._type != .ready_for_final_review {
                return "readiness on a non-ready_for_final_review item"
            }
            if readiness.evaluation_set_digest.isEmpty {
                return "empty readiness evaluation_set_digest"
            }
        }
        if let history = item.yield_history?.value1 {
            if item._type != .ready_for_final_review
                && item._type != .review_diminishing_returns
            {
                return "yield_history on an unsupported item type"
            }
            if history.rounds.isEmpty { return "empty review yield history" }
            var previousRound = 0
            for round in history.rounds {
                if round.round < 1 || round.round <= previousRound {
                    return "invalid review yield round order"
                }
                let counts = [
                    round.findings_ingested, round.new_findings, round.recurring_findings,
                    round.fixed, round.declined, round.deferred,
                ]
                if counts.contains(where: { $0 < 0 }) {
                    return "negative review yield count"
                }
                if round.new_findings + round.recurring_findings != round.findings_ingested
                    || round.fixed + round.declined + round.deferred > round.findings_ingested
                    || (round.round == 1 && round.recurring_findings != 0)
                {
                    return "inconsistent review yield totals"
                }
                if (round.outcome == .clean) != (round.findings_ingested == 0) {
                    return "review yield outcome disagrees with findings"
                }
                previousRound = round.round
            }
            if history.terminal_outcome != history.rounds.last?.outcome {
                return "review yield terminal outcome disagrees with final round"
            }
        }
        // commit_plan_notice mirrors the domain's optional daemon-derived
        // reason (#222): the generated closed enum makes the daemon's
        // ErrInvalidCommitPlanNotice arm unrepresentable here (an unknown
        // reason fails decode), and nil is a valid absence, so no check
        // remains to mirror.
        if item.item_version < 1 { return "non-positive item_version" }
        // Posture mirrors the domain's explicit system_health-only admission
        // effect. The generated enum makes an unknown posture unrepresentable.
        if let posture = item.posture?.value1 {
            if item._type != .system_health {
                return "posture on a non-system_health item"
            }
            if item.blocking_supersession != nil, posture != .blocking {
                return "blocking_supersession on an advisory system_health item"
            }
        } else if item._type == .system_health {
            return "system_health item lacks posture"
        }
        // blocking_supersession mirrors the domain's typed §4 condition
        // (#319/#321): legal only on system_health, and its payload must
        // name a positive repository id. The generated closed kind enum
        // makes the daemon's unknown-kind arm unrepresentable here (an
        // unknown kind fails decode).
        if let condition = item.blocking_supersession {
            if item._type != .system_health {
                return "blocking_supersession on a non-system_health item"
            }
            if condition.value1.repository_id < 1 {
                return "non-positive blocking_supersession repository_id"
            }
        }
        if let recovery = item.review_recovery_binding?.value1 {
            if item._type != .review_contradiction {
                return "review_recovery_binding on a non-review_contradiction item"
            }
            if recovery.run_id.isEmpty || recovery.invocation_id.isEmpty
                || recovery.base_sha.isEmpty || recovery.head_sha.isEmpty
                || recovery.failure_digest.isEmpty
            {
                return "empty review_recovery_binding field"
            }
            if recovery.round < 1 { return "non-positive review recovery round" }
            guard case .run(let subject) = item.subject,
                subject.subject_id == recovery.run_id,
                subject.run_id == recovery.run_id,
                item.pr_head_sha == recovery.head_sha
            else {
                return "review recovery binding disagrees with item subject or head"
            }
        } else if item._type == .review_contradiction {
            return "review_contradiction item lacks review_recovery_binding"
        }
        if let recovery = item.codex_reenrollment_recovery_binding?.value1 {
            if item._type != .system_health {
                return "codex_reenrollment_recovery_binding on a non-system_health item"
            }
            if recovery.auth_identity_id.isEmpty || recovery.auth_store_digest.isEmpty {
                return "empty codex_reenrollment_recovery_binding field"
            }
            if recovery.lease_fence < 1 {
                return "non-positive codex re-enrollment lease fence"
            }
            if !item.requested_decision.contains(.resolve_reenrollment) {
                return "codex re-enrollment binding lacks resolve_reenrollment"
            }
        } else if item.requested_decision.contains(.resolve_reenrollment) {
            return "resolve_reenrollment lacks codex re-enrollment binding"
        }
        if let recovery = item.review_configuration_recovery?.value1 {
            if item._type != .review_configuration {
                return "review_configuration_recovery on a non-review_configuration item"
            }
            if recovery.run_id.isEmpty || recovery.invocation_id.isEmpty
                || recovery.base_sha.isEmpty || recovery.head_sha.isEmpty
                || recovery.failure_digest.isEmpty || recovery.repo.isEmpty
                || recovery.superseded_profile_digest.isEmpty
            {
                return "empty review_configuration_recovery field"
            }
            if recovery.round < 1 { return "non-positive review configuration recovery round" }
            if recovery.repository_id < 1 {
                return "non-positive review configuration recovery repository_id"
            }
            guard case .run(let subject) = item.subject,
                subject.subject_id == recovery.run_id,
                subject.run_id == recovery.run_id,
                item.pr_head_sha == recovery.head_sha
            else {
                return "review configuration recovery disagrees with item subject or head"
            }
        } else if item._type == .review_configuration {
            return "review_configuration item lacks review_configuration_recovery"
        }
        if let binding = item.finding_adjudication?.value1 {
            if item._type != .finding_adjudication {
                return "finding_adjudication binding on a different item type"
            }
            if binding.run_id.isEmpty || binding.round < 1 || binding.adjudication_digest.isEmpty {
                return "invalid finding_adjudication binding coordinates"
            }
            guard case .run(let subject) = item.subject,
                subject.subject_id == binding.run_id,
                subject.run_id == binding.run_id
            else {
                return "finding_adjudication binding disagrees with item subject"
            }
            if binding.proposals.isEmpty { return "finding_adjudication has no proposals" }
            var findingIDs = Set<String>()
            var hasOfferedAlternative = false
            for proposal in binding.proposals {
                if proposal.finding_id.isEmpty
                    || proposal.rationale.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                {
                    return "finding_adjudication proposal has an empty required field"
                }
                // finding_message may be empty (an unfingerprintable finding), but a
                // present finding_location must be well-formed exactly as the domain
                // requires: a non-empty path and either the whole-file marker (0,0) or
                // a positive, non-inverted range.
                if let location = proposal.finding_location?.value1 {
                    if location.path.isEmpty {
                        return "finding_adjudication proposal has an empty finding location path"
                    }
                    let wholeFile = location.start_line == 0 && location.end_line == 0
                    if !wholeFile
                        && (location.start_line < 1 || location.end_line < 1
                            || location.start_line > location.end_line)
                    {
                        return "finding_adjudication proposal has an invalid finding location range"
                    }
                }
                if !findingIDs.insert(proposal.finding_id).inserted {
                    return "finding_adjudication has duplicate finding ids"
                }
                if !validAdjudicationRoute(
                    goal: proposal.goal_relationship,
                    compatibility: proposal.compatibility?.value1,
                    route: proposal.route
                ) {
                    return "finding_adjudication proposal has incompatible axes and route"
                }
                switch proposal.producer {
                case .model:
                    if proposal.confidence == nil {
                        return "finding_adjudication model proposal lacks confidence"
                    }
                    if proposal.compatibility?.value1 == .allowed {
                        return "finding_adjudication model proposal mints allowed"
                    }
                case .engine:
                    if proposal.confidence != nil {
                        return "finding_adjudication engine proposal carries confidence"
                    }
                    if proposal.goal_relationship != .required
                        || proposal.compatibility?.value1 != .allowed
                        || proposal.route != .remediate
                    {
                        return "finding_adjudication engine proposal is not the deterministic fast path"
                    }
                case .engine_model:
                    if proposal.confidence == nil {
                        return "finding_adjudication mixed-origin proposal lacks confidence"
                    }
                    if proposal.goal_relationship != .required
                        || proposal.compatibility?.value1 != .allowed
                        || proposal.route != .remediate
                    {
                        return "finding_adjudication mixed-origin proposal is not allowed remediation"
                    }
                }
                var alternativeRoutes = Set<Components.Schemas.AdjudicationRoute>()
                for alternative in proposal.offered_alternatives {
                    hasOfferedAlternative = true
                    if alternative.route == proposal.route {
                        return "finding_adjudication alternative repeats the recommended route"
                    }
                    if isBlank(alternative.consequence) {
                        return "finding_adjudication alternative has an empty consequence"
                    }
                    if !validAdjudicationRoute(
                        goal: proposal.goal_relationship,
                        compatibility: proposal.compatibility?.value1,
                        route: alternative.route
                    ) {
                        return "finding_adjudication alternative is incompatible with proposal axes"
                    }
                    if !alternativeRoutes.insert(alternative.route).inserted {
                        return "finding_adjudication has duplicate alternative routes"
                    }
                }
            }
            if item.requested_decision.contains(.choose_alternative_route)
                && !hasOfferedAlternative
            {
                return "finding_adjudication has no offered alternatives"
            }
        } else if item._type == .finding_adjudication {
            return "finding_adjudication item lacks its binding"
        }
        if let names = item.display_names?.value1,
            let breach = displayNamesBreach(names)
        {
            return breach
        }
        if let cost = item.billable_cost_so_far?.value1 {
            if item._type != .review_diminishing_returns {
                return "billable_cost_so_far on a different item type"
            }
            if cost.currency.range(of: #"^[A-Z]{3}$"#, options: .regularExpression) == nil
                || cost.amount.range(
                    of: #"^(0|[1-9][0-9]*)(\.[0-9]+)?$"#,
                    options: .regularExpression) == nil
                || cost.invocations < 1
            {
                return "invalid billable_cost_so_far"
            }
        }
        if let failure = item.execution_failure?.value1 {
            if item._type != .execution_failure {
                return "execution_failure facts on a different item type"
            }
            if failure.invocation_id.isEmpty {
                return "empty execution_failure invocation_id"
            }
            guard case .run(let subject) = item.subject, subject.run_id != nil else {
                return "execution_failure facts on a non-run subject"
            }
            let offers = failure.offered_manifests ?? []
            if offers.map(\.name) != offers.map(\.name).sorted()
                || Set(offers.map(\.name)).count != offers.count
                || Set(offers.map(\.digest)).count != offers.count
                || offers.contains(where: {
                    $0.name.isEmpty
                        || $0.digest
                            != capabilityManifestDigest(
                                name: $0.name, egressProfile: $0.egress_profile.rawValue)
                })
            {
                return "invalid execution_failure capability manifests"
            }
            if item.requested_decision.contains(.retry_with_capabilities) != !offers.isEmpty {
                return "retry_with_capabilities does not match offered manifests"
            }
        }
        if let block = item.publish_block?.value1 {
            if item._type != .publish_blocked {
                return "publish_block facts on a different item type"
            }
            if (block.hold_reason == nil) == (block.trust_rule == nil) {
                return "publish_block does not have exactly one variant"
            }
        }
        if let stats = item.diff_stats?.value1 {
            if item._type != .ready_for_final_review {
                return "diff_stats on a different item type"
            }
            if stats.files_changed < 0 || stats.additions < 0 || stats.deletions < 0
                || stats.base_sha.isEmpty || stats.head_sha.isEmpty
            {
                return "invalid diff_stats"
            }
            if stats.head_sha != item.pr_head_sha {
                return "diff_stats head disagrees with item head"
            }
        }
        if let wait = item.blocked_on?.value1 {
            if item._type != .blocked {
                return "blocked_on facts on a different item type"
            }
            if wait.since == daemonZeroInstant {
                return "blocked_on has an unset since"
            }
            switch wait.kind {
            case .spec_approval:
                if wait.item_id?.isEmpty != false || wait.pr_reference != nil {
                    return "blocked_on reference disagrees with its kind"
                }
            case .pr_checks, .external_review:
                guard wait.item_id == nil, let reference = wait.pr_reference?.value1 else {
                    return "blocked_on reference disagrees with its kind"
                }
                let parts = reference.repo.split(separator: "/", omittingEmptySubsequences: false)
                if parts.count != 2
                    || parts.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." })
                    || reference.number < 1
                {
                    return "invalid blocked_on pull request reference"
                }
            }
            if let created = item.created_at, wait.since > created {
                return "blocked_on starts after item creation"
            }
        }
        if let diagnostic = item.health_diagnostic?.value1 {
            if item._type != .system_health {
                return "health_diagnostic on a different item type"
            }
            if diagnostic.code.range(
                of: #"^[a-z0-9][a-z0-9_.-]*$"#, options: .regularExpression) == nil
            {
                return "invalid health_diagnostic code"
            }
        }
        if let dispute = item.review_dispute?.value1 {
            if item._type != .review_dispute {
                return "review_dispute binding on a different item type"
            }
            if dispute.run_id.isEmpty || dispute.round < 1 || dispute.finding_ids.isEmpty
                || dispute.completion_evidence.isEmpty
                || dispute.finding_ids.contains(where: \.isEmpty)
                || Set(dispute.finding_ids).count != dispute.finding_ids.count
            {
                return "invalid review_dispute binding"
            }
            guard case .run(let subject) = item.subject,
                subject.subject_id == dispute.run_id,
                subject.run_id == dispute.run_id
            else {
                return "review_dispute binding disagrees with item subject"
            }
        }
        if let revision = item.spec_revision?.value1 {
            if item._type != .spec_approval {
                return "spec_revision facts on a different item type"
            }
            if revision.iteration < 2 || revision.prior_item_id.isEmpty
                || revision.prior_item_id == item.id
                || revision.prior_spec_artifact_id.isEmpty || revision.prior_spec_digest.isEmpty
                || revision.prior_comments.isEmpty || revision.prior_comments.count > 64
                || revision.claimed_addressals.count > 64 || revision.addressals_digest.isEmpty
                || revision.diff.lines_added < 0 || revision.diff.lines_removed < 0
                || isBlank(revision.diff.unified)
                || revision.diff.unified.lengthOfBytes(using: .utf8) > 65_536
            {
                return "invalid spec_revision facts"
            }
            var commentIDs = Set<String>()
            var artifactIDs = Set<String>()
            var previousIteration = 0
            for comment in revision.prior_comments {
                if isBlank(comment.comment_id) || comment.artifact_id.isEmpty
                    || comment.artifact_id != "spec-feedback-\(comment.comment_id)"
                    || comment.digest.isEmpty || comment.raised_on_item_id.isEmpty
                    || comment.iteration < 1 || comment.iteration >= revision.iteration
                    || comment.iteration <= previousIteration
                    || isBlank(comment.body)
                    || comment.body.lengthOfBytes(using: .utf8) > 8192
                    || sha256Digest(of: comment.body) != comment.digest
                    || !commentIDs.insert(comment.comment_id).inserted
                    || !artifactIDs.insert(comment.artifact_id).inserted
                {
                    return "invalid spec_revision comment"
                }
                previousIteration = comment.iteration
            }
            if revision.prior_comments.last?.raised_on_item_id != revision.prior_item_id {
                return "spec_revision prior item mismatch"
            }
            var addressed = Set<String>()
            for addressal in revision.claimed_addressals {
                if isBlank(addressal.comment_id) || isBlank(addressal.response)
                    || addressal.response.lengthOfBytes(using: .utf8) > 8192
                    || !commentIDs.contains(addressal.comment_id)
                    || !addressed.insert(addressal.comment_id).inserted
                {
                    return "invalid spec_revision addressal"
                }
            }
            if addressalsDigest(revision.claimed_addressals) != revision.addressals_digest {
                return "spec_revision addressals digest mismatch"
            }
            let claims = item.agent_claims.filter { $0.label == "Addressals" }
            if claims.count != 1 || claims[0].digest != revision.addressals_digest {
                return "spec_revision Addressals claim mismatch"
            }
        }
        if item.decision_surface.epoch < 1 {
            return "non-positive decision_surface epoch"
        }
        if item.decision_surface.digest.isEmpty {
            return "empty decision_surface digest"
        }
        if let recommendation = item.recommendation?.value1 {
            if !item.requested_decision.contains(recommendation.action) {
                return "recommendation action is not offered"
            }
            if recommendation.reason.isEmpty {
                return "empty recommendation reason"
            }
            let variants = [
                recommendation.provenance.daemon_policy != nil,
                recommendation.provenance.agent_judgment != nil,
                recommendation.provenance.project_policy != nil,
            ].filter { $0 }.count
            if variants != 1 {
                return "recommendation provenance does not have exactly one variant"
            }
            switch recommendation.source {
            case .daemon_policy:
                guard let provenance = recommendation.provenance.daemon_policy?.value1 else {
                    return "recommendation source and provenance disagree"
                }
                if provenance.rule_digest.isEmpty || provenance.input_digest.isEmpty {
                    return "empty daemon-policy recommendation digest"
                }
            case .agent_judgment:
                guard let provenance = recommendation.provenance.agent_judgment?.value1 else {
                    return "recommendation source and provenance disagree"
                }
                if provenance.invocation_id.isEmpty || provenance.artifact_digest.isEmpty {
                    return "empty agent-judgment recommendation field"
                }
                if !item.artifact_digests.contains(provenance.artifact_digest) {
                    return "recommendation artifact is not bound to the item"
                }
            case .project_policy:
                guard let provenance = recommendation.provenance.project_policy?.value1 else {
                    return "recommendation source and provenance disagree"
                }
                if provenance.policy_key.isEmpty || provenance.resolved_policy_digest.isEmpty
                    || provenance.application_digest.isEmpty
                {
                    return "empty project-policy recommendation field"
                }
            }
        }
        // An empty requested_decision is structurally valid (#96): which
        // types must offer an action is signet policy (itemPolicyBreach).
        if let breach = timingBreach(item.timing) { return breach }
        var evidenceIDs = Set<String>()
        for artifact in item.evidence_snapshot {
            if artifact.id.isEmpty { return "empty evidence artifact id" }
            if artifact.digest.isEmpty { return "empty evidence digest" }
            switch artifact.provenance {
            case .head_bound(let bound):
                if bound.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if bound.source_head_sha.isEmpty { return "empty source_head_sha" }
                if bound.verification_recipe_digest.isEmpty { return "empty recipe digest" }
                // Head-bound evidence must match the head the item names;
                // head-independent evidence is exempt by design (§5.15).
                if !item.pr_head_sha.isEmpty, bound.source_head_sha != item.pr_head_sha {
                    return "head-bound evidence names a different head than the item"
                }
            case .head_independent(let free):
                if free.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if free.verification_recipe_digest.isEmpty { return "empty recipe digest" }
            }
            if !evidenceIDs.insert(artifact.id).inserted {
                return "duplicate evidence artifact id"
            }
        }
        // An artifact id is a content address: it maps to one digest and
        // never spans the two trust channels.
        var claimDigests: [String: String] = [:]
        for claim in item.agent_claims {
            if claim.label.isEmpty { return "empty claim label" }
            if claim.artifact_id.isEmpty { return "empty claim artifact_id" }
            if claim.digest.isEmpty { return "empty claim digest" }
            // Claim provenance is agent-pinned by the schema's producer enum,
            // but the generated recipe-digest container accepts any JSON
            // value, so the representable invariants to check are the
            // non-empty fields domain.Provenance.Validate requires plus the
            // agent-never-recipe-bound rule (agent + non-null digest is
            // ErrProvenanceInconsistent on the daemon). Claims are not
            // head-matched against the item; only evidence is (§5.15).
            switch claim.provenance {
            case .head_bound(let bound):
                if bound.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if bound.source_head_sha.isEmpty { return "empty source_head_sha" }
                if bound.verification_recipe_digest?.value != nil {
                    return "claim recipe digest must be null"
                }
            case .head_independent(let free):
                if free.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if free.verification_recipe_digest?.value != nil {
                    return "claim recipe digest must be null"
                }
            }
            // Mirrors the daemon's text-carrier checks (#217). The generated
            // media_type enum and Swift's always-valid-UTF-8 String make the
            // daemon's ErrInvalidClaimMediaType and ErrClaimTextNotUTF8 arms
            // unrepresentable here; what stays checkable is the sensitivity
            // bar, the non-empty content, the inline size cap, and the
            // binding rule that the claim's digest is the content's address.
            if let text = claim.text {
                // Inline content is barred from the high-sensitivity tier:
                // CachedState persists item metadata to disk, so memory-only
                // prose travels the referenced attachment path (§5.14).
                let sensitivity: Components.Schemas.SensitivityClass
                switch claim.provenance {
                case .head_bound(let bound): sensitivity = bound.sensitivity_class
                case .head_independent(let free): sensitivity = free.sensitivity_class
                }
                if sensitivity == .high_sensitivity {
                    return "high-sensitivity claim carries inline text"
                }
                if text.content.isEmpty { return "empty claim text content" }
                // Mirrors domain.MaxClaimTextBytes (64 KiB over UTF-8 bytes).
                if text.content.utf8.count > 65536 {
                    return "claim text exceeds the inline size cap"
                }
                if sha256Digest(of: text.content) != claim.digest {
                    return "claim digest does not match its text content"
                }
            }
            if evidenceIDs.contains(claim.artifact_id) {
                return "claim reuses an evidence artifact id"
            }
            if let existing = claimDigests[claim.artifact_id], existing != claim.digest {
                return "one claim artifact id maps to two digests"
            }
            claimDigests[claim.artifact_id] = claim.digest
        }
        let bindingDigests = item.finding_adjudication.map { [$0.value1.adjudication_digest] } ?? []
        let union = Array(
            Set(item.evidence_snapshot.map(\.digest) + item.agent_claims.map(\.digest) + bindingDigests)
        ).sorted()
        if item.artifact_digests != union {
            return "artifact_digests is not the canonical union of rendered digests"
        }
        return nil
    }

    private static func isBlank(_ value: String) -> Bool {
        value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    /// Mirrors signet's validateRequestedActions over the authoritative
    /// per-type table (phase1ActionSets matches the merged policy):
    /// blocked is read-only and must offer the empty set (#96); every
    /// other type must offer at least one action from its allowed set.
    static func itemPolicyBreach(
        _ item: Components.Schemas.AttentionItem
    ) -> String? {
        guard let allowed = AttentionFixtures.phase1ActionSets[item._type] else {
            return "unknown attention type \(item._type.rawValue)"
        }
        if item.requested_decision.isEmpty {
            if item._type == .blocked { return nil }
            return "no offered actions"
        }
        // blocked's allowed set is empty, so any offered action on a
        // blocked item fails here, exactly as signet rejects it.
        if let stray = item.requested_decision.first(where: { !allowed.contains($0) }) {
            return "action \(stray.rawValue) is not allowed for \(item._type.rawValue)"
        }
        return nil
    }

    static func validate(_ command: Components.Schemas.ClientCommand) throws {
        try validateStructure(command)
        try validateActionInput(command)
    }

    static func validateStructure(_ command: Components.Schemas.ClientCommand) throws {
        func malformed(_ reason: String) -> MockServer.MalformedCommandError {
            MockServer.MalformedCommandError(commandID: command.command_id, reason: reason)
        }
        guard !command.command_id.isEmpty else { throw malformed("empty command_id") }
        guard !command.device_id.isEmpty else { throw malformed("empty device_id") }
        guard !command.payload.item_id.isEmpty else { throw malformed("empty item_id") }
        guard command.payload.item_version >= 1 else {
            throw malformed("non-positive item_version")
        }
        guard command.expected_entity_version >= 1 else {
            throw malformed("non-positive expected_entity_version")
        }
        guard !command.payload.artifact_digests.contains("") else {
            throw malformed("empty artifact digest")
        }
        // Attachments mirror domain.NewCommand: entries are content
        // addresses (empty is malformed) and a repeat is rejected rather
        // than deduplicated, since order is authored content the daemon
        // never canonicalizes.
        if let attachments = command.payload.attachments {
            guard !attachments.contains("") else {
                throw malformed("empty attachment digest")
            }
            guard Set(attachments).count == attachments.count else {
                throw malformed("duplicate attachment digest")
            }
        }
    }

    static func validateActionInput(_ command: Components.Schemas.ClientCommand) throws {
        func malformed(_ reason: String) -> MockServer.MalformedCommandError {
            MockServer.MalformedCommandError(commandID: command.command_id, reason: reason)
        }
        switch command.payload.action {
        case .retry_with_capabilities:
            guard let digest = command.payload.capability_manifest_digest?.value1,
                !digest.isEmpty,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.alternative_choices == nil
            else { throw malformed("invalid capability manifest selection") }
        case .discuss:
            guard let message = command.payload.message,
                !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
                command.payload.capability_manifest_digest == nil,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.alternative_choices == nil
            else { throw malformed("invalid discuss message") }
        case .request_changes:
            guard let message = command.payload.message,
                !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
                message.lengthOfBytes(using: .utf8) <= 8192,
                command.command_id.lengthOfBytes(using: .utf8) <= 256,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.capability_manifest_digest == nil,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.alternative_choices == nil
            else { throw malformed("invalid request_changes message") }
        case .answer_and_retry, .answer_without_retry, .return_to_agent:
            guard let message = command.payload.message,
                !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
                message.lengthOfBytes(using: .utf8) <= 8192,
                command.command_id.lengthOfBytes(using: .utf8) <= 256,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.capability_manifest_digest == nil,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.alternative_choices == nil
            else { throw malformed("invalid answer or return feedback") }
        case .start_with_changes:
            guard let revision = command.payload.run_proposal_revision?.value1,
                command.payload.snooze_until == nil,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.alternative_choices == nil,
                command.payload.capability_manifest_digest == nil,
                revision.expected_cost_units >= 1,
                revision.expected_cost_units <= 1_000_000,
                revision.scope.component_count >= 1,
                revision.scope.component_count <= 32,
                revision.scope.declared_path_count >= 1,
                revision.scope.declared_path_count <= 4096
            else { throw malformed("invalid run_proposal_revision") }
        case .snooze:
            guard command.payload.snooze_until != nil,
                command.payload.run_proposal_revision == nil,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.alternative_choices == nil,
                command.payload.capability_manifest_digest == nil
            else { throw malformed("invalid snooze_until") }
        case .choose_alternative_route:
            guard let choices = command.payload.alternative_choices, !choices.isEmpty,
                choices.allSatisfy({ !$0.finding_id.isEmpty }),
                Set(choices.map(\.finding_id)).count == choices.count,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.capability_manifest_digest == nil
            else { throw malformed("invalid alternative_choices") }
        case .accept_recommended_route:
            guard command.payload.alternative_choices == nil,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty,
                command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.capability_manifest_digest == nil
            else { throw malformed("finding adjudication input on accept") }
        default:
            guard command.payload.run_proposal_revision == nil,
                command.payload.snooze_until == nil,
                command.payload.alternative_choices == nil,
                command.payload.capability_manifest_digest == nil,
                (command.payload.message ?? "").isEmpty,
                (command.payload.attachments ?? []).isEmpty
            else { throw malformed("proposal input on unrelated action") }
        }
    }

    /// The policy set is passed rather than read from an actor so seed-time
    /// derivation (which runs in init before `approvedRecipes` is stored)
    /// can gate a parent through the same check the serve paths run. The
    /// scanner also rejects non-positive snapshot metadata during
    /// reconstruction, ahead of the item's own validation, and the read
    /// path then re-runs the evidence gate against the current
    /// approved-recipe set — trusted policy state, never the row's word
    /// (EligibleForEvidenceSnapshot; the store trust-boundary re-gate).
    static func snapshotBreach(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        approvedRecipes: Set<String>
    ) -> String? {
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 { return "non-positive as_of_revision" }
        if let breach = itemValidityBreach(snapshot.item) { return breach }
        for artifact in snapshot.item.evidence_snapshot {
            let recipe: String
            switch artifact.provenance {
            case .head_bound(let bound): recipe = bound.verification_recipe_digest
            case .head_independent(let free): recipe = free.verification_recipe_digest
            }
            if !approvedRecipes.contains(recipe) {
                return "evidence artifact \(artifact.id) recipe is not approved"
            }
            // The trusted bit is policy-computed, never the row's word:
            // under an approved recipe the computation yields true, so a
            // stale false is corrupt reconstructed data
            // (EligibleForEvidenceSnapshot re-verifies it).
            if !artifact.publish_eligible {
                return "evidence artifact \(artifact.id) carries a stale publish_eligible bit"
            }
        }
        return nil
    }

    /// Field-for-field mirror of domain.TimingSummary.Validate: count
    /// and endpoints must agree, receipts imply submission, receipt
    /// minima fall on or after it (they carry no order between each
    /// other), and the submit-to-open span exists exactly when both of
    /// its endpoints do and equals their difference.
    static func timingBreach(_ timing: Components.Schemas.TimingSummary) -> String? {
        if timing.delivery_count < 0 { return "negative delivery_count" }
        for (name, endpoint) in [
            ("first_submitted_at", timing.first_submitted_at),
            ("first_accepted_at", timing.first_accepted_at),
            ("first_opened_at", timing.first_opened_at),
        ] {
            if let at = endpoint, at == daemonZeroInstant {
                return "zero \(name)"
            }
        }
        let hasReceipt = timing.first_accepted_at != nil || timing.first_opened_at != nil
        if timing.delivery_count == 0,
            timing.first_submitted_at != nil || hasReceipt
        {
            return "timing without deliveries carries endpoints"
        }
        if timing.delivery_count > 0, timing.first_submitted_at == nil {
            return "deliveries without first_submitted_at"
        }
        if hasReceipt, timing.first_submitted_at == nil {
            return "receipt without submission"
        }
        if let submitted = timing.first_submitted_at {
            if let accepted = timing.first_accepted_at, accepted < submitted {
                return "first_accepted_at before first_submitted_at"
            }
            if let opened = timing.first_opened_at, opened < submitted {
                return "first_opened_at before first_submitted_at"
            }
        }
        let bothEndpoints = timing.first_submitted_at != nil && timing.first_opened_at != nil
        if timing.submit_to_first_open == nil, bothEndpoints {
            return "submit_to_first_open missing"
        }
        if let span = timing.submit_to_first_open {
            guard bothEndpoints,
                let submitted = timing.first_submitted_at,
                let opened = timing.first_opened_at
            else { return "submit_to_first_open without both endpoints" }
            let nanos = durationNanoseconds(
                from: wireDate(submitted), to: wireDate(opened))
            if nanos != span { return "submit_to_first_open disagrees with its endpoints" }
        }
        return nil
    }

    /// Re-validates one delivery snapshot before it is served, as the
    /// daemon's read paths run validateSnapshot plus the domain validator
    /// on every row (signet sync.go, store reconstruction): a seed the
    /// daemon would fail closed on fails the mock's read loudly instead
    /// of letting a client test pass against unservable cache state. The
    /// generated variant structs already make status/receipt
    /// correspondence unrepresentable; what stays checkable here is the
    /// snapshot metadata, the identity fields, and receipt ordering.
    static func deliveryBreach(
        _ snapshot: Components.Schemas.AttentionDeliverySnapshot,
        serverRevision: Int64,
        hasParentItem: Bool
    ) -> String? {
        let key = MockServer.DeliveryKey(snapshot.delivery)
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 || snapshot.as_of_revision > serverRevision {
            return "as_of_revision outside the server revision"
        }
        if key.itemID.isEmpty || key.deviceID.isEmpty || key.channel.isEmpty {
            return "empty identity field"
        }
        if key.attempt < 1 { return "non-positive attempt" }
        // submitted_at is required and never the type's zero value:
        // AttentionDelivery.Validate rejects SubmittedAt.IsZero(), so a
        // seed at or before the daemon zero instant is unproducible state.
        let submittedAt: Date
        switch snapshot.delivery {
        case .submitted(let row): submittedAt = row.submitted_at
        case .channel_accepted(let row): submittedAt = row.submitted_at
        case .opened(let row): submittedAt = row.submitted_at
        }
        if submittedAt == daemonZeroInstant { return "submitted_at is unset" }
        // A delivery row exists only because the pipeline recorded it for
        // an existing item; an orphan row is unrepresentable daemon state.
        if !hasParentItem { return "no parent item" }
        switch snapshot.delivery {
        case .submitted:
            break
        case .channel_accepted(let row):
            if row.channel_accepted_at < row.submitted_at {
                return "channel_accepted_at precedes submitted_at"
            }
        case .opened(let row):
            if row.opened_at < row.submitted_at {
                return "opened_at precedes submitted_at"
            }
            if let accepted = row.channel_accepted_at,
                accepted < row.submitted_at || row.opened_at < accepted
            {
                return "receipt ordering violated"
            }
        }
        return nil
    }

    /// Go's `time.Time{}` zero instant (serialized "0001-01-01T00:00:00Z"),
    /// the exact value `AttentionDelivery.Validate` rejects as an unset
    /// submitted_at.
    // swift-format-ignore: NeverForceUnwrap
    private static let daemonZeroInstant: Date = {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }()

    private static func validAdjudicationRoute(
        goal: Components.Schemas.GoalRelationship,
        compatibility: Components.Schemas.WorkUnitCompatibility?,
        route: Components.Schemas.AdjudicationRoute
    ) -> Bool {
        switch goal {
        case .required:
            guard let compatibility else { return false }
            switch compatibility {
            case .allowed: return route == .remediate
            case .work_unit_revision_required: return route == .park_revision
            case .separate_work_required: return route == .park_separate_work
            case .human_decision_required: return route == .attention_human_decision
            case .unknown: return route == .park_unknown
            }
        case .adjacent:
            return compatibility == nil && route == ._defer
        case .contradictory:
            return compatibility == nil && (route == .decline || route == .dispute)
        case .unclear:
            return compatibility == nil && route == .attention_unclear
        }
    }

    private static func displayNamesBreach(_ names: Components.Schemas.DisplayNames) -> String? {
        if names.project.text.isEmpty || names.work_unit.text.isEmpty {
            return "empty display name"
        }
        return nil
    }

    /// The content address of a text claim's UTF-8 bytes, in the
    /// repository-wide "sha256:<hex>" form; the Swift twin of
    /// domain.ClaimText.ComputeDigest, and the single derivation path the
    /// fixtures and the validation mirror share.
    static func sha256Digest(of content: String) -> String {
        "sha256:" + SHA256.hash(data: Data(content.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    /// Mirrors CapabilityManifest.ComputeDigest's versioned Go struct
    /// encoding. Keep the field order exact because the digest addresses
    /// those bytes, not an unordered JSON object.
    static func capabilityManifestDigest(name: String, egressProfile: String) -> String {
        sha256Digest(
            of: "{\"encoding_version\":1,\"name\":\(goJSONString(name)),"
                + "\"egress_profile\":\(goJSONString(egressProfile))}")
    }

    /// Mirrors Go's `json.Marshal([]SpecAddressalClaim)` byte-for-byte for
    /// the two-field contract shape used as the durable claim body.
    static func addressalsDigest(
        _ addressals: [Components.Schemas.SpecAddressalClaim]
    ) -> String {
        let body = addressals.map {
            "{\"comment_id\":\(goJSONString($0.comment_id)),\"response\":\(goJSONString($0.response))}"
        }.joined(separator: ",")
        return sha256Digest(of: "[\(body)]")
    }

    private static func goJSONString(_ value: String) -> String {
        guard let encoded = try? JSONEncoder().encode(value) else {
            preconditionFailure("String JSON encoding cannot fail")
        }
        return String(decoding: encoded, as: UTF8.self)
            .replacingOccurrences(of: "\\/", with: "/")
            .replacingOccurrences(of: "<", with: "\\u003c")
            .replacingOccurrences(of: ">", with: "\\u003e")
            .replacingOccurrences(of: "&", with: "\\u0026")
            .replacingOccurrences(of: "\u{2028}", with: "\\u2028")
            .replacingOccurrences(of: "\u{2029}", with: "\\u2029")
    }

    /// The generated runtime's RFC 3339 decoder accepts whole seconds, which
    /// is also what the mock's `.iso8601` encoder emits. Derive
    /// timing from those same instants so the duration always agrees with
    /// the timestamps the generated client actually decodes, including when
    /// a fixture supplies finer-grained `Date` values.
    static func wireDate(_ date: Date) -> Date {
        Date(timeIntervalSince1970: date.timeIntervalSince1970.rounded(.down))
    }

    /// Mirrors `time.Time.Sub`: nanosecond spans outside `time.Duration`'s
    /// int64 range saturate instead of trapping. Long but valid RFC 3339
    /// fixture dates therefore remain servable just as they are in Go.
    static func durationNanoseconds(from start: Date, to end: Date) -> Int64 {
        let nanoseconds = (end.timeIntervalSince(start) * 1_000_000_000).rounded()
        if nanoseconds >= Double(Int64.max) { return Int64.max }
        if nanoseconds <= Double(Int64.min) { return Int64.min }
        return Int64(nanoseconds)
    }
}
