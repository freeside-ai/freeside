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

        /// The System Health line counts open findings, the pane's number:
        /// absent before the inbox loads, blind to the project filter, and
        /// unmoved by an acknowledgement, which the daemon records without
        /// closing the finding.
        @Test func thePanelsSystemHealthLineCountsOpenFindings() async throws {
            let server = findingsServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            #expect(DaemonMenuPanel.SystemHealthCount(coordinator: coordinator) == nil)

            await coordinator.bootstrap()
            let pane = SystemHealthFindings(
                open: coordinator.store.openSnapshots, closed: coordinator.store.closedSnapshots)
            #expect(pane.openCount == 2)
            #expect(DaemonMenuPanel.SystemHealthCount(coordinator: coordinator)?.open == pane.openCount)

            coordinator.store.projectID = "proj-empty"
            #expect(DaemonMenuPanel.SystemHealthCount(coordinator: coordinator)?.open == 2)

            try await acknowledge("item-system_health", through: coordinator, server: server)
            await coordinator.heartbeat()
            #expect(coordinator.store.snapshotsByID["item-system_health"]?.item.status == .open)
            #expect(DaemonMenuPanel.SystemHealthCount(coordinator: coordinator)?.open == 2)
        }

        /// The line draws only for a count above zero.
        @Test func theSystemHealthLineIsAbsentAtZeroOrBeforeLoad() {
            func panel(_ count: DaemonMenuPanel.SystemHealthCount?) -> DaemonMenuPanel {
                DaemonMenuPanel(
                    state: .unsupervised, actionError: nil, needsYou: nil, systemHealth: count,
                    actions: .init(
                        openApp: {}, showNeedsYou: {}, showSystemHealth: {}, start: {}, stop: {},
                        openApprovalSettings: {}, quit: {}))
            }
            #expect(panel(nil).openFindings == nil)
            #expect(panel(.init(open: 0)).openFindings == nil)
            #expect(panel(.init(open: 3)).openFindings == 3)
        }
    #endif

    /// The iPhone gear's badge shows the pane's open count, and nothing at
    /// zero or before the inbox loads.
    @Test func theGearBadgeIsThePanesOpenCount() async throws {
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: findingsServer()), cache: InMemoryCacheStore())
        #expect(FreesideRootView.settingsBadge(openFindings: coordinator.openFindingCount) == nil)

        await coordinator.bootstrap()
        let pane = SystemHealthFindings(
            open: coordinator.store.openSnapshots, closed: coordinator.store.closedSnapshots)
        #expect(pane.openCount == 2)
        #expect(coordinator.openFindingCount == pane.openCount)
        #expect(FreesideRootView.settingsBadge(openFindings: coordinator.openFindingCount) == "2")

        let none = SyncCoordinator(
            client: APIClientFactory.mock(
                server: MockServer(items: [AttentionFixtures.resolvedSystemHealth()])),
            cache: InMemoryCacheStore())
        await none.bootstrap()
        #expect(none.openFindingCount == 0)
        #expect(FreesideRootView.settingsBadge(openFindings: none.openFindingCount) == nil)
    }

    /// Open blocking and advisory findings, a closed one, and an item of
    /// another type, so only the two open findings count.
    private func findingsServer() -> MockServer {
        MockServer(items: [
            AttentionFixtures.blockingSystemHealth(),
            AttentionFixtures.fixture(type: .system_health),
            AttentionFixtures.resolvedSystemHealth(),
            AttentionFixtures.fixture(type: .agent_question),
        ])
    }

    private func acknowledge(
        _ itemID: String, through coordinator: SyncCoordinator, server: MockServer
    ) async throws {
        let snapshot = try #require(await server.snapshot(itemID: itemID))
        let command = Components.Schemas.ClientCommand(
            command_id: "cmd-ack",
            device_id: coordinator.store.device.deviceID,
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .decision(
                .init(
                    kind: .decision, item_id: itemID, action: .acknowledge,
                    item_version: snapshot.item.item_version,
                    pr_head_sha: snapshot.item.pr_head_sha,
                    artifact_digests: snapshot.item.artifact_digests)))
        _ = try await coordinator.store.client.submitCommand(body: .json(command)).ok.body.json
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
        #expect(UnifiedListView.filterState(projectID: nil) == "All Projects")
        #expect(UnifiedListView.filterState(projectID: "freeside") == "freeside")
        #expect(UnifiedListView.filterState(projectID: UnifiedList.noProject) == "No project")
    }

    @Test func theFilterButtonIsActiveWhenFilteredOrOpen() {
        #expect(!UnifiedListView.filterIsActive(projectID: nil, isPresented: false))
        #expect(UnifiedListView.filterIsActive(projectID: nil, isPresented: true))
        #expect(UnifiedListView.filterIsActive(projectID: "freeside", isPresented: false))
        #expect(UnifiedListView.filterIsActive(projectID: UnifiedList.noProject, isPresented: false))
    }

    @Test func aMonogramReadsTheProjectsLastWord() {
        #expect(ProjectFilterPopover.monogram("freeside") == "FR")
        #expect(ProjectFilterPopover.monogram("freeside-ai/oriole") == "OR")
        #expect(ProjectFilterPopover.monogram("free_as_bird") == "BI")
        #expect(ProjectFilterPopover.monogram("x") == "X")
    }
}
