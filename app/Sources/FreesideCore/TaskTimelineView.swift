import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

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
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale

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
    func screenshotContent(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        TaskTimelineView(
            coordinator: coordinator, snapshot: snapshot, onOpenRun: onOpenRun, screenshotTimeline: timeline)
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
        }
    }

    /// The eyebrow names the screen and the task id, as the run timeline's
    /// does, keeping the id's spelling for selection and the copy menu. One
    /// text, not a keyword beside a text: a task id is long, and at phone
    /// width it must wrap under the keyword rather than beside it.
    private var eyebrow: some View {
        Text("TASK TIMELINE · \(snapshot.task.id)")
            .font(FreesideFont.keyword)
            .tracking(0.8)
            .foregroundStyle(Color.inkDim)
            .textSelection(.enabled)
            .contextMenu {
                Button("Copy task ID") { copy(snapshot.task.id) }
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
                sectionView(section)
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
                    eventRows(events)
                }
            }
        }
    }

    private func sectionView(_ section: Components.Schemas.TaskTimelineSection) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            KeywordLabel(text: TaskTimelinePresentation.sectionTitle(section))
            if let campaignID = section.campaign_id {
                Text(campaignID)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
                    .textSelection(.enabled)
                    .contextMenu {
                        Button("Copy campaign ID") { copy(campaignID) }
                    }
            }
            ForEach(section.runs, id: \.run_id) { run in
                runCard(run)
            }
        }
    }

    private func runCard(_ run: Components.Schemas.TaskTimelineRun) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Button {
                onOpenRun(run.run_id)
            } label: {
                HStack(alignment: .firstTextBaseline) {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(TaskTimelinePresentation.runTitle(run))
                            .font(FreesideFont.sectionTitle)
                            .foregroundStyle(Color.ink)
                        Text(run.run_id)
                            .font(FreesideFont.monoCaption)
                            .foregroundStyle(Color.inkDim)
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
            .accessibilityLabel(
                "Open detailed run history for \(TaskTimelinePresentation.runTitle(run)), \(run.run_id)"
            )
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
                    Label("Superseded by \(successor)", systemImage: "arrow.turn.down.right")
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
                Text("Parent run: \(parent)")
                    .font(FreesideFont.monoCaption)
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
                        entries: TaskTimelinePresentation.milestoneEntries(run, locale: locale, timeZone: timeZone)),
                    axis: .vertical,
                    showsSummaryText: false,
                    accessibilityStyle: .entries)
            }
            if run.milestones.isEmpty {
                Text("No execution milestones in this run's task history.")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.ground2))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.rule, lineWidth: 1))
    }

    private func eventRows(_ events: [Components.Schemas.TaskEvent]) -> some View {
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
                    if let detail = TaskTimelinePresentation.detail(event) {
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
            event.recorded_at.formatted(
                Date.FormatStyle(date: .abbreviated, time: .shortened, locale: locale, timeZone: timeZone))
        )
        .font(FreesideFont.monoCaption)
        .foregroundStyle(Color.inkDim)
    }

    private func copy(_ string: String) {
        #if os(macOS)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(string, forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string = string
        #endif
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
    /// or missing its attempt number, by its id, as the run timeline does.
    static func runTitle(_ run: Components.Schemas.TaskTimelineRun) -> String {
        run.attempt_number.map { "Attempt \($0)" } ?? run.run_id
    }

    static func sectionTitle(_ section: Components.Schemas.TaskTimelineSection) -> String {
        section.campaign_id == nil ? "Outside a campaign" : "Campaign"
    }

    /// The daemon's milestones as rail entries in the order received (newest
    /// first), the first one current. Not reversed: the run rail reverses
    /// because its source is oldest first; this source already leads with
    /// the newest.
    static func milestoneEntries(
        _ run: Components.Schemas.TaskTimelineRun, locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [DecisionStageRailPresentation.Entry] {
        run.milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: RunHistoryPresentation.detail(milestone),
                timestamp: milestone.recorded_at.formatted(
                    Date.FormatStyle(date: .abbreviated, time: .shortened, locale: locale, timeZone: timeZone)),
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
    /// cannot read as a result for the task's current run or candidate.
    static func detail(_ event: Components.Schemas.TaskEvent) -> String? {
        var parts: [String] = []
        if let number = event.pr_number { parts.append("PR #\(number)") }
        if let campaign = event.campaign_id { parts.append(campaign) }
        if let run = event.run_id ?? event.specification_run_id { parts.append(run) }
        if let review = event.review?.value1 {
            parts += [
                "Round \(review.round)", "Head \(review.head_sha.prefix(12))", "Base \(review.base_sha.prefix(12))",
            ]
            if let failure = review.failure { parts.append(failure.replacingOccurrences(of: "_", with: " ")) }
        }
        if let verification = event.verification?.value1 {
            parts += [
                "Head \(verification.head_sha.prefix(12))", "Base \(verification.base_sha.prefix(12))",
                "Checklist in Inbox: \(verification.item_id)",
            ]
        }
        if let digest = event.approved_spec_digest?.value1 { parts.append(digest) }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }
}
