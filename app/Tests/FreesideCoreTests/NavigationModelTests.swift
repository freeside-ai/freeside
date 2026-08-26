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
}
