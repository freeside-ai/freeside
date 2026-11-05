import Foundation
import Observation

/// Which findings' clamped reasons are expanded, per attention item id, on
/// this device: the same UserDefaults-backed shape as
/// `TaskTimelineDisclosurePreferences`. A reason you opened is open when you
/// come back (#2608). Finding ids such as `finding-1` repeat across items,
/// so the item id is part of the key. Only an item with something open keeps
/// an entry, so the store does not grow with the inbox.
@MainActor
@Observable
package final class FindingReasonPreferences {
    /// The one live instance, so every card reads the same stored state
    /// rather than each caching its own copy.
    package static let shared = FindingReasonPreferences()

    private static let key = "FreesideFindingReasonsExpanded"
    private let defaults: UserDefaults?
    private var expandedByItem: [String: Set<String>] {
        didSet { defaults?.set(expandedByItem.mapValues { $0.sorted() }, forKey: Self.key) }
    }

    /// `defaults` nil keeps the state in memory only: a screenshot must
    /// neither read this device's reasons nor leave any behind.
    package init(defaults: UserDefaults? = .standard) {
        self.defaults = defaults
        let stored = defaults?.dictionary(forKey: Self.key) as? [String: [String]] ?? [:]
        expandedByItem = stored.mapValues(Set.init)
    }

    package func isExpanded(findingID: String, itemID: String) -> Bool {
        expandedByItem[itemID]?.contains(findingID) ?? false
    }

    package func setExpanded(_ expanded: Bool, findingID: String, itemID: String) {
        var findings = expandedByItem[itemID] ?? []
        if expanded {
            findings.insert(findingID)
        } else {
            findings.remove(findingID)
        }
        expandedByItem[itemID] = findings.isEmpty ? nil : findings
    }
}
