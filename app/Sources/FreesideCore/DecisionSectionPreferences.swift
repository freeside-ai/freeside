import Foundation
import Observation

@MainActor
@Observable
final class DecisionSectionPreferences {
    private enum Key {
        static let claims = "FreesideDecisionInspectorClaimsExpanded"
        static let evidence = "FreesideDecisionInspectorEvidenceExpanded"
        static let details = "FreesideDecisionInspectorDetailsExpanded"
    }

    private let defaults: UserDefaults

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
