import Observation

/// One shared routing seam for launch inputs, future notification delivery,
/// and the platform-specific navigation containers.
@MainActor
@Observable
final class NavigationModel {
    enum Destination: Equatable {
        case attentionItem(String)
        case run(String)
    }

    var selectedTab: LaunchInputs.Screen
    var inboxPath: [String]
    var runsPath: [String]
    var attentionSelection: String?
    var runSelection: String?

    init(launchInputs: LaunchInputs) {
        selectedTab = launchInputs.screen
        inboxPath = []
        runsPath = []
        attentionSelection = nil
        runSelection = nil

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
    func route(to destination: Destination) {
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

    static func repairedPath(_ path: [String], availableIDs: Set<String>) -> [String] {
        guard let routedID = path.last, !availableIDs.contains(routedID) else { return path }
        return Array(path.dropLast())
    }
}
