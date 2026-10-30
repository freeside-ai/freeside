import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// Where the list states its counts: each section's head, and the Needs you
/// count in the iPhone navigation title.
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

    @Test func phoneTitleCarriesTheNeedsYouCount() {
        #expect(UnifiedListView.phoneNavigationTitle(needsYouCount: 14) == "Inbox · 14")
        #expect(UnifiedListView.phoneNavigationTitle(needsYouCount: 0) == "Inbox · 0")
        #expect(UnifiedListView.phoneNavigationTitle(needsYouCount: nil) == "Inbox")
    }

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
