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

    @Test func viewPRUsesTheFixtureReferenceAndRecordsTheNavigation() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var openedURLs: [URL] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-ready_for_final_review",
            openURL: {
                openedURLs.append($0)
                return true
            })
        await model.validate()

        await model.submit(.open_pr)

        #expect(openedURLs == [URL(string: "https://github.com/owner/repo/pull/123")!])
        #expect(model.appliedRecord?.action == .open_pr)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.actionsEnabled)
    }

    @Test func degradedReadySummarySurvivesMockSyncAndDrivesDisplay() async {
        let degraded = AttentionFixtures.degradedReady()
        let store = await makeStore(server: MockServer(items: [degraded]))
        let model = DecisionModel(store: store, itemID: degraded.item.id)

        await model.validate()

        let item = try? #require(model.snapshot?.item)
        #expect(item?.readiness?.value1._class == .ready_degraded)
        #expect(item?.yield_history?.value1.rounds.count == 3)
        #expect(item.map(AttentionDisplay.title) == "Ready for final review (degraded)")
    }

    @Test func rejectedPRNavigationDoesNotRecordOperatorEngagement() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(
            store: store,
            itemID: "item-ready_for_final_review",
            openURL: { _ in false })
        await model.validate()

        await model.submit(.open_pr)

        #expect(model.submissionError == "the pull request could not be opened")
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
    }

    @Test func delayedRejectedPRNavigationDoesNotRecordOperatorEngagement() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let reached = AsyncGate()
        let release = AsyncGate()
        var openAttempts = 0
        let model = DecisionModel(
            store: store,
            itemID: "item-ready_for_final_review",
            openURL: { _ in
                openAttempts += 1
                await reached.open()
                await release.wait()
                return false
            })
        await model.validate()

        let submission = Task { await model.submit(.open_pr) }
        await reached.wait()
        #expect(!model.actionsEnabled)
        await model.submit(.open_pr)
        #expect(openAttempts == 1)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(store.isNavigationReserved(itemID: "item-ready_for_final_review"))

        await release.open()
        await submission.value
        #expect(model.submissionError == "the pull request could not be opened")
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(!store.isNavigationReserved(itemID: "item-ready_for_final_review"))
    }

    @Test func concurrentModelsOpenAReadyPullRequestOnlyOnce() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let reached = AsyncGate()
        let release = AsyncGate()
        var openedURLs: [URL] = []
        let openURL: (URL) async -> Bool = {
            openedURLs.append($0)
            await reached.open()
            await release.wait()
            return true
        }
        let first = DecisionModel(
            store: store,
            itemID: "item-ready_for_final_review",
            openURL: openURL)
        let second = DecisionModel(
            store: store,
            itemID: "item-ready_for_final_review",
            openURL: openURL)
        await first.validate()
        await second.validate()

        let firstSubmission = Task { await first.submit(.open_pr) }
        await reached.wait()
        #expect(first.pendingCommand == nil)
        #expect(store.isNavigationReserved(itemID: "item-ready_for_final_review"))
        await second.submit(.open_pr)

        #expect(openedURLs == [URL(string: "https://github.com/owner/repo/pull/123")!])
        #expect(second.appliedRecord == nil)

        await release.open()
        await firstSubmission.value
        #expect(first.appliedRecord?.action == .open_pr)
        #expect(!store.isNavigationReserved(itemID: "item-ready_for_final_review"))
    }

    @Test func pullRequestURLRejectsPathTraversalCoordinates() {
        #expect(
            DecisionModel.pullRequestURL(
                for: .init(repo: "owner/../repo", number: 123)) == nil)
        #expect(
            DecisionModel.pullRequestURL(
                for: .init(repo: "owner/repo", number: 123))
                == URL(string: "https://github.com/owner/repo/pull/123"))
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

    @Test func confirmedActionCannotAcquireReplacementBindings() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-agent_question")
        await model.validate()
        let reviewed = try #require(model.snapshot)

        await server.advance(itemID: reviewed.item.id)
        let replacement = try #require(await server.snapshot(itemID: reviewed.item.id))
        #expect(store.apply(replacement))

        await model.submitConfirmed(.stop, reviewedSnapshot: reviewed)

        #expect(await server.snapshot(itemID: reviewed.item.id) == replacement)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
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
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-agent_question",
            onConclusion: { conclusions.append($0) })
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
        #expect(conclusions.count == 1)
        #expect(conclusions.first?.resultingStatus == .resolved)
    }

    @Test func recoveredSnoozeAdvancesTheObservedRevisionBeforeConclusion() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        var observedRevisions: [Int64] = []
        store.revisionObserver = { observedRevisions.append($0) }
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()
        observedRevisions.removeAll()

        let lostResponses = InjectedFailures(times: 2)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lostResponses.consume() }
        }
        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))
        #expect(model.canRetryLostResponse)
        #expect(observedRevisions.isEmpty)
        let snoozedRevision = try #require(
            await server.snapshot(itemID: "item-run_proposal")?.as_of_revision)

        await model.retryLostResponse()

        #expect(observedRevisions == [snoozedRevision])
        #expect(conclusions.count == 1)
        #expect(model.snapshot == nil)
    }

    @Test func snoozeReplayThatFirstCommitsUsesAPostReplayCanonicalRead() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        let failedFirstSubmit = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { try await failedFirstSubmit.consume() }
        }
        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))

        #expect(model.pendingCommand == nil)
        #expect(!model.canRetryLostResponse)
        #expect(model.snapshot == nil)
        #expect(conclusions.count == 1)
        #expect(conclusions.first?.actionLabel == "Snooze")
    }

    @Test func commandResultMustMatchTheSubmittedCommandBeforeItIsTrusted() {
        let command = makeCommand(itemID: "item-spec_approval")
        let valid = Components.Schemas.CommandResult(
            record: .init(
                command_id: command.command_id,
                device_id: command.device_id,
                item_id: command.payload.item_id,
                item_version: command.payload.item_version,
                pr_head_sha: command.payload.pr_head_sha,
                artifact_digests: [],
                action: command.payload.action,
                message: "",
                attachments: []
            ),
            revision: 2
        )
        #expect(DecisionModel.commandResultIsValid(valid, for: command))

        var invalid = valid
        invalid.revision = 0
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))

        invalid = valid
        invalid.record.command_id = "other-command"
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.device_id = "other-device"
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.item_id = "other-item"
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.item_version += 1
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.pr_head_sha = "other-head"
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.artifact_digests = ["sha256:other"]
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.action = .decline
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.message = "unexpected"
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
        invalid = valid
        invalid.record.attachments = ["sha256:other"]
        #expect(!DecisionModel.commandResultIsValid(invalid, for: command))
    }

    @Test func mismatchedCommandResultKeepsTheDurableRetrySlot() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var observedRevisions: [Int64] = []
        store.revisionObserver = { observedRevisions.append($0) }
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-spec_approval",
            onConclusion: { conclusions.append($0) })
        await model.validate()
        observedRevisions.removeAll()

        await server.setCommandResultTransform { result in
            var mismatched = result
            mismatched.record.item_id = "other-item"
            mismatched.revision = .max
            return mismatched
        }
        await model.submit(.approve)

        #expect(model.appliedRecord == nil)
        #expect(model.canRetryLostResponse)
        #expect(model.pendingCommand != nil)
        #expect(!observedRevisions.contains(.max))
        #expect(conclusions.isEmpty)

        await server.setCommandResultTransform(nil)
        await model.retryLostResponse()

        #expect(model.appliedRecord?.action == .approve)
        #expect(model.pendingCommand == nil)
        #expect(conclusions.count == 1)
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
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-spec_approval",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        await model.submit(.approve)
        #expect(model.phase == .applied)
        #expect(model.appliedRecord?.action == .approve)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .resolved)
        #expect(conclusions.count == 1)
    }

    @Test func discussAppendsTheMessageAndKeepsTheItemOpen() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.offeredActions.contains(.discuss))
        #expect(model.isSubmittable(.discuss))

        let claimed = await model.submitDiscuss(
            message: "Please preserve the rollback order.")

        #expect(claimed)
        #expect(model.phase == .idle)
        #expect(model.appliedRecord?.action == .discuss)
        #expect(model.appliedRecord?.message == "Please preserve the rollback order.")
        #expect(model.submissionError == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.snapshot?.item.item_version == 2)
        #expect(model.conversation?.conversation.status == .awaiting_agent)
        #expect(model.conversation?.conversation.messages.last?.author == .user)
        #expect(!model.isSubmittable(.discuss))
    }

    @Test func mismatchedConversationResponseFailsValidationClosed() async {
        let server = MockServer()
        await server.setConversationTransform { snapshot in
            var mismatched = snapshot
            mismatched.conversation.id = "conv-other"
            return mismatched
        }
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        await model.validate()

        guard case .failed(let message) = model.validation else {
            Issue.record("mismatched conversation identity did not fail validation")
            return
        }
        #expect(message.contains("does not match requested id"))
        #expect(!model.actionsEnabled)
        #expect(store.conversationsByID["conv-other"] == nil)
    }

    @Test func futureConversationResponseCannotAdvanceTheObservedFrontier() async throws {
        let server = MockServer()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server),
            cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let original = try #require(coordinator.cursors)
        await server.setConversationTransform { snapshot in
            var future = snapshot
            future.as_of_revision = original.highestObservedServerRevision + 1
            return future
        }
        let model = DecisionModel(
            store: coordinator.store, itemID: "item-spec_approval")

        await model.validate()

        guard case .failed(let message) = model.validation else {
            Issue.record("future conversation revision did not fail validation closed")
            return
        }
        #expect(message.contains("exceeds frontier"))
        #expect(coordinator.cursors == original)
        let cached = try #require(
            coordinator.store.conversationsByID["conv-item-spec_approval"])
        #expect(cached.as_of_revision <= original.highestObservedServerRevision)
        #expect(!model.actionsEnabled)

        await server.setConversationTransform(nil)
        await coordinator.heartbeat()
        #expect(coordinator.cursors == original)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test(arguments: [
        "zero-revision", "zero-version", "foreign-message", "duplicate-message",
        "noncontiguous-sequence", "empty-message", "missing-timestamp", "empty-attachment",
        "duplicate-attachment",
    ])
    func malformedConversationResponseFailsValidationClosed(_ mutation: String) async {
        let server = MockServer()
        await server.setConversationTransform { snapshot in
            var malformed = snapshot
            switch mutation {
            case "zero-revision":
                malformed.as_of_revision = 0
            case "zero-version":
                malformed.entity_version = 0
            case "foreign-message":
                malformed.conversation.messages[0].conversation_id = "conv-other"
            case "duplicate-message":
                malformed.conversation.messages[1].id = malformed.conversation.messages[0].id
            case "noncontiguous-sequence":
                malformed.conversation.messages[1].sequence = 3
            case "empty-message":
                malformed.conversation.messages[0].id = ""
            case "missing-timestamp":
                malformed.conversation.messages[0].created_at = Date(
                    timeIntervalSince1970: -62_135_769_600)
            case "empty-attachment":
                malformed.conversation.messages[0].attachments = [""]
            case "duplicate-attachment":
                malformed.conversation.messages[0].attachments = ["digest", "digest"]
            default:
                Issue.record("unknown malformed-conversation mutation")
            }
            return malformed
        }
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")

        await model.validate()

        guard case .failed(let message) = model.validation else {
            Issue.record("malformed conversation did not fail validation")
            return
        }
        #expect(message.contains("invalid conversation response"))
        #expect(!model.actionsEnabled)
        #expect(store.conversationsByID["conv-item-spec_approval"] == nil)
    }

    @Test func emptyDiscussMessageSendsNothing() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        let before = await server.snapshot(itemID: "item-spec_approval")

        let claimed = await model.submitDiscuss(message: " \n ")

        #expect(!claimed)
        #expect(model.submissionError == "enter a message before sending")
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(await server.snapshot(itemID: "item-spec_approval") == before)
    }

    @Test func oversizedRequestChangesMessageSendsNothing() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        let before = await server.snapshot(itemID: "item-spec_approval")

        let claimed = await model.submitRequestChanges(
            message: String(repeating: "a", count: 8193))

        #expect(!claimed)
        #expect(model.submissionError == "requested changes must be 8 KiB or less")
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(await server.snapshot(itemID: "item-spec_approval") == before)
    }

    @Test func awaitingConversationDisablesASecondDiscuss() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        await model.submitDiscuss(message: "First message")
        let version = model.snapshot?.entity_version
        let messageCount = model.conversation?.conversation.messages.count

        let claimed = await model.submitDiscuss(message: "Second message")

        #expect(!claimed)
        #expect(model.phase == .idle)
        #expect(model.submissionError == nil)
        #expect(model.snapshot?.entity_version == version)
        #expect(model.conversation?.conversation.status == .awaiting_agent)
        #expect(model.conversation?.conversation.messages.count == messageCount)
        #expect(!model.isSubmittable(.discuss))
    }

    @Test func staleDiscussFromSecondDeviceRendersTheAwaitingConversation() async {
        let server = MockServer()
        let firstStore = await makeStore(server: server)
        let secondStore = await makeStore(server: server)
        let first = DecisionModel(store: firstStore, itemID: "item-spec_approval")
        let second = DecisionModel(store: secondStore, itemID: "item-spec_approval")
        await first.validate()
        await second.validate()
        let staleVersion = second.snapshot?.entity_version

        await first.submitDiscuss(message: "First device message")
        await second.submitDiscuss(message: "Second device message")

        #expect(second.phase == .idle)
        #expect(second.submissionError == "the agent is still replying to the last message")
        #expect(second.snapshot?.entity_version != staleVersion)
        #expect(second.conversation?.conversation.status == .awaiting_agent)
        #expect(second.conversation?.conversation.messages.last?.body == "First device message")
        #expect(!second.isSubmittable(.discuss))
    }

    @Test func staleNonDiscussionConflictRefreshesTheAwaitingConversation() async {
        let server = MockServer()
        let firstStore = await makeStore(server: server)
        let secondStore = await makeStore(server: server)
        let first = DecisionModel(store: firstStore, itemID: "item-spec_approval")
        let second = DecisionModel(store: secondStore, itemID: "item-spec_approval")
        await first.validate()
        await second.validate()

        await first.submitDiscuss(message: "First device message")
        let claimed = await second.submitRequestChanges(message: "Revise the order.")

        #expect(claimed)
        #expect(second.phase == .superseded)
        #expect(second.submissionError == nil)
        #expect(second.conversation?.conversation.status == .awaiting_agent)
        #expect(second.conversation?.conversation.messages.last?.body == "First device message")
    }

    @Test func replayConflictFetchesTheAwaitingConversation() async {
        let server = MockServer()
        let firstStore = await makeStore(server: server)
        let secondStore = await makeStore(server: server)
        let first = DecisionModel(store: firstStore, itemID: "item-spec_approval")
        let second = DecisionModel(store: secondStore, itemID: "item-spec_approval")
        await first.validate()
        await second.validate()
        let lostAttempts = InjectedFailures(times: 2)
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                try await lostAttempts.consume()
            }
        }

        await first.submitDiscuss(message: "First device message")
        #expect(first.canRetryLostResponse)
        await server.setBeforeRespond(nil)
        await second.submitDiscuss(message: "Second device message")

        await first.retryLostResponse()

        #expect(first.phase == .idle)
        #expect(first.validation == .validated)
        #expect(first.submissionError == "the agent is still replying to the last message")
        #expect(first.conversation?.conversation.status == .awaiting_agent)
        #expect(first.conversation?.conversation.messages.last?.body == "Second device message")
        #expect(!first.isSubmittable(.discuss))
    }

    @Test func nonDiscussionReplayConflictRefreshesTheAwaitingConversation() async {
        let server = MockServer()
        let firstStore = await makeStore(server: server)
        let secondStore = await makeStore(server: server)
        let first = DecisionModel(store: firstStore, itemID: "item-spec_approval")
        let second = DecisionModel(store: secondStore, itemID: "item-spec_approval")
        await first.validate()
        await second.validate()
        let lostAttempts = InjectedFailures(times: 2)
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                try await lostAttempts.consume()
            }
        }

        await first.submitRequestChanges(message: "Revise the order.")
        #expect(first.canRetryLostResponse)
        await server.setBeforeRespond(nil)
        await second.submitDiscuss(message: "Second device message")

        await first.retryLostResponse()

        #expect(first.phase == .superseded)
        #expect(first.validation == .validated)
        #expect(first.submissionError == nil)
        #expect(first.conversation?.conversation.status == .awaiting_agent)
        #expect(first.conversation?.conversation.messages.last?.body == "Second device message")
    }

    @Test func failedDiscussionRefetchClearsTheDefinitiveCommand() async {
        let server = MockServer()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server),
            cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let model = DecisionModel(
            store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        let failedConversationFetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getConversation" {
                try await failedConversationFetch.consume()
            }
        }

        await model.submitDiscuss(message: "Please retry the thread read.")

        #expect(model.pendingCommand == nil)
        #expect(!model.canRetryLostResponse)
        #expect(model.appliedRecord?.action == .discuss)
        #expect(model.phase == .idle)

        await server.setBeforeRespond(nil)
        await model.validate()

        #expect(model.validation == .validated)
        #expect(model.conversation?.conversation.messages.last?.body == "Please retry the thread read.")
    }

    @Test func validationRejectsAConversationPairedWithStaleItemBindings() async throws {
        let server = MockServer()
        let writerStore = await makeStore(server: server)
        let writer = DecisionModel(store: writerStore, itemID: "item-spec_approval")
        await writer.validate()
        await writer.submitDiscuss(message: "Complete this between reads.")

        let readerStore = await makeStore(server: server)
        let reader = DecisionModel(store: readerStore, itemID: "item-spec_approval")
        let firstConversationRead = OneShot()
        await server.setBeforeRespond { operationID in
            if operationID == "getConversation", await firstConversationRead.fire() {
                await server.completePendingAgentWork()
            }
        }

        await reader.validate()

        let canonical = try #require(await server.snapshot(itemID: "item-spec_approval"))
        #expect(reader.validation == .validated)
        #expect(reader.snapshot?.entity_version == canonical.entity_version)
        #expect(reader.snapshot?.item.item_version == canonical.item.item_version)
        #expect(reader.conversation?.conversation.status == .idle)
        #expect(reader.isSubmittable(.discuss))
    }

    @Test func requestChangesSupersedesAndFindsTheReplacement() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        await model.submitRequestChanges(message: "Keep the migration order.")

        #expect(model.phase == .applied)
        #expect(model.appliedRecord?.action == .request_changes)
        #expect(model.snapshot?.item.status == .superseded)
        #expect(model.snapshot?.item.decided_at != nil)
        #expect(model.revisedSpecification == nil)
        await server.completePendingAgentWork()
        await store.refresh()
        let replacement = try #require(model.revisedSpecification)
        #expect(replacement.item.status == .open)
        #expect(replacement.item.subject == model.snapshot?.item.subject)
    }

    @Test func undecodableReplayConflictRevalidatesInsteadOfRenderingSuperseded() async {
        let server = MockServer()
        let client = Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: CorruptStatusBodyTransport(
                base: MockServerTransport(server: server),
                operationID: "submitCommand",
                status: 409))
        let store = InboxStore(client: client)
        await store.refresh()
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        let competingStore = await makeStore(server: server)
        let competitor = DecisionModel(store: competingStore, itemID: "item-spec_approval")
        await model.validate()
        await competitor.validate()
        let lostAttempts = InjectedFailures(times: 2)
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                try await lostAttempts.consume()
            }
        }

        await model.submitDiscuss(message: "First device message")
        #expect(model.canRetryLostResponse)
        await server.setBeforeRespond(nil)
        await competitor.submitDiscuss(message: "Second device message")
        #expect(competitor.conversation?.conversation.messages.last?.body == "Second device message")
        let revalidationReads = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                await revalidationReads.increment()
            }
        }

        await model.retryLostResponse()

        #expect(await revalidationReads.count > 0)
        #expect(model.phase == .idle)
        #expect(model.validation == .validated)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.snapshot?.entity_version == competitor.snapshot?.entity_version)
        #expect(model.conversation?.entity_version == competitor.conversation?.entity_version)
        #expect(model.conversation?.conversation.messages.last?.body == "Second device message")
    }

    @Test func continueUnderPolicyConcludesResolved() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(
            store: store, itemID: "item-review_diminishing_returns")
        await model.validate()

        await model.submit(.continue_under_policy)

        #expect(model.phase == .applied)
        #expect(model.snapshot?.item.status == .resolved)
    }

    @Test func runProposalRendersAuthenticatedFactsAndSubmitsTypedRevision() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()

        #expect(model.proposalFacts?.intent == .implement_subject)
        #expect(model.proposalFacts?.expected_cost_units == 12)
        #expect(model.proposalFacts?.scope.component_count == 1)
        #expect(model.actionsEnabled)
        for action in [
            Components.Schemas.Action.start, .start_with_changes, .decline, .snooze,
        ] {
            #expect(model.isSubmittable(action))
        }
        #expect(model.isSubmittable(.start_with_changes))
        #expect(model.isSubmittable(.snooze))

        guard let facts = model.proposalFacts else { return }
        await model.submitRunProposalRevision(
            .init(
                intent: facts.intent,
                expected_cost_units: 20,
                scope: .init(
                    component_count: 2, declared_path_count: facts.scope.declared_path_count,
                    touches_control_plane: true)))

        #expect(model.appliedRecord?.action == .start_with_changes)
        #expect(model.snapshot?.item.status == .superseded)
    }

    @Test func unchangedRunProposalFactsProduceNoRevision() async throws {
        let store = await makeStore(server: MockServer())
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        let facts = try #require(model.proposalFacts)

        #expect(
            DecisionDetailView.runProposalRevision(
                from: facts,
                expectedCost: facts.expected_cost_units,
                componentCount: facts.scope.component_count,
                touchesControlPlane: facts.scope.touches_control_plane
            ) == nil)
    }

    @Test func expectedCostParsingValidatesTheAcceptedRange() {
        for input in ["", "abc", "0", "1000001", "12.5"] {
            #expect(DecisionDetailView.parseExpectedCost(input) == nil)
        }
        #expect(DecisionDetailView.parseExpectedCost("1") == 1)
        #expect(DecisionDetailView.parseExpectedCost("1000000") == 1_000_000)
        #expect(DecisionDetailView.parseExpectedCost(" 12 ") == 12)
    }

    @Test func snoozeRequiresAValueAfterTheCurrentTime() {
        let now = Date(timeIntervalSince1970: 1_786_506_245)

        #expect(!RunProposalSnoozeSheet.isValidSnooze(until: now, now: now))
        #expect(
            !RunProposalSnoozeSheet.isValidSnooze(
                until: now.addingTimeInterval(-1), now: now))
        #expect(
            RunProposalSnoozeSheet.isValidSnooze(
                until: now.addingTimeInterval(1), now: now))
    }

    @Test func parsedOriginalExpectedCostProducesNoRevision() async throws {
        let store = await makeStore(server: MockServer())
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        let facts = try #require(model.proposalFacts)
        let expectedCost = try #require(
            DecisionDetailView.parseExpectedCost(String(facts.expected_cost_units)))

        #expect(
            DecisionDetailView.runProposalRevision(
                from: facts,
                expectedCost: expectedCost,
                componentCount: facts.scope.component_count,
                touchesControlPlane: facts.scope.touches_control_plane
            ) == nil)
    }

    @Test func runProposalRevisionPreservesAuthenticatedFactsOnMixedEdit() async throws {
        let store = await makeStore(server: MockServer())
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        let facts = try #require(model.proposalFacts)

        let revision = try #require(
            DecisionDetailView.runProposalRevision(
                from: facts,
                expectedCost: facts.expected_cost_units + 1,
                componentCount: facts.scope.component_count + 1,
                touchesControlPlane: !facts.scope.touches_control_plane))

        #expect(revision.intent == facts.intent)
        #expect(revision.expected_cost_units == facts.expected_cost_units + 1)
        #expect(revision.scope.component_count == facts.scope.component_count + 1)
        #expect(revision.scope.declared_path_count == facts.scope.declared_path_count)
        #expect(revision.scope.touches_control_plane == !facts.scope.touches_control_plane)
    }

    @Test func runProposalRevisionPreservesDeclaredPathsOnPartialEdit() async throws {
        let store = await makeStore(server: MockServer())
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        let facts = try #require(model.proposalFacts)

        let revision = try #require(
            DecisionDetailView.runProposalRevision(
                from: facts,
                expectedCost: facts.expected_cost_units + 1,
                componentCount: facts.scope.component_count,
                touchesControlPlane: facts.scope.touches_control_plane))

        #expect(revision.scope.declared_path_count == facts.scope.declared_path_count)
    }

    @Test func findingAlternativeControlSubmitsTypedChoice() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-finding_adjudication")
        await model.validate()

        await model.submitFindingAlternatives([
            .init(finding_id: "review-finding-17", route: .dispute)
        ])

        #expect(model.appliedRecord?.action == .choose_alternative_route)
        #expect(model.snapshot?.item.status == .resolved)
    }

    @Test func findingAlternativeChoiceOmitsUntouchedRecommendations() {
        let binding = AttentionFixtures.fixture(type: .finding_adjudication).item
            .finding_adjudication!.value1
        var proposals = binding.proposals
        var untouched = proposals[0]
        untouched.finding_id = "review-finding-19"
        proposals.append(untouched)
        let multiFinding = Components.Schemas.FindingAdjudicationBinding(
            run_id: binding.run_id,
            round: binding.round,
            adjudication_digest: binding.adjudication_digest,
            proposals: proposals
        )

        #expect(DecisionDetailView.selectedAlternatives(multiFinding, selections: [:]).isEmpty)
        #expect(
            DecisionDetailView.selectedAlternatives(
                multiFinding,
                selections: ["review-finding-17": .dispute]
            ) == [.init(finding_id: "review-finding-17", route: .dispute)])
    }

    @Test func findingAlternativeCannotUseUntypedSubmitPath() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-finding_adjudication")
        await model.validate()

        await model.submit(.choose_alternative_route)

        #expect(model.appliedRecord == nil)
        #expect(model.snapshot?.item.status == .open)
    }

    @Test func parameterizedRunProposalActionsCannotUseTheUntypedSubmitPath() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()

        await model.submit(.start_with_changes)
        await model.submit(.snooze)

        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
    }

    @Test func unchangedRunProposalRevisionClearsTheDefinitivelyRejectedCommand() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        guard let facts = model.proposalFacts else { return }

        await model.submitRunProposalRevision(
            .init(
                intent: facts.intent, expected_cost_units: facts.expected_cost_units,
                scope: facts.scope))

        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(model.snapshot?.item.status == .open)
        #expect(model.submissionError != nil)
    }

    @Test(arguments: [Components.Schemas.Action.start, .decline])
    func runProposalTerminalControlsSubmit(action: Components.Schemas.Action) async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()

        await model.submit(action)

        #expect(model.appliedRecord?.action == action)
        #expect(model.snapshot?.item.status == (action == .start ? .resolved : .dismissed))
    }

    @Test func runProposalSnoozeControlSubmitsTypedInstant() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))

        #expect(model.appliedRecord?.action == .snooze)
        #expect(model.snapshot == nil)
        #expect(model.validation == .validated)
        #expect(model.submissionError == nil)
        #expect(!model.actionsEnabled)
        #expect(conclusions.count == 1)
        #expect(conclusions.first?.actionLabel == "Snooze")
        #expect(conclusions.first?.resultingStatus == nil)

        await model.validate()
        #expect(conclusions.count == 1)
    }

    @Test func snoozeAbsenceRequiresTheRecordedCommandToSurviveRestore() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        let submitCalls = Counter()
        await server.setAfterRespond { operationID in
            guard operationID == "submitCommand",
                await submitCalls.incrementAndGet() == 1
            else { return }
            // The first 200 was already constructed. Restore before its
            // read-your-write so both the proposal and command disappear.
            await server.restoreAttentionState(items: [], revision: 1)
        }

        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))

        #expect(await submitCalls.count == 2)
        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(conclusions.isEmpty)
        #expect(model.submissionError == "the decision was not recorded")
    }

    @Test func expiredSnoozeCannotClaimAnotherOperatorsResolution() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        let snoozeUntil = Date(timeIntervalSince1970: 1_786_506_245)
        await model.snooze(until: snoozeUntil)
        #expect(model.appliedRecord == nil)
        #expect(model.canRetryLostResponse)
        #expect(conclusions.isEmpty)

        await server.advanceTime(to: snoozeUntil.addingTimeInterval(1))
        await model.validate()
        #expect(model.snapshot?.item.status == .open)
        #expect(model.appliedRecord == nil)
        #expect(conclusions.isEmpty)

        let otherStore = await makeStore(server: server)
        let otherOperator = DecisionModel(store: otherStore, itemID: "item-run_proposal")
        await otherOperator.validate()
        await otherOperator.submit(.start)
        await model.validate()

        #expect(model.snapshot?.item.status == .resolved)
        #expect(conclusions.isEmpty)
    }

    @Test func expiredSnoozeCannotClaimAResolutionWhenTheOpenIntervalWasMissed() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        let snoozeUntil = Date(timeIntervalSince1970: 1_786_506_245)
        await model.snooze(until: snoozeUntil)
        #expect(model.appliedRecord == nil)
        #expect(model.canRetryLostResponse)
        #expect(conclusions.isEmpty)

        await server.advanceTime(to: snoozeUntil.addingTimeInterval(1))
        let otherStore = await makeStore(server: server)
        let otherOperator = DecisionModel(store: otherStore, itemID: "item-run_proposal")
        await otherOperator.validate()
        await otherOperator.submit(.start)

        // This client never validated the reopened proposal. Its first
        // authoritative read after the outage sees only the later decision.
        await model.validate()

        #expect(model.snapshot?.item.status == .resolved)
        #expect(model.appliedRecord == nil)
        #expect(conclusions.isEmpty)
    }

    @Test func failedSnoozeRefetchRetainsReplayAcrossALaterRestore() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-run_proposal",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))
        #expect(model.appliedRecord == nil)
        #expect(model.canRetryLostResponse)

        await server.restoreAttentionState(items: [], revision: 1)
        await model.validate()

        #expect(model.appliedRecord == nil)
        #expect(model.pendingCommand == nil)
        #expect(conclusions.isEmpty)
        #expect(model.submissionError == "the decision was not recorded")
    }

    @Test func canceledSnoozeReconciliationReturnsTheCommandToRetry() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))

        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { throw CancellationError() }
        }
        await model.retryLostResponse()

        #expect(model.appliedRecord == nil)
        #expect(store.pendingCommandsByItemID["item-run_proposal"]?.state == .unresolved)
        #expect(model.canRetryLostResponse)
        #expect(model.phase == .idle)
    }

    @Test func epochChurnDuringSnoozeReconciliationReturnsTheCommandToRetry() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.snooze(until: Date(timeIntervalSince1970: 1_786_506_245))

        await server.setBeforeRespond(nil)
        await server.setAfterRespond { operationID in
            if operationID == "getAttentionItem" { await store.discardSnapshots() }
        }
        await model.retryLostResponse()

        #expect(model.appliedRecord == nil)
        #expect(store.pendingCommandsByItemID["item-run_proposal"]?.state == .unresolved)
        #expect(model.canRetryLostResponse)
        #expect(model.phase == .idle)
    }

    @Test func selectedRunProposalRevalidatesWhenItsSnapshotTupleAdvances() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-run_proposal")
        await model.validate()
        let beforeID = model.revalidationID
        #expect(model.actionsEnabled)

        await server.advance(itemID: "item-run_proposal")
        guard let advanced = await server.snapshot(itemID: "item-run_proposal") else {
            Issue.record("advanced proposal snapshot disappeared")
            return
        }
        #expect(store.apply(advanced))
        #expect(model.revalidationID != beforeID)
        #expect(!model.actionsEnabled)

        await model.validate()
        #expect(model.proposalFacts?.as_of_revision == advanced.as_of_revision)
        #expect(model.proposalFacts?.entity_version == advanced.entity_version)
        #expect(model.proposalFacts?.item_version == advanced.item.item_version)
        #expect(model.actionsEnabled)
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
        var observedRevisions: [Int64] = []
        store.revisionObserver = { observedRevisions.append($0) }

        let failedRefetch = InjectedFailures(times: 1)
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" { try await failedRefetch.consume() }
        }
        await model.submit(.acknowledge)
        #expect(model.appliedRecord?.action == .acknowledge)
        #expect(model.phase == .applied)
        #expect(!model.actionsEnabled)
        #expect(observedRevisions.count == 1)

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

    @Test func canceledValidationDoesNotSurfaceAsFailure() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        let reached = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                await reached.open()
                try await Task.sleep(for: .seconds(30))
            }
        }

        let validation = Task { await model.validate() }
        await reached.wait()
        validation.cancel()
        await validation.value

        #expect(model.validation == .pending)
        #expect(!model.actionsEnabled)
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
        let fetches = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem", await fetches.incrementAndGet() == 2 {
                await server.advance(itemID: "item-spec_approval")
                await server.advance(itemID: "item-spec_approval")
            }
        }

        await model.validate()

        #expect(model.validation == .validated)
        #expect(model.actionsEnabled)
        #expect(model.snapshot?.entity_version == 3)
    }

    @Test func aVerifiedResolutionEmitsOneConclusionReceipt() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-spec_approval",
            onConclusion: { conclusions.append($0) })
        await model.validate()

        await model.submit(.approve)

        let conclusion = try #require(conclusions.first)
        #expect(conclusions.count == 1)
        #expect(conclusion.itemID == "item-spec_approval")
        #expect(conclusion.actionLabel == "Approve")
        #expect(conclusion.resultingStatus == .resolved)
        #expect(model.snapshot?.item.status == .resolved)
    }

    @Test func anUncertainSubmissionNeverEmitsAConclusion() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        var conclusions: [DecisionConclusion] = []
        let model = DecisionModel(
            store: store,
            itemID: "item-agent_question",
            onConclusion: { conclusions.append($0) })
        await model.validate()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }

        await model.submit(.stop)

        #expect(conclusions.isEmpty)
        #expect(model.phase == .idle)
        #expect(model.canRetryLostResponse)
        #expect(model.pendingCommand != nil)
    }
}
