import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// Where the lists state their counts once the scope controls carry none
/// (visual audit D01, R31): beside Inbox and Tasks in the macOS section
/// switcher, and the open count in the iPhone navigation title.
@MainActor
@Suite struct InboxPresentationTests {
    @Test func countBesideInboxIsTheOpenScopeCountUnderEveryProjectFilter() throws {
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
            // The count names open work whichever scope the list shows.
            for scope in InboxStore.Scope.allCases {
                store.scope = scope
                let openCount = try #require(InboxView.openCount(in: store))
                #expect(openCount == store.count(in: .open))
                let inbox = try #require(
                    FreesideRootView.sectionSegments(openCount: openCount, activeTaskCount: nil).first)
                #expect(inbox.label == "Inbox")
                #expect(inbox.count == openCount)
            }
            counts.append(store.count(in: .open))
        }
        // The fixture makes the filter matter: the projects split the total.
        #expect(counts[0] == counts[1] + counts[2])
        #expect(counts[1] > 0 && counts[2] > 0)
    }

    @Test func countIsAbsentUntilTheInboxHasLoaded() {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        #expect(store.loadState == .idle)
        #expect(InboxView.openCount(in: store) == nil)
        let segments = FreesideRootView.sectionSegments(
            openCount: nil, activeTaskCount: TasksListView.activeCount(in: TaskFixtures.defaultTasks(), loaded: false))
        #expect(segments.map(\.label) == ["Inbox", "Tasks"])
        #expect(segments.allSatisfy { $0.count == nil })

        store.replaceAll(with: [])
        #expect(InboxView.openCount(in: store) == 0)
    }

    @Test func phoneTitleCarriesTheOpenCount() {
        #expect(InboxView.phoneNavigationTitle(openCount: 14) == "Inbox · 14")
        #expect(InboxView.phoneNavigationTitle(openCount: 0) == "Inbox · 0")
        #expect(InboxView.phoneNavigationTitle(openCount: nil) == "Inbox")
    }

    @Test func tasksSegmentCarriesTheActiveCountAcrossProjects() throws {
        // The scope control lost its counts to the switcher (R31). The task
        // list's project filter is the list's own state, so the count beside
        // Tasks is the Active scope's under no filter.
        let fixtures = TaskFixtures.defaultTasks()
        let count = try #require(TasksListView.activeCount(in: fixtures, loaded: true))
        #expect(count == TaskListFilter().count(in: fixtures, scope: .active))
        #expect(count > 0 && count < fixtures.count)
        #expect(TasksListView.activeCount(in: [], loaded: true) == 0)

        let tasks = FreesideRootView.sectionSegments(openCount: 14, activeTaskCount: count).last
        #expect(tasks?.label == "Tasks")
        #expect(tasks?.count == count)
    }

    /// An empty Open scope answers for the project when one is filtered; a
    /// scope that holds a record says only that nothing is in it (R13).
    @Test func emptyOpenScopeNamesTheProjectOnlyUnderAFilter() {
        let open = InboxView.emptyScope(.open, projectID: nil)
        #expect(open.title == "No open items")
        #expect(open.description == "Nothing needs you.")
        let filtered = InboxView.emptyScope(.open, projectID: "freeside-docs")
        #expect(filtered.title == "No open items")
        #expect(filtered.description == "Nothing in this project needs you.")
        let resolved = InboxView.emptyScope(.resolved, projectID: "freeside-docs")
        #expect(resolved.title == "No resolved items")
        #expect(resolved.description == "Attention items in this scope will appear here.")
        let all = InboxView.emptyScope(.all, projectID: "freeside-docs")
        #expect(all.title == "No items")
        #expect(all.description == "Attention items in this scope will appear here.")
    }
}
