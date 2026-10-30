import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

@MainActor
@Suite struct InboxStoreTests {
    @Test func rowHeaderStacksFromXXXLargeThroughAccessibilitySizes() {
        #expect(!InboxRowView.stacksHeader(at: .xxLarge))
        #expect(InboxRowView.stacksHeader(at: .xxxLarge))
        #expect(InboxRowView.stacksHeader(at: .accessibility1))
        #expect(InboxRowView.stacksHeader(at: .accessibility5))
    }

    @Test func refreshReconstructsTheInboxFromTheCanonicalList() async {
        let store = await makeStore(server: MockServer())
        #expect(store.loadState == .loaded)
        #expect(store.rows.count == AttentionFixtures.phase1Types.count)
        #expect(Set(store.rows.map(\.item._type)) == Set(AttentionFixtures.phase1Types))
    }

    @Test func conversationsApplyMonotonicallyAndDiscardWithTheCache() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var current = AttentionFixtures.defaultConversations()[0]
        store.replaceAllConversations(with: [current])

        var newer = current
        newer.entity_version = 3
        newer.conversation.status = .awaiting_agent
        #expect(store.apply(newer))

        current.entity_version = 2
        #expect(!store.apply(current))
        #expect(store.conversationsByID[current.conversation.id] == newer)
        store.replaceAllConversations(with: [current])
        #expect(store.conversationsByID[current.conversation.id] == newer)
        #expect(
            store.conversation(for: AttentionFixtures.fixture(type: .spec_approval).item)
                == newer)

        store.discardSnapshots()
        #expect(store.conversationsByID.isEmpty)
    }

    @Test func refreshFailureIsSurfacedNotMasked() async {
        let server = MockServer()
        await server.setBeforeRespond { operationID in
            if operationID == "listAttentionItems" { throw InjectedFailure() }
        }
        let store = await makeStore(server: server)
        #expect(store.rows.isEmpty)
        guard case .failed = store.loadState else {
            Issue.record("expected .failed, got \(store.loadState)")
            return
        }
    }

    private let orderNow = AttentionFixtures.createdInstant.addingTimeInterval(3 * 86_400)

    private func orderingItem(
        _ id: String, age: TimeInterval?, priority: Components.Schemas.Priority = .normal
    ) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = AttentionFixtures.fixture(type: .spec_approval)
        snapshot.item.id = id
        snapshot.item.created_at = age.map { orderNow.addingTimeInterval(-$0) }
        snapshot.item.priority = priority
        return snapshot
    }

    @Test func rowsAreOpenItemsNewestFirstWithServerOrderBreakingTies() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var closed = orderingItem("closed", age: 0)
        closed.item.status = .resolved
        store.replaceAll(with: [
            orderingItem("undated-first", age: nil),
            orderingItem("old-urgent", age: 172_800, priority: .urgent),
            orderingItem("tie-first", age: 60),
            closed,
            orderingItem("tie-second", age: 60, priority: .low),
            orderingItem("newest", age: 0, priority: .low),
            orderingItem("undated-second", age: nil),
        ])

        #expect(
            store.rows.map(\.item.id) == [
                "newest", "tie-first", "tie-second", "old-urgent", "undated-first", "undated-second",
            ])
    }

    @Test func aDeadlineDoesNotReorderTheRows() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var overdue = orderingItem("overdue", age: 172_800, priority: .low)
        overdue.item.expires_when = orderNow.addingTimeInterval(-1)
        store.replaceAll(with: [overdue, orderingItem("newer", age: 60)])

        #expect(store.rows.map(\.item.id) == ["newer", "overdue"])
    }

    @Test func closedSnapshotsHoldWhatTheOpenRowsDoNot() async throws {
        let store = await makeStore(server: MockServer())
        var resolved = try #require(store.snapshotsByID["item-execution_failure"])
        resolved.item.status = .resolved
        var dismissed = try #require(store.snapshotsByID["item-agent_question"])
        dismissed.item.status = .dismissed
        let snapshots = store.snapshotsByID.values.map { snapshot in
            switch snapshot.item.id {
            case resolved.item.id: resolved
            case dismissed.item.id: dismissed
            default: snapshot
            }
        }
        store.replaceAll(with: snapshots)

        #expect(!store.rows.contains { $0.item.status != .open })
        // The seeded inbox already holds one closed item per conclusion cause.
        let seededClosed = Set(AttentionFixtures.closedInbox().map(\.item.id))
        #expect(
            Set(store.closedSnapshots.map(\.item.id))
                == seededClosed.union([resolved.item.id, dismissed.item.id]))
        #expect(store.openSnapshots.count + store.closedSnapshots.count == snapshots.count)
    }

    @Test func locallyResolvedItemStaysOpenUntilTheNextFullRebuild() async throws {
        let store = await makeStore(server: MockServer())
        var resolved = try #require(store.snapshotsByID["item-spec_approval"])
        resolved.item.status = .resolved

        store.apply(resolved)
        #expect(store.rows.contains { $0.item.id == resolved.item.id })
        #expect(store.openSnapshots.contains { $0.item.id == resolved.item.id })
        #expect(
            OperationalSummary(
                list: UnifiedList(
                    open: store.openSnapshots, closed: [], tasks: [], projectFilter: nil),
                freshness: .fresh
            ).openCount == store.openSnapshots.count)
        #expect(!store.closedSnapshots.contains { $0.item.id == resolved.item.id })

        store.replaceAll(with: Array(store.snapshotsByID.values))
        #expect(!store.rows.contains { $0.item.id == resolved.item.id })
        #expect(!store.openSnapshots.contains { $0.item.id == resolved.item.id })
        #expect(store.closedSnapshots.contains { $0.item.id == resolved.item.id })
    }

    @Test func urgentOpenCountIgnoresTheProjectFilter() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var other = orderingItem("other-project", age: 0, priority: .urgent)
        other.item.project_id = "proj-other"
        store.replaceAll(with: [other, orderingItem("normal", age: 0)])
        store.projectID = "proj-none-urgent"

        #expect(store.urgentOpenCount == 1)
    }

    @Test func projectFilterIsSortedDeduplicatedAndLeavesTheOpenProjectionAlone() async throws {
        let store = await makeStore(server: MockServer())
        var first = try #require(store.snapshotsByID["item-spec_approval"])
        first.item.project_id = "proj-b"
        var second = try #require(store.snapshotsByID["item-execution_failure"])
        second.item.project_id = "proj-a"
        second.item.status = .resolved
        var third = try #require(store.snapshotsByID["item-agent_question"])
        third.item.project_id = "proj-a"
        store.replaceAll(with: [first, second, third])

        #expect(store.projects == ["proj-a", "proj-b"])
        store.projectID = "proj-a"
        #expect(store.rows.map(\.item.id) == [third.item.id])
        #expect(Set(store.openSnapshots.map(\.item.id)) == [first.item.id, third.item.id])
    }

    @Test func aFilterOnAProjectOnlyTasksNameSurvivesRepair() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.taskProjectsProvider = { ["task-only"] }
        store.replaceAll(with: [orderingItem("item", age: 0)])
        store.selectProjectFilter("task-only")

        store.repairProjectFilter()
        #expect(store.projectID == "task-only")

        store.taskProjectsProvider = { [] }
        store.repairProjectFilter()
        #expect(store.projectID == nil)
    }

    @Test func projectFilterRepairsWhenItsProjectDisappears() async throws {
        let store = await makeStore(server: MockServer())
        let surviving = try #require(store.snapshotsByID["item-spec_approval"])
        store.projectID = surviving.item.project_id

        var replacement = surviving
        replacement.item.project_id = "proj-replacement"
        store.replaceAll(with: [replacement])

        #expect(store.projectID == nil)
        #expect(store.rows.map(\.item.id) == [replacement.item.id])
    }

    @Test func invalidLaunchProjectIsRepairedAgainstTheLoadedCache() async {
        let store = await makeStore(server: MockServer())

        store.applyLaunchProjectFilter("proj-missing")

        #expect(store.projectID == nil)
        #expect(!store.rows.isEmpty)
    }

    @Test func launchProjectSurvivesUntilTheInitialLoadCanValidateIt() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))

        store.applyLaunchProjectFilter("proj-1")
        #expect(store.projectID == "proj-1")

        store.replaceAll(with: [AttentionFixtures.fixture(type: .spec_approval)])
        #expect(store.projectID == "proj-1")
    }

    @Test func launchProjectMissingFromCacheCanAppearInTheBootstrap() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var cached = AttentionFixtures.fixture(type: .spec_approval)
        cached.item.project_id = "proj-cached"
        store.replaceAll(with: [cached])

        store.applyLaunchProjectFilter("proj-bootstrap")
        #expect(store.projectID == nil)

        var authoritative = cached
        authoritative.item.project_id = "proj-bootstrap"
        store.replaceAll(with: [authoritative])
        #expect(store.projectID == "proj-bootstrap")
    }

    @Test func unknownLaunchProjectStaysClearedAfterAuthoritativeBootstrap() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        let cached = AttentionFixtures.fixture(type: .spec_approval)
        store.replaceAll(with: [cached])

        store.applyLaunchProjectFilter("proj-missing")
        store.replaceAll(with: [cached])
        store.finishLaunchProjectRepair()

        #expect(store.projectID == nil)
    }

    @Test func unknownLaunchProjectDoesNotLingerWhenTheCacheIsAlreadyFresh() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var current = AttentionFixtures.fixture(type: .spec_approval)
        store.replaceAll(with: [current])
        store.freshness = .fresh

        store.applyLaunchProjectFilter("proj-later")
        #expect(store.projectID == nil)

        current.item.project_id = "proj-later"
        store.replaceAll(with: [current])
        #expect(store.projectID == nil)
    }

    @Test func validLaunchProjectSurvivesAnEpochDiscard() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        let authoritative = AttentionFixtures.fixture(type: .spec_approval)
        store.replaceAll(with: [authoritative])
        store.applyLaunchProjectFilter("proj-1")

        store.discardSnapshots()
        #expect(store.projectID == nil)
        store.replaceAll(with: [authoritative])

        #expect(store.projectID == "proj-1")
    }

    @Test func clearReleasesOnlyTheSettledCommand() async {
        // A late completion from an older replay must never release a
        // newer command's slot: the clear is conditional on the stored
        // command_id matching the one that settled.
        let store = await makeStore(server: MockServer())
        guard let snapshot = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        var older = Components.Schemas.ClientCommand(
            command_id: "cmd-older",
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .decision(
                .init(
                    kind: .decision,
                    item_id: "item-spec_approval",
                    action: .approve,
                    item_version: snapshot.item.item_version,
                    pr_head_sha: snapshot.item.pr_head_sha,
                    artifact_digests: snapshot.item.artifact_digests
                ))
        )
        #expect(store.registerPendingCommand(older) == .registered)
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-older")
        older.command_id = "cmd-newer"
        #expect(store.registerPendingCommand(older) == .registered)

        // The older command's late completion must not release the slot.
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-older")
        #expect(store.pendingCommandsByItemID["item-spec_approval"]?.command.command_id == "cmd-newer")
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-newer")
        #expect(store.pendingCommandsByItemID["item-spec_approval"] == nil)
    }

    @Test func navigationReservationBlocksWithoutEnteringTheReplayLedger() async throws {
        let store = await makeStore(server: MockServer())
        let snapshot = try #require(store.snapshotsByID["item-ready_for_final_review"])
        var ledgerWrites = 0
        store.pendingCommandsObserver = {
            ledgerWrites += 1
            return true
        }
        let command = Components.Schemas.ClientCommand(
            command_id: "cmd-navigation",
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .decision(
                .init(
                    kind: .decision,
                    item_id: snapshot.item.id,
                    action: .open_pr,
                    item_version: snapshot.item.item_version,
                    pr_head_sha: snapshot.item.pr_head_sha,
                    artifact_digests: snapshot.item.artifact_digests
                ))
        )

        #expect(store.reserveNavigation(itemID: snapshot.item.id))
        #expect(!store.reserveNavigation(itemID: snapshot.item.id))
        #expect(store.pendingCommandsByItemID[snapshot.item.id] == nil)
        #expect(ledgerWrites == 0)
        #expect(store.registerPendingCommand(command) == .slotOccupied)

        store.releaseNavigation(itemID: snapshot.item.id)
        #expect(ledgerWrites == 0)
        #expect(store.registerPendingCommand(command) == .registered)
        #expect(ledgerWrites == 1)
    }

    @Test func staleRefreshFailureNeverClobbersANewerSuccess() async {
        // An older refresh that fails late must not overwrite the load
        // state of a newer one that already succeeded.
        let server = MockServer()
        let store = InboxStore(client: APIClientFactory.mock(server: server))

        let firstCall = OneShot()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "listAttentionItems", await firstCall.fire() {
                await reached.open()
                await release.wait()
                throw InjectedFailure()
            }
        }
        let first = Task { await store.refresh() }
        await reached.wait()

        await store.refresh()
        #expect(store.loadState == .loaded)

        await release.open()
        await first.value
        #expect(store.loadState == .loaded)
        #expect(!store.rows.isEmpty)
    }

    @Test func refreshNeverHidesSnapshotsTheListDoesNotCarry() async {
        // An older or lagging list response must not drop an item the
        // store already knows (e.g. one applied from a conflict
        // replacement before the list caught up): rows would hide it
        // while its snapshot stayed cached.
        let store = await makeStore(server: MockServer())
        var extra = AttentionFixtures.fixture(type: .execution_failure)
        extra.item.id = "item-new"
        store.apply(extra)
        #expect(store.rows.contains { $0.item.id == "item-new" })

        // The mock's list does not carry item-new.
        await store.refresh()
        #expect(store.loadState == .loaded)
        #expect(store.rows.contains { $0.item.id == "item-new" })
    }

    @Test func applyNeverDowngradesToAnOlderSnapshot() async {
        // Two reads of one item can complete out of order; the store
        // keeps the newest entity_version, so a late older response
        // cannot re-open an item a card already saw as advanced.
        let server = MockServer()
        let store = await makeStore(server: server)
        guard let older = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        await server.advance(itemID: "item-spec_approval")
        guard let newer = await server.snapshot(itemID: "item-spec_approval") else {
            Issue.record("missing server snapshot")
            return
        }

        store.apply(newer)
        store.apply(older)
        #expect(store.snapshotsByID["item-spec_approval"] == newer)
    }

    @Test func delayedReadCannotRestoreAnUnapprovedTaskName() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        let older = AttentionFixtures.fixture(type: .spec_approval)
        var approved = older
        approved.entity_version += 1
        approved.as_of_revision += 1
        approved.item.display_names?.value1.task = .init(text: "Approved task name", source: .specification)

        #expect(store.apply(approved))
        #expect(!store.apply(older))
        #expect(
            store.snapshotsByID[older.item.id]?.item.display_names?.value1.task
                == approved.item.display_names?.value1.task)
    }

    @Test func applyUpsertsAReplacementSnapshotInPlace() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        await server.advance(itemID: "item-spec_approval")
        guard let replacement = await server.snapshot(itemID: "item-spec_approval") else {
            Issue.record("missing server snapshot")
            return
        }

        store.apply(replacement)
        #expect(store.snapshotsByID["item-spec_approval"] == replacement)
        #expect(store.rows.count == AttentionFixtures.phase1Types.count)
    }

    @Test func visibilityRemovalRejectsStaleResponsesUntilANewerRelease() async throws {
        let store = await makeStore(server: MockServer())
        let stale = try #require(store.snapshotsByID["item-task_proposal"])

        store.removeSnapshot(
            itemID: stale.item.id, atLeastEntityVersion: stale.entity_version)
        #expect(!store.apply(stale))
        store.replaceAll(with: [stale])
        #expect(store.snapshotsByID[stale.item.id] == nil)

        var released = stale
        released.entity_version += 2
        released.item.item_version += 2
        #expect(store.apply(released))
        #expect(store.snapshotsByID[stale.item.id] == released)
    }

    @Test func visibilityRemovalKeepsItsFloorAfterAnotherResponseAlreadyOmittedTheRow()
        async throws
    {
        let store = await makeStore(server: MockServer())
        let stale = try #require(store.snapshotsByID["item-task_proposal"])

        store.replaceAll(with: [])
        store.removeSnapshot(
            itemID: stale.item.id, atLeastEntityVersion: stale.entity_version)
        #expect(!store.apply(stale))
        #expect(store.snapshotsByID[stale.item.id] == nil)
    }
}
