import Foundation
import Observation

/// Which folds of an agent transcript are open, per transcript digest, on
/// this device: the same UserDefaults-backed shape as
/// `FindingReasonPreferences`. A fold you opened is open when you come back
/// (the 4 Nov fold rule). Every fold starts closed, so only a transcript
/// with something open keeps an entry.
@MainActor
@Observable
package final class AgentTranscriptFoldPreferences {
    package static let shared = AgentTranscriptFoldPreferences()

    private static let key = "FreesideAgentTranscriptFoldsOpen"
    private let defaults: UserDefaults?
    private var openByTranscript: [String: Set<String>] {
        didSet { defaults?.set(openByTranscript.mapValues { $0.sorted() }, forKey: Self.key) }
    }

    /// `defaults` nil keeps the state in memory only, starting from `open`:
    /// a screenshot must neither read this device's folds nor leave any
    /// behind.
    package init(defaults: UserDefaults? = .standard, open: [String: Set<String>] = [:]) {
        self.defaults = defaults
        let stored = defaults?.dictionary(forKey: Self.key) as? [String: [String]] ?? [:]
        openByTranscript = stored.mapValues(Set.init).merging(open) { $0.union($1) }
    }

    package func isOpen(_ fold: String, transcript: String) -> Bool {
        openByTranscript[transcript]?.contains(fold) ?? false
    }

    package func setOpen(_ open: Bool, fold: String, transcript: String) {
        var folds = openByTranscript[transcript] ?? []
        if open {
            folds.insert(fold)
        } else {
            folds.remove(fold)
        }
        openByTranscript[transcript] = folds.isEmpty ? nil : folds
    }
}
