import Foundation
import Observation
import SwiftUI

@MainActor
@Observable
public final class DecisionFlowPreferences {
    private enum Key {
        static let advancesAutomatically = "FreesideDecisionAdvancesAutomatically"
    }

    private let defaults: UserDefaults

    public var advancesAutomatically: Bool {
        didSet {
            defaults.set(advancesAutomatically, forKey: Key.advancesAutomatically)
        }
    }

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        advancesAutomatically =
            defaults.object(forKey: Key.advancesAutomatically) == nil
            ? true : defaults.bool(forKey: Key.advancesAutomatically)
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
                    "Advance after decisions",
                    isOn: $preferences.advancesAutomatically)
                Text("Move to the next highest-priority inbox item after a decision is applied.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .padding()
        .frame(width: 440)
    }
}
