import Observation
import SwiftUI

/// The task page's place, held above the views that draw the page, for a
/// page that mounts again for the same task. The mounted page keeps the
/// reader's place itself, holding what they see still as content changes
/// above it (`TaskPageView`); a route change is what replaces it. In the Mac detail column a run opened from the page
/// takes its place, so Back to Task mounts a fresh page, which returns here
/// to the reader's section and chip. The memory also counts the cards the
/// operator opens, which the mounted page moves to. It holds one entry, for
/// the task the route holds, and none for a task the route has left.
@MainActor
@Observable
final class TaskPageMemory {
    /// The task the entry belongs to.
    private(set) var taskID: String?
    /// The section or run the reader had under the pinned outline row. Nil
    /// while the header is in view.
    var anchor: TaskPageAnchor?
    /// The outline chip last chosen.
    var chosenSection: TaskPageSection?
    /// How many cards the operator has opened. The mounted page moves to
    /// the card each one opens, even one that already leads, which the
    /// route doesn't change.
    private(set) var cardOpenings = 0

    /// Starts an empty entry when the route's task changes.
    func reset(forRoutedTaskID taskID: String?) {
        guard taskID != self.taskID else { return }
        self.taskID = taskID
        anchor = nil
        chosenSection = nil
    }

    /// Drops the reader's place, for a card the operator chose: a page
    /// mounting for it then opens at its top, where that card leads.
    func forgetPlace() {
        anchor = nil
        chosenSection = nil
    }

    /// Records that the operator opened a card, and drops the place for it.
    func openCard() {
        forgetPlace()
        cardOpenings += 1
    }

    /// Where a page mounting for `taskID` returns to. Nil is the page's
    /// top, and forgets the place: a Decisions place with no card leading
    /// has nothing to return to.
    func restore(taskID: String, leadsWithCard: Bool) -> TaskPageAnchor? {
        reset(forRoutedTaskID: taskID)
        guard let anchor, anchor != .section(.decisions) || leadsWithCard else {
            forgetPlace()
            return nil
        }
        return anchor
    }
}

extension View {
    /// Opens the card each routed link names, as a tap on it does: the page
    /// moves to the card, including one that already leads, which the route
    /// doesn't change. The root attaches it above the page.
    func opensLinkedCards(from navigation: NavigationModel, in memory: TaskPageMemory) -> some View {
        onChange(of: navigation.linkOpenings) { memory.openCard() }
    }
}
