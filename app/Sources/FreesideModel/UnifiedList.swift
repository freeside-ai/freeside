import Foundation
import FreesideAPI

/// What the list selects: a task, or an item with no task to stand under.
package enum UnifiedSelection: Hashable, Sendable {
    case task(String)
    case item(String)
}

/// One row of the unified list.
package struct UnifiedRow: Identifiable {
    package enum Content {
        /// `openItems` is newest first, so its first item leads the row. It is
        /// empty in Working and Finished.
        case task(
            Components.Schemas.TaskSnapshot, openItems: [Components.Schemas.AttentionItemSnapshot])
        /// A project- or system-scoped item, or one naming a task this client
        /// doesn't hold.
        case item(Components.Schemas.AttentionItemSnapshot)
    }

    package let content: Content

    package var id: UnifiedSelection {
        switch content {
        case .task(let snapshot, _): .task(snapshot.task.id)
        case .item(let snapshot): .item(snapshot.item.id)
        }
    }

    /// The open items a Needs you row stands for, newest first.
    package var openItems: [Components.Schemas.AttentionItemSnapshot] {
        switch content {
        case .task(_, let openItems): openItems
        case .item(let snapshot): [snapshot]
        }
    }

    /// What selecting the row opens: a task that needs you opens its newest
    /// open item's card, any other task its timeline, an item its card.
    package var destination: NavigationModel.Destination {
        switch content {
        case .task(let snapshot, let openItems):
            openItems.first.map { .attentionItem($0.item.id) } ?? .task(snapshot.task.id)
        case .item(let snapshot):
            .attentionItem(snapshot.item.id)
        }
    }

    package var taskSnapshot: Components.Schemas.TaskSnapshot? {
        if case .task(let snapshot, _) = content { snapshot } else { nil }
    }

    fileprivate func nextOpenItemID(excluding itemID: String) -> String? {
        let items: [Components.Schemas.AttentionItemSnapshot] =
            switch content {
            case .task(_, let openItems): openItems
            case .item(let snapshot): [snapshot]
            }
        return items.first { $0.item.id != itemID && $0.item.status == .open }?.item.id
    }
}

/// The list both clients draw, as a pure projection of the items and tasks
/// the client already holds, joined by task id. Each task has exactly one
/// row: one with an open item is in Needs you whatever its lifecycle.
package struct UnifiedList {
    /// The project filter's value for scoped items that name no project.
    package static let noProject = ""

    package let needsYou: [UnifiedRow]
    package let working: [UnifiedRow]
    package let finished: [UnifiedRow]
    /// The daemon's known projects, whatever the filter: what the project
    /// filter offers.
    package let knownProjects: [String]
    /// Whether the filter offers `No project`: a held scoped item names none.
    package let offersNoProject: Bool
    private let openItemIDs: Set<String>
    /// The held task of every held item that has one.
    private let taskIDsByItemID: [String: String]
    private let rowIDs: Set<UnifiedSelection>

    /// `open` is newest first (`InboxStore.openSnapshots`), which orders Needs
    /// you; `open` and `closed` are unfiltered, so a filtered-out task keeps
    /// its items from reading as scoped rows.
    package init(
        open: [Components.Schemas.AttentionItemSnapshot],
        closed: [Components.Schemas.AttentionItemSnapshot],
        tasks: [Components.Schemas.TaskSnapshot],
        projectFilter: String?
    ) {
        let tasksByID = Dictionary(tasks.map { ($0.task.id, $0) }, uniquingKeysWith: { first, _ in first })
        let heldTaskID = { (item: Components.Schemas.AttentionItem) in
            Self.taskID(of: item).flatMap { tasksByID[$0] == nil ? nil : $0 }
        }
        let keepsTask = { (task: Components.Schemas.Task) in
            projectFilter == nil || task.project_id == projectFilter
        }

        var itemsByTaskID: [String: [Components.Schemas.AttentionItemSnapshot]] = [:]
        var needsYouOrder: [UnifiedRow.Content] = []
        for snapshot in open {
            if let taskID = heldTaskID(snapshot.item), let task = tasksByID[taskID] {
                if itemsByTaskID[taskID] == nil {
                    needsYouOrder.append(.task(task, openItems: []))
                }
                itemsByTaskID[taskID, default: []].append(snapshot)
            } else {
                needsYouOrder.append(.item(snapshot))
            }
        }
        needsYou = needsYouOrder.compactMap { content in
            switch content {
            case .task(let task, _):
                keepsTask(task.task)
                    ? UnifiedRow(content: .task(task, openItems: itemsByTaskID[task.task.id] ?? [])) : nil
            case .item(let snapshot):
                Self.keeps(snapshot.item, projectFilter: projectFilter) ? UnifiedRow(content: content) : nil
            }
        }

        let quietTasks = TaskDisplay.sortedTasks(tasks).filter {
            itemsByTaskID[$0.task.id] == nil && keepsTask($0.task)
        }
        working = quietTasks.filter { TaskDisplay.isActive($0.task) }
            .map { UnifiedRow(content: .task($0, openItems: [])) }

        let finishedTasks = quietTasks.filter { !TaskDisplay.isActive($0.task) }
            .map { (instant: $0.task.last_activity_at, row: UnifiedRow(content: .task($0, openItems: []))) }
        let closedScoped = closed.filter {
            heldTaskID($0.item) == nil && Self.keeps($0.item, projectFilter: projectFilter)
        }.map {
            (
                instant: $0.item.decided_at ?? $0.item.created_at ?? .distantPast,
                row: UnifiedRow(content: .item($0))
            )
        }
        finished = (finishedTasks + closedScoped).enumerated().sorted { lhs, rhs in
            lhs.element.instant != rhs.element.instant
                ? lhs.element.instant > rhs.element.instant : lhs.offset < rhs.offset
        }.map(\.element.row)

        let items = (open + closed).map(\.item)
        knownProjects = Set(tasks.map(\.task.project_id) + items.map(\.project_id))
            .subtracting([Self.noProject]).sorted()
        offersNoProject = items.contains { $0.project_id == Self.noProject && heldTaskID($0) == nil }
        openItemIDs = Set(open.map(\.item.id))
        taskIDsByItemID = Dictionary(
            items.compactMap { item in heldTaskID(item).map { (item.id, $0) } },
            uniquingKeysWith: { first, _ in first })
        rowIDs = Set((needsYou + working + finished).map(\.id))
    }

    /// The task id an item's subject names, whether or not the client holds
    /// that task.
    private static func taskID(of item: Components.Schemas.AttentionItem) -> String? {
        switch item.subject {
        case .run(let subject), .proposal_batch(let subject): subject.task_id
        case .task(let subject): subject.task_id
        case .project, .system: nil
        }
    }

    /// The filter's one rule for an item: system-scoped items concern every
    /// project, so they survive it, and anything else follows its project.
    package static func keeps(_ item: Components.Schemas.AttentionItem, projectFilter: String?) -> Bool {
        guard let projectFilter else { return true }
        if case .system = item.subject { return true }
        return item.project_id == projectFilter
    }

    /// The row a selection draws on: an item with a held task selects that
    /// task's row, in whichever section it sits.
    package func selection(attentionItemID: String?, taskID: String?) -> UnifiedSelection? {
        guard let attentionItemID else { return taskID.map(UnifiedSelection.task) }
        return taskIDsByItemID[attentionItemID].map(UnifiedSelection.task) ?? .item(attentionItemID)
    }

    package func taskID(ofItem itemID: String) -> String? {
        taskIDsByItemID[itemID]
    }

    /// Every row in list order, collapsed sections included.
    package var rows: [UnifiedRow] {
        needsYou + working + finished
    }

    /// Whether the list draws this row, collapsed sections included.
    package func holds(_ selection: UnifiedSelection) -> Bool {
        rowIDs.contains(selection)
    }

    package func isOpen(itemID: String) -> Bool {
        openItemIDs.contains(itemID)
    }

    /// The first row's newest item, other than `itemID`, still open by its
    /// live status: the captured-status rule keeps a concluded item listed
    /// until the next full rebuild, and that item is no card to open.
    package func nextOpenItemID(excluding itemID: String) -> String? {
        needsYou.lazy.compactMap { $0.nextOpenItemID(excluding: itemID) }.first
    }

    /// The same rule within one task's row.
    package func nextOpenItemID(inTask taskID: String, excluding itemID: String) -> String? {
        needsYou.first { $0.id == .task(taskID) }?.nextOpenItemID(excluding: itemID)
    }
}

extension SyncCoordinator {
    package var unifiedList: UnifiedList {
        UnifiedList(
            open: store.openSnapshots, closed: store.closedSnapshots, tasks: tasks,
            projectFilter: store.projectID)
    }

    /// Whether Next Item and Previous Item have anywhere to go.
    public var hasNeedsYouRows: Bool {
        !unifiedList.needsYou.isEmpty
    }
}
