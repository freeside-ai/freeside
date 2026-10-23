import Foundation
import Observation
import SwiftUI

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
        // The Settings scene sits outside FreesideRootView, so the window
        // root's copyable-text default doesn't reach this form.
        .textSelection(.enabled)
        .padding()
        .frame(width: 440)
    }
}
