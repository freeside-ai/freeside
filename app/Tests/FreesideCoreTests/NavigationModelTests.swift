import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct NavigationModelTests {
    @Test func launchSelectionStartsInsideItsRequestedStack() {
        let inputs = LaunchInputs(
            colorSchemeRaw: nil,
            selectionRaw: TaskFixtures.retryTaskID,
            screenRaw: "tasks")

        let navigation = NavigationModel(launchInputs: inputs)

        #expect(navigation.selectedTab == .tasks)
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == nil)
        #expect(navigation.inboxPath.isEmpty)
    }

    @Test func aRunLaunchLinkOpensItsTaskWithTheRunPushed() {
        let inputs = LaunchInputs(
            colorSchemeRaw: nil,
            selectionRaw: RunFixtures.activeRunID,
            screenRaw: "tasks")

        let navigation = NavigationModel(launchInputs: inputs)

        #expect(navigation.selectedTab == .tasks)
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID, RunFixtures.activeRunID])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == RunFixtures.activeRunID)
    }

    @Test func theTasksStackDrivesBothSelectionsAndARepairIsNotOperatorNavigation() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, screenRaw: "tasks"))
        let revision = navigation.operatorNavigationRevision

        navigation.setTasksPath([TaskFixtures.retryTaskID, RunFixtures.activeRunID])
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == RunFixtures.activeRunID)
        #expect(navigation.operatorNavigationRevision == revision + 1)

        // Popping the run clears its selection; the task stays.
        navigation.setTasksPath([TaskFixtures.retryTaskID])
        #expect(navigation.runSelection == nil)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)

        navigation.applyTasksPath([])
        #expect(navigation.taskSelection == nil)
        #expect(navigation.operatorNavigationRevision == revision + 2)
    }

    @Test func macTaskSelectionDropsTheRunAndCloseRunReturnsToTheTask() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, screenRaw: "tasks"))
        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))

        navigation.closeRun()
        #expect(navigation.runSelection == nil)
        #expect(navigation.taskSelection == TaskFixtures.retryTaskID)
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID])

        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
        navigation.selectTask(TaskFixtures.retryTaskID)
        #expect(navigation.runSelection == RunFixtures.activeRunID, "reselecting the same task keeps its run")
        navigation.selectTask(TaskFixtures.legacyTaskID)
        #expect(navigation.runSelection == nil)
        #expect(navigation.tasksPath == [TaskFixtures.legacyTaskID])
        navigation.selectTask(nil)
        #expect(navigation.tasksPath.isEmpty)
    }

    @Test func launchExpandedDetailsAlsoOpensTheSharedInspector() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval",
                detailsExpanded: true))

        #expect(navigation.inspectorPresented)
    }

    @Test func routingSelectsThenPushesWithoutDiscardingTheOtherTab() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))

        #expect(navigation.selectedTab == .tasks)
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID, RunFixtures.activeRunID])
        #expect(navigation.inboxPath == ["item-spec_approval"])

        navigation.route(to: .attentionItem("item-blocked"))

        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.inboxPath == ["item-blocked"])
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID, RunFixtures.activeRunID])
    }

    @Test func showingActiveTasksDropsARetainedSelection() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        navigation.route(
            to: .run(taskID: "task-campaign-freeside-completed", runID: RunFixtures.completedRunID))
        navigation.selectTab(.inbox)
        let revision = navigation.operatorNavigationRevision

        navigation.showActiveTasks()

        #expect(navigation.selectedTab == .tasks)
        #expect(navigation.taskSelection == nil)
        #expect(navigation.runSelection == nil)
        #expect(navigation.tasksPath.isEmpty)
        #expect(navigation.operatorNavigationRevision == revision + 1)
        #expect(navigation.inboxPath == ["item-spec_approval"])
    }

    @Test func repairPopsOnlyAPathWhoseDestinationDisappeared() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))

        navigation.inboxPath = NavigationModel.repairedPath(
            navigation.inboxPath,
            availableIDs: ["item-blocked"])
        navigation.applyTasksPath(
            NavigationModel.repairedTaskPath(
                navigation.tasksPath,
                availableTaskIDs: [TaskFixtures.retryTaskID]))

        #expect(navigation.inboxPath.isEmpty)
        #expect(navigation.tasksPath == [TaskFixtures.retryTaskID, RunFixtures.activeRunID])
    }

    @Test func conclusionAdvancesByInboxPriorityThenRendersInboxClear() async throws {
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
                store: store) == .advanced)
        #expect(navigation.attentionSelection == expectedNext)

        let only = try #require(store.snapshotsByID["item-spec_approval"])
        store.replaceAll(with: [only])
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: true,
                store: store) == .inboxClear)
        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.attentionSelection == nil)
        #expect(navigation.inboxPath.isEmpty)
    }

    @Test func conclusionReturnsToTheInboxWhenNotAdvancingToTheNextItem() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        // Default path: open items remain, but the operator has not opted into
        // advancing, so focus returns to the inbox with nothing selected.
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: false,
                store: store) == .returnedToInbox)
        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.attentionSelection == nil)
        #expect(navigation.inboxPath.isEmpty)

        let only = try #require(store.snapshotsByID["item-spec_approval"])
        store.replaceAll(with: [only])
        navigation.route(to: .attentionItem("item-spec_approval"))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: navigation.operatorNavigationRevision,
                advancesToNextItem: false,
                store: store) == .inboxClear)
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
                store: store) == .cancelled)
        #expect(navigation.attentionSelection == "item-blocked")

        let runExpectedRevision = navigation.operatorNavigationRevision
        navigation.route(to: .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: runExpectedRevision,
                advancesToNextItem: false,
                store: store) == .cancelled)
        #expect(navigation.selectedTab == .tasks)
    }

    @Test func macSelectionSynchronizesItsPathBeforeConclusionAdvance() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))

        navigation.selectAttentionItem("item-blocked")
        let expectedRevision = navigation.operatorNavigationRevision

        #expect(navigation.attentionSelection == "item-blocked")
        #expect(navigation.inboxPath == ["item-blocked"])
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-blocked",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: true,
                store: store) == .advanced)
    }

    @Test func conclusionDoesNotOverrideBackNavigationAfterItemDisappears() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.setInboxPath([])
        navigation.attentionSelection = nil

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                store: store) == .cancelled)
    }

    @Test func conclusionDoesNotOverrideFilterDrivenNavigationRepair() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.recordOperatorNavigation()
        navigation.inboxPath = []
        navigation.attentionSelection = nil

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: false,
                store: store) == .cancelled)
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
                store: store) == .cancelled)
    }

    @Test func automaticDisappearanceStillAllowsConclusionAdvance() async {
        let store = await makeStore(server: MockServer())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        let expectedRevision = navigation.operatorNavigationRevision

        navigation.inboxPath = []
        navigation.attentionSelection = nil

        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: expectedRevision,
                advancesToNextItem: true,
                store: store) == .advanced)
    }
}
