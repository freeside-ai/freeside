import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@MainActor
@Suite struct InboxStoreTests {
    @Test func scopeBarStacksFromXXXLargeThroughAccessibilitySizes() {
        #expect(!InboxView.stacksScopeBar(at: .xxLarge))
        #expect(InboxView.stacksScopeBar(at: .xxxLarge))
        #expect(InboxView.stacksScopeBar(at: .accessibility1))
        #expect(InboxView.stacksScopeBar(at: .accessibility5))

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

    @Test func rowsSortOpenBeforeConcludedThenNewestConclusionFirst() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        let open = orderingItem("open", age: 172_800, priority: .low)
        var older = orderingItem("older", age: 60, priority: .urgent)
        older.item.status = .resolved
        older.item.decided_at = orderNow.addingTimeInterval(-600)
        var newer = orderingItem("newer", age: 172_800, priority: .low)
        newer.item.status = .dismissed
        newer.item.decided_at = orderNow.addingTimeInterval(-60)
        var fallback = orderingItem("fallback", age: 300)
        fallback.item.status = .superseded
        var tied = fallback
        tied.item.id = "tied"
        tied.item.priority = .urgent
        var undated = orderingItem("undated", age: nil)
        undated.item.status = .resolved
        store.replaceAll(with: [older, fallback, undated, newer, tied, open])
        store.scope = .all

        #expect(store.rows.map(\.item.id) == ["open", "newer", "fallback", "tied", "older", "undated"])
        store.scope = .resolved
        #expect(store.rows.map(\.item.id) == ["newer", "fallback", "tied", "older", "undated"])
    }

    @Test func recentNormalDecisionLeadsOlderHighPriorityAndIsTheNextItem() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        store.replaceAll(with: [
            orderingItem("old-high", age: 172_800, priority: .high),
            orderingItem("current", age: 0),
            orderingItem("recent-normal", age: 300),
        ])

        #expect(store.rows.map(\.item.id) == ["current", "recent-normal", "old-high"])
        #expect(store.nextOpenItemID(excluding: "current") == "recent-normal")
    }

    @Test func sameBandUsesPriorityThenRecencyThenServerOrder() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        store.replaceAll(with: [
            orderingItem("low", age: 0, priority: .low),
            orderingItem("normal-older", age: 600),
            orderingItem("tie-first", age: 60),
            orderingItem("high", age: 900, priority: .high),
            orderingItem("tie-second", age: 60),
            orderingItem("urgent", age: 1800, priority: .urgent),
        ])

        #expect(
            store.rows.map(\.item.id) == [
                "urgent", "high", "tie-first", "tie-second", "normal-older", "low",
            ])
    }

    @Test func overdueLeadsAndFutureDeadlineDoesNotReorder() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        var overdue = orderingItem("overdue", age: 172_800, priority: .low)
        overdue.item.expires_when = orderNow.addingTimeInterval(-1)
        var atDeadline = orderingItem("at-deadline", age: 172_800)
        atDeadline.item.expires_when = orderNow
        var future = orderingItem("future", age: 1800)
        future.item.expires_when = orderNow.addingTimeInterval(1)
        store.replaceAll(with: [
            orderingItem("urgent", age: 0, priority: .urgent), future,
            orderingItem("newer", age: 60), overdue, atDeadline,
        ])

        #expect(store.rows.map(\.item.id) == ["at-deadline", "overdue", "urgent", "newer", "future"])
    }

    @Test func ageBandEdgesAndMissingDatesAreOrderedExplicitly() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        store.replaceAll(with: [
            orderingItem("undated-first", age: nil),
            orderingItem("day", age: 86_400),
            orderingItem("undated-second", age: nil),
            orderingItem("hour", age: 3600, priority: .urgent),
            orderingItem("under-day", age: 86_399, priority: .low),
            orderingItem("under-hour", age: 3599, priority: .low),
            orderingItem("older", age: 172_800),
        ])

        #expect(
            store.rows.map(\.item.id) == [
                "under-hour", "hour", "under-day", "day", "older", "undated-first", "undated-second",
            ])
    }

    @Test(arguments: [false, true])
    func orderClockChangesOnlyWhenExplicitlyRebuilt(refresh: Bool) async {
        var instant = orderNow
        var clockReads = 0
        let snapshots = [
            orderingItem("older-high", age: 3600, priority: .high),
            orderingItem("recent-normal", age: 3599),
        ]
        let store = InboxStore(
            client: APIClientFactory.mock(server: MockServer(items: snapshots)),
            now: {
                clockReads += 1
                return instant
            })
        store.replaceAll(with: snapshots)
        #expect(clockReads == 1)
        #expect(store.rows.map(\.item.id) == ["recent-normal", "older-high"])

        instant = orderNow.addingTimeInterval(1)
        #expect(store.rows.map(\.item.id) == ["recent-normal", "older-high"])
        #expect(store.nextOpenItemID(excluding: "other") == "recent-normal")
        #expect(clockReads == 1)
        if refresh {
            await store.refresh()
        } else {
            store.replaceAll(with: snapshots)
        }
        #expect(clockReads == 2)
        #expect(store.rows.map(\.item.id) == ["older-high", "recent-normal"])
    }

    @Test func timeBasedOrderRebuildRetainsLocalConclusionFeedback() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()), now: { orderNow })
        var snapshot = orderingItem("confirmed", age: 60)
        store.replaceAll(with: [snapshot])
        snapshot.item.status = .resolved
        snapshot.entity_version += 1
        store.apply(snapshot)

        store.rebuildTimeBasedOrder()

        #expect(store.rows.map(\.item.id) == ["confirmed"])
        #expect(store.count(in: .open) == 1)
        #expect(store.count(in: .resolved) == 0)
        #expect(store.nextOpenItemID(excluding: "other") == nil)
        store.replaceAll(with: [snapshot])
        #expect(store.rows.isEmpty)
        #expect(store.count(in: .resolved) == 1)
    }

    @Test func scopeDefaultsToOpenAndResolvedItemsRemainFindable() async throws {
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

        #expect(store.scope == .open)
        #expect(!store.rows.contains { $0.item.status != .open })

        store.scope = .resolved
        #expect(Set(store.rows.map(\.item.id)) == [resolved.item.id, dismissed.item.id])
        #expect(store.urgentCount(in: .resolved) == 1)

        store.scope = .all
        #expect(store.rows.count == snapshots.count)
    }

    @Test func locallyResolvedItemStaysOpenUntilTheNextFullRebuild() async throws {
        let store = await makeStore(server: MockServer())
        var resolved = try #require(store.snapshotsByID["item-spec_approval"])
        resolved.item.status = .resolved

        store.apply(resolved)
        #expect(store.rows.contains { $0.item.id == resolved.item.id })
        #expect(store.openSnapshots.contains { $0.item.id == resolved.item.id })
        #expect(store.openSnapshots.count == store.count(in: .open))
        #expect(
            OperationalSummary(
                openSnapshots: store.openSnapshots,
                runs: [],
                freshness: .fresh
            ).openCount == store.count(in: .open))
        store.scope = .resolved
        #expect(!store.rows.contains { $0.item.id == resolved.item.id })

        store.replaceAll(with: Array(store.snapshotsByID.values))
        store.scope = .open
        #expect(!store.rows.contains { $0.item.id == resolved.item.id })
        #expect(!store.openSnapshots.contains { $0.item.id == resolved.item.id })
        store.scope = .resolved
        #expect(store.rows.contains { $0.item.id == resolved.item.id })
    }

    @Test func countsShareTheRowsScopeProjectAndCapturedStatusPredicate() async throws {
        let store = await makeStore(server: MockServer())
        var snapshots = Array(store.snapshotsByID.values)
        for index in snapshots.indices {
            snapshots[index].item.project_id = index.isMultiple(of: 2) ? "proj-a" : "proj-b"
            if index.isMultiple(of: 4) {
                snapshots[index].item.status = .resolved
            }
        }
        store.replaceAll(with: snapshots)

        var locallyResolved = try #require(
            store.rows.first(where: { $0.item.status == .open }))
        locallyResolved.item.status = .resolved
        store.apply(locallyResolved)

        for projectID in [String?.none, "proj-a", "proj-b"] {
            store.projectID = projectID
            for scope in InboxStore.Scope.allCases {
                store.scope = scope
                #expect(store.count(in: scope) == store.rows.count)
                #expect(
                    store.urgentCount(in: scope)
                        == store.rows.count { $0.item.priority == .urgent })
            }
        }

        store.projectID = nil
        store.scope = .open
        #expect(store.rows.contains { $0.item.id == locallyResolved.item.id })
        #expect(store.count(in: .open) == store.rows.count)
    }

    @Test func projectFilterIsSortedDeduplicatedAndComposesWithScope() async throws {
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
        store.scope = .resolved
        #expect(store.rows.map(\.item.id) == [second.item.id])
        #expect(store.urgentCount(in: .resolved) == 1)
        store.scope = .all
        #expect(Set(store.rows.map(\.item.id)) == [second.item.id, third.item.id])
    }

    @Test func conclusionAdvanceStaysInsideTheSelectedProject() async throws {
        let store = await makeStore(server: MockServer())
        var current = try #require(store.snapshotsByID["item-spec_approval"])
        current.item.project_id = "proj-a"
        var sameProject = try #require(store.snapshotsByID["item-agent_question"])
        sameProject.item.project_id = "proj-a"
        var otherProject = try #require(store.snapshotsByID["item-execution_failure"])
        otherProject.item.project_id = "proj-b"
        otherProject.item.priority = .urgent
        store.replaceAll(with: [current, sameProject, otherProject])
        store.projectID = "proj-a"

        #expect(store.nextOpenItemID(excluding: current.item.id) == sameProject.item.id)
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

        InboxView.applyLaunchFilters(to: store, scope: .all, projectID: "proj-missing")

        #expect(store.scope == .all)
        #expect(store.projectID == nil)
        #expect(!store.rows.isEmpty)
    }

    @Test func launchProjectSurvivesUntilTheInitialLoadCanValidateIt() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))

        InboxView.applyLaunchFilters(to: store, scope: nil, projectID: "proj-1")
        #expect(store.projectID == "proj-1")

        store.replaceAll(with: [AttentionFixtures.fixture(type: .spec_approval)])
        #expect(store.projectID == "proj-1")
    }

    @Test func launchProjectMissingFromCacheCanAppearInTheBootstrap() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var cached = AttentionFixtures.fixture(type: .spec_approval)
        cached.item.project_id = "proj-cached"
        store.replaceAll(with: [cached])

        InboxView.applyLaunchFilters(to: store, scope: nil, projectID: "proj-bootstrap")
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

        InboxView.applyLaunchFilters(to: store, scope: nil, projectID: "proj-missing")
        store.replaceAll(with: [cached])
        store.finishLaunchProjectRepair()

        #expect(store.projectID == nil)
    }

    @Test func unknownLaunchProjectDoesNotLingerWhenTheCacheIsAlreadyFresh() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var current = AttentionFixtures.fixture(type: .spec_approval)
        store.replaceAll(with: [current])
        store.freshness = .fresh

        InboxView.applyLaunchFilters(to: store, scope: nil, projectID: "proj-later")
        #expect(store.projectID == nil)

        current.item.project_id = "proj-later"
        store.replaceAll(with: [current])
        #expect(store.projectID == nil)
    }

    @Test func validLaunchProjectSurvivesAnEpochDiscard() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        let authoritative = AttentionFixtures.fixture(type: .spec_approval)
        store.replaceAll(with: [authoritative])
        InboxView.applyLaunchFilters(to: store, scope: nil, projectID: "proj-1")

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
            payload: .init(
                item_id: "item-spec_approval",
                action: .approve,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
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
            payload: .init(
                item_id: snapshot.item.id,
                action: .open_pr,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
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
        var extra = AttentionFixtures.fixture(type: .blocked)
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
        let stale = try #require(store.snapshotsByID["item-run_proposal"])

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
        let stale = try #require(store.snapshotsByID["item-run_proposal"])

        store.replaceAll(with: [])
        store.removeSnapshot(
            itemID: stale.item.id, atLeastEntityVersion: stale.entity_version)
        #expect(!store.apply(stale))
        #expect(store.snapshotsByID[stale.item.id] == nil)
    }
}
