import Observation

/// The task page's place, held above the views that draw the page. A leading
/// card coming or going remounts the page, which moves between the card's
/// scroll view and its own (`TaskPageHost`). So the page records its place
/// here and a fresh mount returns to it. It holds one entry, for the task the
/// route holds, and none for a task the route has left.
@MainActor
@Observable
final class TaskPageMemory {
    /// The task the entry belongs to.
    private(set) var taskID: String?
    /// The section or run the reader had under the pinned outline row. Nil
    /// while the header is in view.
    var anchor: TaskPageAnchor?
    /// The card that led the page when it last mounted.
    private(set) var leadingItemID: String?
    /// The outline chip last chosen.
    var chosenSection: TaskPageSection?
    /// The Stop confirmation opened and not yet sent or dismissed. The root
    /// presents it, so the sheet stays up while the page remounts under it.
    var confirmation: TaskStopModel.Confirmation?
    /// Whether the controls' What Happened fold is open.
    var showsStopExplanation = false
    /// How many cards the operator has opened. A page whose card the open
    /// doesn't remount, because that card already leads, moves to it.
    private(set) var cardOpenings = 0

    /// Starts an empty entry when the route's task changes.
    func reset(forRoutedTaskID taskID: String?) {
        guard taskID != self.taskID else { return }
        self.taskID = taskID
        anchor = nil
        leadingItemID = nil
        chosenSection = nil
        confirmation = nil
        showsStopExplanation = false
    }

    /// Drops the reader's place, for a card the operator chose: the page
    /// then opens at its top, where that card leads.
    func forgetPlace() {
        anchor = nil
        chosenSection = nil
    }

    /// Records that the operator opened a card, and drops the place for it.
    func openCard() {
        forgetPlace()
        cardOpenings += 1
    }

    /// Where a page mounting for `taskID` with `leadingItemID`'s card returns
    /// to, and records that card as the page's lead. Nil is the page's top,
    /// and forgets the place.
    ///
    /// The place holds when a card arrives or resolves. It doesn't when the
    /// lead moved off a card that is still open: routing only does that when
    /// the operator picks another card, so the page opens on the one picked.
    /// A Decisions place with no card left has nothing to return to.
    func restore(taskID: String, leadingItemID: String?, openItemIDs: Set<String>) -> TaskPageAnchor? {
        reset(forRoutedTaskID: taskID)
        let previous = self.leadingItemID
        self.leadingItemID = leadingItemID
        let picked = previous.map { $0 != leadingItemID && openItemIDs.contains($0) } ?? false
        let drawn = anchor != .section(.decisions) || leadingItemID != nil
        guard !picked, drawn, let anchor else {
            forgetPlace()
            return nil
        }
        return anchor
    }
}
