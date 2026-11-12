import Foundation
import FreesideAPI
import Observation

/// A change of one role's line on a task saved before it was sent: the exact
/// command an explicit Retry resends, and its receipt once the daemon
/// accepted it. Local delivery evidence, never an authority for the task's
/// lines.
public struct PendingTaskLineChange: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    /// The epoch the change was made in. A receipt from another epoch can't
    /// be compared with the task sync shows.
    public let syncEpoch: String
    public var receipt: Components.Schemas.CommandResult?

    func isValid(deviceID: String) -> Bool {
        guard case .change_task_line = command.payload else { return false }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !command.command_id.isEmpty && !deviceID.isEmpty && command.device_id == deviceID
            && !syncEpoch.isEmpty && command.expected_entity_version == nil && command.expected_bindings == nil
    }

    var payload: Components.Schemas.ChangeTaskLinePayload? {
        guard case .change_task_line(let payload) = command.payload else { return nil }
        return payload
    }

    /// The task the change names.
    public var taskID: String? { payload?.task_id }
}

/// Why the daemon refused a line change, on the task it names.
package struct TaskLineChangeFailure: Equatable, Sendable {
    package let taskID: String
    package let message: String
}

/// Sends the operator's change of one role's line on a task, or its clear
/// back to the lineup (`change_task_line`, #2651), modeled on
/// `ParkedTaskMoveModel`: the command persists before it is sent, an
/// unanswered send is retried only by an explicit Retry that resends it
/// unchanged, and the change reads as recorded only once sync shows its
/// revision. One change at a time per device. The daemon refuses a change
/// while the role runs (a 409 carrying the task); the client never predicts
/// that, and refreshes through sync rather than installing the 409's rows,
/// as `TaskStopModel` does. The saved command, whether it is being sent, and
/// its refusal live in the coordinator, where a reopened task page finds
/// them.
@MainActor @Observable
package final class TaskLineChangeModel {
    package typealias Role = Components.Schemas.TaskLineRole
    package typealias Selection = Components.Schemas.TaskLineSelection

    private let coordinator: SyncCoordinator

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var sending: Bool { coordinator.taskLineChangeSending }
    package var failure: TaskLineChangeFailure? {
        get { coordinator.taskLineChangeFailure }
        set { coordinator.taskLineChangeFailure = newValue }
    }
    package var pending: PendingTaskLineChange? { coordinator.pendingTaskLineChange }

    /// Why no line can be changed now, or nil when one can.
    package var unavailableReason: String? {
        switch coordinator.store.freshness {
        case .fresh, .unvalidated:
            return coordinator.cursors == nil ? "Refresh before changing the agent." : nil
        case .unauthenticated: return "Pair this device again before changing the agent."
        case .unreachable: return "Offline. Connect to the daemon before changing the agent."
        case .syncFailing: return "The task isn't current. Refresh before changing the agent."
        case .incompatible: return "Update the app or the daemon before changing the agent."
        }
    }

    package var canChange: Bool { unavailableReason == nil && pending == nil && !sending }

    /// Sets `role`'s line on the task to `selection`, or clears it back to
    /// the lineup when `selection` is nil.
    package func change(taskID: String, role: Role, to selection: Selection?) async {
        guard canChange, let epoch = coordinator.cursors?.syncEpoch else { return }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID,
            payload: .change_task_line(
                .init(
                    kind: .change_task_line, task_id: taskID, role: role,
                    selection: selection.map { .init(value1: $0) })))
        let entry = PendingTaskLineChange(command: command, syncEpoch: epoch)
        guard coordinator.retainTaskLineChange(entry) else {
            failure = .init(
                taskID: taskID, message: "The change couldn't be saved on this device. Nothing was sent. Try again.")
            return
        }
        await send(entry)
    }

    /// Explicit retry only: resends the saved command unchanged, and only in
    /// the epoch it was made in (sync settles it in any other).
    package func retry() async {
        guard let entry = pending, entry.receipt == nil, entry.syncEpoch == coordinator.cursors?.syncEpoch,
            coordinator.store.freshness != .unauthenticated
        else { return }
        await send(entry)
    }

    private func send(_ entry: PendingTaskLineChange) async {
        guard !sending, let payload = entry.payload else { return }
        coordinator.taskLineChangeSending = true
        failure = nil
        defer { coordinator.taskLineChangeSending = false }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, the unanswered command stays retryable.
                guard coordinator.retainTaskLineChange(accepted) else { return }
                await coordinator.refreshAfterCommit()
                coordinator.settleTaskLineChange()
            case .conflict(let conflict):
                // A refusal for another task answers some other request; the
                // saved command stays unresolved.
                guard case .TaskLineChangeRejection(let rejection) = try conflict.body.json,
                    rejection.replacement_task.task.id == payload.task_id
                else { return }
                coordinator.finishTaskLineChange()
                failure = .init(
                    taskID: payload.task_id,
                    message: Self.runningMessage(role: payload.role, task: rejection.replacement_task.task))
                // Refresh through canonical sync; never install a conflict's rows.
                await coordinator.refreshAfterCommit()
            case .badRequest(let refusal):
                // The selection is refused: the daemon recorded nothing.
                coordinator.finishTaskLineChange()
                failure = .init(
                    taskID: payload.task_id,
                    message: CommandRefusal.message(
                        refusal, otherwise: "The daemon refused the change (status 400). Nothing was changed."))
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 404:
                    // The selection or the task is refused: the daemon
                    // recorded nothing.
                    coordinator.finishTaskLineChange()
                    failure = .init(
                        taskID: payload.task_id,
                        message: "The daemon refused the change (status \(status)). Nothing was changed.")
                    await coordinator.refreshAfterCommit()
                case 401, 403: coordinator.store.publish(.unauthenticated)
                default: break
                }
            }
        } catch {
            // A missing or malformed answer leaves the saved command unresolved.
        }
    }

    /// The refusal of a change to a running role, naming the line the role
    /// keeps on `task`, the task the daemon's 409 carried.
    static func runningMessage(role: Role, task: Components.Schemas.Task) -> String {
        let line = task.task_lines.first { $0.role == role }
        let kept =
            line.flatMap { TaskDisplay.lineLabel(agent: $0.agent, selection: $0.selection?.value1) } ?? "the lineup"
        return "The \(AgentChoiceDisplay.title(for: role).lowercased()) is running, so its agent didn't change. "
            + "It stays on \(kept)."
    }
}
