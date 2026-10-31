import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// Where the list states its counts: each section's head, and the Needs you
/// count in the Mac's menu-bar panel.
@MainActor
@Suite struct InboxPresentationTests {
    private func list(_ store: InboxStore) -> UnifiedList {
        UnifiedList(
            open: store.openSnapshots, closed: store.closedSnapshots, tasks: [],
            projectFilter: store.projectID)
    }

    @Test func theNeedsYouCountFollowsTheProjectFilter() throws {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        var snapshots = AttentionFixtures.defaultInbox()
        for index in snapshots.indices {
            snapshots[index].item.project_id = index.isMultiple(of: 3) ? "proj-a" : "proj-b"
            if index.isMultiple(of: 4) {
                snapshots[index].item.status = .resolved
            }
        }
        store.replaceAll(with: snapshots)

        var counts: [Int] = []
        for projectID in [String?.none, "proj-a", "proj-b"] {
            store.projectID = projectID
            let list = list(store)
            let count = try #require(UnifiedListView.needsYouCount(in: list, store: store))
            #expect(count == list.needsYou.count)
            counts.append(count)
        }
        // The fixture makes the filter matter: the projects split the total.
        #expect(counts[0] >= counts[1] + counts[2])
        #expect(counts[1] > 0 && counts[2] > 0)
    }

    @Test func countIsAbsentUntilTheListHasLoaded() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        #expect(store.loadState == .idle)
        #expect(UnifiedListView.needsYouCount(in: list(store), store: store) == nil)

        store.replaceAll(with: [])
        #expect(UnifiedListView.needsYouCount(in: list(store), store: store) == 0)
    }

    #if os(macOS)
        /// The panel's count is the head's: rows under the list's filter, so a
        /// task holding two items counts once, and absent before the first load.
        @Test func thePanelCountsTheHeadsRowsOnceTheListHasLoaded() async throws {
            let held = TaskFixtures.defaultTasks()[0].task
            func item(_ id: String, taskID: String, project: String) -> Components.Schemas.AttentionItemSnapshot {
                var snapshot = AttentionFixtures.fixture(type: .agent_question)
                snapshot.item.id = id
                snapshot.item.project_id = project
                snapshot.item.subject = .task(
                    .init(subject_type: .task, subject_id: taskID, task_id: taskID))
                return snapshot
            }
            let items = [
                item("item-a", taskID: held.id, project: "proj-a"),
                item("item-b", taskID: held.id, project: "proj-a"),
                item("item-c", taskID: "task-not-held", project: "proj-b"),
            ]
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer(items: items)), cache: InMemoryCacheStore())
            #expect(DaemonMenuPanel.NeedsYouCount(coordinator: coordinator) == nil)

            await coordinator.bootstrap()
            #expect(coordinator.store.openSnapshots.count == 3)
            #expect(DaemonMenuPanel.NeedsYouCount(coordinator: coordinator)?.rows == 2)

            coordinator.store.projectID = "proj-b"
            #expect(DaemonMenuPanel.NeedsYouCount(coordinator: coordinator)?.rows == 1)
            coordinator.store.projectID = "proj-empty"
            #expect(DaemonMenuPanel.NeedsYouCount(coordinator: coordinator)?.rows == 0)
        }
    #endif

    /// An empty Needs you answers for the project when one is filtered (R13).
    @Test func emptyNeedsYouNamesTheProjectOnlyUnderAFilter() {
        let all = UnifiedListView.emptyNeedsYou(projectID: nil)
        #expect(all.title == "Nothing needs you.")
        #expect(all.description == "Decisions you make will show up here.")
        let filtered = UnifiedListView.emptyNeedsYou(projectID: "freeside-docs")
        #expect(filtered.title == "Nothing in this project needs you.")
    }

    @Test func theToolsRowStatesTheFilter() {
        #expect(UnifiedListView.filterState(projectID: nil) == "All projects")
        #expect(UnifiedListView.filterState(projectID: "freeside") == "freeside")
        #expect(UnifiedListView.filterState(projectID: UnifiedList.noProject) == "No project")
    }

    @Test func aMonogramReadsTheProjectsLastWord() {
        #expect(ProjectFilterPopover.monogram("freeside") == "FR")
        #expect(ProjectFilterPopover.monogram("freeside-ai/oriole") == "OR")
        #expect(ProjectFilterPopover.monogram("free_as_bird") == "BI")
        #expect(ProjectFilterPopover.monogram("x") == "X")
    }
}
