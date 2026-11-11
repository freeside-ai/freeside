import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

@Suite struct AttentionDisplayTests {
    /// The unviewed mark (#2930) is for open items no device has opened at
    /// their current surface; a closed item never carries it, whatever the
    /// daemon serves.
    @Test func onlyAnOpenUnopenedItemIsUnviewed() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        for status in Components.Schemas.ItemStatus.allCases {
            item.status = status
            item.surface_first_opened_at = nil
            #expect(AttentionDisplay.isUnviewed(item) == (status == .open), "\(status), never opened")
            item.surface_first_opened_at = Date(timeIntervalSince1970: 1_767_400_000)
            #expect(!AttentionDisplay.isUnviewed(item), "\(status), opened")
        }
    }

    /// The quota failure's card shows each offer's usage reading (#3030),
    /// one line per offer in the daemon's order, in the words the Choose
    /// Another Agent dialog uses, so the card and the dialog agree.
    @Test func otherAgentsRowCarriesEachOffersReading() throws {
        let now = Date(timeIntervalSince1970: 1_767_400_000)
        let locale = Locale(identifier: "en_US_POSIX")
        let utc = try #require(TimeZone(identifier: "UTC"))
        let pools = UsageFixtures.alternatesUsagePools(now: now)
        var item = try #require(
            AttentionFixtures.alternatesInbox().first { $0.item.id == "item-execution_failure" }
        ).item
        let unlisted = "enrollment-no-pool"
        item.alternate_agents?.append(
            .init(
                enrollment: unlisted, pool: "pool-unlisted", harness: .claude_code, model: "claude-opus",
                qualification: .different_pool))

        let rows = AttentionDisplay.cardFacts(item, now: now, pools: pools, locale: locale, timeZone: utc)
            .filter { $0.label == "Other Agents" }
        let row = try #require(rows.first)
        #expect(rows.count == 1)
        guard case .offers(let drawn) = row.form else {
            Issue.record("Other Agents draws as \(row.form), not one block per offer")
            return
        }
        #expect(drawn.map { "\($0.enrollment) · \($0.reading)" } == row.value.components(separatedBy: "\n"))

        let lines = row.value.split(separator: "\n").map(String.init)
        let offers = try #require(item.alternate_agents)
        #expect(lines.count == offers.count)
        for (line, offer) in zip(lines, offers) {
            let reading =
                AgentChoiceDisplay.reading(
                    for: offer.enrollment, pools: pools, now: now, locale: locale, timeZone: utc)
                ?? "no usage reading"
            #expect(line == "\(offer.enrollment) · \(reading)")
        }
        let readAt = FreesideFormat.shortTime(
            now.addingTimeInterval(-300), now: now, locale: locale, timeZone: utc)
        #expect(lines[0] == "\(UsageFixtures.readingSecondEnrollmentID) · 5-hour: 65% left · read \(readAt)")
        #expect(lines[1].hasPrefix("\(UsageFixtures.staleEnrollmentID) · "))
        #expect(lines[1].contains(" · stale since "))
        #expect(lines[2].hasPrefix("\(UsageFixtures.unsupportedCollectorID) · "))
        #expect(lines[2].contains("unsupported"))
        #expect(lines[3] == "\(unlisted) · no usage reading")
    }

    /// A failure with no offers keeps no Other Agents row, pools or not.
    @Test func noOffersMeansNoOtherAgentsRow() {
        let now = Date(timeIntervalSince1970: 1_767_400_000)
        var item = AttentionFixtures.quotaFailure().item
        item.alternate_agents = []
        let facts = AttentionDisplay.cardFacts(
            item, now: now, pools: UsageFixtures.alternatesUsagePools(now: now))
        #expect(!facts.contains { $0.label == "Other Agents" })
    }

    @Test func externalReviewDecisionNamesItsEffects() {
        let item = AttentionFixtures.routedReviewDispute().item
        #expect(AttentionDisplay.ask(item) == "How should these external review findings be handled?")
        #expect(AttentionDisplay.disputeRouteLabel(.remediate, external: true) == "Fix in This Pull Request")
        #expect(AttentionDisplay.disputeRouteLabel(.decline, external: true) == "Decline and Reply")
        #expect(AttentionDisplay.disputeRouteLabel(._defer, external: true) == "Defer and Reply")
        #expect(!AttentionDisplay.hasExternalDisputedFindings(AttentionFixtures.fixture(type: .review_dispute).item))
    }

    /// Each external route carries its own consequence under its label
    /// (R29); a finding with no review thread has no reply to promise.
    @Test func externalDisputeRoutesCarryTheirConsequences() {
        #expect(
            AttentionDisplay.disputeRouteConsequence(.remediate, external: true)
                == "Starts remediation in this pull request.")
        #expect(
            AttentionDisplay.disputeRouteConsequence(.decline, external: true)
                == "Records the outcome and replies in the finding's thread.")
        #expect(
            AttentionDisplay.disputeRouteConsequence(._defer, external: true)
                == "Records the outcome and replies in the finding's thread; files no follow-up issue.")
        for route in [Components.Schemas.AdjudicationRoute.remediate, .decline, ._defer] {
            #expect(AttentionDisplay.disputeRouteConsequence(route, external: false) == nil)
        }
    }

    /// Only a route that replies in an external thread asks for the
    /// operator's reason, and the hint says why Apply waits on a long one.
    @Test func replyingRoutesAskForAReason() {
        #expect(AttentionDisplay.disputeRouteRepliesExternally(.decline, external: true))
        #expect(AttentionDisplay.disputeRouteRepliesExternally(._defer, external: true))
        #expect(!AttentionDisplay.disputeRouteRepliesExternally(.remediate, external: true))
        #expect(!AttentionDisplay.disputeRouteRepliesExternally(.decline, external: false))
        #expect(!AttentionDisplay.disputeRouteRepliesExternally(._defer, external: false))
        #expect(
            AttentionDisplay.disputeReplyReasonHint(" \n")
                == "Required. Posted with the reply in the reviewer's thread.")
        #expect(
            AttentionDisplay.disputeReplyReasonHint("Out of scope.")
                == "Posted with the reply in the reviewer's thread.")
        #expect(
            AttentionDisplay.disputeReplyReasonHint(String(repeating: "a", count: 2049))
                == "Too long to post. Shorten it to send the reply.")
    }

    /// The model's route is favored, never recommended: the fold's closed
    /// line says whether one is favored, at what confidence, on which
    /// thread, and the sentence inside says why it is only favored.
    @Test func disputedFindingFoldNamesTheFavoredRouteConfidenceAndThreshold() throws {
        let findings = try #require(AttentionFixtures.routedReviewDispute().item.review_dispute?.value1.findings)
        #expect(AttentionDisplay.disputedFindingFavoredMark == "Favored")

        let favoring = findings[0]
        #expect(AttentionDisplay.disputedFindingReasonSummary(favoring) == "medium confidence · review-thread-1")
        #expect(
            AttentionDisplay.disputedFindingThreshold(try #require(favoring.adjudication))
                == "Medium confidence is below the dispatch threshold, so this is a favored route, not a recommendation."
        )
        #expect(AttentionDisplay.disputedFindingUnofferedFavorite(favoring) == nil)

        var noRoute = favoring
        noRoute.adjudication?.lean = nil
        noRoute.adjudication?.confidence = .low
        #expect(
            AttentionDisplay.disputedFindingReasonSummary(noRoute)
                == "no favored route · low confidence · review-thread-1")
        #expect(
            AttentionDisplay.disputedFindingThreshold(try #require(noRoute.adjudication))
                == "Low confidence is below the dispatch threshold, so the model favors no route.")
        // A finding with no review thread drops the thread segment.
        #expect(AttentionDisplay.disputedFindingReasonSummary(findings[1]) == "no favored route · low confidence")

        var noBlock = favoring
        noBlock.adjudication = nil
        #expect(AttentionDisplay.disputedFindingReasonSummary(noBlock) == "no adjudication · review-thread-1")

        var heldAbove = try #require(favoring.adjudication)
        heldAbove.below_threshold = false
        #expect(AttentionDisplay.disputedFindingThreshold(heldAbove) == nil)

        // A favored route the finding does not offer has no option to mark,
        // so the fold names it.
        var unoffered = findings[1]
        unoffered.adjudication?.lean = .init(value1: .remediate)
        #expect(
            AttentionDisplay.disputedFindingUnofferedFavorite(unoffered)
                == "The model favors Fix in this PR, which this finding does not offer.")
    }

    /// The face's one mono line says where the finding is and who raised
    /// it, and drops whichever the finding lacks.
    @Test func disputedFindingSourceJoinsLocationAndReviewer() throws {
        let findings = try #require(AttentionFixtures.routedReviewDispute().item.review_dispute?.value1.findings)
        #expect(AttentionDisplay.disputedFindingSource(findings[0]) == "daemon/update.go:42-45 · reviewer")
        #expect(AttentionDisplay.disputedFindingSource(findings[1]) == nil)
        var reviewerOnly = findings[0]
        reviewerOnly.location = nil
        #expect(AttentionDisplay.disputedFindingSource(reviewerOnly) == "reviewer")
    }

    /// Apply counts the picks (R32), and keeps the generic label until the
    /// first one.
    @Test func applyFindingRoutesLabelCountsThePicks() {
        #expect(AttentionDisplay.applyFindingRoutesLabel(selected: 0, of: 2) == "Apply Finding Routes")
        #expect(AttentionDisplay.applyFindingRoutesLabel(selected: 1, of: 2) == "Apply 1 of 2 Routes")
        #expect(AttentionDisplay.applyFindingRoutesLabel(selected: 2, of: 2) == "Apply 2 Routes")
        #expect(AttentionDisplay.applyFindingRoutesLabel(selected: 1, of: 1) == "Apply 1 Route")
        #expect(AttentionDisplay.label(.adjudicate_findings) == "Apply Finding Routes")
    }

    /// Every card that asks for a decision asks it in one sentence. A
    /// retired blocked item, which only a closed row still carries, never
    /// asked for one: where its typed wait says what it waited on, it leads
    /// with that as a statement (survey frame 5.5); without one it keeps the
    /// question.
    @Test func everyAttentionTypeHasAOneSentenceQuestionAsk() {
        var blocked = AttentionFixtures.fixture(type: .blocked).item
        #expect(blocked.requested_decision.isEmpty)
        #expect(AttentionDisplay.ask(blocked) == "Waiting on specification approval.")
        blocked.blocked_on = nil
        for item in AttentionFixtures.phase1Types.map({ AttentionFixtures.fixture(type: $0).item })
            + [blocked]
        {
            let ask = AttentionDisplay.ask(item)
            #expect(!ask.isEmpty)
            #expect(ask.hasSuffix("?"))
            #expect(ask.dropLast().contains("?") == false)
        }
    }

    // Titles follow the project's title-case heading convention. Keying the
    // table by every AttentionType makes a new type fail here until it has
    // a title-case entry.
    @Test func everyAttentionTypeHasATitleCaseTitle() {
        let titles: [Components.Schemas.AttentionType: String] = [
            .spec_approval: "Spec Approval",
            .execution_failure: "Execution Failure",
            .agent_question: "Agent Question",
            .review_diminishing_returns: "Diminishing Returns",
            .review_dispute: "Review Dispute",
            .review_contradiction: "Review Contradiction",
            .review_configuration: "Review Configuration",
            .finding_adjudication: "Finding Adjudication",
            .ready_for_final_review: "Ready for Final Review",
            .publish_blocked: "Publish Blocked",
            .task_proposal: "Task Proposal",
            .effect_proposal: "Effect Proposal",
            .system_health: "System Health",
            .blocked: "Blocked",
        ]

        #expect(Set(titles.keys) == Set(Components.Schemas.AttentionType.allCases))
        for type in Components.Schemas.AttentionType.allCases {
            #expect(AttentionDisplay.title(type) == titles[type], "\(type)")
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

    /// The mechanical rows read their typed fields, so rewording the daemon's
    /// prose cannot change what the row asserts.
    @Test func mechanicalRowSummariesComeFromTypedFactsNotProse() {
        var systemHealth = AttentionFixtures.fixture(type: .system_health).item
        systemHealth.reason = "any prose at all"
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Run projection unavailable. Warns about run visibility; admission stays open.")

        systemHealth.posture = .init(value1: .blocking)
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Run projection unavailable. Run visibility is impaired.")

        systemHealth.health_diagnostic = .init(
            value1: .init(code: "backup_age.stale", impairs: .none))
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Backup age stale. No capability is impaired.")
        systemHealth.posture = .init(value1: .advisory)
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "Backup age stale. No capability is impaired.")

        systemHealth.health_diagnostic = nil
        #expect(
            AttentionDisplay.rowSummary(systemHealth)
                == "A system-health condition needs attention.")

        // blocked is retired: only a closed item exists, and its row says so
        // instead of naming a wait nothing is on any more.
        let blocked = AttentionFixtures.fixture(type: .blocked).item
        #expect(AttentionDisplay.rowSummary(blocked) == "This blocked item is superseded.")
    }

    /// Held work (#3026): an advisory finding reads as a warning that
    /// leaves admission open, beside the Admission row's "Open", and a
    /// finding with no posture reads as advisory, as the pane orders it.
    @Test func anAdvisoryFindingSaysAdmissionStaysOpen() {
        var held = AttentionFixtures.heldWorkSystemHealth().item
        #expect(
            AttentionDisplay.rowSummary(held)
                == "Work held. Warns about unattended admission; admission stays open.")
        held.posture = nil
        #expect(
            AttentionDisplay.rowSummary(held)
                == "Work held. Warns about unattended admission; admission stays open.")
    }

    /// Codes are free-form, so one rule names every one, including those
    /// the daemon builds at runtime.
    @Test func diagnosticLabelsSplitTheCodeIntoASentenceCaseLabel() {
        #expect(AttentionDisplay.diagnosticLabel("work_held") == "Work held")
        #expect(AttentionDisplay.diagnosticLabel("invocation_stalled") == "Invocation stalled")
        #expect(AttentionDisplay.diagnosticLabel("backup_age.stale") == "Backup age stale")
        #expect(AttentionDisplay.diagnosticLabel("doctor-check.git_auth") == "Doctor check git auth")
        #expect(AttentionDisplay.diagnosticLabel("_leading__double.") == "Leading double")
        #expect(AttentionDisplay.diagnosticLabel("") == "")
    }

    @Test func concludedRowSummariesAreNeutralAcrossEveryTypeAndStatus() {
        let statuses: [Components.Schemas.ItemStatus] = [
            .resolved, .superseded, .dismissed, .expired,
        ]
        for type in AttentionFixtures.phase1Types {
            for status in statuses {
                var item = AttentionFixtures.fixture(type: type).item
                item.status = status

                #expect(
                    AttentionDisplay.rowSummary(item)
                        == "This \(AttentionDisplay.title(type).lowercased()) item is "
                        + "\(AttentionDisplay.label(status).lowercased()).")
            }
        }
    }

    @Test func relativeRowTimesUseCoarseUnitsAndDeadlinePrecedence() {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        var ordinary = AttentionFixtures.fixture(type: .spec_approval).item
        ordinary.created_at = now.addingTimeInterval(-59 * 60)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "59m")

        ordinary.created_at = now.addingTimeInterval(-18 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "18h")

        ordinary.created_at = now.addingTimeInterval(-3 * 86_400)
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == "3d")

        var due = AttentionFixtures.fixture(type: .spec_approval).item
        due.created_at = now.addingTimeInterval(-60)
        due.expires_when = now.addingTimeInterval(2 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(due, now: now) == "due 2h")
        #expect(
            AttentionDisplay.exactRowTimestamp(due, now: now)
                == due.expires_when?.formatted(.iso8601))

        due.expires_when = now.addingTimeInterval(30)
        #expect(AttentionDisplay.relativeRowTime(due, now: now) == "due now")

        due.expires_when = now.addingTimeInterval(-30)
        #expect(AttentionDisplay.relativeRowTime(due, now: now) == "due now")

        due.expires_when = now.addingTimeInterval(-2 * 3_600)
        #expect(AttentionDisplay.relativeRowTime(due, now: now) == "overdue 2h")
        #expect(
            AttentionDisplay.exactRowTimestamp(due, now: now)
                == due.expires_when?.formatted(.iso8601))

        // A closed row shows its age and ignores a deadline it no longer has.
        due.created_at = now.addingTimeInterval(-18 * 3_600)
        due.status = .resolved
        #expect(AttentionDisplay.relativeRowTime(due, now: now) == "18h")
        #expect(
            AttentionDisplay.exactRowTimestamp(due, now: now)
                == due.created_at?.formatted(.iso8601))

        // A retired blocked item is only ever closed. Its row counts from
        // creation like any other, not from the wait it once recorded.
        var blocked = AttentionFixtures.fixture(type: .blocked).item
        blocked.created_at = now.addingTimeInterval(-18 * 3_600)
        blocked.blocked_on = .init(
            value1: .init(
                kind: .spec_approval,
                since: now.addingTimeInterval(-2 * 86_400),
                item_id: "item-spec_approval",
                pr_reference: nil))
        #expect(AttentionDisplay.relativeRowTime(blocked, now: now) == "18h")
        #expect(
            AttentionDisplay.exactRowTimestamp(blocked, now: now)
                == blocked.created_at?.formatted(.iso8601))

        ordinary.created_at = nil
        ordinary.expires_when = nil
        #expect(AttentionDisplay.relativeRowTime(ordinary, now: now) == nil)
    }

    @Test func subsetDisputeSeparatesAlreadyRoutedFindings() {
        let now = AttentionFixtures.createdInstant
        let rows = AttentionDisplay.cardFacts(AttentionFixtures.subsetReviewDispute().item, now: now)
        #expect(rows.first { $0.label == "Disputed findings" }?.value == "finding-1")
        #expect(rows.first { $0.label == "Already routed" }?.value == "finding-2")
        let full = AttentionDisplay.cardFacts(AttentionFixtures.routedReviewDispute().item, now: now)
        #expect(!full.contains { $0.label == "Already routed" })
        #expect(full.first { $0.label == "Disputed findings" }?.value == "finding-1, finding-2")
    }

    @Test func relativeRowTimeFormatsBareDatesWithTheSharedUnits() {
        let now = Date(timeIntervalSince1970: 200_000)
        #expect(
            AttentionDisplay.relativeRowTime(
                now.addingTimeInterval(-86_400), now: now) == "1d")
    }

    /// Section 9's per-type leads: every fact a card shows is read from a
    /// typed field, so clearing that field clears the fact rather than
    /// leaving a value recovered from prose.
    @Test func everyTypedLeadComesFromItsOwnFactFieldAndDisappearsWithIt() {
        let expected: [Components.Schemas.AttentionType: [String]] = [
            .execution_failure: [
                "Outcome", "Cause", "Stage", "Invocation", "Harness", "Model", "Effort", "Enrollment",
                "Selection", "Cost Owner", "Credential Mode", "Egress Profile", "Isolation Class",
            ],
            .review_diminishing_returns: ["Cost so Far", "Diff Growth"],
            .review_dispute: ["Run", "Round", "Disputed findings", "Completion evidence"],
            .ready_for_final_review: ["Diff"],
            .publish_blocked: ["Failed trust rule"],
            .system_health: ["Diagnostic", "Impairs", "Posture"],
            .blocked: ["Waiting", "Blocked on"],
            .agent_question: ["Stage", "Blocked on"],
        ]
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        for (type, labels) in expected {
            var item = AttentionFixtures.fixture(type: type).item
            #expect(AttentionDisplay.cardFacts(item, now: now).map(\.label) == labels)

            item.execution_failure = nil
            item.billable_cost_so_far = nil
            item.yield_history = nil
            item.review_dispute = nil
            item.diff_stats = nil
            item.publish_block = nil
            item.health_diagnostic = nil
            item.posture = nil
            item.blocked_on = nil
            item.agent_question = nil
            item.reason = "the build stage failed twice and the run has waited 18h"
            #expect(AttentionDisplay.cardFacts(item, now: now).isEmpty)
        }

        // The remaining types lead with an ask, an artifact, or their own
        // module, so they contribute no fact rows.
        for type in [
            Components.Schemas.AttentionType.spec_approval,
            .finding_adjudication, .task_proposal, .review_contradiction, .review_configuration,
        ] {
            #expect(
                AttentionDisplay.cardFacts(
                    AttentionFixtures.fixture(type: type).item, now: now
                ).isEmpty)
        }
    }

    /// An elapsed publication deadline (#2752) draws as its own fact after
    /// the Change row, with its exact instant in Identifiers; a ready item
    /// with none draws neither.
    @Test func elapsedDeadlinesRenderAsFactsAndIdentifiers() throws {
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        var item = AttentionFixtures.readyWithElapsedChecksDeadline().item
        let checks = try #require(item.elapsed_deadlines?.first)
        let facts = AttentionDisplay.cardFacts(item, now: now)
        #expect(facts.map(\.label) == ["Diff", "Checks"])
        #expect(facts.last?.value == "Not confirmed after 30 min · elapsed 9m ago")
        #expect(
            AttentionDisplay.detailIdentifierRows(item).contains {
                $0.label == "Checks Deadline Elapsed" && $0.value == checks.elapsed_at.formatted(.iso8601)
            })

        let reviewAt = checks.armed_at.addingTimeInterval(86_400 + 60)
        item.elapsed_deadlines?.append(
            .init(
                kind: .review_wait_threshold, armed_at: checks.armed_at, deadline_seconds: 86_400,
                elapsed_at: reviewAt))
        item.diff_stats = nil
        let reviewFacts = AttentionDisplay.cardFacts(item, now: reviewAt.addingTimeInterval(2 * 3_600))
        #expect(reviewFacts.map(\.label) == ["Checks", "Review"])
        #expect(reviewFacts.last?.value == "None after 24 h · elapsed 2h ago")
        #expect(AttentionDisplay.detailIdentifierRows(item).contains { $0.label == "Review Deadline Elapsed" })

        let plain = AttentionFixtures.fixture(type: .ready_for_final_review).item
        #expect(!AttentionDisplay.cardFacts(plain, now: now).contains { $0.label == "Checks" })
        #expect(!AttentionDisplay.detailIdentifierRows(plain).contains { $0.label.hasSuffix("Deadline Elapsed") })
    }

    @Test func typedLeadsRenderTheirDaemonValues() {
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        let failureFacts = AttentionDisplay.cardFacts(
            AttentionFixtures.fixture(type: .execution_failure).item, now: now)
        #expect(
            failureFacts.map(\.value)
                == [
                    "Failed", "Invalid configuration", "Implementation", "inv-execution_failure", "Claude Code",
                    "claude-opus-5-5", "High", "anthropic-a/claude_code", "sha256:eeeeeeeeeeee…",
                    "platform-team", "Subscription, contained", "Provider only",
                    "Fresh VM, read-only volume handoff",
                ])
        // The identifiers render monospaced, the admission's other facts plain.
        #expect(
            failureFacts.filter(\.monospaced).map(\.label) == ["Invocation", "Enrollment", "Selection"])
        // An item raised before the daemon recorded its admission shows only
        // the failure's own facts.
        var unrecorded = AttentionFixtures.fixture(type: .execution_failure).item
        unrecorded.execution_failure?.value1.admitted = nil
        #expect(
            AttentionDisplay.cardFacts(unrecorded, now: now).map(\.label)
                == ["Outcome", "Cause", "Stage", "Invocation"])
        #expect(
            AttentionDisplay.cardFacts(
                AttentionFixtures.fixture(type: .review_diminishing_returns).item, now: now
            ).map(\.value) == [
                "USD 42.75 across 6 invocations, still accruing",
                "+120 \u{2212}8 \u{2192} +210 \u{2212}14",
            ])
        #expect(
            AttentionDisplay.cardFacts(
                AttentionFixtures.fixture(type: .ready_for_final_review).item, now: now
            ).map(\.value) == ["+240 \u{2212}31 · 12 files"])
        #expect(
            AttentionDisplay.cardFacts(
                AttentionFixtures.fixture(type: .publish_blocked).item, now: now
            ).map(\.value) == ["Trust profile drifted"])
        #expect(
            AttentionDisplay.cardFacts(
                AttentionFixtures.fixture(type: .system_health).item, now: now
            ).map(\.value) == ["Run projection unavailable", "Run visibility", "Advisory"])

        var blocked = AttentionFixtures.fixture(type: .blocked).item
        blocked.blocked_on = .init(
            value1: .init(
                kind: .pr_checks,
                since: AttentionFixtures.createdInstant,
                item_id: nil,
                pr_reference: .init(value1: .init(repo: "owner/repo", number: 7))))
        #expect(
            AttentionDisplay.cardFacts(blocked, now: now).map(\.label)
                == ["Waiting", "Blocked on", "Pull Request"])
        #expect(AttentionDisplay.cardFacts(blocked, now: now).last?.value == "owner/repo#7")
        // Nothing to open: the wait names no item.
        #expect(AttentionDisplay.cardFacts(blocked, now: now)[1] == .init("Blocked on", "PR checks"))

        // The card reads the wait as a duration in the register the inbox row
        // uses; the exact instant stays among the identifiers.
        #expect(AttentionDisplay.cardFacts(blocked, now: now)[0].value == "18h")
        #expect(
            AttentionDisplay.detailIdentifierRows(blocked)
                .contains { $0.label == "Waiting Since" && $0.value.hasPrefix("2026-") })
    }

    /// Survey frame 5.5: a blocked card leads with the wait its typed facts
    /// name and links to the item the run waits on. That item's id is a
    /// binding, so the Identifiers keep it (R17).
    @Test func aBlockedCardLeadsWithItsTypedWaitAndLinksTheBlockingItem() throws {
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        var blocked = AttentionFixtures.fixture(type: .blocked).item
        let blockingItem = try #require(blocked.blocked_on?.value1.item_id)

        #expect(AttentionDisplay.ask(blocked) == "Waiting on specification approval.")
        #expect(
            AttentionDisplay.cardFacts(blocked, now: now)
                == [
                    .init("Waiting", "18h"),
                    .init("Blocked on", "Specification approval", linkingItem: blockingItem),
                ])
        #expect(
            AttentionDisplay.detailIdentifierRows(blocked)
                .contains(.init(label: "Blocking Item", value: blockingItem)))

        blocked.blocked_on = nil
        #expect(AttentionDisplay.ask(blocked) == "What is keeping this run blocked?")
    }

    /// Survey frame 5.5: the posture reads with the diagnostic as a chip, in
    /// either posture, and comes from its own typed field.
    @Test func systemHealthFactsCarryThePostureAsAState() {
        let now = AttentionFixtures.createdInstant
        var item = AttentionFixtures.fixture(type: .system_health).item
        item.posture = .init(value1: .blocking)
        #expect(
            AttentionDisplay.cardFacts(item, now: now).last
                == .init("Posture", posture: .blocking))
        #expect(AttentionDisplay.cardFacts(item, now: now).last?.value == "Blocking")

        item.health_diagnostic = nil
        #expect(AttentionDisplay.cardFacts(item, now: now).map(\.label) == ["Posture"])
    }

    /// Held work (#3026): the card names the held task and links to it,
    /// shows the diagnostic as words, and keeps the raw code copyable in
    /// Identifiers. A system-scoped finding names no task.
    @Test func aTaskFindingsCardNamesItsTaskAndKeepsTheCodeInIdentifiers() {
        let now = AttentionFixtures.createdInstant
        var held = AttentionFixtures.heldWorkSystemHealth().item
        #expect(
            AttentionDisplay.cardFacts(held, now: now)
                == [
                    .init("Diagnostic", "Work held"),
                    .init("Impairs", "Unattended admission"),
                    .init("Posture", posture: .advisory),
                    .init(
                        "Task", "Time grammar for inbox rows",
                        linkingTask: "task-run-freeside-specification"),
                ])
        #expect(
            AttentionDisplay.detailIdentifierRows(held)
                .contains(.init(label: "Diagnostic Code", value: "work_held")))

        // A run subject names its stored task.
        held.subject = .run(
            .init(subject_type: .run, subject_id: "run-1", run_id: "run-1", task_id: "task-1"))
        #expect(
            AttentionDisplay.cardFacts(held, now: now).last
                == .init("Task", "Time grammar for inbox rows", linkingTask: "task-1"))

        // A legacy item without names falls back to the task's id, not
        // the run's.
        held.display_names = nil
        #expect(
            AttentionDisplay.cardFacts(held, now: now).last
                == .init("Task", "task-1", linkingTask: "task-1"))

        // A run with no stored task, and a system finding, name no task.
        held.subject = .run(.init(subject_type: .run, subject_id: "run-1", run_id: "run-1", task_id: nil))
        #expect(!AttentionDisplay.cardFacts(held, now: now).contains { $0.label == "Task" })
        let system = AttentionFixtures.fixture(type: .system_health).item
        #expect(!AttentionDisplay.cardFacts(system, now: now).contains { $0.label == "Task" })
    }

    @Test func rowContextRendersTheDaemonsDisplayNamesAndMarksIdentifiers() {
        var run = AttentionFixtures.fixture(type: .execution_failure).item

        #expect(
            AttentionDisplay.rowContext(run)
                == .init(
                    project: .init(value: "owner/repo", isIdentifier: false),
                    workUnit: .init(value: "#724", isIdentifier: false)
                ))

        run.display_names = .init(
            value1: .init(
                project: .init(text: "proj-1", source: .identifier),
                task: .init(text: "run-execution_failure", source: .identifier)))
        #expect(
            AttentionDisplay.rowContext(run)
                == .init(
                    project: .init(value: "proj-1", isIdentifier: true),
                    workUnit: .init(value: "run-execution_failure", isIdentifier: true)
                ))

        // A legacy item carries no labels; the raw identifiers stand in and
        // are marked as identifiers.
        run.display_names = nil
        #expect(
            AttentionDisplay.rowContext(run)
                == .init(
                    project: .init(value: "proj-1", isIdentifier: true),
                    workUnit: .init(value: "run-execution_failure", isIdentifier: true)
                ))

        let system = AttentionFixtures.fixture(type: .system_health).item
        #expect(AttentionDisplay.rowContext(system).workUnit == nil)
    }

    @Test func rowContextMarksAnAgentProposedTaskNameAsAClaim() {
        var run = AttentionFixtures.fixture(type: .execution_failure).item
        #expect(AttentionDisplay.rowContext(run).workUnit?.isAgentClaim == false)

        run.display_names = .init(
            value1: .init(
                project: .init(text: "owner/repo", source: .name),
                task: RunFixtures.readyTaskName))
        #expect(
            AttentionDisplay.rowContext(run).workUnit
                == .init(value: RunFixtures.readyTaskName.text, isIdentifier: false, isAgentClaim: true))

        // A system item has no task, so nothing to mark.
        var system = AttentionFixtures.fixture(type: .system_health).item
        system.display_names = run.display_names
        #expect(AttentionDisplay.rowContext(system).workUnit == nil)
    }

    @Test func copyableSubjectReferencesMatchTheirContractSubject() {
        let run = AttentionFixtures.fixture(type: .execution_failure).item
        #expect(
            AttentionDisplay.copyableSubjectReference(run)
                == .init(label: "Copy run reference", value: "run-execution_failure"))

        let proposal = AttentionFixtures.fixture(type: .task_proposal).item
        #expect(
            AttentionDisplay.copyableSubjectReference(proposal)
                == .init(label: "Copy proposal batch reference", value: "batch-task_proposal"))

        let system = AttentionFixtures.fixture(type: .system_health).item
        #expect(AttentionDisplay.copyableSubjectReference(system) == nil)
    }

    @Test func rowBadgePolicyKeepsOnlyExceptionalStates() {
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
        #expect(AttentionDisplay.title(degraded._type) == "Ready for Final Review")
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

    /// A cause is present exactly when the outcome failed, so a canceled or
    /// lost run's card shows no Cause row rather than an empty one.
    @Test func executionFailureShowsCauseOnlyForAFailedOutcome() {
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        var canceled = AttentionFixtures.fixture(type: .execution_failure).item
        canceled.execution_failure?.value1.outcome = .canceled
        canceled.execution_failure?.value1.cause = nil
        // The admitted rows follow and are covered by typedLeadsRenderTheirDaemonValues.
        canceled.execution_failure?.value1.admitted = nil
        #expect(
            AttentionDisplay.cardFacts(canceled, now: now).map(\.label)
                == ["Outcome", "Stage", "Invocation"])
    }

    @Test func failureCauseLabelsAreExplicit() {
        let labels: [Components.Schemas.FailureCause: String] = [
            .quota: "Quota exhausted",
            .credential_expired: "Credential expired",
            .credential_revoked: "Credential revoked",
            .service_capacity: "Service at capacity",
            .provider_unavailable: "Provider unavailable",
            .invalid_configuration: "Invalid configuration",
            .contract_contradiction: "Contract contradiction",
            .memory_limit: "Memory limit reached",
            .unknown: "Unknown",
        ]
        #expect(Set(labels.keys) == Set(Components.Schemas.FailureCause.allCases))
        for (cause, label) in labels {
            #expect(AttentionDisplay.label(cause) == label)
        }
    }

    @Test func healthPostureLabelsAreExplicit() {
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.blocking) == "Blocking")
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.advisory) == "Advisory")
    }

    /// Routes read as the outcome of accepting them, never as enum values,
    /// and every route that parks the run says so first (#1551). The daemon's
    /// `domain.AdjudicationRouteLabel` writes the same words into the reason.
    @Test func adjudicationRouteLabelsNameTheOutcome() {
        let labels: [Components.Schemas.AdjudicationRoute: String] = [
            .remediate: "Fix in this PR",
            .park_revision: "Park: revise the work unit",
            .park_separate_work: "Park: needs separate work",
            .attention_human_decision: "Park: needs a human decision",
            .park_unknown: "Park: can't tell where it belongs",
            ._defer: "Defer to later work",
            .decline: "Decline the finding",
            .dispute: "Park: dispute the finding",
            .attention_unclear: "Park: unclear if the goal needs it",
        ]
        #expect(Set(labels.keys) == Set(Components.Schemas.AdjudicationRoute.allCases))
        for (route, label) in labels {
            #expect(AttentionDisplay.label(route) == label)
        }
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
        #expect(rows.first?.label == "Evidence Digest")
        // The specification claim names its own channel; every other claim
        // keeps the generic label, and no row's value changes.
        let specDigest = item.agent_claims.first { $0.label == "Specification" }?.digest
        #expect(rows.contains { $0.label == "Specification Digest" && $0.value == specDigest })
        #expect(
            rows.dropFirst().allSatisfy {
                $0.label == "Claim Digest" || $0.label == "Specification Digest"
            })
        #expect(Set(rows.map(\.value)) == Set(item.artifact_digests))
    }

    @Test func specificationClaimDigestNamesItsOwnChannel() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        let specDigest = item.agent_claims.first { $0.label == "Specification" }?.digest

        let rows = AttentionDisplay.attachmentDigestRows(item)

        let specRows = rows.filter { $0.label == "Specification Digest" }
        #expect(specRows.map(\.value) == [specDigest])
        #expect(!rows.contains { $0.label == "Claim Digest" && $0.value == specDigest })
    }

    @Test func sharedAttachmentDigestKeepsBothTrustChannelLabels() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let digest = item.evidence_snapshot[0].digest
        item.agent_claims[0].digest = digest
        item.artifact_digests = Array(Set(item.agent_claims.map(\.digest) + [digest])).sorted()

        let rows = AttentionDisplay.attachmentDigestRows(item)

        // agent_claims[0] on this fixture is the specification claim, so its
        // digest row reads "Specification Digest"; the evidence channel keeps
        // the same value under its own label from the separate seen-set.
        #expect(rows.contains(.init(label: "Evidence Digest", value: digest)))
        #expect(rows.contains(.init(label: "Specification Digest", value: digest)))
    }

    @Test func contextMenuEvidenceDigestsAreUniqueAndStable() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let first = item.evidence_snapshot[0]
        item.evidence_snapshot.append(first)

        let digests = AttentionDisplay.uniqueEvidenceDigests(item)

        #expect(digests == item.evidence_snapshot.dropLast().map(\.digest))
    }

    /// The posture reads in words, so it sits with the reading rows.
    @Test func detailReadingRowsIncludeEveryPresentPosture() {
        var item = AttentionFixtures.fixture(type: .system_health).item
        for (posture, label): (Components.Schemas.HealthPosture, String) in [
            (.advisory, "Advisory"), (.blocking, "Blocking"),
        ] {
            item.posture = .init(value1: posture)
            #expect(
                AttentionDisplay.detailReadingRows(item)
                    .filter { $0.label == "Posture" } == [.init(label: "Posture", value: label)])
            #expect(!AttentionDisplay.detailIdentifierRows(item).contains { $0.label == "Posture" })
        }
        item.posture = nil
        #expect(!AttentionDisplay.detailReadingRows(item).contains { $0.label == "Posture" })
    }

    @Test func detailIdentifiersKeepDistinctLabelsThatShareAValue() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        let rows = AttentionDisplay.detailIdentifierRows(item)

        #expect(rows.contains(.init(label: "PR Head", value: "cafebabe")))
        #expect(rows.contains(.init(label: "Head", value: "cafebabe")))
    }

    /// The diff's base and head are audit coordinates, so the Identifiers carry them
    /// whatever the readiness shape. The card renders Diff alone (#1107), and
    /// the checklist's "Bound to" row needs `readiness_detail`, which the
    /// schema lets a legacy or fake-mode item omit, so without these rows a
    /// legacy item renders its diff's base nowhere at all.
    @Test func detailIdentifiersCarryTheDiffBaseAndHead() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item

        let rows = AttentionDisplay.detailIdentifierRows(item)

        #expect(rows.contains(.init(label: "Diff Base", value: "deadbeef")))
        #expect(rows.contains(.init(label: "Diff Head", value: "cafebabe")))

        var legacy = item
        legacy.readiness_detail = nil
        let legacyRows = AttentionDisplay.detailIdentifierRows(legacy)

        #expect(legacyRows.contains(.init(label: "Diff Base", value: "deadbeef")))
        #expect(legacyRows.contains(.init(label: "Diff Head", value: "cafebabe")))
        #expect(!legacyRows.contains { $0.label == "Bound Base" })
    }

    @Test func detailIdentifiersExposeExactCreatedAndDueTimestamps() {
        var item = AttentionFixtures.fixture(type: .blocked).item
        item.expires_when = item.created_at?.addingTimeInterval(7_200)

        let rows = AttentionDisplay.detailIdentifierRows(item)

        #expect(rows.contains(.init(label: "Created", value: "2026-01-02T03:04:05Z")))
        #expect(rows.contains(.init(label: "Due", value: "2026-01-02T05:04:05Z")))
    }

    @Test func detailIdentifiersPreserveUnscopedSubjectIdentifiers() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        let rows = AttentionDisplay.detailIdentifierRows(item)

        #expect(rows.contains(.init(label: "Subject", value: "system")))
    }

    @Test func proposalBindingSurvivesMatchingAttachmentDigest() {
        let item = AttentionFixtures.fixture(type: .task_proposal).item
        let digest = item.artifact_digests[0]

        let rows = AttentionDisplay.detailIdentifierRows(item, proposalDigest: digest)

        #expect(rows.contains { $0.value == digest && $0.label.hasSuffix("Digest") })
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
                .init(label: "Recovery Run", value: "run-review_contradiction"),
                .init(label: "Invocation", value: "review-run-review_contradiction-1"),
                .init(label: "Round", value: "1"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure Digest",
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
                .init(label: "Recovery Run", value: "run-review_configuration"),
                .init(label: "Invocation", value: "review-run-review_configuration-2"),
                .init(label: "Round", value: "2"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure Digest",
                    value: "sha256:failure-review_configuration"
                ),
                .init(label: "Repository", value: "owner/repo"),
                .init(
                    label: "Superseded Profile",
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
                .init(label: "Auth Identity", value: "codex-primary"),
                .init(label: "Lease Fence", value: "4"),
                .init(label: "Auth Store Digest", value: "sha256:replacement-store"),
                .init(label: "Token Expires", value: "2026-08-12T02:44:05Z"),
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
                    label: "Adjudication Digest",
                    value: "sha256:adjudication-finding_adjudication"),
                .init(label: "Adjudication Run", value: "run-finding_adjudication"),
                .init(label: "Adjudication Round", value: "3"),
            ])
    }

    /// Every producer labels its route with the same fixed keyword; a model's
    /// judgment carries "(unverified)" and draws as a quote, and a compact
    /// mark names who proposed it, with the full producer sentence kept for
    /// hover help and VoiceOver, and the confidence the mark draws said
    /// aloud too (#2608).
    @Test(arguments: [
        (
            Components.Schemas.AdjudicationProducer.model,
            Components.Schemas.AdjudicationConfidence?.some(.high),
            "Proposed route (unverified)", "Agent · high", true, "Model proposal (unverified)",
            String?.some("high confidence")
        ),
        (
            .model, .some(.medium),
            "Proposed route (unverified)", "Agent · medium", true, "Model proposal (unverified)",
            "medium confidence"
        ),
        (
            .model, .some(.low),
            "Proposed route (unverified)", "Agent · low", true, "Model proposal (unverified)",
            "low confidence"
        ),
        (
            .model, nil, "Proposed route (unverified)", "Agent", true,
            "Model proposal (unverified)", nil
        ),
        (
            .engine_model, .some(.high),
            "Proposed route (unverified)", "Agent · engine may fix", true,
            "Model judgment with engine-authorized remediation", nil
        ),
        (.engine, .some(.high), "Proposed route", "Daemon", false, "Daemon recommendation", nil),
        (.human, nil, "Proposed route", "Human", false, "Human decision", nil),
    ])
    func findingAdjudicationProducersMapToKeywordMarkRegisterAndSentence(
        producer: Components.Schemas.AdjudicationProducer,
        confidence: Components.Schemas.AdjudicationConfidence?,
        keyword: String,
        mark: String,
        modelBacked: Bool,
        sentence: String,
        spokenConfidence: String?
    ) {
        let presentation = AttentionDisplay.adjudicationProducerPresentation(
            producer, confidence: confidence)

        #expect(presentation.spokenKeyword == keyword)
        #expect(presentation.mark == mark)
        #expect(presentation.modelBacked == modelBacked)
        #expect(presentation.sentence == sentence)
        #expect(presentation.spokenConfidence == spokenConfidence)
    }

    @Test func readyClassesRenderDistinctlyWithTheirEvaluationSet() {
        let clean = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let degraded = AttentionFixtures.degradedReady().item

        #expect(AttentionDisplay.title(clean) == "Ready for Final Review")
        #expect(AttentionDisplay.title(degraded) == "Ready for Final Review (Degraded)")
        #expect(
            AttentionDisplay.readinessIdentifierRows(clean) == [
                .init(label: "Evaluation Set", value: "sha256:evaluation-clean"),
                .init(label: "Bound Head", value: "cafebabe"),
                .init(label: "Bound Base", value: "main@deadbeef"),
                .init(label: "Handoff Tip", value: "deadbeef"),
            ])
        var moved = clean
        moved.readiness_detail = .init(
            value1: AttentionFixtures.movedTipReadinessDetail(head: clean.pr_head_sha))
        #expect(
            AttentionDisplay.readinessIdentifierRows(moved).suffix(2) == [
                .init(label: "Handoff Tip", value: "facefeed"),
                .init(label: "Handoff Merge", value: "0ddba11"),
            ])
        var untipped = clean
        untipped.readiness_detail?.value1.handoff_tip = nil
        #expect(
            AttentionDisplay.readinessIdentifierRows(untipped).last
                == .init(label: "Bound Base", value: "main@deadbeef"))
        #expect(
            AttentionDisplay.readinessReadingRows(clean) == [
                .init(label: "Readiness", value: "Clean"),
                .init(
                    label: "Requirement clean-verification",
                    value: "Clean verification, Required, Passed, proof sha256:recipe"),
                .init(
                    label: "Requirement independent-review",
                    value: "Independent review, Required, Passed, proof sha256:review-config"),
            ])
        #expect(
            AttentionDisplay.readinessIdentifierRows(degraded).first
                == .init(label: "Evaluation Set", value: "sha256:evaluation-degraded"))
        let degradedRows = AttentionDisplay.readinessReadingRows(degraded)
        #expect(degradedRows.first == .init(label: "Readiness", value: "Degraded"))
        #expect(
            degradedRows.contains(
                .init(
                    label: "Requirement license-headers",
                    value: "Repo change policy, Optional, Not run")))
        #expect(
            degradedRows.contains(
                .init(
                    label: "Requirement repo-change-policy",
                    value: "Repo change policy, Required, Failed")))
        #expect(
            degradedRows.last
                == .init(
                    label: "Waiver waiver-1",
                    value: "repo_change_policy, Explicit human approval, 2026-01-02T02:04:05Z"))

        var legacy = clean
        legacy.readiness_detail = nil
        #expect(
            AttentionDisplay.readinessIdentifierRows(legacy)
                == [.init(label: "Evaluation Set", value: "sha256:evaluation-clean")])
        #expect(
            AttentionDisplay.readinessReadingRows(legacy)
                == [.init(label: "Readiness", value: "Clean")])
    }

    @Test func readyReviewYieldRendersEveryRoundAndTerminalOutcome() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item

        #expect(
            AttentionDisplay.reviewYieldRows(item) == [
                .init(
                    label: "Review Round 1",
                    value: "2 findings · 2 new · 0 recurring · 1 fixed · 0 declined · 1 deferred · Findings"),
                .init(
                    label: "Review Round 2",
                    value: "2 findings · 1 new · 1 recurring · 1 fixed · 1 declined · 0 deferred · Findings"),
                .init(
                    label: "Review Round 3",
                    value: "0 findings · 0 new · 0 recurring · 0 fixed · 0 declined · 0 deferred · Clean"),
                .init(label: "Terminal Review", value: "Clean"),
            ])
    }

    /// Diff growth compares round 1 with the latest measured round, as
    /// counts the card draws in the diff cuts (R28). A round without metrics
    /// is a gap, and the label names the round a gap would hide: without
    /// round 1 the row is the latest measured round's size alone, growth
    /// that stops short of the last round says where, and without any
    /// measurement there is no row.
    @Test func diffGrowthHandlesBothMetricGaps() throws {
        let now = AttentionFixtures.createdInstant
        func diffRow(_ item: Components.Schemas.AttentionItem) -> AttentionDisplay.FactRow? {
            AttentionDisplay.cardFacts(item, now: now).first { $0.label != "Cost so Far" }
        }
        var item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        let latest = try #require(item.yield_history?.value1.rounds.indices.last)
        let first = DiffCounts(added: 120, removed: 8)
        let last = DiffCounts(added: 210, removed: 14)

        let growth = try #require(diffRow(item))
        #expect(growth == .init("Diff Growth", diffs: [first, last]))
        #expect(growth.value == "+120 \u{2212}8 \u{2192} +210 \u{2212}14")
        #expect(
            DiffCounts.spoken([first, last]) == "120 added, 8 removed, then 210 added, 14 removed")

        item.yield_history?.value1.rounds[0].diff_metrics = nil
        #expect(diffRow(item) == .init("Diff Size at Round 3", diffs: [last]))

        item.yield_history?.value1.rounds[latest].diff_metrics = nil
        #expect(diffRow(item) == nil)

        // Round 1 measured alone is a size, not growth.
        item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        item.yield_history?.value1.rounds[latest].diff_metrics = nil
        #expect(diffRow(item) == .init("Diff Size at Round 1", diffs: [first]))

        // Growth measured only to an earlier round names that round.
        item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        let measured = try #require(item.yield_history?.value1.rounds[latest].diff_metrics)
        item.yield_history?.value1.rounds[latest].diff_metrics = nil
        item.yield_history?.value1.rounds[latest - 1].diff_metrics = measured
        #expect(
            diffRow(item)
                == .init("Diff Growth to Round 2", diffs: [first, last]))

        // The row needs no cost: every cause shows it from the yield history.
        item = AttentionFixtures.reviewDiminishing(cause: .growth_without_blockers).item
        item.billable_cost_so_far = nil
        #expect(AttentionDisplay.cardFacts(item, now: now).map(\.label) == ["Diff Growth"])
    }

    @Test func driftAuditItemsNameTheirAuditDigestInDetails() {
        let drift = AttentionFixtures.reviewDiminishing(cause: .drift_audit, verdict: .stuck).item
        #expect(
            AttentionDisplay.detailIdentifierRows(drift).contains(
                .init(label: "Drift Audit", value: "sha256:drift-audit-stuck")))
        #expect(
            !AttentionDisplay.detailIdentifierRows(
                AttentionFixtures.fixture(type: .review_diminishing_returns).item
            ).contains { $0.label == "Drift Audit" })
    }

    @Test func diminishingReviewYieldRendersEveryRoundAndTerminalOutcome() {
        let item = AttentionFixtures.fixture(type: .review_diminishing_returns).item

        #expect(
            AttentionDisplay.reviewYieldRows(item) == [
                .init(
                    label: "Review Round 1",
                    value: "4 findings · 4 new · 0 recurring · 2 fixed · 1 declined · 1 deferred · Findings"),
                .init(
                    label: "Review Round 2",
                    value: "3 findings · 1 new · 2 recurring · 1 fixed · 1 declined · 1 deferred · Findings"),
                .init(
                    label: "Review Round 3",
                    value: "3 findings · 0 new · 3 recurring · 0 fixed · 2 declined · 1 deferred · Findings"),
                .init(label: "Terminal Review", value: "Findings"),
            ])
    }

    @Test func legacyReadyItemOmitsReviewYieldRows() {
        var item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        item.yield_history = nil

        #expect(AttentionDisplay.reviewYieldRows(item).isEmpty)
    }

    private func value(_ rows: [AttentionDisplay.FactRow], _ label: String) -> String? {
        rows.first { $0.label == label }?.value
    }

    @Test func effectProposalRowsNameTheTargetMergeAndTrust() throws {
        let verifiedFacts = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        let recommendedFacts = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.recommendedEffectProposal()))

        let verified = AttentionDisplay.effectProposalRows(verifiedFacts)
        let recommended = AttentionDisplay.effectProposalRows(recommendedFacts)

        #expect(
            verified.map(\.label) == ["Effect", "Target Issue", "On Merge", "Reference", "Closure Flag", "Bound to"])
        #expect(value(verified, "Effect") == "Source issue closure")
        #expect(value(verified, "Target Issue") == "owner/repo#724")
        #expect(value(verified, "On Merge") == "Closes the issue")
        // Origin names the mechanism that emitted the flag (the inference
        // site), not the human work proposal.
        #expect(value(verified, "Closure Flag") == "Emitted by the inference site")

        // A verified and a recommended closure differ in their Reference row;
        // only the recommended card says the person is confirming the reference.
        #expect(value(verified, "Reference") != value(recommended, "Reference"))
        #expect(value(recommended, "Reference")?.lowercased().contains("confirm") == true)
        #expect(value(verified, "Reference")?.lowercased().contains("confirm") == false)

        // The card names the issue reference but shows no PR number.
        #expect(verified.contains { $0.value == "owner/repo#724" })
        #expect(verified.allSatisfy { !$0.label.lowercased().contains("pr") })

        // Bound to names the merge the approval binds to: the head and the base.
        let bound = try #require(value(verified, "Bound to"))
        #expect(bound.contains("cafebabe"))
        #expect(bound.contains("main@deadbeef"))
    }

    @Test func closureOriginLabelsNameEachMechanism() {
        #expect(AttentionDisplay.closureOriginLabel(.policy_default) == "Set by project policy")
        let labels = Components.Schemas.ClosureFlagOrigin.allCases.map(AttentionDisplay.closureOriginLabel)
        #expect(Set(labels).count == Components.Schemas.ClosureFlagOrigin.allCases.count)
    }

    @Test func effectProposalOnMergeFollowsResolves() throws {
        var facts = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        var closure = try #require(facts.source_issue_closure?.value1)
        closure.resolves = false
        facts.source_issue_closure = .init(value1: closure)

        let rows = AttentionDisplay.effectProposalRows(facts)
        #expect(value(rows, "On Merge") == "Doesn't close the issue")
        // The Effect row names the kind neutrally, so a resolve-false proposal
        // does not contradict the "On Merge" row.
        #expect(value(rows, "Effect") == "Source issue closure")
    }

    private func filingFacts() throws -> Components.Schemas.EffectProposalFactsSnapshot {
        try #require(
            AttentionFixtures.effectProposalFacts(
                for: AttentionFixtures.followUpFilingEffectProposal()))
    }

    @Test func followUpFilingRowsNameWhereItFilesWhatItCarriesAndItsSource() throws {
        let rows = AttentionDisplay.effectProposalRows(try filingFacts())

        #expect(
            rows == [
                .init("Effect", "Follow-up issue filing"),
                .init("Repository", "owner/repo", monospaced: true),
                .init("Labels", "deferred-finding, reliability"),
                .init("Milestone", "Backlog"),
                .init("Source", "Finding finding-3 · Deferred disposition"),
                .init("Text Screening", "Title and body: passed under github-issue/1"),
            ])
    }

    @Test func followUpFilingRowsSayNoneForNoLabelsAndNoMilestone() throws {
        var facts = try filingFacts()
        facts.follow_up_filing?.value1.labels = []
        facts.follow_up_filing?.value1.milestone = nil

        let rows = AttentionDisplay.effectProposalRows(facts)
        #expect(value(rows, "Labels") == "None")
        #expect(value(rows, "Milestone") == "None")
    }

    private func adoptionFacts() throws -> Components.Schemas.EffectProposalFactsSnapshot {
        try #require(
            AttentionFixtures.effectProposalFacts(
                for: AttentionFixtures.imagePolicyAdoptionEffectProposal()))
    }

    @Test func imagePolicyAdoptionRowsNameTheShortDigestAndHosts() throws {
        let rows = AttentionDisplay.effectProposalRows(try adoptionFacts())

        #expect(
            rows == [
                .init("Effect", "Image policy adoption"),
                .init("Repository", "owner/repo", monospaced: true),
                .init("Recipe", "sha256:dddddddd", monospaced: true),
                .init("Registry Hosts", "proxy.golang.org", monospaced: true),
            ])
    }

    @Test func imagePolicyAdoptionCardLeadsWithItsSentenceInPlaceOfTheEffectRow() throws {
        let facts = try adoptionFacts()

        #expect(
            AttentionDisplay.effectBinding(facts)
                == .init(
                    lead: "Approving adds the tip's recipe and registry host ",
                    head: "proxy.golang.org",
                    outcome: " to the repository's image policy, so upkeep can rebuild the image."))
        #expect(
            AttentionDisplay.effectProposalCardRows(facts).map(\.label) == ["Repository", "Recipe", "Registry Hosts"])
    }

    @Test func imagePolicyAdoptionSentenceNamesOnlyWhatTheAdoptionAdds() throws {
        var facts = try adoptionFacts()
        facts.image_policy_adoption?.value1.recipe_digest = nil
        facts.image_policy_adoption?.value1.registry_authorities = ["proxy.golang.org", "registry.npmjs.org"]
        #expect(
            AttentionDisplay.effectBinding(facts)?.plain
                == "Approving adds registry hosts proxy.golang.org, registry.npmjs.org "
                + "to the repository's image policy, so upkeep can rebuild the image.")

        facts = try adoptionFacts()
        facts.image_policy_adoption?.value1.registry_authorities = []
        #expect(
            AttentionDisplay.effectBinding(facts)?.plain
                == "Approving adds the tip's recipe to the repository's image policy, so upkeep can rebuild the image.")

        // Facts that add nothing state nothing, and keep the "Effect" row.
        facts.image_policy_adoption?.value1.recipe_digest = nil
        #expect(AttentionDisplay.effectBinding(facts) == nil)
        #expect(AttentionDisplay.effectProposalCardRows(facts).first?.label == "Effect")
    }

    @Test func imagePolicyAdoptionDetailRowsCarryTheFullDigestAndRefusal() throws {
        var facts = try adoptionFacts()
        #expect(
            AttentionDisplay.imagePolicyAdoptionDetailRows(facts) == [
                .init(label: "Repository ID", value: "84958515"),
                .init(label: "Recipe Digest", value: "sha256:" + String(repeating: "d", count: 64)),
                .init(label: "Upkeep Refusal", value: "sha256:" + String(repeating: "e", count: 64)),
            ])

        facts.image_policy_adoption?.value1.recipe_digest = nil
        #expect(
            AttentionDisplay.imagePolicyAdoptionDetailRows(facts).map(\.label) == ["Repository ID", "Upkeep Refusal"])

        let closure = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        #expect(AttentionDisplay.imagePolicyAdoptionDetailRows(closure).isEmpty)
    }

    @Test func imagePolicyAdoptionRowsSayWhatTheAdoptionLeavesAlone() throws {
        var facts = try adoptionFacts()
        facts.image_policy_adoption?.value1.recipe_digest = nil
        facts.image_policy_adoption?.value1.registry_authorities = []

        let rows = AttentionDisplay.effectProposalRows(facts)
        #expect(value(rows, "Recipe") == "Unchanged")
        #expect(value(rows, "Registry Hosts") == "None")
    }

    @Test func imagePolicyAdoptionRowsListEveryHost() throws {
        var facts = try adoptionFacts()
        facts.image_policy_adoption?.value1.registry_authorities = ["proxy.golang.org", "registry.npmjs.org"]

        #expect(
            value(AttentionDisplay.effectProposalRows(facts), "Registry Hosts")
                == "proxy.golang.org, registry.npmjs.org")
    }

    @Test func imagePolicyAdoptionKindWithoutItsArmHasNoRows() throws {
        var facts = try adoptionFacts()
        facts.image_policy_adoption = nil

        #expect(AttentionDisplay.effectProposalRows(facts).isEmpty)
    }

    @Test func followUpFilingSourceRowNamesASeparateWorkVerdict() throws {
        var facts = try filingFacts()
        facts.follow_up_filing?.value1.source.kind = .separate_work_verdict

        #expect(
            value(AttentionDisplay.effectProposalRows(facts), "Source")
                == "Finding finding-3 · Separate-work verdict")
    }

    @Test func followUpFilingScreeningRowNamesEachFieldWhenTheyDiffer() throws {
        var facts = try filingFacts()
        facts.follow_up_filing?.value1.body.verdict = .rejected

        #expect(
            value(AttentionDisplay.effectProposalRows(facts), "Text Screening")
                == "Title: passed under github-issue/1 · Body: rejected under github-issue/1")
    }

    @Test func followUpFilingTitleAndBodyAreNeverFactRows() throws {
        let facts = try filingFacts()
        let filing = try #require(facts.follow_up_filing?.value1)

        // The agent wrote the title and body, so no daemon-fact row carries
        // either; the card draws them from proposedIssueText in their own
        // unverified sections.
        for row in AttentionDisplay.effectProposalRows(facts) {
            #expect(!row.value.contains(filing.title.text))
            #expect(!row.value.contains(filing.body.text))
        }
        for row in AttentionDisplay.followUpFilingDetailRows(facts) {
            #expect(!row.value.contains(filing.title.text))
            #expect(!row.value.contains(filing.body.text))
        }
        #expect(
            AttentionDisplay.proposedIssueText(facts)
                == .init(title: filing.title.text, body: filing.body.text))
    }

    @Test func followUpFilingDetailRowsCarryTheRepositoryIDAndFullDigest() throws {
        #expect(
            AttentionDisplay.followUpFilingDetailRows(try filingFacts()) == [
                .init(label: "Repository ID", value: "84958515"),
                .init(label: "Finding", value: "finding-3"),
                .init(
                    label: "Adjudication Digest",
                    value: "sha256:" + String(repeating: "c", count: 64)),
            ])
    }

    @Test func filingTextAndDetailRowsFollowTheEffectKindNotTheArm() throws {
        // A closure that carries a stray filing arm draws closure rows only:
        // the display follows effect_kind, as the match gate does.
        var closure = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        closure.follow_up_filing = try filingFacts().follow_up_filing

        #expect(AttentionDisplay.proposedIssueText(closure) == nil)
        #expect(AttentionDisplay.followUpFilingDetailRows(closure).isEmpty)
        #expect(value(AttentionDisplay.effectProposalRows(closure), "Effect") == "Source issue closure")

        // A filing kind with no filing arm draws nothing.
        var armless = try filingFacts()
        armless.follow_up_filing = nil
        #expect(AttentionDisplay.effectProposalRows(armless).isEmpty)
        #expect(AttentionDisplay.proposedIssueText(armless) == nil)
    }

    @Test func screenedIssueTextExplanationSaysWhatScreeningDoesNotEstablish() {
        #expect(
            AttentionDisplay.screenedIssueTextExplanation
                == "Written by the agent. The daemon screened this text; it did not check that it is true.")
    }

    @Test func effectProposalBoundToShortensRevisionsToEightCharacters() throws {
        var facts = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        var closure = try #require(facts.source_issue_closure?.value1)
        closure.merge.candidate_head_sha = "0123456789abcdef0123456789abcdef01234567"
        closure.merge.base_sha = "fedcba9876543210fedcba9876543210fedcba98"
        facts.source_issue_closure = .init(value1: closure)

        #expect(
            value(AttentionDisplay.effectProposalRows(facts), "Bound to")
                == "Head 01234567 · Base main@fedcba98")
    }

    @Test func effectProposalSupersededRowAppearsOnlyWithPriorFacts() throws {
        let base = try #require(
            AttentionFixtures.effectProposalFacts(for: AttentionFixtures.fixture(type: .effect_proposal)))
        #expect(!AttentionDisplay.effectProposalRows(base).contains { $0.label == "Superseded Proposal" })

        var revised = base
        revised.supersedes = .init(
            value1: .init(
                proposal_digest: "sha256:prior-effect",
                source_issue_closure: .init(resolves: false)))
        let row = value(AttentionDisplay.effectProposalRows(revised), "Superseded Proposal")
        #expect(row?.contains("leaving the issue open") == true)
        // The prior digest is shown in full, as the sibling "Prior Proposal"
        // binding row does; a Digest is algorithm-prefixed, so it is not
        // abbreviated.
        #expect(row?.contains("sha256:prior-effect") == true)
    }

    @Test func decliningAnEffectProposalNamesTheEffectNotARun() {
        let effect = AttentionFixtures.fixture(type: .effect_proposal).item
        let task = AttentionFixtures.fixture(type: .task_proposal).item

        #expect(
            AttentionDisplay.confirmationConsequence(.decline, for: effect)
                == "The proposal is dismissed and the effect is not applied.")
        #expect(
            AttentionDisplay.confirmationConsequence(.decline, for: task)
                == "The proposal is dismissed and no run starts.")
    }

    @Test func everyReviewGuideEnumHasALabel() {
        #expect(
            Components.Schemas.ReviewAttention.allCases.map(AttentionDisplay.label)
                == ["Tradeoff", "Contract", "Product", "Risk", "Accessibility"])
        #expect(
            Components.Schemas.ReviewExcerptSide.allCases.map(AttentionDisplay.label) == ["Head", "Base"])
        let reasons = Components.Schemas.ReviewExcerptUnresolved.allCases
        #expect(
            reasons.map { AttentionDisplay.label($0, side: .head) } == [
                "No such file at the head.", "The lines run past the end of the file.",
                "The file is binary.", "The lines are too large to show.",
            ])
        #expect(AttentionDisplay.label(.path_missing, side: .base) == "No such file at the base.")
    }

    @Test func excerptRangeNamesTheFileAndItsLines() throws {
        let guide = try #require(AttentionFixtures.readyWithReviewGuide().item.review_guide?.value1)
        var excerpt = guide.stops[0].excerpts[0]
        #expect(AttentionDisplay.excerptRange(excerpt) == "daemon/internal/domain/review_guide.go:10-11")
        excerpt.end_line = 10
        #expect(AttentionDisplay.excerptRange(excerpt) == "daemon/internal/domain/review_guide.go:10")
    }

    @Test func excerptSpokenRangeReadsTheLinesAsWords() throws {
        let guide = try #require(AttentionFixtures.readyWithReviewGuide().item.review_guide?.value1)
        var excerpt = guide.stops[0].excerpts[0]
        #expect(
            AttentionDisplay.excerptSpokenRange(excerpt)
                == "daemon/internal/domain/review_guide.go, lines 10 to 11")
        excerpt.end_line = 10
        #expect(AttentionDisplay.excerptSpokenRange(excerpt) == "daemon/internal/domain/review_guide.go, line 10")
    }

    @Test func aReviewStopReadsAsOneElement() throws {
        let guide = try #require(AttentionFixtures.readyWithReviewGuide().item.review_guide?.value1)
        #expect(
            AttentionDisplay.reviewStopAccessibilityLabel(guide.stops[0], number: 1, of: 2)
                == "Stop 1 of 2, Contract, Start with the guide's validation. "
                + "`Validate` enforces every cap the schema cannot express.")
    }
}
