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
    /// How many times `route(toLinkedItem:)` has run: a notification link, a
    /// launch that names an item, or viewing a concluded item. A link to the
    /// card already leading changes no route, so the root watches this
    /// count to open the card anyway, as a tap on that card does. A count
    /// rather than `linkedItemID`, because a second link to the same item
    /// leaves that unchanged.
    public private(set) var linkOpenings = 0
    /// The New Task composer's presentation flag, shared by the Mac list's tools
    /// row, the iPhone's bottom bar, ⌘N, and the File > New Task menu item, so
    /// all of them open the one sheet through the same gated path.
    public var newTaskComposerPresented = false
    public var submissionRecoveryPresented = false
    /// A pane of Settings, in the Mac source list's order, which the
    /// iPhone's grouped list follows.
    public enum SettingsPage: String, CaseIterable, Hashable, Sendable {
        case general
        case agents
        case workflow
        case usage
        case devices
        case notifications
        case daemon
        case systemHealth

        public var title: String {
            switch self {
            case .general: "General"
            case .agents: "Agents"
            case .workflow: "Workflow"
            case .usage: "Usage"
            case .devices: "Devices"
            case .notifications: "Notifications"
            case .daemon: "Daemon"
            case .systemHealth: "System Health"
            }
        }
    }

    /// One step of the iPhone's pushed Settings path: the grouped list, then
    /// the pane one of its rows opens, then a System Health finding's card
    /// over that pane. The Mac window draws no stack and reads only the
    /// trailing `finding`, as the card it seats in place of the pane.
    public enum SettingsRoute: Hashable, Sendable {
        case list
        case pane(SettingsPage)
        case finding(String)

        var page: SettingsPage? {
            if case .pane(let page) = self { page } else { nil }
        }
    }

    /// One element of the iPhone's one stack: the task route, then the
    /// Settings path above it. The stack binds `phoneRoute`, and every push
    /// onto it, a list row's included, carries this type.
    public enum PhoneRoute: Hashable {
        case destination(Destination)
        case settings(SettingsRoute)
    }

    /// The selected Settings pane, kept across opens: the Mac window's
    /// source list selection and the pane an iPhone row last pushed. The
    /// `-FreesideDevices`, `-FreesideRouting`, `-FreesideUsage`, and
    /// `-FreesideSystemHealth` launch arguments choose it, so a capture
    /// needs no click.
    public private(set) var settingsPane: SettingsPage = .general
    /// The iPhone's pushed Settings path above the task route: empty, the
    /// list, the list and one pane, or those and a finding's card. The Mac
    /// draws Settings in its own window and reads only a trailing finding.
    /// Kept apart from `path`, whose first element means the selected task
    /// or item.
    public private(set) var settingsRoute: [SettingsRoute] = []
    /// A launch argument asked for the Mac's Settings window. The root opens
    /// the window once and consumes it, so a rebuilt root never opens it
    /// again.
    public private(set) var settingsWindowRequested = false
    /// The `-FreesideComposer` launch argument until the card acts on it.
    /// Held here, not in the card, because the Mac detail view is rebuilt on
    /// every selection: view state would reopen the composer each time the
    /// item is reselected.
    public private(set) var launchComposer: LaunchComposerRequest?
    public private(set) var operatorNavigationRevision = 0

    public init(launchInputs: LaunchInputs) {
        path = []
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
        // After the route, which clears the Settings path: a launch that
        // names both opens Settings over the selection.
        let launchPane: SettingsPage? =
            if launchInputs.devicesPresented {
                .devices
            } else if launchInputs.routingPresented {
                .agents
            } else if launchInputs.usagePresented {
                .usage
            } else if launchInputs.systemHealthPresented {
                .systemHealth
            } else {
                nil
            }
        if let launchPane {
            settingsPane = launchPane
            settingsRoute = [.list, .pane(launchPane)]
            settingsWindowRequested = true
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
    /// link the list hasn't caught up to, is its card alone. A task is its
    /// page with no card seated, even with items open: an item that opens
    /// under a reader draws as a one-line row until the operator opens it,
    /// so the page never changes shape under them (#1460).
    package static func surface(for destination: Destination, in list: UnifiedList) -> DetailSurface {
        switch destination {
        case .attentionItem(let itemID):
            list.taskID(ofItem: itemID).map { .taskPage(taskID: $0, leadingItemID: itemID) }
                ?? .card(itemID: itemID)
        case .task(let taskID):
            .taskPage(taskID: taskID, leadingItemID: nil)
        case .run(let taskID, let runID):
            .run(taskID: taskID, runID: runID)
        }
    }

    /// The surface the macOS detail column draws for the route, which shows
    /// its last destination. Nil with nothing routed.
    package func detailSurface(in list: UnifiedList) -> DetailSurface? {
        path.last.map { Self.surface(for: $0, in: list) }
    }

    /// The iPhone stack's whole path: `path`, then `settingsRoute`.
    public var phoneRoute: [PhoneRoute] {
        path.map(PhoneRoute.destination) + settingsRoute.map(PhoneRoute.settings)
    }

    /// The iPhone stack as the operator drives it (a push, a pop). The task
    /// route is the leading run of destinations; whatever follows is the
    /// Settings path, so a pop from a pane never touches the selection.
    public func setPhoneRoute(_ route: [PhoneRoute]) {
        let destinations = route.prefix { if case .destination = $0 { true } else { false } }
        setPath(destinations.compactMap { if case .destination(let d) = $0 { d } else { nil } })
        setSettingsRoute(
            route.dropFirst(destinations.count).compactMap {
                if case .settings(let step) = $0 { step } else { nil }
            })
    }

    /// The iPhone's Settings button: pushes the grouped list and keeps the
    /// selected pane for the next row it opens. The Mac's button opens the
    /// window through the environment's `openSettings` action instead.
    /// Every iPhone Settings move is operator navigation, so a pending
    /// conclusion advance never pops Settings or rewrites the route under it.
    public func openSettings() {
        setSettingsRoute([.list])
    }

    /// The Mac source list's selection. It leaves a seated finding's card
    /// for the pane, as choosing any row of the list does.
    public func selectSettingsPane(_ page: SettingsPage) {
        settingsPane = page
        if case .finding? = settingsRoute.last { settingsRoute.removeLast() }
    }

    /// An iPhone list row: push its pane over the list.
    public func pushSettingsPane(_ page: SettingsPage) {
        setSettingsRoute([.list, .pane(page)])
    }

    /// The iPhone stack's Settings path as the operator drives it (a push,
    /// a pop). A pop keeps the selected pane.
    public func setSettingsRoute(_ route: [SettingsRoute]) {
        guard settingsRoute != route else { return }
        operatorNavigationRevision += 1
        settingsRoute = route
        // A finding's card sits over its pane, so the pane under it counts.
        if let page = route.reversed().lazy.compactMap(\.page).first { settingsPane = page }
    }

    /// A System Health row: its finding's card over the pane, pushed on the
    /// iPhone and seated in the pane on the Mac.
    public func openFinding(_ itemID: String) {
        openSystemHealth(finding: itemID)
    }

    /// Where every ambient health signal lands (the stopped notice, the Mac
    /// menu bar line, a notification link): the System Health pane, with
    /// `itemID`'s card over it when one is named. The iPhone pushes this
    /// path; the Mac's Settings window reads the selected pane and seated
    /// finding from it, so the caller still opens that window.
    public func openSystemHealth(finding itemID: String? = nil) {
        setSettingsRoute([.list, .pane(.systemHealth)] + (itemID.map { [.finding($0)] } ?? []))
        // The Mac source list selects a pane without moving the path, so a
        // path that is already the pane's leaves another pane selected.
        settingsPane = .systemHealth
    }

    /// The seated card's back control: return to the System Health pane.
    public func closeFinding() {
        guard case .finding? = settingsRoute.last else { return }
        setSettingsRoute(Array(settingsRoute.dropLast()))
    }

    /// The finding whose card the Settings path ends on, if any.
    public var settingsFinding: String? {
        if case .finding(let itemID)? = settingsRoute.last { itemID } else { nil }
    }

    /// Leave the iPhone's Settings path, as signing out from Devices does.
    public func closeSettings() {
        setSettingsRoute([])
    }

    public func consumeSettingsWindowRequest() {
        settingsWindowRequested = false
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
        // rendered item (for example, Reveal in Identifiers).
        operatorNavigationRevision += 1
        path = Self.route(to: destination)
        // A link or launch that routes on iPhone shows its destination, not
        // the Settings path pushed above it.
        settingsRoute = []
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
        linkOpenings += 1
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
        if let row = step(by: offset, along: list.needsYou, list: list) {
            route(to: row.destination)
        }
    }

    /// The Mac sidebar's arrow keys: one step along the rows it draws,
    /// Working and Finished included, selecting as a click does.
    package func moveSelection(by offset: Int, along rows: [UnifiedRow], list: UnifiedList) {
        if let row = step(by: offset, along: rows, list: list) {
            select(row.destination)
        }
    }

    /// The row `offset` steps from the selected one, holding at either end;
    /// with no row of `rows` selected, a step forward lands on the first
    /// and a step back on the last.
    private func step(by offset: Int, along rows: [UnifiedRow], list: UnifiedList) -> UnifiedRow? {
        guard !rows.isEmpty else { return nil }
        let selected = list.selection(attentionItemID: attentionSelection, taskID: taskSelection)
        guard let currentIndex = rows.firstIndex(where: { $0.id == selected }) else {
            return offset < 0 ? rows.last : rows.first
        }
        return rows[min(max(currentIndex + offset, 0), rows.count - 1)]
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
    /// purpose stays, and so does a system health finding's card, which no
    /// row holds (a `-FreesideSelect` launch or a viewed concluded item can
    /// still open one in the main route). A selection with no row left
    /// clears, taking any run with it. A task route stays the page with no
    /// card when an item opens; the operator seats the card by opening the
    /// item. `previouslyOpen` is the open item ids before this change.
    package func repairSelection(in list: UnifiedList, previouslyOpen: Set<String> = []) {
        switch path.first {
        case .attentionItem(let itemID):
            guard itemID != linkedItemID, !list.isFinding(itemID: itemID) else { return }
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
        case .run, nil:
            break
        }
    }
}
