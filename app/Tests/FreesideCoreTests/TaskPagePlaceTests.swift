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

    /// Stop Task… hands its confirmation to the page, which presents it
    /// outside the lazy stack that can drop the row the menu sits in.
    @Test func taskActionsHandItsStopConfirmationToThePage() throws {
        let (coordinator, _) = try TaskStageFixtures.coordinator(TaskStageFixtures.page(.implementation))
        var held: TaskStopModel.Confirmation?
        let menu = TaskActionsMenu(
            coordinator: coordinator, taskID: taskID, confirmation: Binding(get: { held }, set: { held = $0 }))
        #expect(menu.offersStop)
        #expect(!menu.isDisabled)
        menu.requestStop()
        let prepared = try #require(held)
        #expect(prepared.entry.taskID == taskID)
        // A quiet task's body draws no Stop view, so it leaves no gap.
        #expect(TaskStopView(coordinator: coordinator, taskID: taskID).isQuiet)
    }
}

#if os(macOS)
    import AppKit
    import Synchronization

    /// The Mac's task page in a window, refreshed the way sync refreshes it:
    /// the mock daemon's state changes, the coordinator bootstraps, the page
    /// refetches its history, and the route repairs through
    /// `NavigationModel.repairSelection`, as the list does. Content that
    /// changes above the reader leaves what they see within 1 pt (#1460),
    /// and the page's one scroll view stays the same object throughout.
    ///
    /// The page's geometry is read through `TaskPagePlaceProbe`: SwiftUI
    /// builds no accessibility tree for a test host to find it by.
    @Suite(.serialized, .needsWindowServerSession) @MainActor struct TaskPagePlaceHostedTests {
        /// What the mock daemon serves beyond its seed, which its bootstrap
        /// transform reads.
        final class Served: Sendable {
            struct State: Sendable {
                var items: [Components.Schemas.AttentionItemSnapshot] = []
                var conversations: [Components.Schemas.ConversationSnapshot] = []
                var held = false

                /// Closes an item as the daemon does: it stays in the
                /// snapshot, resolved.
                mutating func close(_ itemID: String) {
                    items = items.map { snapshot in
                        guard snapshot.item.id == itemID else { return snapshot }
                        return AttentionFixtures.closedByDaemon(
                            snapshot, id: itemID, cause: .resolved_by_daemon, after: 60, reason: "Resolved.")
                    }
                }
            }
            let state = Mutex(State())
        }

        private struct Harness: View {
            let coordinator: SyncCoordinator
            let task: Components.Schemas.TaskSnapshot
            let memory: TaskPageMemory
            let navigation: NavigationModel
            let preferences: DecisionSectionPreferences
            let disclosures: TaskTimelineDisclosurePreferences
            let probe: TaskPagePlaceProbe
            let reveal: Reveal
            let validates: Bool

            var body: some View {
                let list = coordinator.unifiedList
                let lead: String? =
                    if case .taskPage(_, let itemID)? = navigation.detailSurface(in: list) { itemID } else { nil }
                TaskPageHost(
                    coordinator: coordinator,
                    snapshot: coordinator.tasks.first { $0.task.id == task.task.id } ?? task,
                    leadingItemID: lead, memory: memory,
                    onOpenRun: { _ in }, onOpenItem: { Page.openItem($0, memory: memory, navigation: navigation) },
                    disclosures: disclosures, probe: probe,
                    card: { itemID, seat in
                        DecisionDetailView(
                            store: coordinator.store, itemID: itemID,
                            detailsRevealRequest: reveal.request,
                            onConsumeDetailsRevealRequest: { _ in reveal.request = nil },
                            loadsAttachments: false, showsValidationProgress: validates,
                            sectionPreferences: preferences, seat: seat)
                    }
                )
                .opensLinkedCards(from: navigation, in: memory)
            }
        }

        @Observable final class Reveal {
            var request: TechnicalDetailsRevealRequest?
        }

        /// One mounted page and the daemon behind it.
        @MainActor final class Page {
            let server: MockServer
            let served: Served
            let coordinator: SyncCoordinator
            let task: Components.Schemas.TaskSnapshot
            var history: Components.Schemas.TaskTimeline
            let memory = TaskPageMemory()
            let navigation = NavigationModel(launchInputs: .standard())
            let probe = TaskPagePlaceProbe()
            let reveal = Reveal()
            let disclosures: TaskTimelineDisclosurePreferences
            private let defaults: UserDefaults
            private let suite: String
            let host: NSHostingView<AnyView>
            let window: NSWindow
            /// Every offset seen while settling.
            var samples: [CGFloat] = []
            /// The hold point whose top in the view `trackedTops` samples
            /// while settling.
            var tracked: String?
            var trackedTops: [CGFloat] = []

            /// The task's spec approval, closed, which gives the page a
            /// conversation.
            static let approval: Components.Schemas.AttentionItemSnapshot = {
                var snapshot = AttentionFixtures.closedByDaemon(
                    TaskStageFixtures.item(.spec_approval, id: "place-approval", hoursOld: 3), id: "place-approval",
                    cause: .expired, after: 60, reason: "Expired.")
                snapshot.item.conversation_id = "conv-item-spec_approval"
                return snapshot
            }()
            static let question = TaskStageFixtures.item(.agent_question, id: "place-question", hoursOld: 0)
            static let sibling = TaskStageFixtures.item(.agent_question, id: "place-sibling", hoursOld: 1)
            /// A question on the approval's thread, which its card draws.
            static let discussed: Components.Schemas.AttentionItemSnapshot = {
                var snapshot = TaskStageFixtures.item(.agent_question, id: "place-discussed", hoursOld: 0)
                snapshot.item.conversation_id = approval.item.conversation_id
                return snapshot
            }()

            static func openItem(_ itemID: String, memory: TaskPageMemory, navigation: NavigationModel) {
                memory.openCard()
                navigation.route(to: .attentionItem(itemID))
            }

            /// A page whose card `validates` draws its validation row while
            /// it checks its item, as the app's does.
            init(height: CGFloat = 360, validates: Bool = false) async throws {
                _ = FreesideFont.registration
                let history = TaskStageFixtures.history(.secondCampaign)
                let page = TaskStageFixtures.page(history)
                var task = TaskFixtures.approvedCampaign().task
                task.task = page.task
                self.task = task
                self.history = history
                let served = Served()
                served.state.withLock {
                    $0.items = [Self.approval]
                    $0.conversations = AttentionFixtures.defaultConversations()
                }
                self.served = served
                // The stage fixture renames its run and leaves the stages
                // bound to the old id, which the mock daemon refuses.
                let runs = page.runs.map { run in
                    var run = run
                    run.run.stages = run.run.stages.map { stage in
                        var stage = stage
                        stage.run_id = run.run.id
                        return stage
                    }
                    return run
                }
                // The bootstrap serves whichever items a test chooses; the
                // daemon also holds each so a card's own read finds it.
                server = MockServer(
                    items: [Self.approval, Self.question, Self.sibling, Self.discussed], conversations: [], runs: runs,
                    tasks: [task],
                    schedules: [], timelines: [],
                    taskTimelines: [history])
                await server.setBootstrapTransform { snapshot in
                    var snapshot = snapshot
                    let state = served.state.withLock { $0 }
                    snapshot.attention_items = state.items
                    snapshot.conversations = state.conversations
                    if state.held {
                        snapshot.runs = snapshot.runs.map { run in
                            var run = run
                            run.run.hold_reason = .init(value1: .input_unavailable)
                            return run
                        }
                    }
                    return snapshot
                }
                coordinator = SyncCoordinator(
                    client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
                await coordinator.bootstrap()
                suite = "FreesideTaskPagePlace-\(UUID().uuidString)"
                defaults = try #require(UserDefaults(suiteName: suite))
                disclosures = TaskTimelineDisclosurePreferences(defaults: defaults)
                memory.reset(forRoutedTaskID: task.task.id)
                navigation.route(to: .task(task.task.id))
                host = NSHostingView(
                    rootView: AnyView(
                        Harness(
                            coordinator: coordinator, task: task, memory: memory, navigation: navigation,
                            preferences: DecisionSectionPreferences(defaults: defaults), disclosures: disclosures,
                            probe: probe, reveal: reveal, validates: validates)))
                window = NSWindow(
                    contentRect: CGRect(x: 0, y: 0, width: 1_000, height: height), styleMask: [.titled],
                    backing: .buffered, defer: false)
                window.contentView = host
                window.orderFront(nil)
                try await settle(until: { self.coordinator.taskTimelineLoadStates[task.task.id] == .loaded })
            }

            func close() {
                window.orderOut(nil)
                defaults.removePersistentDomain(forName: suite)
            }

            var scrollView: NSScrollView {
                get throws { try #require(Self.pageScroll(in: host)) }
            }
            var offset: CGFloat {
                (try? scrollView.contentView.bounds.minY) ?? .nan
            }
            var maxOffset: CGFloat {
                guard let scroll = try? scrollView, let document = scroll.documentView else { return .nan }
                return max(0, document.frame.height - scroll.contentView.bounds.height)
            }
            var lead: String? {
                if case .taskPage(_, let itemID)? = navigation.detailSurface(in: coordinator.unifiedList) {
                    itemID
                } else {
                    nil
                }
            }

            /// Lets the main actor run, which the page's moves wait on, and
            /// samples the offset as it goes.
            func settle(_ seconds: Double = 0.5) async throws {
                let end = Date().addingTimeInterval(seconds)
                while Date() < end {
                    try await Task.sleep(for: .milliseconds(10))
                    host.layoutSubtreeIfNeeded()
                    samples.append(offset)
                    if let tracked, let top = probe.viewTops[tracked] { trackedTops.append(top) }
                }
            }

            func settle(until done: () throws -> Bool) async throws {
                let deadline = Date().addingTimeInterval(5)
                while try !done(), Date() < deadline {
                    try await settle(0.05)
                }
                try await settle(0.3)
            }

            func scroll(by distance: CGFloat) async throws {
                let scroll = try scrollView
                scroll.contentView.scroll(to: NSPoint(x: 0, y: scroll.contentView.bounds.minY + distance))
                scroll.reflectScrolledClipView(scroll.contentView)
                try await settle(0.05)
            }

            /// Scrolls the way a reader does until a row of the Timeline's
            /// rail is under the outline row, then `further` into the rail.
            func scrollIntoTheRail(further: CGFloat = 40) async throws {
                var steps = 0
                while probe.reading.map({ !$0.key.contains("|") }) ?? true, steps < 600 {
                    try await scroll(by: 8)
                    steps += 1
                }
                try #require(probe.reading?.key.contains("|") == true)
                try await scroll(by: further)
                try await settle(0.2)
            }

            /// Changes what the daemon serves, then refreshes as sync does:
            /// a bootstrap, the page's refetch, and the route's repair.
            func refresh(
                _ serve: (inout Served.State) -> Void = { _ in },
                history change: ((inout Components.Schemas.TaskTimeline) -> Void)? = nil
            ) async throws {
                if let change {
                    change(&history)
                    await server.setTaskTimeline(history)
                }
                let previouslyOpen = Set(coordinator.store.openSnapshots.map(\.item.id))
                served.state.withLock { serve(&$0) }
                let generation = coordinator.cacheGeneration
                await coordinator.bootstrap()
                navigation.repairSelection(in: coordinator.unifiedList, previouslyOpen: previouslyOpen)
                let taskID = task.task.id
                try await settle(until: {
                    self.coordinator.cacheGeneration != generation
                        && self.coordinator.taskTimelineLoadStates[taskID] == .loaded
                })
                try #require(coordinator.store.freshness == .fresh, "the mock daemon refused the refresh")
                try #require(coordinator.cacheGeneration != generation)
            }

            /// The page's own scroll view: the tallest document in the
            /// window. The outline row's chips scroll sideways in one of
            /// their own.
            static func pageScroll(in view: NSView) -> NSScrollView? {
                func all(_ view: NSView) -> [NSScrollView] {
                    ((view as? NSScrollView).map { [$0] } ?? []) + view.subviews.flatMap(all)
                }
                return all(view).max {
                    ($0.documentView?.frame.height ?? 0) < ($1.documentView?.frame.height ?? 0)
                }
            }
        }

        /// The reader's content stays within 1 pt of where it was, and the
        /// page stays the one it was.
        private func expectHeld(
            _ page: Page, _ reading: (key: String, top: CGFloat), scroll: NSScrollView, document: NSView,
            _ step: Comment, sourceLocation: SourceLocation = #_sourceLocation
        ) throws {
            let top = page.probe.viewTops[reading.key]
            #expect(
                top.map { abs($0 - reading.top) <= 1 } == true,
                "\(step): \(String(describing: top)) from \(reading.top)", sourceLocation: sourceLocation)
            #expect(try page.scrollView === scroll, step, sourceLocation: sourceLocation)
            #expect(scroll.documentView === document, step, sourceLocation: sourceLocation)
        }

        @Test func anItemOpeningSeatsNoCardAndHoldsTheReader() async throws {
            let page = try await Page()
            defer { page.close() }
            let scroll = try page.scrollView
            let document = try #require(scroll.documentView)

            // At the top: the item's row draws, nothing scrolls, and the chip
            // stays Task.
            try await page.refresh { $0.items.append(Page.question) }
            #expect(page.navigation.path == [.task(page.task.task.id)])
            #expect(page.lead == nil)
            #expect(page.probe.oneLineItemIDs == [Page.question.item.id])
            #expect(page.offset == 0)
            #expect(page.probe.selectedSection == .task)
            try await page.refresh { $0.close(Page.question.item.id) }
            #expect(page.probe.oneLineItemIDs.isEmpty)

            // In the rail: the row draws above the reader, who doesn't move,
            // and the page never passes through the top on the way.
            try await page.scrollIntoTheRail()
            let reading = try #require(page.probe.reading)
            let readingOffset = page.offset
            let chip = page.probe.selectedSection
            page.samples = []
            try await page.refresh { $0.items.append(Page.question) }
            #expect(page.lead == nil)
            #expect(page.probe.oneLineItemIDs == [Page.question.item.id])
            #expect(page.probe.selectedSection == chip)
            try expectHeld(page, reading, scroll: scroll, document: document, "after an item opens")
            #expect(page.offset > readingOffset)
            #expect(page.samples.allSatisfy { $0 >= readingOffset - 0.5 }, "no trip to the top")
        }

        @Test func contentArrivingAboveHoldsAReaderInTheRail() async throws {
            let page = try await Page()
            defer { page.close() }
            let scroll = try page.scrollView
            let document = try #require(scroll.documentView)
            try await page.scrollIntoTheRail()

            // A row prepended to the rail, the newest event.
            var reading = try #require(page.probe.reading)
            var readingOffset = page.offset
            page.samples = []
            try await page.refresh(history: { history in
                // A terminal milestone always draws its own row; a repeated
                // start folds into the row it repeats.
                guard var event = history.events.first(where: { $0.milestone?.value1.kind == .terminal_recorded }),
                    let newest = history.events.first?.recorded_at
                else { return }
                event.recorded_at = newest.addingTimeInterval(60)
                history.events.insert(event, at: 0)
            })
            try expectHeld(page, reading, scroll: scroll, document: document, "after a rail row arrives")
            #expect(page.offset > readingOffset, "the row arrived above the reader")
            #expect(page.samples.allSatisfy { $0 >= readingOffset - 0.5 }, "no trip to the top")

            // A hold callout in the header. A working task's status line
            // names its hold itself, so the callout draws beside an open
            // decision, which the reader has above them already.
            try await page.refresh { $0.items.append(Page.question) }
            try expectHeld(page, reading, scroll: scroll, document: document, "after an item opens")
            reading = try #require(page.probe.reading)
            readingOffset = page.offset
            page.samples = []
            try await page.refresh { $0.held = true }
            try expectHeld(page, reading, scroll: scroll, document: document, "after a hold callout draws")
            #expect(page.offset > readingOffset, "the callout drew above the reader")
            #expect(page.samples.allSatisfy { $0 >= readingOffset - 0.5 }, "no trip to the top")

            // A fold the operator opens in view grows below the reader.
            let before = page.offset
            let campaign = try #require(page.history.sections.last?.campaign_id)
            page.disclosures.setExpanded(true, .campaign(campaign), taskID: page.task.task.id)
            try await page.settle()
            #expect(page.offset == before)
        }

        @Test func aSeatedCardGrowingOrLeavingHoldsTheReader() async throws {
            let page = try await Page()
            defer { page.close() }
            let scroll = try page.scrollView
            let document = try #require(scroll.documentView)
            try await page.refresh { $0.items += [Page.discussed, Page.sibling] }
            Page.openItem(Page.discussed.item.id, memory: page.memory, navigation: page.navigation)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            func messageArrives(_ sequence: Int) async throws {
                try await page.refresh { state in
                    var conversation = state.conversations[0]
                    var message = conversation.conversation.messages[1]
                    message.id = "msg-agent-later-\(sequence)"
                    message.sequence = sequence
                    message.body = "A later answer, long enough to take a line of its own on the page."
                    conversation.conversation.messages.append(message)
                    conversation.entity_version += 1
                    state.conversations = [conversation]
                }
            }

            // A message in the card's thread, above a reader at the card's
            // controls under it.
            var steps = 0
            while page.probe.reading?.key != "card:thread", steps < 400 {
                try await page.scroll(by: 8)
                steps += 1
            }
            var reading = try #require(page.probe.reading)
            try #require(reading.key == "card:thread")
            var readingOffset = page.offset
            try await messageArrives(3)
            try expectHeld(page, reading, scroll: scroll, document: document, "after a message arrives in the card")
            #expect(page.offset > readingOffset, "the message arrived above the reader")

            // And above a reader in the rail.
            try await page.scrollIntoTheRail()
            reading = try #require(page.probe.reading)
            readingOffset = page.offset
            page.samples = []
            try await messageArrives(4)
            try expectHeld(page, reading, scroll: scroll, document: document, "after a message arrives")
            #expect(page.offset > readingOffset, "the message arrived above the reader")
            #expect(page.samples.allSatisfy { $0 >= readingOffset - 0.5 }, "no trip to the top")

            // The card grows.
            reading = try #require(page.probe.reading)
            readingOffset = page.offset
            try await page.refresh { state in
                guard let index = state.items.firstIndex(where: { $0.item.id == Page.discussed.item.id }) else {
                    return
                }
                // The agent asks more of it: a question with its options.
                state.items[index].item.agent_question = AttentionFixtures.scopeConflictQuestion().item.agent_question
                state.items[index].item.item_version += 1
                state.items[index].entity_version += 1
            }
            try expectHeld(page, reading, scroll: scroll, document: document, "after the card grows")
            #expect(page.offset > readingOffset, "the card grew above the reader")

            // It resolves, and the lead passes to its sibling.
            reading = try #require(page.probe.reading)
            readingOffset = page.offset
            try await page.refresh { $0.close(Page.discussed.item.id) }
            #expect(page.lead == Page.sibling.item.id)
            try expectHeld(page, reading, scroll: scroll, document: document, "after the lead passes")

            // The last card resolves.
            reading = try #require(page.probe.reading)
            try await page.refresh { $0.close(Page.sibling.item.id) }
            #expect(page.lead == nil)
            try expectHeld(page, reading, scroll: scroll, document: document, "after the last card resolves")
        }

        /// The seated card checks its item again whenever the daemon's
        /// revision moves under it, which a busy task's sync rounds do,
        /// drawing its validation row under its controls and then taking it
        /// away. A reader below the controls holds still through both.
        @Test func aReaderInsideTheCardHoldsAsItRevalidates() async throws {
            let page = try await Page(validates: true)
            defer { page.close() }
            let scroll = try page.scrollView
            let document = try #require(scroll.documentView)
            try await page.refresh { $0.items.append(Page.question) }
            Page.openItem(Page.question.item.id, memory: page.memory, navigation: page.navigation)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            var steps = 0
            while page.probe.reading?.key != "card:controls", steps < 400 {
                try await page.scroll(by: 8)
                steps += 1
            }
            let reading = try #require(page.probe.reading)
            try #require(reading.key == "card:controls")
            let readingOffset = page.offset

            // The card's check waits on the daemon, so its row stays long
            // enough to sample.
            await page.server.setBeforeRespond { operation in
                if operation == "getAttentionItem" { try? await Task.sleep(for: .milliseconds(300)) }
            }
            page.tracked = reading.key
            page.trackedTops = []
            page.samples = []
            // A daemon write moves the item's revision, which is what makes
            // the card check its actions again.
            await page.server.advance(itemID: Page.question.item.id)
            let advanced = try #require(await page.server.snapshot(itemID: Page.question.item.id))
            try await page.refresh { state in
                guard let index = state.items.firstIndex(where: { $0.item.id == advanced.item.id }) else { return }
                state.items[index] = advanced
            }
            try await page.settle(0.8)
            #expect(page.samples.contains { $0 > readingOffset + 1 }, "the validation row drew above the reader")
            #expect(
                page.trackedTops.allSatisfy { abs($0 - reading.top) <= 1 },
                "\(page.trackedTops.filter { abs($0 - reading.top) > 1 }) from \(reading.top)")
            try expectHeld(page, reading, scroll: scroll, document: document, "after the card revalidates")
        }

        /// A rail row the reader is held by can take a new key, as a row
        /// does when its label or time changes. The page holds by another
        /// point from then on.
        @Test func aHeldRowThatChangesHandsTheHoldOn() async throws {
            let page = try await Page()
            defer { page.close() }
            let scroll = try page.scrollView
            let document = try #require(scroll.documentView)
            try await page.scrollIntoTheRail()
            let held = try #require(page.probe.reading)

            // Every event moves by a second, which the rail doesn't show,
            // and every row takes a new key.
            try await page.refresh(history: { history in
                for index in history.events.indices { history.events[index].recorded_at.addTimeInterval(1) }
            })
            #expect(page.probe.viewTops[held.key] == nil, "the held row's key went")

            let reading = try #require(page.probe.reading)
            let readingOffset = page.offset
            try await page.refresh(history: { history in
                guard var event = history.events.first(where: { $0.milestone?.value1.kind == .terminal_recorded }),
                    let newest = history.events.first?.recorded_at
                else { return }
                event.recorded_at = newest.addingTimeInterval(60)
                history.events.insert(event, at: 0)
            })
            try expectHeld(page, reading, scroll: scroll, document: document, "after a rail row arrives")
            #expect(page.offset > readingOffset, "the row arrived above the reader")

            // In one refresh the held row takes a new key, as its time
            // moving by a second the rail doesn't show gives it, and a row
            // arrives above it. Rows are prepended, so the reader's row
            // keeps its place counted from the rail's oldest row.
            func railRows() -> [(key: String, value: CGFloat)] {
                page.probe.viewTops.filter { $0.key.contains("|") }.sorted { $0.value > $1.value }
                    .map { ($0.key, $0.value) }
            }
            let last = try #require(page.probe.reading)
            let fromOldest = try #require(railRows().firstIndex { $0.key == last.key }, "held by a rail row")
            let lastOffset = page.offset
            try await page.refresh(history: { history in
                guard var event = history.events.first(where: { $0.milestone?.value1.kind == .terminal_recorded }),
                    let newest = history.events.first?.recorded_at
                else { return }
                let heldTime = last.key.split(separator: "|")[2]
                for index in history.events.indices
                where String(history.events[index].recorded_at.timeIntervalSince1970) == heldTime {
                    history.events[index].recorded_at.addTimeInterval(1)
                }
                event.recorded_at = newest.addingTimeInterval(120)
                history.events.insert(event, at: 0)
            })
            let rows = railRows()
            let row = try #require(rows.indices.contains(fromOldest) ? rows[fromOldest] : nil)
            #expect(row.key != last.key, "the held row's key went")
            #expect(abs(row.value - last.top) <= 1, "\(row.value) from \(last.top)")
            #expect(page.offset > lastOffset, "the row arrived above the reader")
        }

        @Test func aCardLeavingNearTheEndHoldsOrRestsAtTheEnd() async throws {
            let page = try await Page()
            defer { page.close() }
            try await page.refresh { $0.items.append(Page.question) }
            Page.openItem(Page.question.item.id, memory: page.memory, navigation: page.navigation)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            // The card draws over several passes, moving the end, and a
            // reader still at Decisions goes to the top when the card
            // leaves; scroll until the reader is past it.
            var steps = 0
            repeat {
                try await page.scroll(by: page.maxOffset - page.offset - 30)
                try await page.settle(0.2)
                steps += 1
            } while page.memory.anchor == .section(.decisions) && steps < 10
            try #require(page.memory.anchor != .section(.decisions), "the reader is past the card")
            let reading = try #require(page.probe.reading)

            try await page.refresh { $0.close(Page.question.item.id) }
            #expect(page.lead == nil)
            let top = page.probe.viewTops[reading.key]
            let held = top.map { abs($0 - reading.top) <= 1 } == true
            #expect(held || abs(page.offset - page.maxOffset) <= 1, "\(String(describing: top)) from \(reading.top)")
        }

        @Test func theOperatorOpeningAnItemMovesToItsCard() async throws {
            let page = try await Page()
            defer { page.close() }
            try await page.refresh { $0.items += [Page.question] }

            // From its one-line row.
            try await page.scrollIntoTheRail()
            Page.openItem(Page.question.item.id, memory: page.memory, navigation: page.navigation)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            #expect(page.lead == Page.question.item.id)
            #expect(page.memory.anchor == .section(.decisions))
            #expect(page.probe.selectedSection == .decisions)

            // From the Decisions chip, with no card seated.
            page.navigation.route(to: .task(page.task.task.id))
            try await page.settle(0.3)
            #expect(page.lead == nil)
            try await page.scrollIntoTheRail()
            let select = try #require(page.probe.select)
            select(.decisions)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            #expect(page.lead == Page.question.item.id)
            #expect(page.memory.anchor == .section(.decisions))

            // From a route change to the item, which counts no opening.
            page.navigation.route(to: .task(page.task.task.id))
            try await page.settle(0.3)
            try await page.scrollIntoTheRail()
            let openings = page.memory.cardOpenings
            page.navigation.route(to: .attentionItem(Page.question.item.id))
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            #expect(page.lead == Page.question.item.id)
            #expect(page.memory.anchor == .section(.decisions))
            #expect(page.memory.cardOpenings == openings)

            // A link to the card already leading moves the page to it too.
            try await page.scrollIntoTheRail()
            page.navigation.route(toLinkedItem: Page.question.item.id)
            try await page.settle(until: { page.memory.anchor == .section(.decisions) })
            #expect(page.memory.anchor == .section(.decisions))

            // A reveal request opens the Identifiers footer and scrolls down
            // to it (#2607).
            let beforeReveal = page.offset
            page.reveal.request = .init(itemID: Page.question.item.id, nonce: UUID())
            try await page.settle(until: { page.reveal.request == nil && page.offset > beforeReveal })
            #expect(page.disclosures.isExpanded(.identifiers, taskID: page.task.task.id))
            #expect(page.offset > beforeReveal)
        }

        @Test func foldsSurviveARefresh() async throws {
            let page = try await Page()
            defer { page.close() }
            let taskID = page.task.task.id
            let campaign = try #require(page.history.sections.last?.campaign_id)
            let run = try #require(page.history.sections.first?.runs.first?.run_id)
            let persisted: [TaskTimelineDisclosurePreferences.Section] = [
                .identifiers, .runDetails(run), .campaign(campaign),
            ]
            for fold in persisted { page.disclosures.setExpanded(true, fold, taskID: taskID) }
            let openFolder = try #require(page.probe.openStateFold)
            openFolder("Specification")
            openFolder(TaskPagePlaceProbe.whatHappened)
            try await page.settle(0.2)
            #expect(page.probe.openStateFolds == ["Specification", TaskPagePlaceProbe.whatHappened])

            try await page.refresh(
                { $0.items.append(Page.question) }, history: { $0.events[0].recorded_at.addTimeInterval(1) })
            for fold in persisted { #expect(page.disclosures.isExpanded(fold, taskID: taskID), "\(fold)") }
            #expect(page.probe.openStateFolds == ["Specification", TaskPagePlaceProbe.whatHappened])
        }
    }
#endif
