import FreesideAPI
import FreesideModel
import SwiftUI

/// The Task Actions menu's Change Agent submenu (#2651): one submenu per
/// role, each the role chips' menu (`RoleSelectionMenu`) with the role's
/// current line chosen and Clear in place of Default. Each pick sends one
/// `change_task_line`; what follows speaks in `TaskLineChangeView`, on the
/// page. The daemon decides whether a role can change, so nothing here
/// predicts that a role runs.
struct TaskLineChangeMenu: View {
    typealias Role = AgentChoiceDisplay.Role

    let coordinator: SyncCoordinator
    let task: Components.Schemas.Task

    private var model: TaskLineChangeModel { coordinator.taskLineChange }

    /// Whether the menu offers the change: while the task is active and no
    /// Stop was requested, since a stopping task starts no new attempt.
    static func offers(_ task: Components.Schemas.Task?) -> Bool {
        task.map { TaskDisplay.isActive($0) && $0.cancellation == nil } ?? false
    }

    /// What `role`'s submenu offers: the synced catalog and usage pools, the
    /// ones New Task's role chips offer, with the role's line chosen.
    static func offer(
        _ role: Role, task: Components.Schemas.Task, coordinator: SyncCoordinator
    ) -> RoleSelectionMenu.Offer {
        let line = task.task_lines.first { $0.role == role }
        return .init(
            role: role, catalog: coordinator.agentCatalog, pools: coordinator.usagePools,
            current: line?.selection?.value1, byName: line?.selection == nil ? line?.agent : nil)
    }

    /// Why the submenu is disabled, or nil while a pick can send. A device
    /// sends one change at a time, so a change waiting on another task
    /// names it: only that task's page offers its Retry.
    var blockedReason: String? {
        if let reason = model.unavailableReason { return reason }
        if model.canChange { return nil }
        let waiting = model.pending.flatMap { pending in
            pending.taskID == task.id ? nil : coordinator.tasks.first { $0.task.id == pending.taskID }
        }
        guard let waiting else { return "An agent change is still waiting for the daemon." }
        return
            "An agent change on \u{201C}\(waiting.task.display_names.task.text)\u{201D} is still waiting for the daemon. "
            + "Retry it from that task's page."
    }

    var body: some View {
        Menu("Change Agent") {
            ForEach(AgentChoiceDisplay.roleOrder, id: \.self) { role in
                Menu(roleTitle(role)) {
                    RoleSelectionMenu(
                        offer: Self.offer(role, task: task, coordinator: coordinator),
                        lineupLabel: AgentChoiceDisplay.clearLabel(
                            role: role, policy: coordinator.routingPolicy?.policy, catalog: coordinator.agentCatalog)
                    ) { selection in
                        Task { await model.change(taskID: task.id, role: role, to: selection) }
                    }
                }
            }
        }
        .disabled(blockedReason != nil)
        // A menu can't show a disabled item's help, so the reason is an
        // item of its own.
        if let blockedReason { Text(blockedReason) }
    }

    /// A role's submenu names the role and what it runs on now: its line,
    /// or, with none, the lineup's settings for it.
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
