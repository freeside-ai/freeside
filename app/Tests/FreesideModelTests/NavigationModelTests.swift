import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

@Suite @MainActor struct NavigationModelTests {
    private func list(
        _ store: InboxStore, tasks: [Components.Schemas.TaskSnapshot] = []
    ) -> UnifiedList {
        UnifiedList(
            open: store.openSnapshots, closed: store.closedSnapshots, tasks: tasks,
            projectFilter: store.projectID)
    }

    /// A store holding `items` of a held task, and that task.
    private func task(
        holding itemIDs: [String] = ["item-a"]
    ) -> (store: InboxStore, task: Components.Schemas.TaskSnapshot) {
        let task = TaskFixtures.defaultTasks()[0]
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.replaceAll(
            with: itemIDs.enumerated().map { index, id in
                var snapshot = AttentionFixtures.fixture(type: .agent_question)
                snapshot.item.id = id
                snapshot.item.created_at = Date(timeIntervalSince1970: 1000 - Double(index))
                snapshot.item.subject = .task(
                    .init(subject_type: .task, subject_id: task.task.id, task_id: task.task.id))
                return snapshot
            })
        return (store, task)
    }

    @Test func aTaskLaunchOpensTheTask() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: TaskFixtures.retryTaskID))

        #expect(navigation.path == [.task(TaskFixtures.retryTaskID)])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == nil)
        #expect(navigation.attentionSelection == nil)
    }

    @Test func theDevicesLaunchOpensSettingsOnDevicesAndTheToolbarOpensItsRows() {
        let plain = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        #expect(!plain.settingsPresented)
        #expect(!plain.settingsOpensDevices)

        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, devicesPresented: true))
        #expect(navigation.settingsPresented)
        #expect(navigation.settingsOpensDevices)

        navigation.settingsPresented = false
        navigation.openSettings()
        #expect(navigation.settingsPresented)
        #expect(!navigation.settingsOpensDevices)
    }

    @Test func aLaunchComposerIsOfferedToItsItemUntilConsumed() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil, selectionRaw: "item-spec_approval",
                composerRaw: "request_changes"))

        #expect(
            navigation.launchComposer
                == .init(itemID: "item-spec_approval", composer: .requestChanges))
        #expect(navigation.launchComposer(for: "item-spec_approval") == .requestChanges)
        #expect(navigation.launchComposer(for: "item-agent_question") == nil)

        // Selecting another item and coming back leaves the request alone;
        // only the card that opens the composer consumes it.
        navigation.route(to: .attentionItem("item-agent_question"))
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(navigation.launchComposer(for: "item-spec_approval") == .requestChanges)

        navigation.consumeLaunchComposer()
        #expect(navigation.launchComposer == nil)
        #expect(navigation.launchComposer(for: "item-spec_approval") == nil)
    }

    @Test func aLaunchWithoutAComposerRequestsNone() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-spec_approval"))

        #expect(navigation.attentionSelection == "item-spec_approval")
        #expect(navigation.launchComposer == nil)
    }

    @Test func aRunLaunchLinkOpensItsTaskWithTheRunPushed() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: RunFixtures.activeRunID))

        #expect(
            navigation.path == [
                .task(TaskFixtures.retryTaskID),
                .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID),
            ])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == RunFixtures.activeRunID)
    }

    @Test func anItemLaunchStaysRoutedAsALinkDoes() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-spec_approval"))

        #expect(navigation.path == [.attentionItem("item-spec_approval")])
        #expect(navigation.linkedItemID == "item-spec_approval")
    }

    @Test func theStackDrivesTheSelectionsAndARepairIsNotOperatorNavigation() {
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        let revision = navigation.operatorNavigationRevision
        let task = NavigationModel.Destination.task(TaskFixtures.retryTaskID)
        let run = NavigationModel.Destination.run(
            taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID)

        navigation.setPath([task, run])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == RunFixtures.activeRunID)
        #expect(navigation.operatorNavigationRevision == revision + 1)

        // Popping the run clears its selection; the task stays.
        navigation.setPath([task])
        #expect(navigation.runSelection == nil)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)

        navigation.repairSelection(in: list(InboxStore(client: APIClientFactory.mock(server: MockServer()))))
        #expect(navigation.taskSelection == nil)
        #expect(navigation.operatorNavigationRevision == revision + 2)
    }

    @Test func macSelectionDropsTheRunAndCloseRunReturnsToTheTask() {
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))

        navigation.closeRun()
        #expect(navigation.runSelection == nil)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.path == [.task(TaskFixtures.retryTaskID)])

        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
        navigation.select(.task(TaskFixtures.retryTaskID))
        #expect(navigation.runSelection == RunFixtures.activeRunID, "reselecting the same task keeps its run")
        navigation.select(.task(TaskFixtures.legacyTaskID))
        #expect(navigation.runSelection == nil)
        #expect(navigation.path == [.task(TaskFixtures.legacyTaskID)])
        navigation.select(nil)
        #expect(navigation.path.isEmpty)
    }

    @Test func oneRouteHoldsAnItemOrATaskNeverBoth() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
        #expect(navigation.attentionSelection == nil)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)

        navigation.route(to: .attentionItem("item-blocked"))
        #expect(navigation.path == [.attentionItem("item-blocked")])
        #expect(navigation.taskSelection == nil)
        #expect(navigation.runSelection == nil)
    }

    @Test func aLinkedItemStaysRoutedWhenTheListDoesNotHoldIt() {
        // A notification's link can name an item the list doesn't hold, one
        // a cold start's cache hasn't caught up to, and its card must stay up.
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.replaceAll(with: AttentionFixtures.defaultInbox().filter { $0.item.id == "item-blocked" })
        let navigation = NavigationModel(launchInputs: .standard())

        navigation.route(toLinkedItem: "item-spec_approval")
        navigation.repairSelection(in: list(store))
        #expect(navigation.path == [.attentionItem("item-spec_approval")])

        // The same item reached any other way is repaired as before.
        navigation.route(to: .attentionItem("item-blocked"))
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(navigation.linkedItemID == nil)
        navigation.repairSelection(in: list(store))
        #expect(navigation.path.isEmpty)
    }

    @Test func leavingALinkedItemEndsItsExemption() {
        let navigation = NavigationModel(launchInputs: .standard())

        navigation.route(toLinkedItem: "item-spec_approval")
        #expect(navigation.linkedItemID == "item-spec_approval")
        // Routing to the linked item again keeps it.
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(navigation.linkedItemID == "item-spec_approval")

        // Back to the list, as the stack's back button writes it.
        navigation.setPath([])
        #expect(navigation.linkedItemID == nil)

        navigation.route(toLinkedItem: "item-spec_approval")
        navigation.select(.attentionItem("item-blocked"))
        #expect(navigation.linkedItemID == nil)
    }

    @Test func aTaskThatLeavesTheListTakesItsRunWithIt() {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .run(taskID: task.task.id, runID: "run-1"))

        navigation.repairSelection(in: list(store, tasks: [task]))
        #expect(navigation.runSelection == "run-1")

        navigation.repairSelection(in: list(store, tasks: []))
        #expect(navigation.path.isEmpty)
    }

    @Test func theLastOpenItemResolvingMovesTheRouteToItsTask() throws {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .attentionItem("item-a"))
        let revision = navigation.operatorNavigationRevision
        let previouslyOpen = Set(store.openSnapshots.map(\.item.id))

        var resolved = try #require(store.snapshotsByID["item-a"])
        resolved.item.status = .resolved
        store.replaceAll(with: [resolved])

        navigation.repairSelection(in: list(store, tasks: [task]), previouslyOpen: previouslyOpen)
        #expect(navigation.path == [.task(task.task.id)])
        #expect(navigation.operatorNavigationRevision == revision)
    }

    @Test func anItemResolvingBesideASiblingMovesTheRouteToTheSibling() throws {
        let (store, task) = task(holding: ["item-a", "item-b"])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .attentionItem("item-a"))
        let previouslyOpen = Set(store.openSnapshots.map(\.item.id))
        let revision = navigation.operatorNavigationRevision

        var resolved = try #require(store.snapshotsByID["item-a"])
        resolved.item.status = .resolved
        store.replaceAll(with: [resolved, try #require(store.snapshotsByID["item-b"])])
        navigation.repairSelection(in: list(store, tasks: [task]), previouslyOpen: previouslyOpen)

        #expect(navigation.path == [.attentionItem("item-b")])
        // The repair is the list's move, so the next-item preference still
        // runs when the concluded item's delay ends.
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-a", expectedOperatorNavigationRevision: revision,
                advancesToNextItem: true, list: list(store, tasks: [task])) == .advanced)
    }

    @Test func aClosedItemOpenedOnPurposeStaysOpen() throws {
        let (store, task) = task()
        var resolved = try #require(store.snapshotsByID["item-a"])
        resolved.item.status = .resolved
        store.replaceAll(with: [resolved])
        let navigation = NavigationModel(launchInputs: .standard())

        navigation.route(to: .attentionItem("item-a"))
        navigation.repairSelection(in: list(store, tasks: [task]))

        #expect(navigation.path == [.attentionItem("item-a")])
    }

    @Test func aResolvedScopedItemStaysSelectedInFinished() throws {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var scoped = AttentionFixtures.fixture(type: .agent_question)
        scoped.item.id = "item-scoped"
        store.replaceAll(with: [scoped])
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .attentionItem("item-scoped"))
        let previouslyOpen = Set(store.openSnapshots.map(\.item.id))

        scoped.item.status = .resolved
        store.replaceAll(with: [scoped])
        let after = list(store)
        navigation.repairSelection(in: after, previouslyOpen: previouslyOpen)

        #expect(navigation.path == [.attentionItem("item-scoped")])
        #expect(after.finished.map(\.id) == [.item("item-scoped")])
    }

    @Test func conclusionAdvancesAlongNeedsYouThenRendersInboxClear() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedNext = try #require(
            store.rows.first { $0.item.id != "item-spec_approval" }?.item.id)

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: true,
                list: list(store)) == .advanced)
        #expect(navigation.attentionSelection == expectedNext)

        // The item left the list while its receipt showed, and nothing is left.
        store.replaceAll(with: [])
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: true,
                list: list(store)) == .inboxClear)
        #expect(navigation.attentionSelection == nil)
        #expect(navigation.path.isEmpty)
    }

    @Test func conclusionKeepsItsTaskSelectedWithoutThePreference() throws {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .attentionItem("item-a"))
        var resolved = try #require(store.snapshotsByID["item-a"])
        resolved.item.status = .resolved
        store.replaceAll(with: [resolved])

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-a",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: false,
                list: list(store, tasks: [task])) == .returnedToList)
        #expect(navigation.path == [.task(task.task.id)])
    }

    @Test func conclusionAfterTheListMovedTheRouteToTheTaskIsTheSameRoute() throws {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .attentionItem("item-a"))
        let revision = navigation.operatorNavigationRevision
        let previouslyOpen = Set(store.openSnapshots.map(\.item.id))
        var resolved = try #require(store.snapshotsByID["item-a"])
        resolved.item.status = .resolved
        store.replaceAll(with: [resolved])
        let after = list(store, tasks: [task])
        navigation.repairSelection(in: after, previouslyOpen: previouslyOpen)

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-a", expectedOperatorNavigationRevision: revision,
                advancesToNextItem: false, list: after) == .returnedToList)
        #expect(navigation.path == [.task(task.task.id)])
    }

    @Test func conclusionOpensTheTasksNextItemWithoutThePreference() {
        // The task's row stays selected and still needs the operator, and
        // selecting the row again can't open a card it already shows.
        let (store, task) = task(holding: ["item-a", "item-b"])
        let navigation = NavigationModel(launchInputs: .standard())
        navigation.route(to: .attentionItem("item-a"))

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-a",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: false,
                list: list(store, tasks: [task])) == .returnedToList)
        #expect(navigation.path == [.attentionItem("item-b")])
    }

    @Test func conclusionKeepsAScopedItemSelectedInFinished() throws {
        var scoped = AttentionFixtures.fixture(type: .system_health)
        scoped.item.status = .resolved
        scoped.item.decided_at = nil
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.replaceAll(with: [scoped])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .attentionItem(scoped.item.id))

        let result = navigation.advanceAfterConclusion(
            itemID: scoped.item.id,
            expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
            advancesToNextItem: true, list: list(store))

        #expect(result == .returnedToList)
        #expect(navigation.path == [.attentionItem(scoped.item.id)])
    }

    @Test func conclusionDoesNotOverrideManualNavigationDuringTheDelay() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        let expectedRevision = navigation.operatorNavigationRevision
        navigation.route(to: .attentionItem("item-blocked"))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                list: list(store)) == .cancelled)
        #expect(navigation.attentionSelection == "item-blocked")

        let runExpectedRevision = navigation.operatorNavigationRevision
        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: runExpectedRevision,
                advancesToNextItem: false,
                list: list(store)) == .cancelled)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
    }

    @Test func macSelectionSynchronizesItsPathBeforeConclusionAdvance() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        navigation.select(.attentionItem("item-blocked"))
        let expectedRevision = navigation.operatorNavigationRevision

        #expect(navigation.attentionSelection == "item-blocked")
        #expect(navigation.path == [.attentionItem("item-blocked")])
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-blocked",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: true,
                list: list(store)) == .advanced)
    }

    @Test func conclusionDoesNotOverrideBackNavigationAfterItemDisappears() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.setPath([])

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                list: list(store)) == .cancelled)
    }

    @Test func conclusionDoesNotOverrideFilterDrivenNavigationRepair() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.recordOperatorNavigation()

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                list: list(store)) == .cancelled)
    }

    @Test func conclusionDoesNotOverrideAnAwayAndBackNavigationSequence() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.route(to: .attentionItem("item-blocked"))
        navigation.route(to: .attentionItem("item-spec_approval"))

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                list: list(store)) == .cancelled)
    }

    @Test func automaticDisappearanceStillAllowsConclusionAdvance() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        // The list dropped the item, which a filter or a rebuild can.
        navigation.repairSelection(in: UnifiedList(open: [], closed: [], tasks: [], projectFilter: nil))

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: true,
                list: list(store)) == .advanced)
    }

    @Test func nextAndPreviousMoveAlongNeedsYouRows() {
        let (store, task) = task(holding: ["item-a", "item-b"])
        var scoped = AttentionFixtures.fixture(type: .agent_question)
        scoped.item.id = "item-scoped"
        scoped.item.created_at = Date(timeIntervalSince1970: 1)
        store.replaceAll(with: store.openSnapshots + [scoped])
        let rows = list(store, tasks: [task])
        let navigation = NavigationModel(launchInputs: .standard())

        navigation.moveAttentionSelection(by: 1, list: rows)
        #expect(navigation.path == [.attentionItem("item-a")])

        navigation.moveAttentionSelection(by: 1, list: rows)
        #expect(navigation.path == [.attentionItem("item-scoped")])

        navigation.moveAttentionSelection(by: 1, list: rows)
        #expect(navigation.path == [.attentionItem("item-scoped")], "the last row holds")

        navigation.moveAttentionSelection(by: -1, list: rows)
        #expect(navigation.path == [.attentionItem("item-a")])
    }

    @Test func anItemItsTaskOwnsLeadsTheTasksPage() {
        let (store, task) = task(holding: ["item-a", "item-b"])
        let rows = list(store, tasks: [task])

        // A row, a link, and a popup all route to the item; the page reads
        // the same route and leads with that card.
        #expect(
            NavigationModel.surface(for: .attentionItem("item-b"), in: rows)
                == .taskPage(taskID: task.task.id, leadingItemID: "item-b"))
        // Opening the task leads with its newest open item.
        #expect(
            NavigationModel.surface(for: .task(task.task.id), in: rows)
                == .taskPage(taskID: task.task.id, leadingItemID: "item-a"))
    }

    @Test func aResolvedItemItsTaskOwnsLeadsThePageInItsCurrentState() {
        let (store, task) = task()
        var resolved = AttentionFixtures.closedByDaemon(
            AttentionFixtures.fixture(type: .agent_question), id: "item-resolved", cause: .expired, after: 60,
            reason: "Expired.")
        resolved.item.subject = .task(
            .init(subject_type: .task, subject_id: task.task.id, task_id: task.task.id))
        store.replaceAll(with: store.orderedSnapshots + [resolved])

        #expect(
            NavigationModel.surface(for: .attentionItem("item-resolved"), in: list(store, tasks: [task]))
                == .taskPage(taskID: task.task.id, leadingItemID: "item-resolved"))
    }

    @Test func anItemNoHeldTaskOwnsIsItsCardAlone() {
        let (store, _) = task()
        // The store's item names a task this list doesn't hold, as a scoped
        // item and a link ahead of the list both do.
        #expect(
            NavigationModel.surface(for: .attentionItem("item-a"), in: list(store))
                == .card(itemID: "item-a"))
    }

    @Test func aQuietTasksPageHasNoLeadingCard() {
        let (store, task) = task(holding: [])
        #expect(
            NavigationModel.surface(for: .task(task.task.id), in: list(store, tasks: [task]))
                == .taskPage(taskID: task.task.id, leadingItemID: nil))
    }

    @Test func theLastOpenItemResolvingKeepsThePageAndDropsTheCard() {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .attentionItem("item-a"))

        store.replaceAll(with: [
            AttentionFixtures.closedByDaemon(
                store.orderedSnapshots[0], id: "item-a", cause: .expired, after: 60, reason: "Expired.")
        ])
        let rows = list(store, tasks: [task])
        navigation.repairSelection(in: rows, previouslyOpen: ["item-a"])

        #expect(
            navigation.detailSurface(in: rows)
                == .taskPage(taskID: task.task.id, leadingItemID: nil))
    }

    @Test func aRunSurfacesOverItsTask() {
        let (store, task) = task(holding: [])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .run(taskID: task.task.id, runID: "run-a"))

        #expect(
            navigation.detailSurface(in: list(store, tasks: [task]))
                == .run(taskID: task.task.id, runID: "run-a"))
    }

    @Test func aRunOpenedFromThePageReturnsToItsLeadingCard() {
        let (store, task) = task(holding: ["item-a", "item-b"])
        let rows = list(store, tasks: [task])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .attentionItem("item-b"))

        navigation.openRun(taskID: task.task.id, runID: "run-a")
        #expect(navigation.detailSurface(in: rows) == .run(taskID: task.task.id, runID: "run-a"))
        #expect(navigation.holdsCard(in: rows))

        navigation.closeRun()
        #expect(
            navigation.detailSurface(in: rows)
                == .taskPage(taskID: task.task.id, leadingItemID: "item-b"))
    }

    @Test func anItemResolvingUnderARunKeepsTheRunOpen() {
        let (store, task) = task()
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .attentionItem("item-a"))
        navigation.openRun(taskID: task.task.id, runID: "run-a")

        store.replaceAll(with: [
            AttentionFixtures.closedByDaemon(
                store.orderedSnapshots[0], id: "item-a", cause: .expired, after: 60, reason: "Expired.")
        ])
        navigation.repairSelection(in: list(store, tasks: [task]), previouslyOpen: ["item-a"])

        #expect(navigation.path == [.task(task.task.id), .run(taskID: task.task.id, runID: "run-a")])
    }

    /// The card leading a task's route stays when a newer item arrives, so
    /// the sheet or composer open on it stays too.
    @Test func aNewerItemDoesNotSwapTheCardLeadingATask() {
        let (store, task) = task(holding: ["item-new", "item-lead"])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .task(task.task.id))

        navigation.repairSelection(in: list(store, tasks: [task]), previouslyOpen: ["item-lead"])

        #expect(navigation.path == [.attentionItem("item-lead")])
    }

    @Test func aQuietTaskHoldsNoCard() {
        let (store, task) = task(holding: [])
        let navigation = NavigationModel(launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil))
        navigation.route(to: .task(task.task.id))
        #expect(!navigation.holdsCard(in: list(store, tasks: [task])))
    }
}
