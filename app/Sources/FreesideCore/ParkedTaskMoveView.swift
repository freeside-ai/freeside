import FreesideAPI
import FreesideModel
import SwiftUI

/// The move a task parked on its implementer's exhausted quota window offers
/// (issue #869): a menu of the agents the hold offers, the saved move's
/// Retry while the daemon hasn't answered it, and the daemon's refusal. The
/// daemon re-derives that the chosen agent still qualifies, so the menu
/// lists the hold's snapshot without judging it. The saved move and its
/// refusal outlive the hold: the daemon clears the hold when it records the
/// move, and an unanswered move's Retry is how the operator learns that.
struct ParkedTaskMoveView: View {
    let coordinator: SyncCoordinator
    let taskID: String
    /// The agents the task's live hold offers; empty once the hold clears.
    let offers: [Components.Schemas.AlternateAgentOffer]

    private var model: ParkedTaskMoveModel { coordinator.parkedTaskMove }

    var pending: PendingParkedTaskMove? {
        model.pending.flatMap { $0.taskID == taskID ? $0 : nil }
    }

    var failure: String? {
        model.failure.flatMap { $0.taskID == taskID ? $0.message : nil }
    }

    /// Whether the page offers the move menu: the hold offers an agent and
    /// the daemon serves the move.
    var offersMove: Bool { !offers.isEmpty && model.isServed }

    /// True while there is nothing to say for this task, so the page leaves
    /// the view out.
    var isQuiet: Bool { !offersMove && pending == nil && failure == nil }

    /// Why the menu is disabled, or nil while a pick can send. A device
    /// sends one move at a time, so a move waiting on another task names
    /// it: only that task's page offers its Retry, and only while the move
    /// is unanswered.
    var blockedReason: String? {
        if let reason = model.unavailableReason { return reason }
        if model.canMove { return nil }
        guard let pending = model.pending, pending.taskID != taskID,
            let waiting = coordinator.tasks.first(where: { $0.task.id == pending.taskID })
        else { return "A move is still waiting for the daemon." }
        let name = "\u{201C}\(waiting.task.display_names.task.text)\u{201D}"
        if model.sending || pending.receipt != nil { return "A move of \(name) is being recorded." }
        return "A move of \(name) is still waiting for the daemon. Retry it from that task's page."
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let pending {
                if pending.receipt == nil, !model.sending {
                    statement("The daemon didn't answer the move. Retry sends the same request.")
                    Button("Retry Sending Move") { Task { await model.retry() } }
                        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                        .disabled(!model.canRetry)
                    // Unpaired, Retry can't send; offline or stale, it may
                    // fail again. Either way the page says why.
                    if let reason = model.unavailableReason {
                        statement(reason)
                    }
                } else {
                    statement("Moving the task. The page updates once the daemon records the move.")
                }
            } else if offersMove {
                menu
                // A disabled menu can't open, so its reason stands beside it.
                if let blockedReason {
                    statement(blockedReason)
                }
            }
            if let failure {
                statement(failure)
            }
        }
    }

    private var menu: some View {
        Menu {
            ForEach(AgentChoiceDisplay.offerOptions(offers, pools: coordinator.usagePools, now: Date())) { option in
                Button(option.label) {
                    Task { await model.move(taskID: taskID, to: option.choice) }
                }
            }
        } label: {
            Text("Move to Another Agent \u{25BE}")
        }
        .menuStyle(.button)
        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
        .disabled(blockedReason != nil)
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
