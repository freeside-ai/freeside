import FreesideAPI
import SwiftUI

/// A task's history as the daemon computes it (`GET /tasks/{id}/timeline`):
/// task-wide events lead the page, then campaign sections list their runs
/// and detailed milestones. Every list keeps the daemon's newest-first order;
/// unlike the run rail, nothing here is reversed or sorted by timestamp.
struct TaskTimelineView: View {
    /// Keys the timeline refetch task, as `RunTimelineView.TimelineRequestKey`
    /// does: the task's own revision plus the epoch and full-snapshot
    /// revision, so every same-epoch bootstrap refetches while cached
    /// content stays on screen.
    struct TimelineRequestKey: Hashable {
        let taskID: String
        let syncEpoch: String?
        let lastFullSnapshotRevision: Int64?
        let revision: Int64

        init(snapshot: Components.Schemas.TaskSnapshot, cursors: SyncCursors?) {
            taskID = snapshot.task.id
            syncEpoch = cursors?.syncEpoch
            lastFullSnapshotRevision = cursors?.lastFullSnapshotRevision
            revision = snapshot.as_of_revision
        }
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.TaskSnapshot
    /// Opens a run's timeline from its section header: the iOS stack pushes
    /// it, the macOS detail column shows it in place of this view.
    let onOpenRun: (String) -> Void
    /// Supplies the existing screenshot composition without loading tasks or
    /// a scroll viewport. Rendering through body installs environment values
    /// before the content helpers read locale, time zone, and text size.
    var screenshotTimeline: Components.Schemas.TaskTimeline?
    /// Screenshot-only: start every technical-details disclosure expanded so a
    /// baseline can capture the section's rows and copy controls. Live use
    /// leaves it false, so the sections open only on tap.
    var expandsTechnicalDetails = false
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow

    private var timeline: Components.Schemas.TaskTimeline? {
        coordinator.taskTimelinesByTaskID[snapshot.task.id]
    }

    @ViewBuilder var body: some View {
        if let screenshotTimeline {
            VStack(alignment: .leading, spacing: 22) {
                header(screenshotTimeline)
                content(screenshotTimeline)
            }
            .padding(24)
            .frame(maxWidth: 820, alignment: .leading)
            .foregroundStyle(Color.ink)
        } else {
            liveContent
        }
    }

    private var liveContent: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                if let timeline {
                    content(timeline)
                } else if coordinator.taskTimelineLoadStates[snapshot.task.id] == .unavailable {
                    UnavailableStateView(
                        title: "Timeline unavailable",
                        systemImage: "exclamationmark.triangle",
                        description: "Freeside could not load the daemon's history for this task."
                    )
                    .frame(maxWidth: .infinity, minHeight: 180)
                } else {
                    ProgressView("Loading timeline…")
                        .tint(.waterText)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                        .frame(maxWidth: .infinity, minHeight: 180)
                }
            }
            .padding(24)
            .frame(maxWidth: 820, alignment: .leading)
            .foregroundStyle(Color.ink)
        }
        .navigationTitle(TaskDisplay.projectName(snapshot.task))
        .task(id: TimelineRequestKey(snapshot: snapshot, cursors: coordinator.cursors)) {
            await coordinator.refreshTaskTimeline(for: snapshot.task.id)
        }
        .task(id: coordinator.taskReviewRequestKey(for: snapshot.task.id, revision: snapshot.as_of_revision)) {
            await coordinator.refreshTaskReviews(for: snapshot.task.id, revision: snapshot.as_of_revision)
        }
    }

    /// The composition with fixture data supplied directly, because
    /// ImageRenderer never executes the loading task.
    func screenshotContent(
        _ timeline: Components.Schemas.TaskTimeline, expandsTechnicalDetails: Bool = false
    ) -> some View {
        TaskTimelineView(
            coordinator: coordinator, snapshot: snapshot, onOpenRun: onOpenRun, screenshotTimeline: timeline,
            expandsTechnicalDetails: expandsTechnicalDetails)
    }

    private var header: some View {
        header(timeline)
    }

    private func header(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
        let lifecycle = TaskDisplay.lifecycleLabel(task)
        return VStack(alignment: .leading, spacing: 10) {
            eyebrow
            TaskNameLabel(
                name: TaskTimelinePresentation.headerName(timeline: timeline, snapshot: snapshot),
                font: FreesideFont.largeTitle,
                monoFont: FreesideFont.mono(.title2),
                lineLimit: 3)
            HStack(spacing: 14) {
                Label(TaskDisplay.projectName(task), systemImage: "folder")
                if let issue = TaskDisplay.issueReference(task) {
                    Label(issue, systemImage: "number")
                }
                Label(
                    lifecycle.text,
                    systemImage: lifecycle.systemImage)
            }
            .font(FreesideFont.subheadline)
            .foregroundStyle(Color.inkDim)
            Text(TaskDisplay.sourceLine(task))
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
            KeywordLabel(text: "Daemon observations")
            TaskStopView(coordinator: coordinator, taskID: task.id)
            TechnicalDetailsSection(
                rows: TaskTimelinePresentation.technicalRows(taskID: task.id),
                startsExpanded: expandsTechnicalDetails)
        }
    }

    /// The eyebrow names the screen. The task id moved to the header's
    /// technical details; the copy menu keeps a quick path to it.
    private var eyebrow: some View {
        Text("TASK TIMELINE")
            .font(FreesideFont.keyword)
            .tracking(0.8)
            .foregroundStyle(Color.inkDim)
            .contextMenu {
                Button("Copy task ID") { Clipboard.copy(snapshot.task.id) }
            }
    }

    /// Campaign and run details lead; recorded task events follow the work.
    private func content(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        let events = TaskTimelinePresentation.events(timeline)
        return VStack(alignment: .leading, spacing: 22) {
            if let message = TaskTimelinePresentation.availabilityMessage(
                state: coordinator.taskTimelineLoadStates[snapshot.task.id], freshness: coordinator.store.freshness)
            {
                Text(message)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            if timeline.sections.isEmpty {
                Text("No campaigns or runs in this history.")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            ForEach(Array(timeline.sections.enumerated()), id: \.offset) { _, section in
                sectionView(section, in: timeline)
            }
            VStack(alignment: .leading, spacing: 10) {
                Text("Task Events").font(FreesideFont.title)
                Text(
                    "Recorded workflow milestones, newest first. Historical results do not establish current readiness."
                )
                .font(FreesideFont.callout)
                .foregroundStyle(Color.inkDim)
                if events.isEmpty {
                    Text("No recorded events in this history.")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                } else {
                    eventRows(events, in: timeline)
                }
            }
        }
    }

    private func sectionView(
        _ section: Components.Schemas.TaskTimelineSection, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            KeywordLabel(
                text: TaskTimelinePresentation.sectionTitle(
                    section, disambiguate: TaskTimelinePresentation.campaignCount(timeline) > 1))
            TechnicalDetailsSection(
                rows: TaskTimelinePresentation.technicalRows(section: section),
                startsExpanded: expandsTechnicalDetails)
            ForEach(section.runs, id: \.run_id) { run in
                runCard(run, in: timeline)
            }
        }
    }

    private func runCard(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Button {
                onOpenRun(run.run_id)
            } label: {
                HStack(alignment: .firstTextBaseline) {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(TaskTimelinePresentation.runTitle(run))
                            .font(FreesideFont.sectionTitle)
                            .foregroundStyle(Color.ink)
                        Text("Open run history")
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.accentText)
                    }
                    Spacer()
                    Image(systemName: "chevron.right")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.accentText)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(TaskTimelinePresentation.runCardAccessibilityLabel(run, in: timeline))
            .accessibilityHint("Shows this run's recorded milestones and invocation observations.")
            if run.role == nil || run.role?.value1 == .implementation {
                RunReviewSection(
                    coordinator: coordinator, runID: run.run_id,
                    facts: coordinator.timelinesByRunID[run.run_id]?.review?.value1,
                    hasTimeline: coordinator.timelinesByRunID[run.run_id] != nil)
                Divider().padding(.vertical, 6)
            }
            HStack(spacing: 14) {
                if let role = run.role?.value1 {
                    Label(TaskTimelinePresentation.label(role), systemImage: "square.stack.3d.up")
                }
                if let successor = run.superseded_by {
                    Label(
                        "Superseded by \(TaskTimelinePresentation.runReference(successor, in: timeline))",
                        systemImage: "arrow.turn.down.right")
                }
            }
            .font(FreesideFont.subheadline)
            .foregroundStyle(Color.inkDim)
            if let reason = run.attempt_reason {
                Text("Reason: \(reason)")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            if let parent = run.parent_run_id {
                Text("Parent run: \(TaskTimelinePresentation.runReference(parent, in: timeline))")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            if let hold = run.hold?.value1 {
                Label("Recorded hold: \(RunDisplay.label(hold.reason))", systemImage: "pause.circle.fill")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.accentText)
                Text("Hold code: \(hold.reason.rawValue)")
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
            KeywordLabel(text: "Run Activity")
            if !run.milestones.isEmpty {
                KeywordLabel(text: "Milestones")
                    .padding(.top, 4)
                StageRail(
                    title: nil,
                    presentation: .timeline(
                        entries: TaskTimelinePresentation.milestoneEntries(
                            run, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)),
                    axis: .vertical,
                    showsSummaryText: false,
                    accessibilityStyle: .entries)
            }
            if run.milestones.isEmpty {
                Text("No execution milestones in this run's task history.")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            TechnicalDetailsSection(
                rows: TaskTimelinePresentation.technicalRows(run: run, in: timeline),
                startsExpanded: expandsTechnicalDetails)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.ground2))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.rule, lineWidth: 1))
    }

    private func eventRows(
        _ events: [Components.Schemas.TaskEvent], in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(Array(events.enumerated()), id: \.offset) { _, event in
                VStack(alignment: .leading, spacing: 4) {
                    ViewThatFits(in: .horizontal) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            eventLabel(event)
                            Spacer(minLength: 8)
                            eventTime(event)
                        }
                        VStack(alignment: .leading, spacing: 4) {
                            eventLabel(event)
                            eventTime(event)
                        }
                    }
                    if let detail = TaskTimelinePresentation.detail(event, in: timeline) {
                        Text(detail)
                            .font(FreesideFont.monoCaption)
                            .foregroundStyle(Color.inkDim)
                            .fixedSize(horizontal: false, vertical: true)
                            .textSelection(.enabled)
                    }
                }
            }
        }
    }

    private func eventLabel(_ event: Components.Schemas.TaskEvent) -> some View {
        Text(TaskTimelinePresentation.label(event)).font(FreesideFont.callout)
    }

    private func eventTime(_ event: Components.Schemas.TaskEvent) -> some View {
        Text(
            FreesideFormat.shortTime(
                event.recorded_at, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
        )
        .font(FreesideFont.monoCaption)
        .foregroundStyle(Color.inkDim)
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
        if state != .loaded || freshness != .fresh { return "Saved task history. Freshness unconfirmed." }
        return nil
    }

    /// The page's elements flattened in render order, so a test can pin the
    /// order without a view.
    enum Entry: Equatable {
        case section(campaignID: String?)
        case run(String)
        case milestone(runID: String, kind: Components.Schemas.RunMilestoneKind)
        case event(Components.Schemas.TaskEventKind)
    }

    static func entries(_ timeline: Components.Schemas.TaskTimeline) -> [Entry] {
        var entries: [Entry] = []
        for section in timeline.sections {
            entries.append(.section(campaignID: section.campaign_id))
            for run in section.runs {
                entries.append(.run(run.run_id))
                entries += run.milestones.map { .milestone(runID: run.run_id, kind: $0.kind) }
            }
        }
        return entries + events(timeline).map { .event($0.kind) }
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
    /// does. The exact id is in the run's technical details.
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

    /// The number of campaign sections, so a title or event line shows a
    /// disambiguating short campaign id only when more than one campaign is on
    /// screen.
    static func campaignCount(_ timeline: Components.Schemas.TaskTimeline) -> Int {
        timeline.sections.count { $0.campaign_id != nil }
    }

    static func sectionTitle(
        _ section: Components.Schemas.TaskTimelineSection, disambiguate: Bool = false
    ) -> String {
        guard let campaign = section.campaign_id else { return "Outside a campaign" }
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

    /// The task header's technical details: the exact task id.
    static func technicalRows(taskID: String) -> [AttentionDisplay.BindingRow] {
        [.init(label: "Task ID", value: taskID)]
    }

    /// A campaign section's technical details: the exact campaign id and, when
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
            rows.append(.init(label: "Approved specification digest", value: digest))
        }
        return rows
    }

    /// A run card's technical details: the exact run, parent, and superseding
    /// ids, and every verification Inbox item id a verification event records
    /// for this run. A run that returned to the agent or remediated can record
    /// several, each its own ready item, so all are kept, not just the first.
    static func technicalRows(
        run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [.init(label: "Run ID", value: run.run_id)]
        if let parent = run.parent_run_id {
            rows.append(.init(label: "Parent run ID", value: parent))
        }
        if let successor = run.superseded_by {
            rows.append(.init(label: "Superseded by run ID", value: successor))
        }
        // Number the rows when a remediated run recorded more than one, so each
        // row's copy control has a distinct accessibility label.
        let itemIDs = verificationItemIDs(for: run.run_id, in: timeline)
        for (index, itemID) in itemIDs.enumerated() {
            let label =
                itemIDs.count > 1
                ? "Verification Inbox item ID \(index + 1)" : "Verification Inbox item ID"
            rows.append(.init(label: label, value: itemID))
        }
        return rows
    }

    /// Every distinct verification Inbox item id a verification event records
    /// for the run, in event order. The daemon emits one verification event per
    /// ready item, so a remediated run has more than one.
    private static func verificationItemIDs(
        for runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> [String] {
        var seen: Set<String> = []
        var ids: [String] = []
        for event in events(timeline) where event.run_id == runID {
            if let itemID = event.verification?.value1.item_id, seen.insert(itemID).inserted {
                ids.append(itemID)
            }
        }
        return ids
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

    /// A task event's run named by its role and attempt when the run is in the
    /// timeline, so an event line stays legible without the opaque run id.
    private static func runContext(
        _ runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        for section in timeline.sections {
            guard let run = section.runs.first(where: { $0.run_id == runID }) else { continue }
            switch run.role?.value1 {
            case .specification:
                return "Specification run"
            case .implementation:
                return run.attempt_number.map { "Implementation attempt \($0)" } ?? "Implementation run"
            case nil:
                return run.attempt_number.map { "Attempt \($0)" } ?? "Run \(ShortIdentifier.short(runID))"
            }
        }
        return "Run \(ShortIdentifier.short(runID))"
    }

    /// The daemon's milestones as rail entries in the order received (newest
    /// first), the first one current. Not reversed: the run rail reverses
    /// because its source is oldest first; this source already leads with
    /// the newest.
    static func milestoneEntries(
        _ run: Components.Schemas.TaskTimelineRun, now: Date = Date(), locale: Locale = .current,
        timeZone: TimeZone = .current
    ) -> [DecisionStageRailPresentation.Entry] {
        run.milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: RunHistoryPresentation.detail(milestone),
                timestamp: FreesideFormat.shortTime(
                    milestone.recorded_at, now: now, locale: locale, timeZone: timeZone),
                state: index == 0 ? .current : .completed)
        }
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
        }
    }

    static func label(_ event: Components.Schemas.TaskEvent) -> String {
        if let milestone = event.milestone?.value1 {
            let detail = RunHistoryPresentation.detail(milestone)
            return RunDisplay.label(milestone.kind) + (detail.map { " · \($0)" } ?? "")
        }
        if let outcome = event.review?.value1.outcome?.value1 {
            return "Review completed · \(outcome.rawValue.replacingOccurrences(of: "_", with: " ").capitalized)"
        }
        if let verification = event.verification?.value1 {
            return verification._class == .ready_clean
                ? "Verification recorded · Clean" : "Verification recorded · Degraded"
        }
        return label(event.kind)
    }

    /// Keep the source beside the summary so old campaigns and review rounds
    /// cannot read as a result for the task's current run or candidate: the
    /// run is named by its role and attempt, and a second campaign is named by
    /// its short id. The exact ids and digests live in each section's and
    /// run's technical details; this line carries meaning, not opaque values.
    static func detail(
        _ event: Components.Schemas.TaskEvent, in timeline: Components.Schemas.TaskTimeline
    ) -> String? {
        var parts: [String] = []
        if let number = event.pr_number { parts.append("PR #\(number)") }
        let eventRunID = event.run_id ?? event.specification_run_id
        // A run-scoped event carries no campaign id of its own, so recover the
        // campaign from the run's section; otherwise two campaigns' events
        // (same role, same restarted attempt) read alike and an old campaign's
        // result can pass for the current one.
        if campaignCount(timeline) > 1,
            let campaign = event.campaign_id
                ?? eventRunID.flatMap({ campaignID(forRun: $0, in: timeline) })
        {
            parts.append(ShortIdentifier.short(campaign))
        }
        if let run = eventRunID {
            parts.append(runContext(run, in: timeline))
        }
        if let review = event.review?.value1 {
            parts += [
                "Round \(review.round)", "Head \(review.head_sha.prefix(12))", "Base \(review.base_sha.prefix(12))",
            ]
            if let failure = review.failure { parts.append(failure.replacingOccurrences(of: "_", with: " ")) }
        }
        if let verification = event.verification?.value1 {
            parts += [
                "Head \(verification.head_sha.prefix(12))", "Base \(verification.base_sha.prefix(12))",
                "Checklist in Inbox",
            ]
        }
        if let digest = event.approved_spec_digest?.value1 {
            parts.append(ShortIdentifier.short(digest))
        }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }
}
