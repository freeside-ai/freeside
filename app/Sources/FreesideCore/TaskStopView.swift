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

    /// True while the control is the Stop button alone, with no sentence
    /// or second button. Only then can a host set it beside something else;
    /// any state that speaks takes its own row.
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
                Button(role: .destructive) {
                    confirmation = model.prepare(taskID: taskID)
                } label: {
                    Label {
                        Text("Stop task…")
                    } icon: {
                        Image(systemName: "stop.fill").font(.system(size: 9))
                    }
                }
                // The wax outline, never filled: the tone the consequence
                // sheet uses for a destructive choice. It hugs its label so
                // it can share the header row.
                .buttonStyle(FreesideActionButtonStyle(tone: .destructive, compact: true, expands: false))
                .disabled(model.unavailableReason != nil || snapshot == nil)
                .accessibilityHint("Review what stopping this task will do")
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

struct TaskStopConfirmationView: View {
    let entry: PendingTaskStop
    let onConfirm: () -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        ScrollView { content }
            .background(Color.ground2)
            .freesideSheetPresentation()
            .frame(idealWidth: 460, minHeight: 340)
    }

    var content: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Stop this task?").font(FreesideFont.title)
            Text(entry.taskName).font(FreesideFont.callout.weight(.semibold))
            Text(entry.projectName).foregroundStyle(Color.inkDim)
            Text(
                "Stop any remaining work owned by this task and prevent further work. Existing history and PRs remain. The daemon must confirm that execution has stopped."
            )
            Button("Stop task", role: .destructive, action: onConfirm)
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
            Button("Keep task") { dismiss() }
                .keyboardShortcut(.cancelAction)
        }
        .font(FreesideFont.callout)
        .fixedSize(horizontal: false, vertical: true)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(24)
        .foregroundStyle(Color.ink)
        .background(Color.ground2)
    }
}

struct TaskStopRecoveryView: View {
    let coordinator: SyncCoordinator
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                Text("Pending Stops").font(FreesideFont.title)
                Text("Saved requests stay here until their results are recovered. Opening this view never sends them.")
                ForEach(coordinator.taskStop.pending, id: \.command.command_id) { entry in
                    VStack(alignment: .leading, spacing: 10) {
                        Text(entry.taskName).font(FreesideFont.callout.weight(.semibold))
                        Text(entry.projectName).foregroundStyle(Color.inkDim)
                        TaskStopView(coordinator: coordinator, taskID: entry.taskID)
                    }
                    Divider()
                }
                if coordinator.taskStop.pending.isEmpty { Text("No pending Stop requests.") }
                Button("Close") { dismiss() }.keyboardShortcut(.cancelAction)
            }
            .font(FreesideFont.callout)
            .padding(24)
            .foregroundStyle(Color.ink)
            .background(Color.ground2)
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(idealWidth: 480, minHeight: 340)
    }
}
