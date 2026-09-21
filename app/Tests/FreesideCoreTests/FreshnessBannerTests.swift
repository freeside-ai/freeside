import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct FreshnessBannerTests {
    @Test func rePairActionShowsOnlyForRevokedWithAHandler() {
        // #1458 acceptance 6: only the revoked (unauthenticated) banner
        // offers "Pair Again", and only when a handler is wired.
        let states: [InboxStore.Freshness] = [
            .unvalidated,
            .fresh,
            .unreachable,
            .syncFailing,
            .contractMismatch(daemonContract: "sha256:" + String(repeating: "a", count: 64)),
            .unauthenticated,
        ]
        for state in states {
            let isRevoked: Bool
            if case .unauthenticated = state { isRevoked = true } else { isRevoked = false }

            #expect(FreshnessBanner.showsRePairAction(for: state, hasHandler: true) == isRevoked)
            #expect(!FreshnessBanner.showsRePairAction(for: state, hasHandler: false))
        }
    }

    @Test func confirmationNamesUnresolvedActionsMadeUnderTheOldPairing() {
        // The confirmation warns only when unresolved commands from the old
        // pairing exist; the app drops them once the device id changes. It
        // says "won't be retried", not "unsent": a lost response may already
        // have committed on the daemon.
        #expect(
            !FreesideRootView.rePairConfirmationMessage(pendingUnderOldPairing: 0)
                .contains("won't be retried"))
        #expect(
            FreesideRootView.rePairConfirmationMessage(pendingUnderOldPairing: 1)
                .contains("1 unresolved action made under the old pairing won't be retried"))
        #expect(
            FreesideRootView.rePairConfirmationMessage(pendingUnderOldPairing: 3)
                .contains("3 unresolved actions made under the old pairing won't be retried"))
    }

    @Test func failureCopyReportsUnconfirmedRemovalNotStillPaired() {
        // #1458 review: `rePair()` throws both when the credential definitely
        // remains and when removal is indeterminate (the reload also failed),
        // so the alert must not assert the device is still paired.
        #expect(FreesideRootView.rePairFailureMessage.contains("removal isn't confirmed"))
        #expect(!FreesideRootView.rePairFailureMessage.contains("so it's still paired"))
    }

    @Test func unsentActionCountExcludesAcceptedTaskStops() async throws {
        // #1458 review: a Stop keeps its entry with a non-nil receipt after
        // the daemon accepts it, until canonical refresh reconciles
        // cancellation. That action was sent, so the re-pair confirmation
        // must not count it among the unsent actions it warns will be dropped.
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()),
            device: DeviceIdentity(deviceID: "device-mock"),
            cache: InMemoryCacheStore(), submissionDaemonID: "mock")
        await coordinator.refresh()
        let task = try #require(coordinator.tasks.first { $0.task.cancellation == nil })
        let prepared = try #require(coordinator.taskStop.prepare(taskID: task.task.id))
        let unsent = prepared.entry
        let output = try await coordinator.store.client.submitCommand(
            body: .json(prepared.entry.command))
        var accepted = prepared.entry
        accepted.receipt = try output.ok.body.json

        #expect(
            FreesideRootView.unsentActionCount(
                pendingCommands: 0, pendingTaskSubmissions: 0, taskStops: [accepted]) == 0)
        #expect(
            FreesideRootView.unsentActionCount(
                pendingCommands: 0, pendingTaskSubmissions: 0, taskStops: [unsent]) == 1)
        #expect(
            FreesideRootView.unsentActionCount(
                pendingCommands: 2, pendingTaskSubmissions: 1, taskStops: [accepted, unsent]) == 4)
    }
}
