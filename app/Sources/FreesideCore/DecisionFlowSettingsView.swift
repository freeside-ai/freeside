import SwiftUI

/// The General pane's body: the Decision Flow toggle, the one preference
/// the app stores (◆1 of the 4 Nov 2026 handoff).
struct DecisionFlowSettingsView: View {
    let preferences: DecisionFlowPreferences
    var rendersInteractiveControls = true

    var body: some View {
        @Bindable var preferences = preferences
        SettingsSection(keyword: "Decision flow") {
            SettingsGroup(["advance"]) { _ in
                SettingsRow(
                    label: "Advance to the next item",
                    line:
                        "After a decision is applied, open the next item that needs you instead of returning to the list."
                ) {
                    if rendersInteractiveControls {
                        Toggle("Advance to the next item", isOn: $preferences.advancesToNextItem)
                            .labelsHidden()
                            .toggleStyle(.switch)
                    } else {
                        SettingsToggleStandIn(isOn: preferences.advancesToNextItem)
                    }
                }
            }
        }
    }
}
