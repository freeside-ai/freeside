import FreesideAPI
import SwiftUI

/// Shared Mac/iPhone control. Synced cancellation always outranks a receipt.
struct TaskStopView: View {
    let coordinator: SyncCoordinator
    let taskID: String
    @State private var confirmation: TaskStopModel.Confirmation?
    @State private var showsExplanation = false

    private var model: TaskStopModel { coordinator.taskStop }
    private var snapshot: Components.Schemas.TaskSnapshot? {
        coordinator.tasks.first { $0.task.id == taskID }
    }

    /// True while the control is the More Actions menu alone, with no
    /// sentence or second button: Stop is offered and nothing about it is
    /// in flight.
    var showsOnlyTheStopButton: Bool {
        snapshot?.task.cancellation == nil && !model.sending.contains(taskID)
            && model.pending(for: taskID) == nil && model.unavailableReason == nil
            && model.messages[taskID] == nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let cancellation = snapshot?.task.cancellation?.value1 {
                Self.cancellationNotice(cancellation.state)
                if coordinator.store.freshness != .fresh {
                    note("Last synced status. Refresh to check current task state.")
                }
            }
            if model.sending.contains(taskID) {
                note("Sending Stop…")
            } else if let pending = model.pending(for: taskID) {
                if pending.receipt != nil {
                    if snapshot?.task.cancellation == nil {
                        Notice(
                            tone: .neutral, keyword: "Requested",
                            sentence: "Stop accepted; waiting for the daemon to confirm."
                        )
                        .accessibilityElement(children: .combine)
                    }
                } else {
                    Notice(
                        tone: .accent, keyword: "Unconfirmed",
                        sentence: snapshot?.task.cancellation == nil
                            ? "The daemon did not answer the stop. Nothing is assumed."
                            : "The original request's receipt is unresolved."
                    )
                    .accessibilityElement(children: .combine)
                }
            } else if snapshot?.task.cancellation == nil {
                // Stop is the page's one consequential action, so it sits
                // behind the overflow menu the decision card uses, never as
                // a standing button beside the task's name (5.6). Only the
                // offer folds away: every state above and below this branch
                // stays on the page.
                Menu {
                    Button("Stop Task…", role: .destructive) {
                        confirmation = model.prepare(taskID: taskID)
                    }
                } label: {
                    Text("More Actions \u{25BE}")
                }
                .menuStyle(.button)
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                .disabled(model.unavailableReason != nil || snapshot == nil)
                .accessibilityLabel("More task actions")
                .accessibilityHint("Holds Stop Task, which reviews what stopping this task will do")
            }
            if let reason = model.unavailableReason { note(reason) }
            if let message = model.messages[taskID] { note(message, color: .ink) }
            if let explanation {
                SentenceDisclosure(label: "What Happened", isExpanded: $showsExplanation) {
                    note(explanation)
                }
            }
            if snapshot?.task.cancellation != nil || model.pending(for: taskID) != nil || model.unavailableReason != nil
            {
                // The notice above states; the acts are buttons on their
                // own row (R14). Side by side while the row fits, and one
                // under the other across the column where it does not.
                ViewThatFits(in: .horizontal) {
                    HStack(spacing: FreesideLadder.current.controlGap) {
                        controls(expands: false)
                    }
                    VStack(spacing: FreesideLadder.current.controlGap) {
                        controls(expands: true)
                    }
                }
            }
        }
        .fixedSize(horizontal: false, vertical: true)
        .sheet(item: $confirmation) { prepared in
            TaskStopConfirmationView(entry: prepared.entry) {
                confirmation = nil
                Task { await model.confirm(prepared) }
            }
        }
    }

    /// The Stop the daemon never answered: the request Retry resends.
    private var unanswered: PendingTaskStop? {
        guard !model.sending.contains(taskID), let pending = model.pending(for: taskID),
            pending.receipt == nil
        else { return nil }
        return pending
    }

    @ViewBuilder
    private func controls(expands: Bool) -> some View {
        if let unanswered {
            Button("Retry Sending Stop") {
                Task { await model.retry(unanswered.command.command_id) }
            }
            .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: expands))
            .disabled(coordinator.store.freshness == .unauthenticated)
        }
        Button("Refresh Task Status") { Task { await model.refresh() } }
            .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: expands))
    }

    /// The explanation the visible state leaves behind `What Happened`
    /// (R12): what Retry resends, and that a second Stop does not restart
    /// cancellation. Nil where the notice already says all there is.
    private var explanation: String? {
        let cancellation = snapshot?.task.cancellation?.value1
        if !model.sending.contains(taskID), let pending = model.pending(for: taskID), pending.receipt == nil {
            return cancellation == nil
                ? "Retry sends the same request to recover its result."
                : "Retry recovers that receipt; it does not restart cancellation."
        }
        if cancellation?.state == .failed_to_stop {
            return "Refresh to check again; sending Stop again does not restart cancellation."
        }
        return nil
    }

    private func note(_ text: String, color: Color = .inkDim) -> some View {
        Text(text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(color)
    }

    /// The synced cancellation as a receipt notice (R12): the keyword names
    /// the state and the sentence is `cancellationText`.
    private static func cancellationNotice(_ state: Components.Schemas.TaskCancellationState) -> some View {
        let (tone, keyword): (Notice.Tone, String) =
            switch state {
            case .requested: (.neutral, "Requested")
            case .failed_to_stop: (.wax, "Failed")
            case .confirmed: (.neutral, "Recorded")
            }
        return Notice(tone: tone, keyword: keyword, sentence: cancellationText(state))
            .accessibilityElement(children: .combine)
    }

    static func cancellationText(_ state: Components.Schemas.TaskCancellationState) -> String {
        switch state {
        case .requested: "Stop sent; waiting for the daemon to confirm."
        case .failed_to_stop: "The task did not stop. Execution may continue."
        case .confirmed: "Stop confirmed by the daemon. Existing history and PRs remain available."
        }
    }
}

/// The confirmation the task page's Stop opens (R11): what stopping does,
/// the task it binds to, and the wax-outlined submit.
struct TaskStopConfirmationView: View {
    static let consequence =
        "Stop any remaining work owned by this task and prevent further work. "
        + "Existing history and PRs remain. The daemon must confirm that execution has stopped."

    let entry: PendingTaskStop
    let onConfirm: () -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(spacing: 0) {
            ScrollView { facts }
            actionRow
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(idealWidth: 460, minHeight: 340)
    }

    /// The sheet laid out in place of its scroll view, for the screenshot
    /// suite.
    var content: some View {
        VStack(spacing: 0) {
            facts
            actionRow
        }
        .background(Color.ground2)
    }

    private var facts: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Stop task", ask: "Stop this task?", consequence: Self.consequence,
                binding: entry.taskID, askLineLimit: nil)
            VStack(alignment: .leading, spacing: 11) {
                FactRow(label: "Task", value: entry.taskName)
                FactRow(label: "Project", value: entry.projectName)
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    private var actionRow: some View {
        FreesideSheetActionRow(
            submitLabel: "Stop Task", tone: .destructive, submitHint: Self.consequence,
            submitsOnReturn: false, submit: onConfirm, cancel: { dismiss() })
    }
}

/// The saved Stop requests whose results are not recovered yet (R11). Opening
/// it never sends one; each entry carries its own state and its own Retry.
struct TaskStopRecoveryView: View {
    let coordinator: SyncCoordinator
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(spacing: 0) {
            ScrollView { entries }
            FreesideSheetActionRow.done { dismiss() }
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(idealWidth: 480, minHeight: 340)
    }

    /// The sheet laid out in place of its scroll view, for the screenshot
    /// suite.
    var content: some View {
        VStack(spacing: 0) {
            entries
            FreesideSheetActionRow.done {}
        }
        .background(Color.ground2)
    }

    private var pending: [PendingTaskStop] { coordinator.taskStop.pending }

    private var entries: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Stop requests",
                chip: StateChip(label: "\(pending.count)", cut: .attention),
                ask: "Pending Stops",
                consequence:
                    "Saved requests stay here until their results are recovered. Opening this view never sends them.",
                askLineLimit: nil)
            VStack(alignment: .leading, spacing: 12) {
                ForEach(pending, id: \.command.command_id) { entry in
                    VStack(alignment: .leading, spacing: 11) {
                        FactRow(label: "Task", value: entry.taskName)
                        FactRow(label: "Project", value: entry.projectName)
                        TaskStopView(coordinator: coordinator, taskID: entry.taskID)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(16)
                    .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                    .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
                }
                if pending.isEmpty {
                    Text("No pending Stop requests.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }
}
