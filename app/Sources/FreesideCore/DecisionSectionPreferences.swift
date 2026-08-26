import Foundation
import Observation

@MainActor
@Observable
final class DecisionSectionPreferences {
    private enum Key {
        static let facts = "FreesideDecisionInspectorFactsExpanded"
        static let proposal = "FreesideDecisionInspectorProposalExpanded"
        static let claims = "FreesideDecisionInspectorClaimsExpanded"
        static let evidence = "FreesideDecisionInspectorEvidenceExpanded"
        static let details = "FreesideDecisionInspectorDetailsExpanded"
    }

    private let defaults: UserDefaults

    var factsExpanded: Bool {
        didSet { defaults.set(factsExpanded, forKey: Key.facts) }
    }
    var proposalExpanded: Bool {
        didSet { defaults.set(proposalExpanded, forKey: Key.proposal) }
    }
    var claimsExpanded: Bool {
        didSet { defaults.set(claimsExpanded, forKey: Key.claims) }
    }
    var evidenceExpanded: Bool {
        didSet { defaults.set(evidenceExpanded, forKey: Key.evidence) }
    }
    var detailsExpanded: Bool {
        didSet { defaults.set(detailsExpanded, forKey: Key.details) }
    }

    init(defaults: UserDefaults = .standard, detailsExpandedOverride: Bool? = nil) {
        self.defaults = defaults
        factsExpanded = Self.value(defaults, key: Key.facts, defaultValue: true)
        proposalExpanded = Self.value(defaults, key: Key.proposal, defaultValue: true)
        claimsExpanded = Self.value(defaults, key: Key.claims, defaultValue: false)
        evidenceExpanded = Self.value(defaults, key: Key.evidence, defaultValue: false)
        detailsExpanded =
            detailsExpandedOverride
            ?? Self.value(defaults, key: Key.details, defaultValue: false)
    }

    private static func value(
        _ defaults: UserDefaults,
        key: String,
        defaultValue: Bool
    ) -> Bool {
        defaults.object(forKey: key) == nil ? defaultValue : defaults.bool(forKey: key)
    }
}
