import Observation
import SwiftUI

/// The task page's place, held above the views that draw the page, for a
/// page that mounts again for the same task. The mounted page keeps the
/// reader's place itself, holding what they see still as content changes
/// above it (`TaskPageView`); a route change is what replaces it. In the
/// Mac detail column a run or an earlier card opened from the page takes
/// its place, so Back to Task mounts a fresh page, which returns here to
/// the reader's section. The memory also counts the cards the
/// operator opens, which the mounted page moves to. It holds one entry, for
/// the task the route holds, and none for a task the route has left.
@MainActor
@Observable
final class TaskPageMemory {
    /// The task the entry belongs to.
    private(set) var taskID: String?
    /// The section or run the reader had at the view's top. Nil while
    /// the header is in view.
    var anchor: TaskPageAnchor?
    /// How many cards the operator has opened. The mounted page moves to
    /// the card each one opens, even one that already leads, which the
    /// route doesn't change.
    private(set) var cardOpenings = 0
    /// The card the last opening named, when only the page leading it
    /// moves: an iPhone page left under an earlier card keeps its place.
    /// Nil moves whichever page is mounted.
    private(set) var openedCardLead: String?
    /// The page's place when an earlier card opened over it, which Back to
    /// Task returns to; the layer's own page records into `anchor`.
    private var returnAnchor: TaskPageAnchor?

    /// Starts an empty entry when the route's task changes.
    func reset(forRoutedTaskID taskID: String?) {
        guard taskID != self.taskID else { return }
        self.taskID = taskID
        anchor = nil
        returnAnchor = nil
    }

    /// Drops the reader's place, for a card the operator chose: a page
    /// mounting for it then opens at its top, where that card leads.
    func forgetPlace() {
        anchor = nil
    }

    /// Records that the operator opened a card, and drops the place for it.
    /// `leading` names a card a page already leads, so only that page moves.
    func openCard(leading itemID: String? = nil) {
        forgetPlace()
        openedCardLead = itemID
        cardOpenings += 1
    }

    /// Leaves the page for an earlier card opened over it from the
    /// Timeline, which a page mounting at its top leads. `overPage` keeps
    /// the reader's place for Back to Task; it is false when another
    /// earlier card already covers the page, whose place stays the one kept.
    /// Not counted as an opened card: the card's page is a fresh mount, and
    /// an iPhone page left under the layer must not move to its own card.
    func openEarlierCard(overPage: Bool) {
        if overPage { returnAnchor = anchor }
        forgetPlace()
    }

    /// Back to Task from an earlier card: the page mounting under it
    /// returns to the place it held.
    func returnFromEarlierCard() {
        anchor = returnAnchor
        returnAnchor = nil
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

/// The route moves the task page makes over an earlier card (#3454), with
/// the place each keeps, shared by the root and its tests.
extension TaskPageMemory {
    /// Opens a Timeline row's item over the task page, so the page stays
    /// the one to return to. A card the route already shows is moved to
    /// instead, as a tap on it does, with no layer that would lead back to
    /// itself: the top page's lead, or the lead of the page under a layer,
    /// which closes the layer.
    func openTimelineItem(_ itemID: String, in navigation: NavigationModel) {
        let item = NavigationModel.Destination.attentionItem(itemID)
        if navigation.path.last == item {
            openCard(leading: itemID)
        } else if navigation.earlierCardSelection != nil, navigation.path.first == item {
            returnFromEarlierCard()
            openCard(leading: itemID)
            navigation.closeEarlierCard()
        } else {
            openEarlierCard(overPage: navigation.earlierCardSelection == nil)
            navigation.openEarlierCard(itemID: itemID)
        }
    }

    /// Back to Task over an earlier card, on the Mac: the page returns to
    /// the place it held.
    func closeEarlierCard(in navigation: NavigationModel) {
        returnFromEarlierCard()
        navigation.closeEarlierCard()
    }

    /// Opens a run from the task page. A run replaces an earlier card, so
    /// Back to Task from it returns to the page's place, not the layer's.
    func openRun(_ runID: String, taskID: String, in navigation: NavigationModel) {
        if navigation.earlierCardSelection != nil { returnFromEarlierCard() }
        navigation.openRun(taskID: taskID, runID: runID)
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
