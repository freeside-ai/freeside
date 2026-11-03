import Foundation
import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's place: which anchor the reader is at, and what the
/// memory returns a page mounting again for the same task to.
@Suite @MainActor struct TaskPagePlaceTests {
    private let taskID = TaskStageFixtures.taskID

    // MARK: Anchor rule

    @Test func noAnchorWhileTheHeaderIsInView() {
        // Every target sits below the row while the row sits under the header.
        #expect(
            TaskPageAnchor.reading(offsets: [
                .section(.decisions): 16, .section(.task): 240, .section(.timeline): 900,
            ]) == nil)
        #expect(TaskPageAnchor.reading(offsets: [:]) == nil)
    }

    @Test func theAnchorIsTheLastSectionScrolledUnderTheRow() {
        #expect(
            TaskPageAnchor.reading(offsets: [
                .section(.task): -420, .section(.conversation): -30, .section(.timeline): 380,
            ]) == .section(.conversation))
        // A jump lands its target on the row's top, give or take rounding.
        #expect(
            TaskPageAnchor.reading(offsets: [.section(.task): -300, .section(.timeline): 0.5])
                == .section(.timeline))
    }

    @Test func aRunWinsOverTheTimelineOnceItIsUnderTheRow() {
        #expect(
            TaskPageAnchor.reading(offsets: [.section(.timeline): -10, .run("run-1"): 120])
                == .section(.timeline))
        #expect(
            TaskPageAnchor.reading(offsets: [
                .section(.timeline): -600, .run("run-1"): -40, .run("run-2"): 200,
            ]) == .run("run-1"))
    }

    // MARK: Restore rule

    /// The Mac detail column replaces the page with a run, so Back to Task
    /// mounts a fresh page, which returns to the place and chip the memory
    /// holds, with a card leading or not.
    @Test func aPageMountingAgainForTheSameTaskReturnsToItsPlace() {
        let memory = TaskPageMemory()
        memory.reset(forRoutedTaskID: taskID)
        #expect(memory.restore(taskID: taskID, leadsWithCard: false) == nil)
        memory.anchor = .run("run-1")
        memory.chosenSection = .runs

        // The route moves to the run and back: the task stays the same.
        memory.reset(forRoutedTaskID: taskID)
        #expect(memory.restore(taskID: taskID, leadsWithCard: true) == .run("run-1"))
        #expect(memory.restore(taskID: taskID, leadsWithCard: false) == .run("run-1"))
        #expect(memory.chosenSection == .runs)
    }

    @Test func theMemoryResetsWhenTheRoutesTaskChanges() {
        let memory = TaskPageMemory()
        memory.reset(forRoutedTaskID: taskID)
        memory.anchor = .run("run-1")
        memory.chosenSection = .runs

        memory.reset(forRoutedTaskID: taskID)
        #expect(memory.anchor == .run("run-1"))

        memory.reset(forRoutedTaskID: "another-task")
        #expect(memory.taskID == "another-task")
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)

        // A route with no task page drops the entry, so coming back to the
        // task restores nothing.
        memory.anchor = .section(.task)
        memory.reset(forRoutedTaskID: nil)
        #expect(memory.restore(taskID: "another-task", leadsWithCard: false) == nil)
        // A page mounting for another task claims the entry the same way.
        memory.anchor = .section(.task)
        #expect(memory.restore(taskID: taskID, leadsWithCard: false) == nil)
        #expect(memory.taskID == taskID)
    }

    @Test func aDecisionsAnchorWithNoCardLeftRestoresToTheTop() {
        let memory = TaskPageMemory()
        _ = memory.restore(taskID: taskID, leadsWithCard: true)
        memory.anchor = .section(.decisions)
        memory.chosenSection = .decisions
        #expect(memory.restore(taskID: taskID, leadsWithCard: false) == nil)
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)

        // A card leading keeps it, whichever card it is.
        memory.anchor = .section(.decisions)
        #expect(memory.restore(taskID: taskID, leadsWithCard: true) == .section(.decisions))
    }

    /// Every card the operator opens forgets the place and is counted, so
    /// the mounted page moves to it even when it already leads.
    @Test func everyOpenedCardForgetsThePlaceAndIsCounted() {
        let memory = TaskPageMemory()
        memory.reset(forRoutedTaskID: taskID)
        memory.anchor = .section(.timeline)
        memory.chosenSection = .timeline
        memory.openCard()
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)
        #expect(memory.cardOpenings == 1)
        memory.openCard()
        #expect(memory.cardOpenings == 2)
    }

    // MARK: Stop

    @Test func aStopViewGivenBindingsHandsItsConfirmationToTheCaller() throws {
        let (coordinator, _) = try TaskStageFixtures.coordinator(TaskStageFixtures.page(.implementation))
        var held: TaskStopModel.Confirmation?
        var expanded = false
        let view = TaskStopView(
            coordinator: coordinator, taskID: taskID,
            presentedConfirmation: Binding(get: { held }, set: { held = $0 }),
            explanationExpanded: Binding(get: { expanded }, set: { expanded = $0 }))
        #expect(!view.presentsConfirmation)
        view.requestStop()
        let prepared = try #require(held)
        #expect(prepared.entry.taskID == taskID)

        #expect(TaskStopView(coordinator: coordinator, taskID: taskID).presentsConfirmation)
    }
}

#if os(macOS)
    import AppKit

    /// The Mac's task page in a window: scrolled to its Timeline, a card
    /// arrives, resolves, and hands the lead to a sibling, and the page's
    /// scroll view stays the same object throughout, so nothing remounts.
    /// After each change the Timeline's jump target sits on the pinned
    /// outline row's top, so the section starts in view just under the row.
    /// Opening the leading card, by a tap or a link, moves the page to it,
    /// and a reveal request scrolls the page to the card's technical
    /// details.
    ///
    /// The page's geometry is read through its own recorder: SwiftUI builds
    /// no accessibility tree for a test host to find the section by.
    @Suite(.serialized) @MainActor struct TaskPagePlaceHostedTests {
        @Observable final class Lead {
            var itemID: String?
            var reveal: TechnicalDetailsRevealRequest?
        }

        private struct Harness: View {
            let coordinator: SyncCoordinator
            let task: Components.Schemas.TaskSnapshot
            let memory: TaskPageMemory
            let lead: Lead
            let navigation: NavigationModel
            let preferences: DecisionSectionPreferences

            var body: some View {
                TaskPageHost(
                    coordinator: coordinator, snapshot: task, leadingItemID: lead.itemID, memory: memory,
                    onOpenRun: { _ in }, onOpenItem: { _ in },
                    card: { itemID, seat in
                        DecisionDetailView(
                            store: coordinator.store, itemID: itemID,
                            detailsRevealRequest: lead.reveal,
                            onConsumeDetailsRevealRequest: { _ in lead.reveal = nil },
                            loadsAttachments: false, showsValidationProgress: false,
                            sectionPreferences: preferences, seat: seat)
                    }
                )
                .opensLinkedCards(from: navigation, in: memory)
            }
        }

        @Test func theTimelineHoldsInOneScrollViewAsCardsComeAndGo() async throws {
            _ = FreesideFont.registration
            var page = TaskStageFixtures.page(.implementation)
            page.items = []
            let (coordinator, task) = try TaskStageFixtures.coordinator(
                page, conversations: AttentionFixtures.defaultConversations())
            let question = TaskStageFixtures.item(.agent_question, id: "place-question", hoursOld: 0)
            let sibling = TaskStageFixtures.item(.agent_question, id: "place-sibling", hoursOld: 1)
            let memory = TaskPageMemory()
            memory.reset(forRoutedTaskID: task.task.id)
            let lead = Lead()
            let navigation = NavigationModel(launchInputs: .standard())
            // The reveal step opens the card's details, which persist; a
            // suite of its own keeps that out of later tests' cards.
            let suite = "FreesideTaskPagePlace-\(UUID().uuidString)"
            let defaults = try #require(UserDefaults(suiteName: suite))
            defer { defaults.removePersistentDomain(forName: suite) }

            let host = NSHostingView(
                rootView: Harness(
                    coordinator: coordinator, task: task, memory: memory, lead: lead, navigation: navigation,
                    preferences: DecisionSectionPreferences(defaults: defaults)))
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 1_000, height: 360), styleMask: [.titled],
                backing: .buffered, defer: false)
            window.contentView = host
            window.orderFront(nil)
            defer { window.orderOut(nil) }

            /// Lets the main actor run, which the page's moves wait on.
            func settle(_ seconds: Double = 0.5) async throws {
                let end = Date().addingTimeInterval(seconds)
                while Date() < end {
                    try await Task.sleep(for: .milliseconds(10))
                    host.layoutSubtreeIfNeeded()
                }
            }
            /// Waits for the page to move once a change has laid out. A new
            /// card's first layout and validation hold the main actor the
            /// move waits on, so the wait is for the move, not a fixed beat.
            func settle(until moved: () throws -> Bool) async throws {
                let deadline = Date().addingTimeInterval(5)
                while try !moved(), Date() < deadline {
                    try await settle(0.05)
                }
                try await settle(0.3)
            }
            func pageScroll() throws -> NSScrollView {
                try #require(Self.pageScroll(in: host))
            }
            func scroll(by distance: CGFloat) async throws {
                let scroll = try pageScroll()
                scroll.contentView.scroll(to: NSPoint(x: 0, y: scroll.contentView.bounds.minY + distance))
                scroll.reflectScrolledClipView(scroll.contentView)
                try await settle(0.05)
            }
            try await settle()

            // The page's one scroll view and its document, which every change
            // below must leave in place.
            let theScroll = try pageScroll()
            let theDocument = try #require(theScroll.documentView)
            func expectNoRemount(_ step: Comment) throws {
                #expect(try pageScroll() === theScroll, step)
                #expect(theScroll.documentView === theDocument, step)
            }

            /// Scrolls down the way a reader does, until the Timeline is the
            /// section under the pinned row.
            func scrollToTheTimeline() async throws {
                var steps = 0
                while memory.anchor != .section(.timeline), steps < 500 {
                    try await scroll(by: 8)
                    steps += 1
                }
                try #require(memory.anchor == .section(.timeline))
            }
            try await scrollToTheTimeline()
            let readingOffset = theScroll.contentView.bounds.minY
            var offset: CGFloat { theScroll.contentView.bounds.minY }

            /// The Timeline's target is on the row's top: the recorder reads
            /// the Timeline there, and the section above once the page
            /// scrolls back 2 points.
            func expectTimelineAtTheRow(_ step: Comment) async throws {
                #expect(memory.anchor == .section(.timeline), step)
                try await scroll(by: -2)
                #expect(memory.anchor != .section(.timeline), step)
                try await scroll(by: 2)
                #expect(memory.anchor == .section(.timeline), step)
            }

            // The card above the page pushes the Timeline down its document,
            // and the page follows it.
            coordinator.store.replaceAll(with: [question])
            lead.itemID = question.item.id
            try await settle { offset > readingOffset + 8 }
            try expectNoRemount("after the card arrives")
            #expect(offset > readingOffset + 8)
            try await expectTimelineAtTheRow("after the card arrives")

            coordinator.store.replaceAll(with: [])
            lead.itemID = nil
            try await settle { abs(offset - readingOffset) <= 8 }
            try expectNoRemount("after the card resolves")
            // The same document as before the card, at the same place.
            #expect(abs(offset - readingOffset) <= 8)
            try await expectTimelineAtTheRow("after the card resolves")

            // Two cards: the leader resolves and its sibling takes the lead.
            coordinator.store.replaceAll(with: [question, sibling])
            lead.itemID = question.item.id
            try await settle { offset > readingOffset + 8 }
            try expectNoRemount("after two cards arrive")
            try await expectTimelineAtTheRow("after two cards arrive")
            // The sibling's one-line row goes, so the Timeline moves up.
            let twoCardsOffset = offset
            coordinator.store.replaceAll(with: [sibling])
            lead.itemID = sibling.item.id
            try await settle { offset < twoCardsOffset }
            try expectNoRemount("after the lead passes to the sibling")
            try await expectTimelineAtTheRow("after the lead passes to the sibling")

            // A reveal request scrolls the page to the seated card's technical
            // details, which sit in the Decisions section.
            lead.reveal = .init(itemID: sibling.item.id, nonce: UUID())
            try await settle { lead.reveal == nil }
            #expect(lead.reveal == nil)
            #expect(memory.anchor == .section(.decisions))

            // The operator opens the card that leads: routing changes nothing,
            // and the page moves to the card.
            try await scrollToTheTimeline()
            memory.openCard()
            try await settle { memory.anchor == .section(.decisions) }
            #expect(memory.anchor == .section(.decisions))

            // Back at the Timeline, a link to the card still leading moves
            // the page to it the same way, though it changes no route.
            try await scrollToTheTimeline()
            memory.chosenSection = .timeline
            let openings = memory.cardOpenings
            navigation.route(toLinkedItem: sibling.item.id)
            try await settle { memory.anchor == .section(.decisions) }
            #expect(memory.cardOpenings == openings + 1)
            #expect(memory.chosenSection == nil)
            #expect(memory.anchor == .section(.decisions))

            // A lead moving off a card still open, as a sidebar task row
            // routes it, is the operator choosing the other card: the page
            // moves to it though no open was counted.
            coordinator.store.replaceAll(with: [question, sibling])
            try await settle(0.5)
            try await scrollToTheTimeline()
            let countedOpenings = memory.cardOpenings
            lead.itemID = question.item.id
            try await settle { memory.anchor == .section(.decisions) }
            try expectNoRemount("after the lead moves to another open card")
            #expect(memory.anchor == .section(.decisions))
            #expect(memory.cardOpenings == countedOpenings)

            // At Decisions, the last card resolving leaves nothing to return
            // to, so the page goes to its top.
            coordinator.store.replaceAll(with: [])
            lead.itemID = nil
            try await settle { offset == 0 }
            try expectNoRemount("after the last card resolves")
            #expect(offset == 0)
            #expect(memory.anchor == nil)
        }

        /// The page's own scroll view: the tallest document in the window.
        /// The outline row's chips scroll sideways in one of their own.
        private static func pageScroll(in view: NSView) -> NSScrollView? {
            func all(_ view: NSView) -> [NSScrollView] {
                ((view as? NSScrollView).map { [$0] } ?? []) + view.subviews.flatMap(all)
            }
            return all(view).max { ($0.documentView?.frame.height ?? 0) < ($1.documentView?.frame.height ?? 0) }
        }
    }
#endif
