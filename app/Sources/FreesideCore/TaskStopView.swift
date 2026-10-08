import FreesideAPI
import SwiftUI

/// Shared Mac/iPhone control. Synced cancellation always outranks a receipt.
struct TaskStopView: View {
    let coordinator: SyncCoordinator
    let taskID: String
    @State private var confirmation: TaskStopModel.Confirmation?

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
                Text(Self.cancellationText(cancellation.state))
                    .font(FreesideFont.callout)
                if coordinator.store.freshness != .fresh {
                    Text("Last synced status. Refresh to check current task state.")
                }
            }
            if model.sending.contains(taskID) {
                Label("Sending Stop…", systemImage: "arrow.up.circle")
            } else if let pending = model.pending(for: taskID) {
                if pending.receipt != nil {
                    if snapshot?.task.cancellation == nil {
                        Text("Stop accepted. Awaiting current daemon confirmation.")
                    }
                } else {
                    Text(
                        snapshot?.task.cancellation == nil
                            ? "Stop delivery is uncertain. Retry sends the same request to recover its result."
                            : "The original request's receipt is unresolved. Retry recovers that receipt; it does not restart cancellation."
                    )
                    Button("Retry sending Stop") { Task { await model.retry(pending.command.command_id) } }
                        .disabled(coordinator.store.freshness == .unauthenticated)
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
            if let reason = model.unavailableReason { Text(reason) }
            if let message = model.messages[taskID] { Text(message) }
            if snapshot?.task.cancellation != nil || model.pending(for: taskID) != nil || model.unavailableReason != nil
            {
                Button("Refresh task status") { Task { await model.refresh() } }
            }
        }
        .font(FreesideFont.callout)
        .foregroundStyle(Color.ink)
        .fixedSize(horizontal: false, vertical: true)
        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
        .sheet(item: $confirmation) { prepared in
            TaskStopConfirmationView(entry: prepared.entry) {
                confirmation = nil
                Task { await model.confirm(prepared) }
            }
        }
    }

    static func cancellationText(_ state: Components.Schemas.TaskCancellationState) -> String {
        switch state {
        case .requested: "Stop requested. Awaiting daemon confirmation."
        case .failed_to_stop:
            "Failed to stop. Execution may continue. Refresh to check again; sending Stop again does not restart cancellation."
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
            cancelIsOutlined: true, submitsOnReturn: false, submit: onConfirm, cancel: { dismiss() })
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
