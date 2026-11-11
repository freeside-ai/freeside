import FreesideAPI
import SwiftUI

/// What a task's Stop states say, shared by Mac and iPhone: the synced
/// cancellation, a request in flight or unanswered, and the acts that
/// recover one. Synced cancellation always outranks a receipt. Stop itself
/// starts from `TaskActionsMenu`, so nothing here starts one.
struct TaskStopView: View {
    let coordinator: SyncCoordinator
    let taskID: String
    /// The What Happened fold, held by the caller when given.
    var explanationExpanded: Binding<Bool>? = nil
    @State private var showsExplanation = false

    private var model: TaskStopModel { coordinator.taskStop }
    private var snapshot: Components.Schemas.TaskSnapshot? {
        coordinator.tasks.first { $0.task.id == taskID }
    }

    /// True while there is no Stop state to state: no cancellation, nothing
    /// in flight, and no reason or message. The task page leaves the view
    /// out then, so a quiet task keeps no gap for it.
    var isQuiet: Bool {
        snapshot?.task.cancellation == nil && !model.sending.contains(taskID)
            && !model.confirming.contains(taskID)
            && model.pending(for: taskID) == nil && model.unavailableReason == nil
            && model.messages[taskID] == nil
    }

    /// True when nothing vouches for the synced cancellation on screen: sync
    /// is failing, or the row came from the cache and no round has succeeded
    /// in this session.
    var cancellationMayBeOutdated: Bool {
        let freshness = coordinator.store.freshness
        return freshness.isFailing || (freshness == .unvalidated && coordinator.lastUpdatedAt == nil)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let cancellation = snapshot?.task.cancellation?.value1 {
                Self.cancellationNotice(cancellation.state)
                if cancellationMayBeOutdated {
                    note("Last synced status. Refresh to check current task state.")
                }
            }
            if model.sending.contains(taskID) {
                note("Sending Stop…")
            } else if model.confirming.contains(taskID) {
                // The confirmed Stop is validating current state: no Stop
                // in the menu, so a second one cannot start beside it.
                note("Checking current task state…")
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
            }
            if let reason = model.unavailableReason { note(reason) }
            if let message = model.messages[taskID] { note(message, color: .ink) }
            if let explanation {
                SentenceDisclosure(label: "What Happened", isExpanded: explanationExpanded ?? $showsExplanation) {
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
            case .failed_to_stop: (.wax, "Unconfirmed")
            case .confirmed: (.neutral, "Recorded")
            }
        return Notice(tone: tone, keyword: keyword, sentence: cancellationText(state))
            .accessibilityElement(children: .combine)
    }

    static func cancellationText(_ state: Components.Schemas.TaskCancellationState) -> String {
        switch state {
        case .requested: "Stop sent; waiting for the daemon to confirm."
        case .failed_to_stop:
            "Freeside starts no new work for this task but hasn't confirmed that work already running has ended."
        case .confirmed: "Stop confirmed by the daemon. Existing history and PRs remain available."
        }
    }
}

/// The task's one menu, `Task Actions ▾` (◆B, 4 Nov handoff): Change Agent
/// while the task is active (#2651), the page's own items, then Stop Task…
/// under a divider. The page pins it in the outline
/// row, or the iPhone's navigation bar, so Stop stays in reach while the page
/// scrolls; the Stop states it leads to speak in `TaskStopView`, in the body.
struct TaskActionsMenu: View {
    /// A page's own item, drawn above Stop.
    struct Action {
        let title: String
        let perform: () -> Void
    }

    let coordinator: SyncCoordinator
    let taskID: String
    /// Offered in every Stop state: a page's own item needs no daemon, so
    /// the menu holding one stays, and enabled, while Stop is unavailable,
    /// in flight, or done.
    var actions: [Action] = []
    /// Where Stop Task… puts its prepared confirmation. The page presents it
    /// outside its lazy stack, which can drop the row this menu sits in.
    let confirmation: Binding<TaskStopModel.Confirmation?>
    /// Where Change Agent puts the role whose picker it opens, which the
    /// page presents for the same reason.
    let lineChange: Binding<TaskLineChangeRequest?>
    /// False draws the label alone, for a renderer that can't draw a menu.
    var rendersInteractiveControls = true

    private var model: TaskStopModel { coordinator.taskStop }
    private var snapshot: Components.Schemas.TaskSnapshot? {
        coordinator.tasks.first { $0.task.id == taskID }
    }

    /// Whether the menu offers Stop: no Stop is in flight and the task
    /// carries no cancellation. Every other Stop state speaks in the body.
    var offersStop: Bool {
        snapshot?.task.cancellation == nil && !model.sending.contains(taskID)
            && !model.confirming.contains(taskID) && model.pending(for: taskID) == nil
    }

    var stopIsUnavailable: Bool { model.unavailableReason != nil || snapshot == nil }

    /// Whether the menu offers Change Agent: while the task is active and
    /// Stop is on offer, since a task being stopped starts no new attempt.
    var offersLineChange: Bool { offersStop && TaskLineChangeMenu.offers(snapshot?.task) }

    /// Whether there is anything to hold: Change Agent, the page's own
    /// items, or Stop.
    var drawsMenu: Bool { offersStop || !actions.isEmpty || offersLineChange }

    /// Disabled, never hidden, while it would hold an unavailable Stop
    /// alone. Change Agent keeps it enabled even when it can't send, so its
    /// reason, an item in the menu, can be read.
    var isDisabled: Bool { actions.isEmpty && stopIsUnavailable && !offersLineChange }

    /// Stop Task…: prepares the confirmation where the page presents it.
    func requestStop() {
        confirmation.wrappedValue = model.prepare(taskID: taskID)
    }

    /// Stop is the page's one consequential action, so it sits behind this
    /// menu, never as a standing button beside the task's name (5.6). Only
    /// the offer folds away: every Stop state stays on the page. While a
    /// Stop is in flight or the task is cancelled the menu holds the page's
    /// own items alone, so a second Stop cannot start beside the first.
    @ViewBuilder var body: some View {
        if drawsMenu {
            if rendersInteractiveControls {
                Menu {
                    if offersLineChange, let task = snapshot?.task {
                        TaskLineChangeMenu(coordinator: coordinator, task: task, request: lineChange)
                        if !actions.isEmpty { Divider() }
                    }
                    ForEach(Array(actions.enumerated()), id: \.offset) { _, action in
                        Button(action.title, action: action.perform)
                    }
                    if offersStop {
                        if !actions.isEmpty || offersLineChange { Divider() }
                        Button("Stop Task…", role: .destructive, action: requestStop)
                            .disabled(stopIsUnavailable)
                    }
                } label: {
                    label
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .menuIndicator(.hidden)
                .fixedSize()
                .freesideFocusRing(cornerRadius: 4)
                .disabled(isDisabled)
                // The window's selection default would make the label a
                // selectable text view, and that view then answers for the
                // menu button's spoken label: the construct the ready
                // card's accessibility recursion ran through (#2993). A
                // control's label is not selectable (app/README.md,
                // Copyable Text).
                .textSelection(.disabled)
                .accessibilityLabel("Task actions")
                .accessibilityHint(hint)
            } else {
                label
            }
        }
    }

    /// Dim at keyword weight, as the canvas's outline draws it (`.tmenu`).
    private var label: some View {
        Text("Task Actions \u{25BE}")
            .font(FreesideFont.noticeAction)
            .foregroundStyle(isDisabled ? Color.inkFaint : Color.inkDim)
            .padding(.horizontal, 8)
            .padding(.vertical, 5)
            .contentShape(Rectangle())
    }

    private var hint: String {
        let items =
            (offersLineChange ? ["Change Agent, one role at a time"] : []) + actions.map(\.title)
            + (offersStop ? ["Stop Task, which reviews what stopping this task will do"] : [])
        return "Holds \(items.formatted(.list(type: .and)))"
    }
}

extension View {
    /// Presents the task page's Stop confirmation held in `confirmation`.
    /// The page attaches it outside its lazy stack, which can drop the Stop
    /// controls while a card leaving shrinks the document under the reader.
    func taskStopConfirmation(
        _ confirmation: Binding<TaskStopModel.Confirmation?>, model: TaskStopModel,
        dynamicTypeSize: DynamicTypeSize
    ) -> some View {
        sheet(item: confirmation) { prepared in
            TaskStopConfirmationView(entry: prepared.entry) {
                confirmation.wrappedValue = nil
                Task { await model.confirm(prepared) }
            }
            .dynamicTypeSize(dynamicTypeSize)
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
            FactGroup(spacing: 11) {
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
                        FactGroup(spacing: 11) {
                            FactRow(label: "Task", value: entry.taskName)
                            FactRow(label: "Project", value: entry.projectName)
                        }
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
