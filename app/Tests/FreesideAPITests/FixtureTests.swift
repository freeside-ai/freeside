import FreesideAPI
import Testing

@Suite struct FixtureTests {
    /// Independent transcription of plan §4's per-type action table
    /// (docs/plan.md §4 "Actions"), for the nine types §4 defines;
    /// signet's policy pins `blocked` read-only (no actions), and the
    /// fixture's placeholder set lasts only until #96 relaxes the
    /// contract's non-empty requested_decision.
    static let planSection4:
        [Components.Schemas.AttentionType: [Components.Schemas.Action]] = [
            .spec_approval: [.approve, .request_changes, .discuss, .stop],
            .review_diminishing_returns: [
                .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
            ],
            .review_dispute: [.adjudicate, .discuss, .stop],
            .execution_failure: [.retry, .retry_with_capabilities, .discuss, .stop],
            .agent_question: [.answer_and_retry, .answer_without_retry, .stop],
            .publish_blocked: [
                .rerun_trust_evaluation, .choose_alternate_profile, .inspect_trust_failure, .stop,
            ],
            .ready_for_final_review: [.open_pr, .return_to_agent, .mark_seen, .dismiss, .stop],
            .run_proposal: [.start, .start_with_changes, .decline, .snooze],
            .system_health: [.acknowledge, .run_doctor, .stop_unattended],
        ]

    @Test func actionSetsMatchPlanSection4() {
        for (type, actions) in Self.planSection4 {
            #expect(AttentionFixtures.phase1ActionSets[type] == actions)
        }
        // blocked is pinned read-only by signet policy, but the contract
        // still requires at least one action (requested_decision
        // minItems 1) until #96 relaxes it; the placeholder holds.
        let blocked = AttentionFixtures.phase1ActionSets[.blocked]
        #expect(!(blocked ?? []).isEmpty)
        #expect(AttentionFixtures.phase1ActionSets.count == 10)
    }

    @Test func defaultInboxCoversEveryPhase1TypeOnce() {
        let inbox = AttentionFixtures.defaultInbox()
        #expect(inbox.map(\.item._type) == AttentionFixtures.phase1Types)
        #expect(Set(inbox.map(\.item.id)).count == inbox.count)
    }

    @Test(arguments: AttentionFixtures.phase1Types)
    func fixtureIsValidAndOffersExactlyItsActionSet(
        type: Components.Schemas.AttentionType
    ) {
        let item = AttentionFixtures.fixture(type: type).item
        #expect(item.requested_decision == AttentionFixtures.phase1ActionSets[type])
        #expect(item.status == .open)
        // artifact_digests is the daemon-derived canonical binding set:
        // the sorted, deduplicated union of evidence and claim digests.
        let union = item.evidence_snapshot.map(\.digest) + item.agent_claims.map(\.digest)
        #expect(item.artifact_digests == Array(Set(union)).sorted())
    }
}
