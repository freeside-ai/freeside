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
        case run(String)
    }

    public var selectedTab: LaunchInputs.Screen
    public var inboxPath: [String]
    public var runsPath: [String]
    public var attentionSelection: String?
    public var runSelection: String?
    public var inspectorPresented: Bool
    public private(set) var operatorNavigationRevision = 0

    public init(launchInputs: LaunchInputs) {
        selectedTab = launchInputs.screen
        inboxPath = []
        runsPath = []
        attentionSelection = nil
        runSelection = nil
        inspectorPresented = launchInputs.detailsExpanded

        guard let selection = launchInputs.selection else { return }
        switch launchInputs.screen {
        case .inbox:
            route(to: .attentionItem(selection))
        case .runs:
            route(to: .run(selection))
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
        case .run(let runID):
            selectedTab = .runs
            runSelection = runID
            runsPath = [runID]
        }
    }

    public func selectTab(_ screen: LaunchInputs.Screen) {
        guard selectedTab != screen else { return }
        operatorNavigationRevision += 1
        selectedTab = screen
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
}
