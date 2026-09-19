import Foundation
import Observation
import SwiftUI

/// Which folded sections of a task's timeline are open, per task id, on
/// this device: the same UserDefaults-backed shape as
/// `DecisionSectionPreferences`. Everything starts collapsed; only a task
/// with something open keeps an entry, so the store does not grow with the
/// task list.
@MainActor
@Observable
final class TaskTimelineDisclosurePreferences {
    /// The sections a timeline folds. Raw ids keep a run's or campaign's
    /// own state apart from its neighbours'.
    enum Section: Hashable {
        /// A non-current campaign, by its campaign id. Never by position:
        /// a new campaign is prepended, so every index shifts and a stored
        /// index would reopen a different campaign. Nil is the one legacy
        /// section outside any campaign.
        case campaign(String?)
        /// A prior run's one-line card.
        case run(String)
        case runDetails(String)
        /// A review round's facts, by the round's invocation id.
        case roundFacts(String)
        /// A run's folded earlier review rounds.
        case priorRounds(String)
        case taskEvents

        var id: String {
            switch self {
            case .campaign(let campaignID): campaignID.map { "campaign:\($0)" } ?? "outside-campaign"
            case .run(let runID): "run:\(runID)"
            case .runDetails(let runID): "details:\(runID)"
            case .roundFacts(let invocationID): "facts:\(invocationID)"
            case .priorRounds(let runID): "rounds:\(runID)"
            case .taskEvents: "events"
            }
        }
    }

    /// The one live instance. The task timeline and the run timeline both
    /// fold a run's review rounds, so they share state rather than each
    /// caching its own copy of the stored dictionary.
    static let shared = TaskTimelineDisclosurePreferences()

    private static let key = "FreesideTaskTimelineExpandedSections"
    private let defaults: UserDefaults?
    private var expandedByTask: [String: Set<String>] {
        didSet { defaults?.set(expandedByTask.mapValues { $0.sorted() }, forKey: Self.key) }
    }

    /// `defaults` nil keeps the state in memory only: a screenshot must
    /// neither read this device's folds nor leave any behind.
    init(defaults: UserDefaults? = .standard) {
        self.defaults = defaults
        let stored = defaults?.dictionary(forKey: Self.key) as? [String: [String]] ?? [:]
        expandedByTask = stored.mapValues(Set.init)
    }

    func isExpanded(_ section: Section, taskID: String) -> Bool {
        expandedByTask[taskID]?.contains(section.id) ?? false
    }

    func setExpanded(_ expanded: Bool, _ section: Section, taskID: String) {
        var sections = expandedByTask[taskID] ?? []
        if expanded {
            sections.insert(section.id)
        } else {
            sections.remove(section.id)
        }
        expandedByTask[taskID] = sections.isEmpty ? nil : sections
    }

    func binding(_ section: Section, taskID: String) -> Binding<Bool> {
        Binding(
            get: { self.isExpanded(section, taskID: taskID) },
            set: { self.setExpanded($0, section, taskID: taskID) })
    }
}
