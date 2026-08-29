import FreesideAPI
import Testing

@Suite struct ActionOutcomeTests {
    @Test func phaseOneActionsMirrorDaemonOutcomeTable() {
        let expected: [Components.Schemas.Action: ActionOutcome] = [
            .approve: .concludes(.resolved),
            .request_changes: .concludes(.superseded),
            .discuss: .discusses,
            .stop: .concludes(.resolved),
            .finish_now: .concludes(.resolved),
            .apply_then_finish: .concludes(.resolved),
            .continue_under_policy: .concludes(.resolved),
            .convert_to_policy: .pending,
            .retry: .concludes(.resolved),
            .retry_with_capabilities: .pending,
            .answer_and_retry: .pending,
            .answer_without_retry: .pending,
            .rerun_trust_evaluation: .concludes(.resolved),
            .choose_alternate_profile: .pending,
            .inspect_trust_failure: .records,
            .open_pr: .records,
            .return_to_agent: .pending,
            .mark_seen: .records,
            .dismiss: .concludes(.dismissed),
            .start: .concludes(.resolved),
            .start_with_changes: .revisesProposal,
            .decline: .concludes(.dismissed),
            .snooze: .snoozesProposal,
            .acknowledge: .records,
            .run_doctor: .records,
            .stop_unattended: .stopsUnattended,
            .resume_unattended: .resumesUnattended,
            .recover_review: .recoversReview,
            .adopt_review_configuration: .adoptsReviewConfiguration,
            .resolve_reenrollment: .resolvesReenrollment,
            .accept_recommended_route: .concludes(.resolved),
            .choose_alternative_route: .concludes(.resolved),
        ]

        #expect(expected.count == AttentionFixtures.phase1Actions.count)
        for action in AttentionFixtures.phase1Actions {
            #expect(ActionOutcome.of(action) == expected[action])
        }
        #expect(
            Set(expected.compactMap { $0.value == .pending ? $0.key : nil }) == [
                .convert_to_policy, .retry_with_capabilities,
                .choose_alternate_profile, .answer_and_retry, .answer_without_retry,
                .return_to_agent,
            ])
    }
}
