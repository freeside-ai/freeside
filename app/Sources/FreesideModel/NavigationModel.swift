import Observation

/// One shared routing seam for launch inputs, future notification delivery,
/// and the platform-specific navigation containers.
@MainActor
@Observable
public final class NavigationModel {
    package enum ConclusionAdvanceResult: Equatable {
        case advanced
        case returnedToList
        case inboxClear
        case cancelled
    }

    /// One composer to present on one inbox item, once per launch.
    public struct LaunchComposerRequest: Equatable {
        public let itemID: String
        public let composer: LaunchInputs.Composer
    }

    public enum Destination: Hashable {
        case attentionItem(String)
        case task(String)
        /// A run is reached through its task: routing selects the task and
        /// pushes the run timeline over it.
        case run(taskID: String, runID: String)
    }

    /// What the detail column draws for a route.
    package enum DetailSurface: Equatable {
        /// A task's page, with the item whose card leads it, if any.
        case taskPage(taskID: String, leadingItemID: String?)
        /// An item with no held task: its card alone.
        case card(itemID: String)
        case run(taskID: String, runID: String)
    }

    /// The one route: an item, a task, or a task with its run over it. The
    /// selections read from it, so an item and a task can't both be open.
    public private(set) var path: [Destination] {
        didSet {
            if path.last != linkedItemID.map(Destination.attentionItem) { linkedItemID = nil }
        }
    }
    /// The item a notification link opened, while it is still the routed
    /// item. List repair keeps it routed even when the rows
    /// don't hold it: a link can name an item that has since resolved, or
    /// one that a cold start's cached list hasn't caught up to, and the card
    /// shows either from canonical state. Leaving the card ends it.
    public private(set) var linkedItemID: String?
    /// The New Task composer's presentation flag, shared by the list's tools
    /// row, ⌘N, and the File > New Task menu item, so all three open the one
    /// sheet through the same gated path.
    public var newTaskComposerPresented = false
    public var submissionRecoveryPresented = false
    /// The Devices sheet's presentation flag, shared by the toolbar buttons
    /// and the `-FreesideDevices` launch argument.
    public var devicesPresented: Bool
    /// The `-FreesideComposer` launch argument until the card acts on it.
    /// Held here, not in the card, because the Mac detail view is rebuilt on
    /// every selection: view state would reopen the composer each time the
    /// item is reselected.
    public private(set) var launchComposer: LaunchComposerRequest?
    public private(set) var operatorNavigationRevision = 0

    public init(launchInputs: LaunchInputs) {
        path = []
        devicesPresented = launchInputs.devicesPresented
        // `LaunchInputs` leaves the composer nil without an item selection.
        if case .attentionItem(let itemID)? = launchInputs.selection {
            launchComposer = launchInputs.composer.map {
                LaunchComposerRequest(itemID: itemID, composer: $0)
            }
            // A launch names its item on purpose, as a link does, so it stays
            // routed whether or not the item is open.
            route(toLinkedItem: itemID)
        } else if let selection = launchInputs.selection {
            route(to: selection)
        }
    }

    public var attentionSelection: String? {
        if case .attentionItem(let itemID)? = path.first { itemID } else { nil }
    }

    public var taskSelection: String? {
        if case .task(let taskID)? = path.first { taskID } else { nil }
    }

    public var runSelection: String? {
        if case .run(_, let runID)? = path.last { runID } else { nil }
    }

    /// The surface a destination opens. An item a held task owns leads that
    /// task's page, open or resolved, so a link and a list row land in the
    /// same place; one no held task owns, which covers a scoped item and a
    /// link the list hasn't caught up to, is its card alone. A task leads
    /// with its newest open item.
    package static func surface(for destination: Destination, in list: UnifiedList) -> DetailSurface {
        switch destination {
        case .attentionItem(let itemID):
            list.taskID(ofItem: itemID).map { .taskPage(taskID: $0, leadingItemID: itemID) }
                ?? .card(itemID: itemID)
        case .task(let taskID):
            .taskPage(taskID: taskID, leadingItemID: list.openItems(inTask: taskID).first?.item.id)
        case .run(let taskID, let runID):
            .run(taskID: taskID, runID: runID)
        }
    }

    /// The surface the macOS detail column draws for the route, which shows
    /// its last destination. Nil with nothing routed.
    package func detailSurface(in list: UnifiedList) -> DetailSurface? {
        path.last.map { Self.surface(for: $0, in: list) }
    }

    /// The launch's composer for this item's card, or nil for any other item
    /// and once the request is consumed.
    public func launchComposer(for itemID: String) -> LaunchInputs.Composer? {
        launchComposer?.itemID == itemID ? launchComposer?.composer : nil
    }

    public func consumeLaunchComposer() {
        launchComposer = nil
    }

    /// Replace the stack with the destination's canonical route.
    public func route(to destination: Destination) {
        // Routing is operator intent even when it targets the currently
        // rendered item (for example, Reveal Technical Details).
        operatorNavigationRevision += 1
        path = Self.route(to: destination)
    }

    private static func route(to destination: Destination) -> [Destination] {
        if case .run(let taskID, _) = destination {
            return [.task(taskID), destination]
        }
        return [destination]
    }

    /// Open a run from the task page: over the route that page came from, so
    /// returning lands on the same page with the same card leading.
    public func openRun(taskID: String, runID: String) {
        setPath(Array(path.prefix(1)) + [.run(taskID: taskID, runID: runID)])
    }

    /// Route to the item a notification link named; see `linkedItemID`.
    public func route(toLinkedItem itemID: String) {
        route(to: .attentionItem(itemID))
        linkedItemID = itemID
    }

    /// The iOS stack as the operator drives it (a push, a pop).
    public func setPath(_ path: [Destination]) {
        guard self.path != path else { return }
        operatorNavigationRevision += 1
        self.path = path
    }

    /// The macOS sidebar selection, or its absence. Choosing the row already
    /// selected keeps a run opened under it.
    public func select(_ destination: Destination?) {
        guard path.first != destination else { return }
        setPath(destination.map(Self.route(to:)) ?? [])
    }

    /// Whether the route's page holds a card, which is what an operator may
    /// have work in progress on. A run over the page leaves the card held.
    package func holdsCard(in list: UnifiedList) -> Bool {
        switch path.first.map({ Self.surface(for: $0, in: list) }) {
        case .card: true
        case .taskPage(_, let leadingItemID): leadingItemID != nil
        case .run, nil: false
        }
    }

    /// Returns from a run timeline to its task on macOS, which has no stack
    /// to pop.
    public func closeRun() {
        guard runSelection != nil else { return }
        setPath(Array(path.dropLast()))
    }

    package func recordOperatorNavigation() {
        operatorNavigationRevision += 1
    }

    /// Leave a just-concluded item once its receipt delay expires. The
    /// default keeps the item's row selected: its task's page, or another
    /// of the task's open items, since selecting the row again can't reach
    /// those, or a scoped item's own card. Only when `advancesToNextItem` is
    /// set, and an open item remains, does focus move to the next row.
    package func advanceAfterConclusion(
        itemID: String,
        expectedOperatorNavigationRevision: Int,
        advancesToNextItem: Bool,
        list: UnifiedList
    ) -> ConclusionAdvanceResult {
        let taskID = list.taskID(ofItem: itemID)
        // A list rebuild may move the route off the concluded item before the
        // delay expires, along its task's row or to nothing, so those remain
        // the same route. A different concrete destination is deliberate
        // operator navigation and must win over the automatic move.
        let selected = list.selection(attentionItemID: attentionSelection, taskID: taskSelection)
        guard operatorNavigationRevision == expectedOperatorNavigationRevision,
            path.isEmpty || path == [.attentionItem(itemID)]
                || taskID.map({ selected == .task($0) }) == true
        else {
            return .cancelled
        }
        let nextItemID = list.nextOpenItemID(excluding: itemID)
        if advancesToNextItem, let nextItemID {
            route(to: .attentionItem(nextItemID))
            return .advanced
        }
        if let taskID, list.holds(.task(taskID)) {
            let sibling = list.nextOpenItemID(inTask: taskID, excluding: itemID)
            path = [sibling.map(Destination.attentionItem) ?? .task(taskID)]
            return .returnedToList
        }
        if list.holds(.item(itemID)) {
            path = [.attentionItem(itemID)]
            return .returnedToList
        }
        path = []
        return nextItemID == nil ? .inboxClear : .returnedToList
    }

    /// Next Item and Previous Item, and the sidebar's j and k: one step along
    /// the Needs you rows, opening what the row opens.
    package func moveAttentionSelection(by offset: Int, list: UnifiedList) {
        let rows = list.needsYou
        guard !rows.isEmpty else { return }
        let selected = list.selection(attentionItemID: attentionSelection, taskID: taskSelection)
        let currentIndex = rows.firstIndex { $0.id == selected }
        let nextIndex: Int
        if let currentIndex {
            nextIndex = min(max(currentIndex + offset, rows.startIndex), rows.index(before: rows.endIndex))
        } else {
            nextIndex = offset < 0 ? rows.index(before: rows.endIndex) : rows.startIndex
        }
        route(to: rows[nextIndex].destination)
    }

    public func moveAttentionSelection(by offset: Int, coordinator: SyncCoordinator) {
        moveAttentionSelection(by: offset, list: coordinator.unifiedList)
    }

    /// Keep the route on a row after the list changed under it, without
    /// counting as operator navigation against a pending conclusion advance.
    /// An item that was open and has resolved under its task hands the route
    /// along the task's row: to another of its open items, else the task, so
    /// the row stays selected and the page stays, its card gone. A closed
    /// item opened on
    /// purpose stays. A selection with no row left
    /// clears, taking any run with it. A task with an open item settles on
    /// the item leading its page, so a newer item can't swap the card under
    /// an operator working on it. `previouslyOpen` is the open item ids
    /// before this change.
    package func repairSelection(in list: UnifiedList, previouslyOpen: Set<String> = []) {
        switch path.first {
        case .attentionItem(let itemID):
            guard itemID != linkedItemID else { return }
            guard let row = list.selection(attentionItemID: itemID, taskID: nil), list.holds(row) else {
                path = []
                return
            }
            if case .task(let taskID) = row, !list.isOpen(itemID: itemID), previouslyOpen.contains(itemID) {
                let sibling = list.nextOpenItemID(inTask: taskID, excluding: itemID)
                path = [sibling.map(Destination.attentionItem) ?? .task(taskID)] + path.dropFirst()
            }
        case .task(let taskID):
            guard list.holds(.task(taskID)) else {
                path = []
                return
            }
            let open = list.openItems(inTask: taskID)
            if path.count == 1, let lead = open.first(where: { previouslyOpen.contains($0.item.id) }) ?? open.first {
                path = [.attentionItem(lead.item.id)]
            }
        case .run, nil:
            break
        }
    }
}
