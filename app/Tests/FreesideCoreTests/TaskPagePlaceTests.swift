import Foundation
import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's place across a remount: which anchor the reader is at,
/// what the memory returns a fresh mount to, and where the Stop
/// confirmation is held.
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

    @Test func theMemoryRestoresForTheSameTaskAcrossACardComingAndGoing() throws {
        let memory = TaskPageMemory()
        memory.reset(forRoutedTaskID: taskID)
        #expect(memory.restore(taskID: taskID, leadingItemID: nil, openItemIDs: []) == nil)
        memory.anchor = .section(.timeline)
        memory.chosenSection = .timeline
        let confirmation = try prepared()
        memory.confirmation = confirmation
        memory.showsStopExplanation = true

        // A card arrives.
        #expect(memory.restore(taskID: taskID, leadingItemID: "item", openItemIDs: ["item"]) == .section(.timeline))
        // It resolves.
        #expect(memory.restore(taskID: taskID, leadingItemID: nil, openItemIDs: []) == .section(.timeline))
        #expect(memory.chosenSection == .timeline)
        #expect(memory.confirmation?.id == confirmation.id)
        #expect(memory.showsStopExplanation)
    }

    @Test func theMemoryResetsWhenTheRoutesTaskChanges() throws {
        let memory = TaskPageMemory()
        memory.reset(forRoutedTaskID: taskID)
        memory.anchor = .run("run-1")
        memory.chosenSection = .runs
        memory.confirmation = try prepared()
        memory.showsStopExplanation = true

        memory.reset(forRoutedTaskID: taskID)
        #expect(memory.anchor == .run("run-1"))

        memory.reset(forRoutedTaskID: "another-task")
        #expect(memory.taskID == "another-task")
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)
        #expect(memory.confirmation == nil)
        #expect(!memory.showsStopExplanation)

        // A route with no task page drops the entry, so coming back to the
        // task restores nothing.
        memory.anchor = .section(.task)
        memory.reset(forRoutedTaskID: nil)
        #expect(memory.restore(taskID: "another-task", leadingItemID: nil, openItemIDs: []) == nil)
        // A page mounting for another task claims the entry the same way.
        memory.anchor = .section(.task)
        #expect(memory.restore(taskID: taskID, leadingItemID: nil, openItemIDs: []) == nil)
        #expect(memory.taskID == taskID)
    }

    @Test func aDecisionsAnchorWithNoCardLeftRestoresToTheTop() {
        let memory = TaskPageMemory()
        _ = memory.restore(taskID: taskID, leadingItemID: "item", openItemIDs: ["item"])
        memory.anchor = .section(.decisions)
        memory.chosenSection = .decisions
        #expect(memory.restore(taskID: taskID, leadingItemID: nil, openItemIDs: []) == nil)
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)

        // A sibling taking the resolved card's place keeps it.
        _ = memory.restore(taskID: taskID, leadingItemID: "item", openItemIDs: ["item", "sibling"])
        memory.anchor = .section(.decisions)
        #expect(
            memory.restore(taskID: taskID, leadingItemID: "sibling", openItemIDs: ["sibling"])
                == .section(.decisions))
    }

    /// Routing moves the lead off a card still open only when the operator
    /// picks another, so the page opens on the card picked.
    @Test func aCardPickedOverAnOpenOneOpensAtTheTop() throws {
        let memory = TaskPageMemory()
        _ = memory.restore(taskID: taskID, leadingItemID: "item", openItemIDs: ["item", "other"])
        memory.anchor = .section(.timeline)
        let confirmation = try prepared()
        memory.confirmation = confirmation
        #expect(memory.restore(taskID: taskID, leadingItemID: "other", openItemIDs: ["item", "other"]) == nil)
        #expect(memory.anchor == nil)
        #expect(memory.confirmation?.id == confirmation.id)

        // Every card the operator opens forgets the place and is counted, so
        // a page whose card already leads still moves to it.
        memory.anchor = .section(.timeline)
        memory.chosenSection = .timeline
        memory.openCard()
        #expect(memory.anchor == nil)
        #expect(memory.chosenSection == nil)
        #expect(memory.cardOpenings == 1)
        #expect(memory.confirmation?.id == confirmation.id)
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

    private func prepared() throws -> TaskStopModel.Confirmation {
        let (coordinator, _) = try TaskStageFixtures.coordinator(TaskStageFixtures.page(.implementation))
        return try #require(coordinator.taskStop.prepare(taskID: taskID))
    }
}

#if os(macOS)
    import AppKit

    /// The Mac's task page in a window: scrolled to its Timeline, a card
    /// arrives and then resolves, and after each the Timeline's jump target
    /// sits on the pinned outline row's top, so the section starts in view
    /// just under the row, while a Stop confirmation stays up. Opening the
    /// leading card, by a tap or a link, then moves the page to it.
    ///
    /// The page's geometry is read through its own recorder: SwiftUI builds
    /// no accessibility tree for a test host to find the section by.
    @Suite(.serialized) @MainActor struct TaskPagePlaceHostedTests {
        @Observable final class Lead {
            var itemID: String?
        }

        private struct Harness: View {
            let coordinator: SyncCoordinator
            let task: Components.Schemas.TaskSnapshot
            let memory: TaskPageMemory
            let lead: Lead
            let navigation: NavigationModel

            var body: some View {
                TaskPageHost(
                    coordinator: coordinator, snapshot: task, leadingItemID: lead.itemID, memory: memory,
                    onOpenRun: { _ in }, onOpenItem: { _ in },
                    card: { itemID, page in
                        DecisionDetailView(
                            store: coordinator.store, itemID: itemID, loadsAttachments: false,
                            showsValidationProgress: false, page: page)
                    }
                )
                .taskStopConfirmation(memory, model: coordinator.taskStop, dynamicTypeSize: .large)
                .opensLinkedCards(from: navigation, in: memory)
            }
        }

        @Test func theTimelineAndAnOpenStopOutlastACardComingAndGoing() async throws {
            _ = FreesideFont.registration
            var page = TaskStageFixtures.page(.implementation)
            page.items = []
            let (coordinator, task) = try TaskStageFixtures.coordinator(
                page, conversations: AttentionFixtures.defaultConversations())
            let question = TaskStageFixtures.item(.agent_question, id: "place-question", hoursOld: 0)
            let memory = TaskPageMemory()
            memory.reset(forRoutedTaskID: task.task.id)
            let lead = Lead()
            let navigation = NavigationModel(launchInputs: .standard())

            let host = NSHostingView(
                rootView: Harness(
                    coordinator: coordinator, task: task, memory: memory, lead: lead, navigation: navigation))
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 1_000, height: 360), styleMask: [.titled],
                backing: .buffered, defer: false)
            window.contentView = host
            window.orderFront(nil)
            defer { window.orderOut(nil) }

            /// Lets the main actor run, which the page's restore waits on.
            func settle(_ seconds: Double = 0.5) async throws {
                let end = Date().addingTimeInterval(seconds)
                while Date() < end {
                    try await Task.sleep(for: .milliseconds(10))
                    host.layoutSubtreeIfNeeded()
                }
            }
            /// The page's scroll view, found afresh: a remount replaces it.
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
            let readingOffset = try pageScroll().contentView.bounds.minY

            memory.confirmation = try #require(coordinator.taskStop.prepare(taskID: task.task.id))
            try await settle()
            #expect(window.attachedSheet != nil)

            /// The Timeline's target is on the row's top: the recorder reads
            /// the Timeline there, and the section above once the page
            /// scrolls back 2 points.
            func expectTimelineAtTheRow(_ step: Comment) async throws {
                #expect(memory.anchor == .section(.timeline), step)
                try await scroll(by: -2)
                #expect(memory.anchor != .section(.timeline), step)
                try await scroll(by: 2)
                #expect(memory.anchor == .section(.timeline), step)
                #expect(memory.confirmation != nil, step)
                #expect(window.attachedSheet != nil, step)
            }

            /// Waits for the remounted page, in a scroll view of its own, to
            /// scroll back to its place.
            func settleOnceRemounted(from previous: NSScrollView) async throws {
                let deadline = Date().addingTimeInterval(5)
                while Date() < deadline {
                    let scroll = try pageScroll()
                    if scroll !== previous, scroll.contentView.bounds.minY > 0 { break }
                    try await settle(0.05)
                }
                try await settle(0.3)
            }

            var previous = try pageScroll()
            coordinator.store.replaceAll(with: [question])
            lead.itemID = question.item.id
            try await settleOnceRemounted(from: previous)
            // The card above the page pushes the Timeline down its document.
            #expect(try pageScroll().contentView.bounds.minY > readingOffset)
            try await expectTimelineAtTheRow("after the card arrives")

            previous = try pageScroll()
            coordinator.store.replaceAll(with: [])
            lead.itemID = nil
            try await settleOnceRemounted(from: previous)
            // The same document as before the card, at the same place.
            #expect(abs(try pageScroll().contentView.bounds.minY - readingOffset) <= 8)
            try await expectTimelineAtTheRow("after the card resolves")

            // The card arrives again, and the operator opens it: routing
            // changes nothing, and the page moves to the card.
            previous = try pageScroll()
            coordinator.store.replaceAll(with: [question])
            lead.itemID = question.item.id
            try await settleOnceRemounted(from: previous)
            #expect(memory.anchor == .section(.timeline))
            memory.openCard()
            try await settle()
            #expect(memory.anchor == .section(.decisions))

            // Back at the Timeline, a link to the card still leading moves
            // the page to it the same way, though it changes no route.
            try await scrollToTheTimeline()
            memory.chosenSection = .timeline
            let openings = memory.cardOpenings
            navigation.route(toLinkedItem: question.item.id)
            try await settle()
            #expect(memory.cardOpenings == openings + 1)
            #expect(memory.chosenSection == nil)
            #expect(memory.anchor == .section(.decisions))
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
