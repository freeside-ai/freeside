import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@MainActor
@Suite struct DecisionModelTests {
    // MARK: - Acceptance 1: exactly the item's §4 action set

    @Test(arguments: AttentionFixtures.phase1Types)
    func offersExactlyTheRequestedDecisionSet(
        type: Components.Schemas.AttentionType
    ) async {
        let store = await makeStore(server: MockServer())
        let model = DecisionModel(store: store, itemID: "item-\(type.rawValue)")
        #expect(model.offeredActions == AttentionFixtures.phase1ActionSets[type])
    }

    // MARK: - Acceptance 2: stale submission swaps in the replacement

    @Test func staleSubmissionSwapsInTheReplacementWithoutCorruption() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        // A second device resolves the race by writing first.
        await server.advance(itemID: "item-spec_approval")

        await model.submit(.approve)
        #expect(model.phase == .superseded)
        #expect(model.appliedRecord == nil)
        let replacement = await server.snapshot(itemID: "item-spec_approval")
        #expect(model.snapshot == replacement)
        #expect(store.snapshotsByID["item-spec_approval"] == replacement)
        // The replacement is canonical and open: deciding again is allowed.
        #expect(model.actionsEnabled)
    }

    // MARK: - Acceptance 3: read-your-write; pending never renders applied

    @Test func pendingCommandNeverRendersAppliedAndAppliesOnRelease() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                await reached.open()
                await release.wait()
            }
        }

        let submission = Task { await model.submit(.approve) }
        await reached.wait()
        // In flight: pending renders as pending, never as applied.
        #expect(model.phase == .submitting(.approve))
        #expect(model.appliedRecord == nil)
        #expect(!model.actionsEnabled)
        #expect(model.snapshot?.item.status == .open)

        await release.open()
        await submission.value
        // Read-your-write: the acknowledged decision reflects immediately.
        #expect(model.phase == .applied)
        #expect(model.appliedRecord?.action == .approve)
        #expect(model.snapshot?.item.status == .resolved)
        #expect(!model.actionsEnabled)
    }

    @Test func lostSubmissionEntersTheLedgerAndRetriesWithTheSameCommandID() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-agent_question")
        await model.validate()

        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        await model.submit(.stop)
        // The response was lost: nothing renders as applied, the command
        // sits in the store's ledger, and no new command can be minted.
        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
        #expect(model.submissionError != nil)
        #expect(!model.actionsEnabled)
        #expect(model.canRetryLostResponse)
        let minted = store.pendingCommandsByItemID["item-agent_question"]?.command.command_id

        await server.setBeforeRespond(nil)
        await model.retryLostResponse()
        #expect(model.phase == .applied)
        #expect(model.appliedRecord?.command_id == minted)
        #expect(model.pendingCommand == nil)
    }

    @Test func transientServerErrorSettlesByImmediateReplay() async {
        // A 503 is not an authoritative rejection: the command enters the
        // ledger and the immediate settling resend recovers it once the
        // transient failure clears, with the same command_id.
        let server = MockServer()
        let client = Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: StatusOverrideTransport(
                base: MockServerTransport(server: server),
                operationID: "submitCommand",
                status: 503,
                once: OneShot()
            )
        )
        let store = InboxStore(client: client)
        await store.refresh()
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        await model.submit(.approve)
        #expect(model.phase == .applied)
        #expect(model.appliedRecord?.action == .approve)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .resolved)
    }

    @Test func pendingActionIsRefusedLocallyWithoutARequest() async {
        // discuss is offered but its transaction belongs to a later
        // unit: the card renders it disabled and the model refuses to
        // build a command for it.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.offeredActions.contains(.discuss))
        #expect(!model.isSubmittable(.discuss))

        await model.submit(.discuss)
        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
        #expect(model.submissionError == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
    }

    @Test func blockedItemOffersNoActionableDecision() async {
        // Signet policy pins blocked read-only: since #96 it offers the
        // empty set, so the card renders no action button, and even a
        // stray action stays unsubmittable.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-blocked")
        await model.validate()

        #expect(model.offeredActions.isEmpty)
        #expect(!model.isSubmittable(.acknowledge))

        await model.submit(.acknowledge)
        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
    }

    @Test func lostResponseAfterANonTerminalCommitIsRecoveredByReplay() async throws {
        // The daemon committed acknowledge but the response was lost. The
        // retry resends the original command verbatim so the recorded
        // result is replayed, never a re-prepared body under the reused
        // id (which the daemon rejects as misuse).
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()

        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        await model.submit(.acknowledge)
        #expect(model.appliedRecord == nil)
        guard let original = model.pendingCommand else {
            Issue.record("missing pending command")
            return
        }

        // The first attempt did reach the daemon: commit it as sent.
        await server.setBeforeRespond(nil)
        let client = APIClientFactory.mock(server: server)
        _ = try await client.submitCommand(body: .json(original)).ok.body.json
        let committed =
            try await client
            .getAttentionItem(path: .init(item_id: "item-system_health")).ok.body.json
        #expect(committed.item.status == .open)

        await model.retryLostResponse()
        #expect(model.submissionError == nil)
        #expect(model.appliedRecord?.command_id == original.command_id)
        #expect(model.pendingCommand == nil)
        // Replayed, not reapplied: the record-only item never advanced.
        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-system_health")).ok.body.json
        #expect(after.item.item_version == committed.item.item_version)
    }

    @Test func itemClosedElsewhereFailsClosedInsteadOfReEnablingTheStaleCard() async throws {
        // Another device resolves the item after this card validated;
        // the daemon rejects the submission as a closed-item 409 carrying
        // the canonical closed item (the #65 decision). The card must not
        // re-enable against its stale open snapshot: the replacement
        // swaps in and the status gate disables every action.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        let otherDevice = APIClientFactory.mock(server: server)
        let current =
            try await otherDevice
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        _ = try await otherDevice.submitCommand(
            body: .json(
                .init(
                    command_id: "cmd-other-device",
                    device_id: "device-other",
                    expected_entity_version: current.entity_version,
                    expected_bindings: .init(additionalProperties: [:]),
                    payload: .init(
                        item_id: "item-spec_approval",
                        action: .approve,
                        item_version: current.item.item_version,
                        pr_head_sha: current.item.pr_head_sha,
                        artifact_digests: current.item.artifact_digests
                    )
                ))
        ).ok.body.json

        await model.submit(.approve)
        #expect(model.appliedRecord == nil)
        // Closure shares the 409 replacement shape (the #65 decision):
        // the closed replacement swaps in, the status gate disables the
        // card, and no ledger entry suggests a recoverable result.
        #expect(model.phase == .superseded)
        #expect(model.snapshot?.item.status == .resolved)
        #expect(!model.actionsEnabled)
        #expect(!model.canRetryLostResponse)
        #expect(model.pendingCommand == nil)
    }

    @Test func nonResolvingDecisionKeepsTheItemOpenAndDecidable() async {
        // Plan §4: acknowledge means seen, never resolved; a system_health
        // item stays blocking, so further actions remain available.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()

        await model.submit(.acknowledge)
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.phase == .idle)
        #expect(model.actionsEnabled)

        await model.submit(.stop_unattended)
        #expect(model.appliedRecord?.action == .stop_unattended)
        #expect(model.snapshot?.item.status == .resolved)
        #expect(model.phase == .applied)
        #expect(!model.actionsEnabled)
    }

    @Test func pendingCommandBlocksNewSubmissionsUntilSettled() async throws {
        // acknowledge committed with its response lost twice (the
        // original and the settling resend). The ledger blocks every new
        // command for the item — an in-flight command can still commit
        // after any refetch — until an explicit retry settles it; only
        // then does a different action proceed.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()
        guard let before = model.snapshot else {
            Issue.record("missing snapshot")
            return
        }

        let lostResponses = InjectedFailures(times: 2)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponses.consume() }
        }
        await model.submit(.acknowledge)
        #expect(model.pendingCommand?.payload.action == .acknowledge)
        #expect(!model.actionsEnabled)
        #expect(model.canRetryLostResponse)

        // Blocked: the guard refuses a new command outright.
        await model.submit(.stop_unattended)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand?.payload.action == .acknowledge)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.pendingCommand == nil)
        #expect(model.actionsEnabled)

        await model.submit(.stop_unattended)
        #expect(model.appliedRecord?.action == .stop_unattended)
        #expect(model.phase == .applied)
        // acknowledge is record-only and stop_unattended concludes: the
        // item advanced exactly once, so the retry replayed rather than
        // reapplied.
        #expect(model.snapshot?.item.status == .resolved)
        #expect(model.snapshot?.item.item_version == before.item.item_version + 1)
    }

    @Test func replayConflictPresentsAsSuperseded() async {
        // The lost command never committed and another device advanced
        // the item before the retry. The resend's 409 must present like
        // a live conflict: replacement swapped in, superseded banner,
        // deciding again allowed against the canonical state.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        await model.submit(.approve)
        #expect(model.pendingCommand != nil)

        await server.setBeforeRespond(nil)
        await server.advance(itemID: "item-spec_approval")
        await model.retryLostResponse()

        #expect(model.phase == .superseded)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        let replacement = await server.snapshot(itemID: "item-spec_approval")
        #expect(model.snapshot == replacement)
        #expect(model.actionsEnabled)
    }

    @Test func modelRecreationDuringASuspendedSubmissionStaysBlocked() async throws {
        // The slot is claimed before the first request leaves: while the
        // original submission is still suspended awaiting its response,
        // a recreated card must see the in-flight command, keep actions
        // disabled, and refuse to mint a second command — otherwise two
        // record-only commands could both commit against one item version.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                await reached.open()
                await release.wait()
            }
        }
        let submission = Task { await model.submit(.acknowledge) }
        await reached.wait()

        let recreated = DecisionModel(store: store, itemID: "item-system_health")
        await recreated.validate()
        #expect(recreated.pendingCommand != nil)
        #expect(!recreated.actionsEnabled)
        // The first attempt is still in flight, not lost: no retry
        // affordance may invite a concurrent resend.
        #expect(!recreated.canRetryLostResponse)

        // A second submit from the recreated card is refused outright.
        await recreated.submit(.acknowledge)
        #expect(recreated.phase == .idle)
        #expect(recreated.appliedRecord == nil)

        await release.open()
        await submission.value
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.pendingCommand == nil)
        // Exactly one command committed; the record-only item never moved.
        let client = APIClientFactory.mock(server: server)
        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-system_health")).ok.body.json
        #expect(after.item.item_version == model.snapshot?.item.item_version)
        #expect(after.item.status == .open)
    }

    @Test func displacedReplayCompletionNeverOverwritesANewerSubmission() async {
        // The held automatic settle for acknowledge completes only after
        // the user's Retry already recovered it and a newer
        // stop_unattended submission (itself lost) owns the slot. The
        // stale completion must not write appliedRecord/phase, or the
        // newer command's retry would be stranded.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()

        let reached = AsyncGate()
        let release = AsyncGate()
        let script = ScriptedResponses([
            .fail,  // acknowledge submit: lost, uncommitted
            .hold(reached: reached, release: release),  // automatic settle: held
            .pass,  // user Retry: recovers acknowledge
            .fail,  // stop_unattended submit: lost
            .fail,  // its automatic settle: lost again
        ])
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { try await script.next() }
        }

        let acknowledge = Task { await model.submit(.acknowledge) }
        await reached.wait()
        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.pendingCommand == nil)

        await model.submit(.stop_unattended)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand?.payload.action == .stop_unattended)

        await release.open()
        await acknowledge.value
        // The stale acknowledge completion wrote nothing: the newer
        // command still owns the slot and stays recoverable.
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand?.payload.action == .stop_unattended)
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .stop_unattended)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .resolved)
    }

    @Test func olderRecordInAnotherInstanceNeverHidesThePendingRetry() async {
        // Instance A applied a record-only acknowledge earlier; instance
        // B's later stop_unattended is lost and owns the pending slot.
        // A's stale local record must not suppress the retry affordance
        // for B's command, or closing B's view strands it.
        let server = MockServer()
        let store = await makeStore(server: server)
        let first = DecisionModel(store: store, itemID: "item-system_health")
        await first.validate()
        await first.submit(.acknowledge)
        #expect(first.appliedRecord?.action == .acknowledge)

        let second = DecisionModel(store: store, itemID: "item-system_health")
        await second.validate()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        await second.submit(.stop_unattended)
        #expect(second.pendingCommand?.payload.action == .stop_unattended)

        // The first instance still shows its old record, but the pending
        // command belongs to a different decision: Retry stays offered.
        #expect(first.appliedRecord?.action == .acknowledge)
        #expect(!first.actionsEnabled)
        #expect(first.canRetryLostResponse)

        await server.setBeforeRespond(nil)
        await first.retryLostResponse()
        #expect(first.appliedRecord?.action == .stop_unattended)
        #expect(first.pendingCommand == nil)
        #expect(first.snapshot?.item.status == .resolved)
    }

    @Test func pendingCommandSurvivesModelRecreation() async {
        // The ledger is store-owned: navigating away recreates the model,
        // but the ambiguous command still blocks new submissions and
        // stays recoverable from the fresh card.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        await model.submit(.approve)
        #expect(model.pendingCommand != nil)
        let minted = model.pendingCommand?.command_id

        let recreated = DecisionModel(store: store, itemID: "item-spec_approval")
        await recreated.validate()
        #expect(!recreated.actionsEnabled)
        #expect(recreated.canRetryLostResponse)

        await server.setBeforeRespond(nil)
        await recreated.retryLostResponse()
        #expect(recreated.appliedRecord?.command_id == minted)
        #expect(recreated.phase == .applied)
        #expect(recreated.pendingCommand == nil)
    }

    @Test func lostResponseAfterATerminalCommitRecoversTheRecordedResult() async {
        // approve committed and resolved the item, but the response was
        // lost. The refetch shows the item closed, so the model resends
        // the preserved command: against a closed item that can only be
        // a replay, recovering the recorded CommandResult instead of
        // stranding it behind the disabled card (sync test 4).
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        let lostResponse = InjectedFailures(times: 1)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponse.consume() }
        }
        await model.submit(.approve)

        #expect(model.appliedRecord?.action == .approve)
        #expect(model.submissionError == nil)
        #expect(model.phase == .applied)
        #expect(model.snapshot?.item.status == .resolved)
        #expect(!model.actionsEnabled)
    }

    @Test func priorAppliedRecordDoesNotMaskALaterLostResponseRetry() async {
        // acknowledge applied and displayed; a later terminal
        // stop_unattended commits but both its response and the automatic
        // replay are lost. The earlier record must not hide the retry
        // affordance for the newer command's recorded result.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()
        await model.submit(.acknowledge)
        #expect(model.appliedRecord?.action == .acknowledge)

        let lostResponses = InjectedFailures(times: 2)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponses.consume() }
        }
        await model.submit(.stop_unattended)
        #expect(model.appliedRecord == nil)
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .stop_unattended)
        #expect(model.phase == .applied)
    }

    @Test func failedReplayLeavesARetryAffordanceThatRecoversTheResult() async {
        // The response is lost twice: after the terminal commit and again
        // on the automatic replay. The card must keep an explicit retry
        // for the preserved command (its actions are correctly disabled
        // by the closed status) instead of stranding the recorded result.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        let lostResponses = InjectedFailures(times: 2)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponses.consume() }
        }
        await model.submit(.approve)
        #expect(model.appliedRecord == nil)
        #expect(!model.actionsEnabled)
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .approve)
        #expect(model.submissionError == nil)
        #expect(model.phase == .applied)
        #expect(!model.canRetryLostResponse)
    }

    @Test func laterRevalidationUnstrandsARecordOnlyDecisionWithFailedRefetch() async {
        // acknowledge returns 200 but the immediate post-commit refetch
        // fails, leaving an applied phase over unknown state. A later
        // successful revalidation shows the item still open, so the
        // phase converges back to idle and the item stays decidable.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-system_health")
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.submit(.acknowledge)
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.phase == .applied)
        #expect(!model.actionsEnabled)

        await model.validate()
        #expect(model.validation == .validated)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.phase == .idle)
        #expect(model.actionsEnabled)
    }

    @Test func lostResponseWithFailedRevalidationKeepsTheRetryAffordance() async {
        // The submit response is lost after a terminal commit, and the
        // post-failure refetch fails too: current state is unknown, the
        // normal actions are disabled, but the preserved command must
        // stay resendable; recreating the model on navigation would drop
        // it and strand the recorded result.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        let lostResponses = InjectedFailures(times: 2)
        let failedRefetch = InjectedFailures(times: 1)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponses.consume() }
        }
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.submit(.approve)
        guard case .failed = model.validation else {
            Issue.record("expected failed validation, got \(model.validation)")
            return
        }
        #expect(model.appliedRecord == nil)
        #expect(!model.actionsEnabled)
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.action == .approve)
        #expect(model.phase == .applied)
        #expect(model.validation == .validated)
        #expect(model.snapshot?.item.status == .resolved)
    }

    // MARK: - Acceptance 4: consequential actions gated on validation

    @Test func actionsStayDisabledWhileValidationIsPending() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { await release.wait() }
        }

        // Never validated: disabled even though the item is open.
        #expect(!model.actionsEnabled)
        let validation = Task { await model.validate() }
        #expect(model.validation == .pending)
        #expect(!model.actionsEnabled)

        await release.open()
        await validation.value
        #expect(model.validation == .validated)
        #expect(model.actionsEnabled)
    }

    @Test func actionsStayDisabledWhenValidationFails() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { throw InjectedFailure() }
        }

        await model.validate()
        guard case .failed = model.validation else {
            Issue.record("expected .failed, got \(model.validation)")
            return
        }
        #expect(!model.actionsEnabled)
        // A submit against unvalidated state is refused outright.
        await model.submit(.approve)
        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
    }

    @Test func staleValidationFailureNeverClobbersANewerSuccess() async {
        // An older validate() that fails late must not overwrite the
        // outcome of a newer one that already succeeded; only the newest
        // call writes the result.
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        let firstCall = OneShot()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem", await firstCall.fire() {
                await reached.open()
                await release.wait()
                throw InjectedFailure()
            }
        }
        let first = Task { await model.validate() }
        await reached.wait()

        await model.validate()
        #expect(model.validation == .validated)

        await release.open()
        await first.value
        #expect(model.validation == .validated)
        #expect(model.actionsEnabled)
    }

    @Test func validationSwapsInTheCanonicalStateItFetched() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        // The item advanced after the inbox was listed.
        await server.advance(itemID: "item-spec_approval")
        await model.validate()

        let canonical = await server.snapshot(itemID: "item-spec_approval")
        #expect(model.snapshot == canonical)
        #expect(model.actionsEnabled)
    }

    @Test func resolvedItemExposesNoStaleAction() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(model.phase == .applied)

        // A late deep-link renders the same item again: canonical state
        // is resolved, so no action is enabled (plan §5.14 sync test 9).
        let late = DecisionModel(store: store, itemID: "item-spec_approval")
        await late.validate()
        #expect(late.validation == .validated)
        #expect(late.snapshot?.item.status == .resolved)
        #expect(!late.actionsEnabled)
    }

    // MARK: - Acceptance 5: validation is epoch-scoped (#162)

    @Test func validationRefusesASnapshotAStaleCacheShadows() async {
        // A daemon restore resets the authoritative entity_version below a
        // dead pre-restore cache entry (revisions and versions never
        // compare across epochs). validate() fetches the reset snapshot,
        // apply refuses it because the higher cached version shadows it,
        // and the card must not certify — or enable an action against —
        // a snapshot it never rendered (#162; plan §5.14).
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        guard var stale = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        stale.entity_version = 50
        #expect(store.apply(stale))
        #expect(store.snapshotsByID["item-spec_approval"]?.entity_version == 50)

        // The mock daemon is authoritative at entity_version 1 (fixture
        // default): validate races ahead of any heartbeat eviction.
        await model.validate()

        // apply still refuses the reset, so the stale row stays rendered —
        // but it is not certified, and no action is offered.
        #expect(store.snapshotsByID["item-spec_approval"]?.entity_version == 50)
        #expect(model.validation != .validated)
        #expect(!model.actionsEnabled)
    }

    @Test func retryAfterRestoreDoesNotCertifyAShadowedReplacement() async {
        // The pending-command ledger survives an epoch eviction, so a
        // preserved retry can fire before any heartbeat. Post-restore the
        // resend draws a 409 whose replacement is the reset low version;
        // apply refuses it under the dead pre-restore cache entry, and the
        // retry must fail closed rather than certify the shadowed
        // replacement as superseded (#162).
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        // A dead pre-restore snapshot at entity_version 50.
        guard var stale = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        stale.entity_version = 50
        store.apply(stale)

        // A preserved unresolved command whose expected version (50) no
        // longer matches the restored daemon's current (1) → 409.
        var command = makeCommand(itemID: "item-spec_approval", commandID: "cmd-restore")
        command.expected_entity_version = 50
        store.restorePendingCommands([
            "item-spec_approval": .init(command: command, state: .unresolved)
        ])
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()

        // Failed closed: not certified, no action, stale row still shown,
        // and the 409 released the slot (the command never committed).
        #expect(model.validation != .validated)
        #expect(!model.actionsEnabled)
        #expect(store.snapshotsByID["item-spec_approval"]?.entity_version == 50)
        #expect(store.pendingCommandsByItemID["item-spec_approval"] == nil)
        #expect(model.phase == .idle)
    }

    @Test func aSameEpochOutOfOrderReadRevalidatesInsteadOfFailing() async {
        // Within an epoch the daemon is monotonic, so a validate() fetch
        // apply refuses is a stale out-of-order read, not a restore: the
        // daemon's next response supersedes it. validate re-fetches and
        // certifies the current version rather than failing closed the way
        // a genuine restore does (#162).
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        // A newer canonical read (entity_version 2) is already rendered
        // while the daemon's first validate response is still version 1.
        guard var ahead = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        ahead.entity_version = 2
        store.apply(ahead)

        // The daemon catches up only on the re-fetch: the first
        // getAttentionItem answers the stale version, the second the
        // current one.
        let firstFetch = OneShot()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem", !(await firstFetch.fire()) {
                await server.advance(itemID: "item-spec_approval")
                await server.advance(itemID: "item-spec_approval")
            }
        }

        await model.validate()

        #expect(model.validation == .validated)
        #expect(model.actionsEnabled)
        #expect(model.snapshot?.entity_version == 3)
    }
}
