import FreesideAPI
import FreesideModel
import SwiftUI

/// The move a task parked on its implementer's exhausted quota window offers
/// (issue #869): a menu of the agents the hold offers, the saved move's
/// Retry while the daemon hasn't answered it, and the daemon's refusal. The
/// daemon re-derives that the chosen agent still qualifies, so the menu
/// lists the hold's snapshot without judging it.
struct ParkedTaskMoveView: View {
    let model: ParkedTaskMoveModel
    let taskID: String
    let offers: [Components.Schemas.AlternateAgentOffer]
    /// The synced usage pools each entry's reading comes from.
    let pools: [Components.Schemas.UsagePoolSnapshot]

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let pending = model.pending, pending.taskID == taskID {
                if pending.receipt == nil, !model.sending {
                    statement("The daemon didn't answer the move. Retry sends the same request.")
                    Button("Retry Sending Move") { Task { await model.retry() } }
                        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                } else {
                    statement("Moving the task. The page updates once the daemon records the move.")
                }
            } else {
                menu
            }
            if let failure = model.failure {
                statement(failure)
            }
        }
    }

    private var menu: some View {
        Menu {
            ForEach(AgentChoiceDisplay.offerOptions(offers, pools: pools, now: Date())) { option in
                Button(option.label) {
                    Task { await model.move(taskID: taskID, to: option.choice) }
                }
            }
        } label: {
            Text("Move to Another Agent \u{25BE}")
        }
        .menuStyle(.button)
        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
        .disabled(!model.canMove)
        .accessibilityLabel("Move to another agent")
        .accessibilityHint("Offers \(offers.map(\.enrollment).formatted(.list(type: .or)))")
    }

    private func statement(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}
