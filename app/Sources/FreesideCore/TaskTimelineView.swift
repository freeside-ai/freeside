import FreesideAPI
import SwiftUI

/// A task's history as the daemon computes it (`GET /tasks/{id}/timeline`):
/// the task's events lead the page as one open rail that marks where each
/// stage began, then every campaign folds its runs and detailed milestones.
/// Every list keeps the daemon's newest-first order; unlike the run rail,
/// nothing here is reversed or sorted by timestamp.
struct TaskTimelineView: View {
    /// Keys the timeline refetch task, as `RunTimelineView.TimelineRequestKey`
    /// does: the task's own revision plus the epoch, full-snapshot revision,
    /// and cache generation, so every canonical replacement refetches while
    /// cached content stays on screen, including a bootstrap that dropped a
    /// read in flight without moving any revision.
    struct TimelineRequestKey: Hashable {
        let taskID: String
        let syncEpoch: String?
        let lastFullSnapshotRevision: Int64?
        let cacheGeneration: Int
        let revision: Int64

        init(snapshot: Components.Schemas.TaskSnapshot, cursors: SyncCursors?, cacheGeneration: Int) {
            taskID = snapshot.task.id
            syncEpoch = cursors?.syncEpoch
            lastFullSnapshotRevision = cursors?.lastFullSnapshotRevision
            self.cacheGeneration = cacheGeneration
            revision = snapshot.as_of_revision
        }
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.TaskSnapshot
    /// Opens a run's timeline from its section header: the iOS stack pushes
    /// it, the macOS detail column shows it in place of this view.
    let onOpenRun: (String) -> Void
    /// Opens an Inbox item by id: where the header's guidance leads when it
    /// names one. Nil (a screenshot, a host with no Inbox route) draws the
    /// guidance as a plain sentence, never as a link that goes nowhere.
    var onOpenInboxItem: ((String) -> Void)?
    /// Supplies the existing screenshot composition without loading tasks or
    /// a scroll viewport. Rendering through body installs environment values
    /// before the content helpers read locale, time zone, and text size.
    var screenshotTimeline: Components.Schemas.TaskTimeline?
    /// The gap above the card where a standing notice sits over it. Nil
    /// keeps the detail column's top margin.
    private let topMargin: CGFloat?
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow
    /// Which folds are open, per task id on this device. A screenshot
    /// passes its own instance over throwaway defaults.
    @State private var disclosures: TaskTimelineDisclosurePreferences

    init(
        coordinator: SyncCoordinator, snapshot: Components.Schemas.TaskSnapshot,
        onOpenRun: @escaping (String) -> Void, onOpenInboxItem: ((String) -> Void)? = nil,
        topMargin: CGFloat? = nil,
        screenshotTimeline: Components.Schemas.TaskTimeline? = nil,
        disclosures: TaskTimelineDisclosurePreferences? = nil
    ) {
        self.coordinator = coordinator
        self.snapshot = snapshot
        self.onOpenRun = onOpenRun
        self.onOpenInboxItem = onOpenInboxItem
        self.topMargin = topMargin
        self.screenshotTimeline = screenshotTimeline
        _disclosures = State(initialValue: disclosures ?? .shared)
    }

    private var now: Date { pinnedNow ?? Date() }

    private func fold(_ section: TaskTimelineDisclosurePreferences.Section) -> Binding<Bool> {
        disclosures.binding(section, taskID: snapshot.task.id)
    }

    private var timeline: Components.Schemas.TaskTimeline? {
        coordinator.taskTimelinesByTaskID[snapshot.task.id]
    }

    @ViewBuilder var body: some View {
        if let screenshotTimeline {
            VStack(alignment: .leading, spacing: 22) {
                header(screenshotTimeline)
                schedules
                content(screenshotTimeline)
            }
            .foregroundStyle(Color.ink)
            .timelinePage(compact: horizontalSizeClass == .compact, topMargin: topMargin)
        } else {
            liveContent
        }
    }

    private var liveContent: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                schedules
                if let timeline {
                    content(timeline)
                } else if coordinator.taskTimelineLoadStates[snapshot.task.id] == .unavailable {
                    UnavailableStateView(
                        glyph: .tasks, title: "Timeline unavailable",
                        description: "Freeside could not load the daemon's history for this task."
                    )
                    .frame(maxWidth: .infinity, minHeight: 180)
                } else {
                    ProgressView("Loading timeline…")
                        .tint(.waterText)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .frame(maxWidth: .infinity, minHeight: 180)
                }
            }
            .foregroundStyle(Color.ink)
            .timelinePage(compact: horizontalSizeClass == .compact, topMargin: topMargin)
        }
        .navigationTitle(TaskDisplay.projectName(snapshot.task))
        .task(
            id: TimelineRequestKey(
                snapshot: snapshot, cursors: coordinator.cursors, cacheGeneration: coordinator.cacheGeneration)
        ) {
            await coordinator.refreshTaskTimeline(for: snapshot.task.id)
        }
        .task(id: coordinator.taskReviewRequestKey(for: snapshot.task.id, revision: snapshot.as_of_revision)) {
            await coordinator.refreshTaskReviews(for: snapshot.task.id, revision: snapshot.as_of_revision)
        }
    }

    /// The composition with fixture data supplied directly, because
    /// ImageRenderer never executes the loading task.
    func screenshotContent(
        _ timeline: Components.Schemas.TaskTimeline, disclosures: TaskTimelineDisclosurePreferences? = nil
    ) -> some View {
        TaskTimelineView(
            coordinator: coordinator, snapshot: snapshot, onOpenRun: onOpenRun,
            onOpenInboxItem: onOpenInboxItem, screenshotTimeline: timeline,
            disclosures: disclosures ?? TaskTimelineDisclosurePreferences(defaults: nil))
    }

    private var header: some View {
        header(timeline)
    }

    /// Layer 0: what the work is and where it stands, without scrolling
    /// (5.6): the screen's name with the state beside it, the task's name
    /// over its identity line, the hold when there is one, and where to act.
    /// Stop sits last, behind More Actions. Internal so a screenshot golden
    /// can draw the header on its own.
    func header(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
        let position = TaskDisplay.position(
            task, runs: coordinator.runs, attentionItems: coordinator.store.orderedSnapshots, history: timeline)
        let lines = TaskDisplay.rowLines(task, position: position)
        let stage = TaskDisplay.detailStage(
            task, runs: coordinator.runs, history: timeline,
            reviewRounds: timeline.map {
                TaskTimelinePresentation.reviewRounds($0, runTimelines: coordinator.timelinesByRunID)
            } ?? [:])
        return VStack(alignment: .leading, spacing: 12) {
            // The task id lives in the page's Identifiers footer; the copy
            // menu keeps a quick path to it.
            CardEyebrow(
                keyword: "Task timeline",
                chip: StateChip(
                    label: TaskDisplay.detailStatus(lines.status, stage: stage),
                    cut: TaskDisplay.statusCut(task, position: position))
            )
            .copyMenu {
                Button("Copy task ID") { Clipboard.copy(snapshot.task.id) }
            }
            TaskNameLabel(
                name: TaskTimelinePresentation.headerName(timeline: timeline, snapshot: snapshot),
                font: FreesideFont.ask,
                monoFont: FreesideFont.mono(.title2),
                lineLimit: 3)
            // The run timeline's identity line, in the same place and face.
            Text(TaskTimelinePresentation.headerMetaLine(task))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            if let hold = TaskDisplay.holdCallout(task, position: position, stage: stage) {
                holdCallout(hold)
            }
            switch lines.visibleGuidance(attention: position?.attention == true) {
            case .link(let title):
                // Link styling promises navigation, so it is drawn only
                // where tapping it opens the item the guidance names.
                if let itemID = position?.attentionItemID, let onOpenInboxItem {
                    Button {
                        onOpenInboxItem(itemID)
                    } label: {
                        FreesideLink(title: title, face: FreesideFont.noticeAction)
                            .multilineTextAlignment(.leading)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .buttonStyle(.plain)
                } else {
                    guidanceSentence(lines.guidance)
                }
            case .sentence(let sentence): guidanceSentence(sentence)
            case nil: EmptyView()
            }
            TaskStopView(
                coordinator: coordinator, taskID: task.id,
                moreActions: timeline.map { timeline in
                    let text = TaskTimelinePresentation.allIdentifiersText(
                        TaskTimelinePresentation.identifierGroups(timeline, taskID: task.id))
                    return [.init(title: "Copy all identifiers") { Clipboard.copy(text) }]
                } ?? [])
        }
    }

    /// The current hold, set apart under the name so the reason the task
    /// waits is read before its history (visual audit D02). Every hold kind
    /// takes this one treatment: the hold's own words, the status chip, and
    /// the guidance sentence are what tell a capacity wait from a hold that
    /// needs the operator. The stage the task waits in leads, when one is
    /// known, and no round appears. VoiceOver reads the stage, then the hold.
    private func holdCallout(_ hold: TaskDisplay.HoldCallout) -> some View {
        SystemCallout {
            if let stage = hold.stage {
                Text(stage)
                    .font(FreesideFont.cardBody)
            }
            Text(hold.hold)
                .font(FreesideFont.statement)
        }
        .foregroundStyle(Color.ink)
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
    }

    private func guidanceSentence(_ sentence: String) -> some View {
        Text(sentence)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    /// The task's armed watches and deadlines, each with when it next
    /// fires. They sit on the task's own page, not on its list row (6.1),
    /// and draw nothing while none is armed.
    @ViewBuilder private var schedules: some View {
        let armed = TaskDisplay.armedSchedules(for: snapshot.task, in: coordinator.schedules)
        if !armed.isEmpty {
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Schedules")
                ForEach(armed, id: \.schedule.id) { snapshot in
                    let schedule = snapshot.schedule
                    if let fireAt = schedule.fire_at {
                        FactRow(
                            label: RunDisplay.label(schedule.kind),
                            value: FreesideFormat.shortTime(fireAt, now: now, locale: locale, timeZone: timeZone)
                        )
                        .exactInstant(fireAt)
                    } else {
                        Text(RunDisplay.label(schedule.kind))
                            .font(FreesideFont.factLabel)
                    }
                }
            }
        }
    }

    /// Layer 1 is the task's events, always open: the rail that says which
    /// stage the task is in and how it got there. Under it every campaign,
    /// the current one included, is one folded disclosure row, and the
    /// page's exact identifiers close it under one hairline.
    private func content(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        let rows = TaskTimelinePresentation.rows(
            timeline,
            reviewRounds: TaskTimelinePresentation.reviewRounds(timeline, runTimelines: coordinator.timelinesByRunID))
        let disambiguate = TaskTimelinePresentation.campaignCount(timeline) > 1
        return VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Task Events")
                if let message = TaskTimelinePresentation.availabilityMessage(
                    state: coordinator.taskTimelineLoadStates[snapshot.task.id], freshness: coordinator.store.freshness)
                {
                    Text(message)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                if rows.isEmpty {
                    Text("No recorded events in this history.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                } else {
                    TimelineRail(rows: rows)
                }
            }
            if timeline.sections.isEmpty {
                Text("No campaigns or runs in this history.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            ForEach(Array(timeline.sections.enumerated()), id: \.offset) { index, section in
                let ids = TaskTimelinePresentation.technicalRows(section: section)
                SentenceDisclosure(
                    label: TaskTimelinePresentation.sectionTitle(section, disambiguate: disambiguate),
                    summary: TaskTimelinePresentation.sectionSummary(
                        section, isCurrent: index == 0, now: now, locale: locale, timeZone: timeZone),
                    isExpanded: fold(.campaign(section.campaign_id)),
                    // A section outside a campaign may hold no id at all.
                    menuItems: ids.isEmpty ? nil : { copyItems(ids) }
                ) {
                    sectionBody(section, in: timeline, leadsWithFullCard: index == 0)
                        .padding(.top, 3)
                }
            }
            identifiers(timeline)
        }
    }

    /// The page's one hairline, then its one Identifiers fold (R26), as the
    /// run timeline closes its page: every exact id on the page, grouped by
    /// what it names, under everything the page says in words. Each card
    /// keeps a copy menu for its own ids, so a single copy needs no trip
    /// here.
    private func identifiers(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        TechnicalDetailsSection(
            groups: TaskTimelinePresentation.identifierGroups(timeline, taskID: snapshot.task.id),
            summary: TaskTimelinePresentation.identifiersSummary(timeline),
            isExpanded: fold(.identifiers)
        )
        .padding(.top, 18)
        .overlay(alignment: .top) {
            Color.rule.frame(height: 1)
        }
    }

    /// One context-menu item per identifier, each copying the exact value.
    private func copyItems(_ rows: [AttentionDisplay.BindingRow]) -> some View {
        ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
            Button(TaskTimelinePresentation.copyMenuTitle(for: row.label)) { Clipboard.copy(row.value) }
        }
    }

    /// A section's runs. The current section leads with its newest run as a
    /// full card; every other run, here and in an earlier campaign, is a
    /// one-line card that opens to the same content.
    private func sectionBody(
        _ section: Components.Schemas.TaskTimelineSection, in timeline: Components.Schemas.TaskTimeline,
        leadsWithFullCard: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            ForEach(Array(section.runs.enumerated()), id: \.element.run_id) { index, run in
                if leadsWithFullCard && index == 0 {
                    item(verticalPadding: 18, horizontalPadding: 20) {
                        VStack(alignment: .leading, spacing: 16) {
                            openRunButton(run, in: timeline, showsTitle: true)
                            runBody(run, in: timeline)
                        }
                    }
                } else {
                    item(verticalPadding: 12, horizontalPadding: 18) { priorRun(run, in: timeline) }
                }
            }
        }
    }

    /// A run as a bordered item on the page's ground (R3): the border
    /// alone sets it apart, with no fill of its own.
    private func item<Content: View>(
        verticalPadding: CGFloat, horizontalPadding: CGFloat, @ViewBuilder content: () -> Content
    ) -> some View {
        content()
            .padding(.vertical, verticalPadding)
            .padding(.horizontal, horizontalPadding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    private func chip(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    )
        -> (label: String, cut: StateChip.Cut)?
    {
        let task = snapshot.task
        return TaskTimelinePresentation.runChip(
            run, task: task,
            position: TaskDisplay.position(
                task, runs: coordinator.runs, attentionItems: coordinator.store.orderedSnapshots, history: timeline),
            synced: coordinator.runs.first { $0.run.id == run.run_id }?.run)
    }

    /// The run's way into its own history. With the title it is the full
    /// card's title row; without, it is the link alone under a prior run's
    /// fold. Either way the whole row is the button and says what it said.
    private func openRunButton(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline, showsTitle: Bool
    ) -> some View {
        Button {
            onOpenRun(run.run_id)
        } label: {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    // Under a prior run's fold the link stands alone, so it
                    // leads; beside a title it trails.
                    if showsTitle {
                        runTitle(run, in: timeline, dimmed: false)
                        Spacer(minLength: 8)
                    }
                    FreesideLink(title: "Open run history", face: FreesideFont.noticeAction)
                    if !showsTitle { Spacer(minLength: 0) }
                }
                VStack(alignment: .leading, spacing: 6) {
                    if showsTitle { runTitle(run, in: timeline, dimmed: false) }
                    FreesideLink(title: "Open run history", face: FreesideFont.noticeAction)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .copyMenu { copyItems(TaskTimelinePresentation.technicalRows(run: run, in: timeline)) }
        .accessibilityLabel(TaskTimelinePresentation.runCardAccessibilityLabel(run, in: timeline))
        .accessibilityHint("Shows this run's recorded milestones and invocation observations.")
    }

    /// `Attempt N` with its chip beside it: the title is fixed vocabulary
    /// that cannot wrap, which is the one case a chip sits beside a title.
    private func runTitle(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline, dimmed: Bool
    ) -> some View {
        WrappingHStack(horizontalSpacing: 10, verticalSpacing: 4) {
            Text(TaskTimelinePresentation.runTitle(run))
                .font(dimmed ? FreesideFont.factLabel : FreesideFont.statement)
                .foregroundStyle(dimmed ? Color.inkDim : Color.ink)
            if let chip = chip(run, in: timeline) {
                StateChip(label: chip.label, cut: chip.cut)
            }
        }
    }

    /// A prior run as one line (chevron, title, faint chip, newest
    /// milestone time) that opens to the full card's content in place.
    private func priorRun(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        let isExpanded = fold(.run(run.run_id))
        return VStack(alignment: .leading, spacing: 16) {
            Button {
                isExpanded.wrappedValue.toggle()
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Image(systemName: "arrowtriangle.right.fill")
                        .font(FreesideFont.disclosureGlyph)
                        .foregroundStyle(Color.accentText)
                        .rotationEffect(.degrees(isExpanded.wrappedValue ? 90 : 0))
                        .accessibilityHidden(true)
                    ViewThatFits(in: .horizontal) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            runTitle(run, in: timeline, dimmed: true)
                            Spacer(minLength: 8)
                            milestoneTime(run)
                        }
                        VStack(alignment: .leading, spacing: 4) {
                            runTitle(run, in: timeline, dimmed: true)
                            milestoneTime(run)
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            .copyMenu { copyItems(TaskTimelinePresentation.technicalRows(run: run, in: timeline)) }
            .accessibilityValue(isExpanded.wrappedValue ? "Expanded" : "Collapsed")
            if isExpanded.wrappedValue {
                openRunButton(run, in: timeline, showsTitle: false)
                runBody(run, in: timeline)
            }
        }
    }

    @ViewBuilder
    private func milestoneTime(_ run: Components.Schemas.TaskTimelineRun) -> some View {
        if let time = TaskTimelinePresentation.newestMilestoneTime(run) {
            Text(FreesideFormat.shortTime(time, now: now, locale: locale, timeZone: timeZone))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .exactInstant(time)
        }
    }

    /// A run's stages from its synced snapshot, which name the attempt each
    /// review round reviewed. Empty until the run syncs; labels then fall
    /// back to the attempt's kind.
    private func stages(of runID: String) -> [Components.Schemas.Stage] {
        coordinator.runs.first { $0.run.id == runID }?.run.stages ?? []
    }

    /// Everything under a run's title: review, milestones, details.
    @ViewBuilder
    private func runBody(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        if run.role == nil || run.role?.value1 == .implementation {
            RunReviewSection(
                coordinator: coordinator, runID: run.run_id,
                facts: coordinator.timelinesByRunID[run.run_id]?.review?.value1,
                stages: stages(of: run.run_id),
                hasTimeline: coordinator.timelinesByRunID[run.run_id] != nil,
                folds: .init(
                    facts: { fold(.roundFacts($0)) }, priorRounds: fold(.priorRounds(run.run_id))))
        }
        VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Milestones")
            if run.milestones.isEmpty {
                Text("No execution milestones in this run's task history.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            } else {
                TimelineRail(
                    rows: TaskTimelinePresentation.milestoneRows(
                        run,
                        reviewRounds: coordinator.timelinesByRunID[run.run_id]?.review?.value1.rounds ?? [],
                        stages: stages(of: run.run_id)))
            }
        }
        if TaskTimelinePresentation.hasRunDetails(run) {
            SentenceDisclosure(
                label: "Run Details", summary: TaskTimelinePresentation.runDetailsSummary(run),
                isExpanded: fold(.runDetails(run.run_id))
            ) {
                runDetails(run, in: timeline)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// The run's role, lineage, and recorded hold as facts. A run this
    /// history holds is a link to its own timeline; one it doesn't hold is
    /// named by its short id, with nowhere to go.
    private func runDetails(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            if let role = run.role?.value1 {
                FactRow(label: "Role", value: TaskTimelinePresentation.label(role))
            }
            if let reason = run.attempt_reason {
                FactRow(label: "Reason", value: reason)
            }
            if let parent = run.parent_run_id {
                runFact("Parent", runID: parent, in: timeline)
            }
            if let successor = run.superseded_by {
                runFact("Superseded By", runID: successor, in: timeline)
            }
            if let hold = run.hold?.value1 {
                FactRow(label: "Recorded Hold", value: RunDisplay.label(hold.reason))
                FactRow(label: "Hold Code", value: hold.reason.rawValue, valueColor: .inkDim)
            }
        }
    }

    @ViewBuilder
    private func runFact(
        _ label: String, runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        if let title = TaskTimelinePresentation.runLinkTitle(runID, in: timeline) {
            FactLinkRow(label: label, value: title) { onOpenRun(runID) }
        } else {
            FactRow(label: label, value: ShortIdentifier.short(runID))
        }
    }
}

/// Current projections retain daemon ordering. Legacy cached projections
/// recover their nested events in newest-first order.
enum TaskTimelinePresentation {
    /// Older saved projections put only creation at task level. Recover their
    /// recorded nested events without changing the current daemon projection.
    static func events(_ timeline: Components.Schemas.TaskTimeline) -> [Components.Schemas.TaskEvent] {
        guard timeline.events.allSatisfy({ $0.kind == .task_created }) else { return timeline.events }
        var events = timeline.events
        for section in timeline.sections {
            for event in section.events + section.runs.flatMap(\.events) where !events.contains(event) {
                events.append(event)
            }
        }
        return events.enumerated().sorted {
            if $0.element.recorded_at != $1.element.recorded_at {
                return $0.element.recorded_at > $1.element.recorded_at
            }
            return $0.offset < $1.offset
        }.map(\.element)
    }

    /// A cached partition remains useful after a failed refresh, but neither
    /// its contents nor an empty array establish current, complete history.
    static func availabilityMessage(
        state: SyncCoordinator.TimelineLoadState?, freshness: InboxStore.Freshness
    ) -> String? {
        if state == .loading { return "Showing saved task history while refreshing…" }
        if state == .unavailable { return "Task history refresh failed. Showing saved history." }
        if state != .loaded || freshness.isFailing { return "Saved task history. Freshness unconfirmed." }
        return nil
    }

    /// The page's elements flattened in render order, so a test can pin the
    /// order without a view: the event rail's rows by their words, then
    /// each campaign's runs and milestones.
    enum Entry: Equatable {
        case row(String)
        case section(campaignID: String?)
        case run(String)
        case milestone(runID: String, kind: Components.Schemas.RunMilestoneKind)
    }

    static func entries(_ timeline: Components.Schemas.TaskTimeline) -> [Entry] {
        var entries: [Entry] = rows(timeline).map { .row($0.label) }
        for section in timeline.sections {
            entries.append(.section(campaignID: section.campaign_id))
            for run in section.runs {
                entries.append(.run(run.run_id))
                entries += run.milestones.map { .milestone(runID: run.run_id, kind: $0.kind) }
            }
        }
        return entries
    }

    /// Where the walk in `rows(_:)` stands: a stage within one campaign. A
    /// new campaign is a new position even when its first stage repeats the
    /// last one, so a revised specification gets its own start point.
    private struct StagePosition: Equatable {
        let stage: Components.Schemas.StageName
        let campaignID: String?
    }

    /// The event rail's rows, newest first in the order `events(_:)` gives.
    /// Nothing here sorts, so events at one instant keep the daemon's order.
    ///
    /// The daemon records no stage starts, so they are read off its events
    /// (devlog/2026-10-11-1140-task-stage-timeline.md). Walking oldest to
    /// newest, a run's submission enters its role's stage and a review
    /// request enters Review, and a verification result or a publication
    /// milestone enters Verification; each draws a start point directly
    /// under itself when that changes where the task stands. Verification's
    /// start therefore carries the time of the first event that moved the
    /// task there, which trails the real start by the daemon's own
    /// verification (devlog/2026-10-23-0856-verification-start-point.md).
    /// No other event moves the stage: an attempt's result can be recorded
    /// after the review of its work was requested, and must not read as a
    /// return to implementation.
    ///
    /// A remediation is a return to Implementation. `reviewRounds` holds
    /// each run's review rounds by run id; a round whose findings went to a
    /// remediator names its invocation. That invocation's start draws
    /// "Remediation started" with an Implementation start point under it,
    /// and the decision that sent the findings there directly under both,
    /// at the decision's own time. Until the remediator starts, the
    /// decision sits where its time falls. The remediator's result reads as the
    /// remediation's and is not counted as a pass of the attempt.
    ///
    /// Three kinds of event are machinery and leave the rail: a campaign's
    /// allocation, any other invocation's start, and the result of a review
    /// round's own invocation, which the round's entry already states.
    static func rows(
        _ timeline: Components.Schemas.TaskTimeline,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> [TimelineRail.Row] {
        let events = events(timeline)
        var runs: [String: (run: Components.Schemas.TaskTimelineRun, campaignID: String?)] = [:]
        for section in timeline.sections {
            for run in section.runs where runs[run.run_id] == nil {
                runs[run.run_id] = (run, section.campaign_id)
            }
        }
        let reviewInvocations = Set(events.compactMap { $0.review?.value1.invocation_id })
        func remediated(_ milestone: Components.Schemas.RunMilestone?) -> Components.Schemas.RunReviewRound? {
            guard let milestone, let invocation = milestone.invocation_id else { return nil }
            return reviewRounds[milestone.run_id]?.first { $0.remediation?.value1.invocation_id == invocation }
        }
        let remediatorStarts = Set(
            events.compactMap { event -> String? in
                guard let milestone = event.milestone?.value1, milestone.kind == .invocation_started,
                    remediated(milestone) != nil
                else { return nil }
                return milestone.invocation_id
            })
        // A decision whose remediator has not started sits where its own
        // time falls among the events: the head of the rail at first, and
        // under whatever happens after it, such as a stop that means the
        // remediator never starts. One with no recorded time leads the rail.
        var unstarted = timeline.sections.reversed().flatMap { $0.runs.reversed() }
            .flatMap { run in
                (reviewRounds[run.run_id] ?? []).compactMap { round in
                    round.remediation.map { (round: round, remediation: $0.value1) }
                }
            }
            .filter { !remediatorStarts.contains($0.remediation.invocation_id) }
            .enumerated()
            .sorted {
                ($0.element.remediation.decided_at ?? .distantFuture, $0.offset)
                    < ($1.element.remediation.decided_at ?? .distantFuture, $1.offset)
            }
            .map(\.element)
        var position: StagePosition?
        var results: [String: Int] = [:]
        var startedRemediations: Set<String> = []
        var oldestFirst: [TimelineRail.Row] = []
        for event in events.reversed() {
            while let next = unstarted.first, let decidedAt = next.remediation.decided_at,
                decidedAt < event.recorded_at
            {
                oldestFirst.append(decidedRow(next.round, next.remediation))
                unstarted.removeFirst()
            }
            let milestone = event.milestone?.value1
            let source = (event.run_id ?? milestone?.run_id).flatMap { runs[$0] }
            var pass = 1
            var start: Components.Schemas.StageName?
            switch (event.kind, milestone?.kind) {
            case (_, .invocation_started):
                guard let round = remediated(milestone), let remediation = round.remediation?.value1,
                    startedRemediations.insert(remediation.invocation_id).inserted
                else { continue }
                oldestFirst.append(decidedRow(round, remediation))
                let next = StagePosition(stage: .implementation, campaignID: source?.campaignID)
                if next != position {
                    oldestFirst.append(
                        TimelineRail.Row(
                            kind: .stageStart(.implementation), label: "Implementation started",
                            recordedAt: event.recorded_at))
                }
                position = next
                oldestFirst.append(
                    TimelineRail.Row(
                        kind: .event, label: "Remediation started · Round \(round.round)",
                        recordedAt: event.recorded_at))
                continue
            case (.campaign_allocated, _):
                continue
            case (_, .run_submitted):
                let next = source?.run.role.map {
                    StagePosition(stage: stage($0.value1), campaignID: source?.campaignID)
                }
                if let next, next != position { start = next.stage }
                // A run with no role has no stage to enter, and leaves the
                // one before it: what follows is not known to continue it.
                position = next
            case (_, .terminal_recorded):
                if let milestone, remediated(milestone) != nil {
                    oldestFirst.append(
                        TimelineRail.Row(
                            kind: recordsFailure(event) ? .failure : .event,
                            label: "Remediation " + (terminalWord(milestone) ?? "ended"),
                            recordedAt: event.recorded_at))
                    continue
                }
                if let invocation = milestone?.invocation_id, reviewInvocations.contains(invocation) { continue }
                if let runID = milestone?.run_id {
                    pass = results[runID, default: 0] + 1
                    results[runID] = pass
                }
            case (.review_requested, _), (.external_review_reentered, _):
                // An external review re-entry opens the round its request
                // later names, so the withdrawn ready card has its cause.
                let next = StagePosition(stage: .review, campaignID: source?.campaignID)
                if next != position { start = .review }
                position = next
            case (.verification_recorded, _), (_, .publication_ready), (_, .publication_blocked):
                let next = StagePosition(stage: .verification, campaignID: source?.campaignID)
                if next != position { start = .verification }
                position = next
            default:
                break
            }
            if let start {
                oldestFirst.append(
                    TimelineRail.Row(
                        kind: .stageStart(start), label: "\(AttentionDisplay.label(start)) started",
                        recordedAt: event.recorded_at))
            }
            oldestFirst.append(
                TimelineRail.Row(
                    kind: recordsFailure(event) ? .failure : .event,
                    label: rowLabel(event, run: source?.run, pass: pass), recordedAt: event.recorded_at))
        }
        oldestFirst += unstarted.map { decidedRow($0.round, $0.remediation) }
        var rows = Array(oldestFirst.reversed())
        if !rows.isEmpty { rows[0].isNewest = true }
        if rows.count > 1, case .stageStart = rows[1].kind { rows[1].isNewest = true }
        return rows
    }

    /// The decision that sent a round's findings to a remediator, with the
    /// findings the operator declined when the record counts them.
    private static func decidedRow(
        _ round: Components.Schemas.RunReviewRound, _ remediation: Components.Schemas.ReviewRoundRemediation
    ) -> TimelineRail.Row {
        var label = "Round \(round.round) findings decided · \(remediation.finding_ids.count) to remediation"
        if let declined = round.dispositions?.value1.declined, declined > 0 { label += " · \(declined) declined" }
        return TimelineRail.Row(kind: .event, label: label, recordedAt: remediation.decided_at)
    }

    /// Each run's review rounds by run id, for `rows(_:reviewRounds:)`:
    /// the rounds of every run in `timeline` whose own timeline is loaded.
    static func reviewRounds(
        _ timeline: Components.Schemas.TaskTimeline, runTimelines: [String: Components.Schemas.RunTimeline]
    ) -> [String: [Components.Schemas.RunReviewRound]] {
        var rounds: [String: [Components.Schemas.RunReviewRound]] = [:]
        for run in timeline.sections.flatMap(\.runs) {
            if let review = runTimelines[run.run_id]?.review?.value1 { rounds[run.run_id] = review.rounds }
        }
        return rounds
    }

    /// The stage the rail's newest start point names, which the header's
    /// chip and hold callout repeat.
    static func newestStage(_ rows: [TimelineRail.Row]) -> Components.Schemas.StageName? {
        for row in rows {
            if case .stageStart(let stage?) = row.kind { return stage }
        }
        return nil
    }

    private static func stage(_ role: Components.Schemas.TaskRunRole) -> Components.Schemas.StageName {
        switch role {
        case .specification: .specification
        case .implementation: .implementation
        }
    }

    /// A failed review round, a failed stop, or a work result the daemon
    /// recorded as failed: the entries whose own words say so.
    private static func recordsFailure(_ event: Components.Schemas.TaskEvent) -> Bool {
        if event.kind == .review_failed || event.kind == .stop_failed { return true }
        guard let milestone = event.milestone?.value1, milestone.kind == .terminal_recorded else { return false }
        return terminalWord(milestone) == Components.Schemas.ObservedInvocationStatus.failed.rawValue
    }

    /// A work result in the word the run rail's detail uses for it,
    /// lowercased to follow the attempt it ends.
    private static func terminalWord(_ milestone: Components.Schemas.RunMilestone) -> String? {
        (milestone.terminal?.value1.rawValue ?? milestone.outcome?.value1.rawValue)?
            .replacingOccurrences(of: "_", with: " ")
    }

    /// An event's words on the rail. A run is named by its attempt and a
    /// review round by its number, never by an id; a run with no role or
    /// no attempt number keeps the label it had, since there is nothing to
    /// name it by. `pass` counts the run's work results, oldest first: a
    /// fix-up pass records a second one for the same attempt.
    private static func rowLabel(
        _ event: Components.Schemas.TaskEvent, run: Components.Schemas.TaskTimelineRun?, pass: Int
    ) -> String {
        let attempt = run?.role == nil ? nil : run?.attempt_number
        if let milestone = event.milestone?.value1, let attempt {
            if milestone.kind == .run_submitted { return "Attempt \(attempt) started" }
            if milestone.kind == .terminal_recorded, let word = terminalWord(milestone) {
                return "Attempt \(attempt)" + (pass > 1 ? ", pass \(pass)" : "") + " \(word)"
            }
        }
        // The reviewer by login and the pull request by number: the two
        // names a reader can find on the forge.
        if let external = event.external_review?.value1, let number = event.pr_number,
            event.kind == .external_review_reentered
        {
            return "External review by @\(external.reviewer_login) on #\(number) · Round \(external.round)"
        }
        if let review = event.review?.value1 {
            switch event.kind {
            case .review_requested: return "Round \(review.round) requested"
            case .review_failed: return "Round \(review.round) failed"
            case .review_completed:
                let outcome = review.outcome?.value1.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
                return "Round \(review.round) completed" + (outcome.map { " · \($0)" } ?? "")
            default: break
            }
        }
        // The PR number is the one identifier the rail keeps: it names the
        // pull request to a reader, where a run id names nothing.
        if let number = event.pr_number, event.kind == .pr_opened || event.kind == .pr_merged {
            return "\(label(event.kind)) · #\(number)"
        }
        return label(event)
    }

    /// The header's name: the fetched timeline carries the task's current
    /// name at a newer revision than the bootstrap snapshot (a rename can
    /// land between the two), so it wins once loaded; the snapshot's name
    /// stands in while the timeline loads or is unavailable.
    static func headerName(
        timeline: Components.Schemas.TaskTimeline?, snapshot: Components.Schemas.TaskSnapshot
    ) -> Components.Schemas.DisplayName {
        timeline?.name ?? snapshot.task.display_names.task
    }

    /// A run inside a campaign is titled by its attempt; a run outside one,
    /// or missing its attempt number, by its short id, as the run timeline
    /// does. The exact id is in the page's Identifiers footer.
    static func runTitle(_ run: Components.Schemas.TaskTimelineRun) -> String {
        run.attempt_number.map { "Attempt \($0)" } ?? "Run \(ShortIdentifier.short(run.run_id))"
    }

    /// The run card's accessibility label: the title and the run's role,
    /// without the opaque id VoiceOver would otherwise read character by
    /// character. Attempt numbers restart per campaign, so when another run
    /// shares this title and role the label names the campaign; the label
    /// stands in for the button's content, and two controls must not read
    /// alike.
    static func runCardAccessibilityLabel(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        var label = "Open detailed run history for \(runTitle(run))"
        if let role = run.role?.value1 { label += ", \(self.label(role)) run" }
        if !isUniqueRunControl(run, in: timeline),
            let campaign = campaignID(forRun: run.run_id, in: timeline)
        {
            label += ", campaign \(ShortIdentifier.short(campaign))"
        }
        return label
    }

    /// Whether the run's title and role are unique among the timeline's runs.
    /// Not unique means attempts have restarted in another campaign, so the
    /// control needs a campaign to tell it apart.
    private static func isUniqueRunControl(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> Bool {
        let key = runControlKey(run)
        var matches = 0
        for section in timeline.sections {
            for other in section.runs where runControlKey(other) == key {
                matches += 1
                if matches > 1 { return false }
            }
        }
        return true
    }

    private static func runControlKey(
        _ run: Components.Schemas.TaskTimelineRun
    ) -> String {
        "\(runTitle(run))|\(run.role?.value1.rawValue ?? "")"
    }

    /// The number of campaign sections, so a section's title shows a
    /// disambiguating short campaign id only when more than one campaign is on
    /// screen.
    static func campaignCount(_ timeline: Components.Schemas.TaskTimeline) -> Int {
        timeline.sections.count { $0.campaign_id != nil }
    }

    static func sectionTitle(
        _ section: Components.Schemas.TaskTimelineSection, disambiguate: Bool = false
    ) -> String {
        guard let campaign = section.campaign_id else { return "Outside a Campaign" }
        return disambiguate ? "Campaign \(ShortIdentifier.short(campaign))" : "Campaign"
    }

    /// Names another run by its role and attempt when it sits in this timeline,
    /// so a supersession or parent line reads as "implementation attempt 2"
    /// within the history; otherwise by its short id, since the run is not on
    /// screen to number. The role is kept because a campaign's specification
    /// run and its first implementation run both carry attempt 1, so a bare
    /// "attempt 1" reference could not tell them apart.
    static func runReference(_ runID: String, in timeline: Components.Schemas.TaskTimeline) -> String {
        for section in timeline.sections {
            guard let run = section.runs.first(where: { $0.run_id == runID }) else { continue }
            switch run.role?.value1 {
            case .specification:
                return "specification run"
            case .implementation:
                return run.attempt_number.map { "implementation attempt \($0)" } ?? "implementation run"
            case nil:
                return run.attempt_number.map { "attempt \($0)" } ?? ShortIdentifier.short(runID)
            }
        }
        return ShortIdentifier.short(runID)
    }

    /// The task's identifiers: the exact task id.
    static func technicalRows(taskID: String) -> [AttentionDisplay.BindingRow] {
        [.init(label: "Task ID", value: taskID)]
    }

    /// A campaign section's identifiers: the exact campaign id and, when
    /// the section recorded one, the approved specification digest.
    static func technicalRows(
        section: Components.Schemas.TaskTimelineSection
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = []
        if let campaign = section.campaign_id {
            rows.append(.init(label: "Campaign ID", value: campaign))
        }
        if let digest = section.events.first(where: { $0.kind == .specification_approved })?
            .approved_spec_digest?.value1
        {
            rows.append(.init(label: "Approved Specification Digest", value: digest))
        }
        return rows
    }

    /// A run card's identifiers: the exact run, parent, and superseding
    /// ids, and for every verification a verification event records for this
    /// run, its Inbox item id and the head and base it was run against. A
    /// run that returned to the agent or remediated can record several, each
    /// its own ready item, so all are kept, not just the first. The event
    /// rail names a verification in words only, so its commits live here.
    static func technicalRows(
        run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [.init(label: "Run ID", value: run.run_id)]
        if let parent = run.parent_run_id {
            rows.append(.init(label: "Parent Run ID", value: parent))
        }
        if let successor = run.superseded_by {
            rows.append(.init(label: "Superseded by Run ID", value: successor))
        }
        // Number the rows when a remediated run recorded more than one, so each
        // row's copy control has a distinct accessibility label.
        let verifications = verifications(for: run.run_id, in: timeline)
        for (index, verification) in verifications.enumerated() {
            let number = verifications.count > 1 ? " \(index + 1)" : ""
            rows += [
                .init(label: "Verification Inbox Item ID\(number)", value: verification.item_id),
                .init(label: "Verified Head\(number)", value: verification.head_sha),
                .init(label: "Verified Base\(number)", value: verification.base_sha),
            ]
        }
        return rows
    }

    /// The verification each verification event records for the run, one per
    /// distinct Inbox item id, in event order. The daemon emits one
    /// verification event per ready item, so a remediated run has more than
    /// one.
    private static func verifications(
        for runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> [Components.Schemas.TaskEventVerification] {
        var seen: Set<String> = []
        var verifications: [Components.Schemas.TaskEventVerification] = []
        for event in events(timeline) where event.run_id == runID {
            if let verification = event.verification?.value1, seen.insert(verification.item_id).inserted {
                verifications.append(verification)
            }
        }
        return verifications
    }

    /// The campaign a run belongs to, taken from its section. A daemon
    /// run-scoped event (milestone, review, verification, PR) carries only a
    /// run id, so its campaign is recovered here rather than read off the
    /// event.
    static func campaignID(
        forRun runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> String? {
        for section in timeline.sections where section.runs.contains(where: { $0.run_id == runID }) {
            return section.campaign_id
        }
        return nil
    }

    /// The run card's milestones as rail rows, newest first, read the way
    /// the run's own page reads them. The task history holds them newest
    /// first, so they are reversed to the oldest-first order the run page's
    /// rows take. With
    /// the run's review rounds loaded, each milestone names its
    /// invocation's role and the findings adjudications that started a
    /// remediation join the rail.
    static func milestoneRows(
        _ run: Components.Schemas.TaskTimelineRun, reviewRounds: [Components.Schemas.RunReviewRound] = [],
        stages: [Components.Schemas.Stage] = []
    ) -> [TimelineRail.Row] {
        RunHistoryPresentation.rows(
            milestones: run.milestones.reversed(),
            context: {
                RunHistoryPresentation.roleContext(invocationID: $0, reviewRounds: reviewRounds, stages: stages)
            },
            reviewRounds: reviewRounds)
    }

    /// The header's one meta line: project, issue, lifecycle, source.
    static func headerMetaLine(_ task: Components.Schemas.Task) -> String {
        ([TaskDisplay.projectName(task)] + [TaskDisplay.issueReference(task)].compactMap { $0 } + [
            TaskDisplay.lifecycleLabel(task).text, TaskDisplay.sourceLine(task),
        ]).joined(separator: " · ")
    }

    /// When a section's work began: its campaign's allocation when the
    /// daemon recorded one, else its earliest milestone. Nil for a section
    /// with neither, which then names no date.
    static func sectionStart(_ section: Components.Schemas.TaskTimelineSection) -> Date? {
        let events = section.events + section.runs.flatMap(\.events)
        if let allocated = events.filter({ $0.kind == .campaign_allocated }).map(\.recorded_at).min() {
            return allocated
        }
        return section.runs.flatMap(\.milestones).map(\.recorded_at).min()
    }

    /// The closed summary beside a section's keyword. The current section
    /// reads `current · 2 attempts · from Sep 11`; an earlier one reads
    /// `1 run · Sep 10`. Why an earlier campaign ended is not a recorded
    /// fact, so the summary does not say.
    static func sectionSummary(
        _ section: Components.Schemas.TaskTimelineSection, isCurrent: Bool, now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String {
        let count = section.runs.count
        let start = sectionStart(section).map {
            FreesideFormat.shortDate($0, now: now, locale: locale, timeZone: timeZone)
        }
        if isCurrent {
            return
                (["current", "\(count) \(count == 1 ? "attempt" : "attempts")"]
                + [start.map { "from \($0)" }]
                .compactMap { $0 }).joined(separator: " · ")
        }
        return (["\(count) \(count == 1 ? "run" : "runs")"] + [start].compactMap { $0 }).joined(separator: " · ")
    }

    /// The closed summary of a run's details: role, attempt reason, and the
    /// recorded hold. Nil when the run carries none of them, and then the
    /// disclosure has nothing to fold.
    static func runDetailsSummary(_ run: Components.Schemas.TaskTimelineRun) -> String? {
        let parts =
            [run.role.map { label($0.value1) }, run.attempt_reason]
            + [run.hold.map { "hold: \(RunDisplay.label($0.value1.reason))" }]
        let present = parts.compactMap { $0 }
        return present.isEmpty ? nil : present.joined(separator: " · ")
    }

    /// How a fact names a run this history holds, or nil for one it
    /// doesn't: `runReference` in sentence case, as a value that leads its
    /// own line. A run with neither role nor attempt number keeps its
    /// short id as written.
    static func runLinkTitle(_ runID: String, in timeline: Components.Schemas.TaskTimeline) -> String? {
        guard let run = timeline.sections.lazy.flatMap(\.runs).first(where: { $0.run_id == runID }) else {
            return nil
        }
        let reference = runReference(runID, in: timeline)
        guard run.role != nil || run.attempt_number != nil else { return reference }
        return reference.prefix(1).uppercased() + reference.dropFirst()
    }

    /// The page's Identifiers footer, in page order: the task, then each
    /// campaign, each followed by its attempts, every owner under the name
    /// the page gives it. An owner with no identifiers (a section outside a
    /// campaign with no approved specification) is left out.
    static func identifierGroups(
        _ timeline: Components.Schemas.TaskTimeline, taskID: String
    ) -> [IdentifierGroup] {
        let disambiguate = campaignCount(timeline) > 1
        var groups = [IdentifierGroup(owner: "Task", rows: technicalRows(taskID: taskID))]
        for section in timeline.sections {
            groups.append(
                .init(owner: sectionTitle(section, disambiguate: disambiguate), rows: technicalRows(section: section)))
            for run in section.runs {
                groups.append(
                    .init(owner: identifierOwner(run, in: timeline), rows: technicalRows(run: run, in: timeline)))
            }
        }
        return groups.filter { !$0.rows.isEmpty }
    }

    /// A run's name over its ids in the footer and in the copied text, where
    /// no card stands beside it to say which run it is: its role as
    /// `runLinkTitle` names it, since a campaign's specification run and its
    /// first implementation run both carry attempt 1, and the campaign too
    /// when attempts restart in another one, as the run card's spoken label
    /// does. A run with no role keeps its card title. No two owners read
    /// alike.
    static func identifierOwner(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        let title = run.role == nil ? runTitle(run) : runLinkTitle(run.run_id, in: timeline) ?? runTitle(run)
        guard !isUniqueRunControl(run, in: timeline), let campaign = campaignID(forRun: run.run_id, in: timeline)
        else { return title }
        return "\(title), campaign \(ShortIdentifier.short(campaign))"
    }

    /// What the closed footer says it holds: `task · campaign · 2 runs`.
    static func identifiersSummary(_ timeline: Components.Schemas.TaskTimeline) -> String {
        let campaigns = timeline.sections.count { !technicalRows(section: $0).isEmpty }
        let runs = timeline.sections.reduce(0) { $0 + $1.runs.count }
        return
            (["task"]
            + [campaigns > 0 ? (campaigns == 1 ? "campaign" : "\(campaigns) campaigns") : nil]
            + [runs > 0 ? "\(runs) \(runs == 1 ? "run" : "runs")" : nil])
            .compactMap { $0 }.joined(separator: " · ")
    }

    /// Words a copy item keeps capitalized: `ID` is an initialism and
    /// `Inbox` names the screen.
    private static let copyTitleKeptWords: Set<String> = ["ID", "Inbox"]

    /// A row's context-menu item: `Copy ` and the row's label in sentence
    /// case (`Copy parent run ID`, `Copy verified head 1`).
    static func copyMenuTitle(for label: String) -> String {
        let words = label.split(separator: " ").map { word in
            copyTitleKeptWords.contains(String(word)) ? String(word) : word.lowercased()
        }
        return (["Copy"] + words).joined(separator: " ")
    }

    /// `Copy all identifiers`: each group as a block, its owner on a line
    /// and each `Label: value` under it, a blank line between blocks, for
    /// pasting into a terminal or an issue.
    static func allIdentifiersText(_ groups: [IdentifierGroup]) -> String {
        groups.map { group in
            ([group.owner] + group.rows.map { "\($0.label): \($0.value)" }).joined(separator: "\n")
        }
        .joined(separator: "\n\n")
    }

    static func hasRunDetails(_ run: Components.Schemas.TaskTimelineRun) -> Bool {
        runDetailsSummary(run) != nil || run.superseded_by != nil || run.parent_run_id != nil
    }

    /// A run card's chip. The run the task currently stands on repeats the
    /// task's own status and cut. Every other run is history, so faint: its
    /// synced outcome in the run vocabulary, marked superseded when the
    /// timeline says so, and no chip when neither is known.
    static func runChip(
        _ run: Components.Schemas.TaskTimelineRun, task: Components.Schemas.Task,
        position: TaskDisplay.Position?, synced: Components.Schemas.Run?
    ) -> (label: String, cut: StateChip.Cut)? {
        if task.current_position?.value1.run_id == run.run_id {
            return (
                position?.status ?? TaskDisplay.rowStatus(task, run: synced),
                TaskDisplay.statusCut(task, position: position)
            )
        }
        let outcome = synced.map { RunDisplay.label($0.outcome) }
        let parts = [outcome, run.superseded_by == nil ? nil : "Superseded"].compactMap { $0 }
        return parts.isEmpty ? nil : (parts.joined(separator: " · "), .faint)
    }

    static func newestMilestoneTime(_ run: Components.Schemas.TaskTimelineRun) -> Date? {
        run.milestones.first?.recorded_at
    }

    static func label(_ role: Components.Schemas.TaskRunRole) -> String {
        switch role {
        case .specification: "Specification"
        case .implementation: "Implementation"
        }
    }

    static func label(_ kind: Components.Schemas.TaskEventKind) -> String {
        switch kind {
        case .task_created: "Task created"
        case .task_started: "Task started"
        case .task_completed: "Task work completed"
        case .task_abandoned: "Task work abandoned"
        case .stop_requested: "Stop requested"
        case .task_stopped: "Task stopped"
        case .stop_failed: "Stop failed"
        case .run_milestone: "Run activity"
        case .review_requested: "Review requested"
        case .review_completed: "Review completed"
        case .review_failed: "Review failed"
        case .verification_recorded: "Verification recorded"
        case .campaign_allocated: "Campaign allocated"
        case .specification_approved: "Specification approved"
        case .pr_opened: "PR opened"
        case .pr_merged: "PR merged"
        case .external_review_reentered: "External review re-entered"
        }
    }

    static func label(_ event: Components.Schemas.TaskEvent) -> String {
        if let milestone = event.milestone?.value1 {
            let detail = RunHistoryPresentation.detail(milestone)
            return RunDisplay.label(milestone.kind) + (detail.map { " · \($0)" } ?? "")
        }
        if let verification = event.verification?.value1 {
            return verification._class == .ready_clean
                ? "Verification recorded · Clean" : "Verification recorded · Degraded"
        }
        return label(event.kind)
    }
}
