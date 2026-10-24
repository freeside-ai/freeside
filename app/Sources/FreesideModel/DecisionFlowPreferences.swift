import Foundation
import Observation

@MainActor
@Observable
public final class DecisionFlowPreferences {
    private enum Key {
        // Kept from when the default was "advance": an operator's explicit
        // choice survives the flip. An unset key now means the inbox, and an
        // explicit value still selects that operator's behavior. The only two
        // behaviors are the inbox (off) and the next item (on).
        static let advancesToNextItem = "FreesideDecisionAdvancesAutomatically"
    }

    private let defaults: UserDefaults

    public var advancesToNextItem: Bool {
        didSet {
            defaults.set(advancesToNextItem, forKey: Key.advancesToNextItem)
        }
    }

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        // Unset reads false: the default is to return to the inbox.
        advancesToNextItem = defaults.bool(forKey: Key.advancesToNextItem)
    }
}
