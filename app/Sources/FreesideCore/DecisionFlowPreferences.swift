import Foundation
import Observation
import SwiftUI

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

public struct DecisionFlowSettingsView: View {
    private let preferences: DecisionFlowPreferences

    public init(preferences: DecisionFlowPreferences) {
        self.preferences = preferences
    }

    public var body: some View {
        @Bindable var preferences = preferences
        Form {
            Section("Decision Flow") {
                Toggle(
                    "Advance to the next item",
                    isOn: $preferences.advancesToNextItem)
                Text(
                    "After a decision is applied, open the next highest-priority inbox item instead of returning to the inbox."
                )
                .font(.callout)
                .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .padding()
        .frame(width: 440)
    }
}
