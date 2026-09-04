import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct NavigationModelTests {
    @Test func launchSelectionStartsInsideItsRequestedStack() {
        let inputs = LaunchInputs(
            colorSchemeRaw: nil,
            selectionRaw: RunFixtures.activeRunID,
            screenRaw: "runs")

        let navigation = NavigationModel(launchInputs: inputs)

        #expect(navigation.selectedTab == .runs)
        #expect(navigation.runsPath == [RunFixtures.activeRunID])
        #expect(navigation.inboxPath.isEmpty)
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

        navigation.route(to: .run(RunFixtures.activeRunID))

        #expect(navigation.selectedTab == .runs)
        #expect(navigation.runsPath == [RunFixtures.activeRunID])
        #expect(navigation.inboxPath == ["item-spec_approval"])

        navigation.route(to: .attentionItem("item-blocked"))

        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.inboxPath == ["item-blocked"])
        #expect(navigation.runsPath == [RunFixtures.activeRunID])
    }

    @Test func repairPopsOnlyAPathWhoseDestinationDisappeared() {
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(
                colorSchemeRaw: nil,
                selectionRaw: "item-spec_approval"))
        navigation.route(to: .run(RunFixtures.activeRunID))

        navigation.inboxPath = NavigationModel.repairedPath(
            navigation.inboxPath,
            availableIDs: ["item-blocked"])
        navigation.runsPath = NavigationModel.repairedPath(
            navigation.runsPath,
            availableIDs: [RunFixtures.activeRunID])

        #expect(navigation.inboxPath.isEmpty)
        #expect(navigation.runsPath == [RunFixtures.activeRunID])
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
        navigation.route(to: .run(RunFixtures.activeRunID))
        #expect(
            navigation.advanceAfterConclusion(
                itemID: "item-spec_approval",
                expectedOperatorNavigationRevision: runExpectedRevision,
                advancesToNextItem: false,
                store: store) == .cancelled)
        #expect(navigation.selectedTab == .runs)
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
