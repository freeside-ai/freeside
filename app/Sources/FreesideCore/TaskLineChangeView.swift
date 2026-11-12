import FreesideAPI
import FreesideModel
import SwiftUI

/// The role a task page's Change Agent opened the picker for.
struct TaskLineChangeRequest: Identifiable, Equatable {
    let taskID: String
    let role: AgentChoiceDisplay.Role

    var id: String { "\(taskID) \(role.rawValue)" }
}

/// The Task Actions menu's Change Agent submenu (#2651): one item per
/// role, naming what it runs on now. An item only opens the role's picker
/// (`taskLineChangePicker`, #3027), whose confirm sends one
/// `change_task_line`; what follows speaks in `TaskLineChangeView`, on the
/// page. The daemon decides whether a role can change, so nothing here
/// predicts that a role runs.
struct TaskLineChangeMenu: View {
    typealias Role = AgentChoiceDisplay.Role

    let coordinator: SyncCoordinator
    let task: Components.Schemas.Task
    /// Where an item puts the role it opens. The page presents the picker,
    /// since the menu closes as the item is chosen.
    let request: Binding<TaskLineChangeRequest?>

    private var model: TaskLineChangeModel { coordinator.taskLineChange }

    /// Whether the menu offers the change: while the task is active and no
    /// Stop was requested, since a stopping task starts no new attempt.
    static func offers(_ task: Components.Schemas.Task?) -> Bool {
        task.map { TaskDisplay.isActive($0) && $0.cancellation == nil } ?? false
    }

    /// `role`'s picker on `task`: New Task's picker over the same synced
    /// catalog, holding the role's line, with Clear in place of Default.
    static func picker(
        _ role: Role, task: Components.Schemas.Task, coordinator: SyncCoordinator
    ) -> RolePicker {
        let line = task.task_lines.first { $0.role == role }
        return .change(
            role: role, current: line?.selection?.value1, byName: line?.selection == nil ? line?.agent : nil,
            catalog: coordinator.agentCatalog)
    }

    /// Why the submenu is disabled, or nil while a change can send.
    var blockedReason: String? { Self.blockedReason(taskID: task.id, coordinator: coordinator) }

    /// Why no change on `taskID` can send now, or nil while one can. A
    /// device sends one change at a time, so a change waiting on another
    /// task names it: only that task's page offers its Retry.
    static func blockedReason(taskID: String, coordinator: SyncCoordinator) -> String? {
        let model = coordinator.taskLineChange
        if let reason = model.unavailableReason { return reason }
        if model.canChange { return nil }
        let waiting = model.pending.flatMap { pending in
            pending.taskID == taskID ? nil : coordinator.tasks.first { $0.task.id == pending.taskID }
        }
        guard let waiting else { return "An agent change is still waiting for the daemon." }
        return
            "An agent change on \u{201C}\(waiting.task.display_names.task.text)\u{201D} is still waiting for the daemon. "
            + "Retry it from that task's page."
    }

    var body: some View {
        Menu("Change Agent") {
            ForEach(AgentChoiceDisplay.roleOrder, id: \.self) { role in
                Button(roleTitle(role)) { request.wrappedValue = .init(taskID: task.id, role: role) }
            }
        }
        .disabled(blockedReason != nil)
        // A menu can't show a disabled item's help, so the reason is an
        // item of its own.
        if let blockedReason { Text(blockedReason) }
    }

    /// A role's item names the role and what it runs on now: its line, or,
    /// with none, the lineup's settings for it.
    private func roleTitle(_ role: Role) -> String {
        let line = task.task_lines.first { $0.role == role }
        let lineup = AgentChoiceDisplay.defaultSettingsLabel(
            role: role, policy: coordinator.routingPolicy?.policy, catalog: coordinator.agentCatalog)
        let current =
            line.flatMap { line in
                line.selection.map {
                    AgentChoiceDisplay.choiceLabel($0.value1, roles: [role], catalog: coordinator.agentCatalog)
                } ?? line.agent
            } ?? lineup.map { "lineup \($0)" } ?? "lineup"
        return "\(AgentChoiceDisplay.title(for: role)) · \(current)"
    }
}

/// The picker Change Agent opens for one role of a running task. It reads
/// the task, the catalog, and whether a change can send on every draw, so
/// a line, a Stop, or a sync that changes while it is open shows in its
/// footer and its confirm.
struct TaskLineChangePicker: View {
    static let endedReason = "Stop was requested or the task ended, so its agent can't change."

    let request: TaskLineChangeRequest
    let coordinator: SyncCoordinator
    /// Called once, by the confirm, with the selection to send.
    let onConfirm: (AgentChoiceDisplay.Selection?) -> Void
    let onCancel: () -> Void

    private var task: Components.Schemas.Task? {
        coordinator.tasks.first { $0.task.id == request.taskID }?.task
    }

    /// Why the confirm can't send: the task no longer takes a change (the
    /// rule Task Actions offers Change Agent by), or no change can send.
    var unavailableReason: String? {
        let menu = TaskActionsMenu(
            coordinator: coordinator, taskID: request.taskID, confirmation: .constant(nil), lineChange: .constant(nil))
        guard menu.offersLineChange else { return Self.endedReason }
        return TaskLineChangeMenu.blockedReason(taskID: request.taskID, coordinator: coordinator)
    }

    /// Sends a confirmed change: closes the picker `opened` is held in,
    /// then sends one `change_task_line`. Closing first means a second
    /// confirm while the first is on its way finds the picker closed and
    /// sends nothing. Returns the send, or nil when nothing was sent.
    @discardableResult
    static func send(
        _ selection: AgentChoiceDisplay.Selection?, for opened: TaskLineChangeRequest,
        request: Binding<TaskLineChangeRequest?>, coordinator: SyncCoordinator
    ) -> Task<Void, Never>? {
        guard request.wrappedValue == opened else { return nil }
        request.wrappedValue = nil
        return Task {
            await coordinator.taskLineChange.change(taskID: opened.taskID, role: opened.role, to: selection)
        }
    }

    var body: some View {
        RolePickerView(
            picker: task.map { TaskLineChangeMenu.picker(request.role, task: $0, coordinator: coordinator) }
                ?? .change(role: request.role, current: nil, byName: nil, catalog: coordinator.agentCatalog),
            catalog: coordinator.agentCatalog, policy: coordinator.routingPolicy?.policy,
            pools: coordinator.usagePools,
            liveMode: task.map { TaskLineChangeMenu.picker(request.role, task: $0, coordinator: coordinator).mode },
            unavailableReason: unavailableReason, onConfirm: onConfirm, onCancel: onCancel)
    }
}

extension View {
    /// Presents the picker the task page's Change Agent opened, held in
    /// `request`: a popover anchored on the view this modifies on the Mac,
    /// a sheet on the iPhone. Its confirm is the one place a line change
    /// is sent (`TaskLineChangePicker.send`).
    func taskLineChangePicker(
        _ request: Binding<TaskLineChangeRequest?>, coordinator: SyncCoordinator, dynamicTypeSize: DynamicTypeSize
    ) -> some View {
        rolePickerPresentation(item: request) { opened in
            TaskLineChangePicker(
                request: opened, coordinator: coordinator,
                onConfirm: { TaskLineChangePicker.send($0, for: opened, request: request, coordinator: coordinator) },
                onCancel: { request.wrappedValue = nil }
            )
            .dynamicTypeSize(dynamicTypeSize)
        }
    }
}

/// What a task's line change says on its page, drawn only while there is
/// something to say: the change being sent or awaiting sync, the saved
/// change's Retry while the daemon hasn't answered it, and the daemon's
/// refusal.
struct TaskLineChangeView: View {
    let model: TaskLineChangeModel
    let taskID: String

    private var pending: PendingTaskLineChange? {
        model.pending.flatMap { $0.taskID == taskID ? $0 : nil }
    }

    private var failure: String? {
        model.failure.flatMap { $0.taskID == taskID ? $0.message : nil }
    }

    /// True while there is nothing to say for this task, so the page leaves
    /// the view out.
    var isQuiet: Bool { pending == nil && failure == nil }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let pending {
                if pending.receipt == nil, !model.sending {
                    statement("The daemon didn't answer the change. Retry sends the same request.")
                    Button("Retry Sending Change") { Task { await model.retry() } }
                        .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                } else {
                    statement("Changing the agent. The page updates once the daemon records the change.")
                }
            }
            if let failure {
                statement(failure)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func statement(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}
