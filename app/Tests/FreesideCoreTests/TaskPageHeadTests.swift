import Foundation
import FreesideAPI
import FreesideTestSupport
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's head (#2609): the outline row's chips and the task's
/// menu.
@Suite @MainActor struct TaskPageHeadTests {
    // MARK: Outline and Task Actions

    /// Task Actions sits in the outline row at regular width and in the
    /// navigation bar on a phone, so the phone's row holds no menu. Its own
    /// item draws while the Identifiers footer does: once the history
    /// loads, or while the leading decision names identifiers.
    @Test func taskActionsSitWherePinnedAndHoldTodaysItems() {
        #expect(TaskPageView.TaskActionsSlot(compact: false) == .outlineRow)
        #expect(TaskPageView.TaskActionsSlot(compact: true) == .navigationBar)
        #expect(TaskPageView.taskActions(nil, taskID: "task-1").isEmpty)
        #expect(
            TaskPageView.taskActions(nil, taskID: "task-1", decision: [.init(label: "Item", value: "item-1")])
                .map(\.title) == ["Copy all identifiers"])
        let history = TaskStageFixtures.page(.review).history
        #expect(TaskPageView.taskActions(history, taskID: "task-1").map(\.title) == ["Copy all identifiers"])
    }

}
