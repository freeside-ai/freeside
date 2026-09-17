import Observation

/// One shared routing seam for launch inputs, future notification delivery,
/// and the platform-specific navigation containers.
@MainActor
@Observable
public final class NavigationModel {
    enum ConclusionAdvanceResult: Equatable {
        case advanced
        case returnedToInbox
        case inboxClear
        case cancelled
    }

    public enum Destination: Equatable {
        case attentionItem(String)
        case task(String)
        /// A run is reached through its task: routing selects the task and
        /// pushes the run timeline over it.
        case run(taskID: String, runID: String)
    }

    public var selectedTab: LaunchInputs.Screen
    public var inboxPath: [String]
    /// The tasks stack: the selected task, then the run opened under it.
    public var tasksPath: [String]
    public var attentionSelection: String?
    public var taskSelection: String?
    public var runSelection: String?
    public var inspectorPresented: Bool
    /// The New Task composer's presentation flag, shared by the Tasks toolbar
    /// button, ⌘N, and the File > New Task menu item, so all three open the one
    /// sheet through the same gated path.
    public var newTaskComposerPresented = false
    public var submissionRecoveryPresented = false
    public private(set) var operatorNavigationRevision = 0

    public init(launchInputs: LaunchInputs) {
        selectedTab = launchInputs.screen
        inboxPath = []
        tasksPath = []
        attentionSelection = nil
        taskSelection = nil
        runSelection = nil
        inspectorPresented = launchInputs.detailsExpanded

        switch launchInputs.screen {
        case .inbox:
            if let selection = launchInputs.selection {
                route(to: .attentionItem(selection))
            }
        case .tasks:
            switch launchInputs.taskSelection {
            case .task(let taskID)?:
                route(to: .task(taskID))
            case .run(let taskID, let runID)?:
                route(to: .run(taskID: taskID, runID: runID))
            case nil:
                break
            }
        }
    }

    /// Select the destination's top-level section before replacing that
    /// section's stack with the canonical detail route.
    public func route(to destination: Destination) {
        // Routing is operator intent even when it targets the currently
        // rendered item (for example, Reveal Technical Details).
        operatorNavigationRevision += 1
        switch destination {
        case .attentionItem(let itemID):
            selectedTab = .inbox
            attentionSelection = itemID
            inboxPath = [itemID]
        case .task(let taskID):
            selectedTab = .tasks
            taskSelection = taskID
            runSelection = nil
            tasksPath = [taskID]
        case .run(let taskID, let runID):
            selectedTab = .tasks
            taskSelection = taskID
            runSelection = runID
            tasksPath = [taskID, runID]
        }
    }

    public func selectTab(_ screen: LaunchInputs.Screen) {
        guard selectedTab != screen else { return }
        operatorNavigationRevision += 1
        selectedTab = screen
    }

    /// Open the Tasks screen on its active list. A selection kept from an
    /// earlier visit is dropped first, because the list reveals its selected
    /// task's scope on appear and a finished task would open the Finished
    /// list under a link that named the active count.
    public func showActiveTasks() {
        operatorNavigationRevision += 1
        selectedTab = .tasks
        taskSelection = nil
        runSelection = nil
        tasksPath = []
    }

    /// The iOS tasks stack as the operator drives it (a push, a pop). The
    /// selections follow the path, so popping the run clears its selection.
    public func setTasksPath(_ path: [String]) {
        guard tasksPath != path else { return }
        operatorNavigationRevision += 1
        applyTasksPath(path)
    }

    /// A list repair after a filter change or a data update, not operator
    /// navigation: it must not count as such against a pending inbox
    /// conclusion advance.
    public func applyTasksPath(_ path: [String]) {
        tasksPath = path
        taskSelection = path.first
        runSelection = path.count > 1 ? path[1] : nil
    }

    /// The macOS sidebar selection. Choosing another task drops a run opened
    /// under the previous one; a repair that clears the selection drops it
    /// too, so the detail column never shows a run of a task no longer listed.
    public func selectTask(_ taskID: String?) {
        guard taskSelection != taskID else { return }
        operatorNavigationRevision += 1
        taskSelection = taskID
        runSelection = nil
        tasksPath = taskID.map { [$0] } ?? []
    }

    /// Returns from a run timeline to its task on macOS, which has no stack
    /// to pop.
    public func closeRun() {
        guard runSelection != nil else { return }
        operatorNavigationRevision += 1
        runSelection = nil
        tasksPath = taskSelection.map { [$0] } ?? []
    }

    public func setInboxPath(_ path: [String]) {
        guard inboxPath != path else { return }
        operatorNavigationRevision += 1
        inboxPath = path
        attentionSelection = path.last
    }

    public func selectAttentionItem(_ itemID: String?) {
        let path = itemID.map { [$0] } ?? []
        guard attentionSelection != itemID || inboxPath != path else { return }
        operatorNavigationRevision += 1
        attentionSelection = itemID
        inboxPath = path
    }

    func recordOperatorNavigation() {
        operatorNavigationRevision += 1
    }

    /// Leave a just-concluded item once its receipt delay expires. The default
    /// destination is the inbox: clear the selection so the list stays visible
    /// with no item open. Only when `advancesToNextItem` is set, and an open
    /// item remains, does focus move to the next one instead.
    func advanceAfterConclusion(
        itemID: String,
        expectedOperatorNavigationRevision: Int,
        advancesToNextItem: Bool,
        store: InboxStore
    ) -> ConclusionAdvanceResult {
        // Rebuilding the open scope may remove the concluded item from either
        // navigation container before the delay expires, so nil remains the
        // same route. A different concrete destination is deliberate operator
        // navigation and must win over the automatic move.
        guard operatorNavigationRevision == expectedOperatorNavigationRevision,
            selectedTab == .inbox,
            attentionSelection == nil || attentionSelection == itemID,
            inboxPath.last == nil || inboxPath.last == itemID
        else {
            return .cancelled
        }
        let nextItemID = store.nextOpenItemID(excluding: itemID)
        if advancesToNextItem, let nextItemID {
            route(to: .attentionItem(nextItemID))
            return .advanced
        }
        selectedTab = .inbox
        attentionSelection = nil
        inboxPath = []
        return nextItemID == nil ? .inboxClear : .returnedToInbox
    }

    public func moveAttentionSelection(by offset: Int, store: InboxStore) {
        let itemIDs = store.rows.map(\.item.id)
        guard !itemIDs.isEmpty else { return }
        let currentIndex = attentionSelection.flatMap(itemIDs.firstIndex(of:))
        let nextIndex: Int
        if let currentIndex {
            nextIndex = min(max(currentIndex + offset, itemIDs.startIndex), itemIDs.index(before: itemIDs.endIndex))
        } else {
            nextIndex = offset < 0 ? itemIDs.index(before: itemIDs.endIndex) : itemIDs.startIndex
        }
        route(to: .attentionItem(itemIDs[nextIndex]))
    }

    static func repairedPath(_ path: [String], availableIDs: Set<String>) -> [String] {
        guard let routedID = path.last, !availableIDs.contains(routedID) else { return path }
        return Array(path.dropLast())
    }

    /// The tasks stack is rooted at a task and may hold that task's run
    /// above it, so it stands or falls with its root: a task that left the
    /// visible rows takes the run pushed over it along.
    static func repairedTaskPath(_ path: [String], availableTaskIDs: Set<String>) -> [String] {
        guard let taskID = path.first, !availableTaskIDs.contains(taskID) else { return path }
        return []
    }
}
