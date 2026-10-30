import Foundation
import Observation

/// Whether the unified list's Working and Finished sections are open, per
/// device. Needs you has no disclosure: it is what the list is for.
@MainActor
@Observable
package final class ListSectionPreferences {
    private enum Key {
        static let working = "FreesideListWorkingExpanded"
        static let finished = "FreesideListFinishedExpanded"
    }

    private let defaults: UserDefaults

    package var workingExpanded: Bool {
        didSet { defaults.set(workingExpanded, forKey: Key.working) }
    }
    package var finishedExpanded: Bool {
        didSet { defaults.set(finishedExpanded, forKey: Key.finished) }
    }

    package init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        workingExpanded = Self.value(defaults, key: Key.working, defaultValue: false)
        finishedExpanded = Self.value(defaults, key: Key.finished, defaultValue: false)
    }

    private static func value(
        _ defaults: UserDefaults,
        key: String,
        defaultValue: Bool
    ) -> Bool {
        defaults.object(forKey: key) == nil ? defaultValue : defaults.bool(forKey: key)
    }
}
