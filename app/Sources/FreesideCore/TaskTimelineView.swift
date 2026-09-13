import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

/// A task's history as the daemon computes it (`GET /tasks/{id}/timeline`):
/// the header names the task, then each campaign section lists its runs,
/// each run its milestones and events, and the task-level events close the
/// page. Every list keeps the daemon's newest-first order; unlike the run
/// rail, nothing here is reversed or sorted by timestamp.
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
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    private var timeline: Components.Schemas.TaskTimeline? {
        coordinator.taskTimelinesByTaskID[snapshot.task.id]
    }

    var body: some View {
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
    }

    /// The composition with fixture data supplied directly, because
    /// ImageRenderer never executes the loading task.
    @ViewBuilder
    func screenshotContent(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            header(timeline)
            content(timeline)
        }
        .padding(24)
        .frame(maxWidth: 820, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    private var header: some View {
        header(timeline)
    }

    private func header(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
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
                    TaskDisplay.isActive(task) ? "Active" : "Finished",
                    systemImage: TaskDisplay.isActive(task) ? "circle.dotted" : "checkmark.circle")
            }
            .font(FreesideFont.subheadline)
            .foregroundStyle(Color.inkDim)
            Text(TaskDisplay.sourceLine(task))
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
            KeywordLabel(text: "Daemon observations")
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

    /// Sections first, newest campaign leading, then the task-level events:
    /// the newest activity leads and the creation event closes the page.
    private func content(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            ForEach(Array(timeline.sections.enumerated()), id: \.offset) { _, section in
                sectionView(section)
            }
            if !timeline.events.isEmpty {
                VStack(alignment: .leading, spacing: 10) {
                    Text("Task Events")
                        .font(FreesideFont.title)
                    eventRows(timeline.events)
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
            if !section.events.isEmpty {
                eventRows(section.events)
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
                    }
                    Spacer()
                    Image(systemName: "chevron.right")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.accentText)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Open \(TaskTimelinePresentation.runTitle(run)), \(run.run_id)")
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
                Label(RunDisplay.label(hold.reason), systemImage: "pause.circle.fill")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.accentText)
            }
            if !run.milestones.isEmpty {
                KeywordLabel(text: "Milestones")
                    .padding(.top, 4)
                StageRail(
                    title: nil,
                    presentation: .timeline(entries: TaskTimelinePresentation.milestoneEntries(run)),
                    axis: .vertical,
                    showsSummaryText: false,
                    accessibilityStyle: .entries)
            }
            if !run.events.isEmpty {
                eventRows(run.events)
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
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(TaskTimelinePresentation.label(event.kind))
                        .font(FreesideFont.callout)
                    if let detail = TaskTimelinePresentation.detail(event) {
                        Text(detail)
                            .font(FreesideFont.monoCaption)
                            .foregroundStyle(Color.inkDim)
                    }
                    Spacer(minLength: 8)
                    Text(event.recorded_at.formatted(date: .abbreviated, time: .shortened))
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkDim)
                }
            }
        }
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

/// The task timeline's labels and its one ordering rule: everything renders
/// in the order the daemon computed it.
enum TaskTimelinePresentation {
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
            entries += section.events.map { .event($0.kind) }
            for run in section.runs {
                entries.append(.run(run.run_id))
                entries += run.milestones.map { .milestone(runID: run.run_id, kind: $0.kind) }
                entries += run.events.map { .event($0.kind) }
            }
        }
        entries += timeline.events.map { .event($0.kind) }
        return entries
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
        _ run: Components.Schemas.TaskTimelineRun
    ) -> [DecisionStageRailPresentation.Entry] {
        run.milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: RunHistoryPresentation.detail(milestone),
                timestamp: milestone.recorded_at.formatted(date: .abbreviated, time: .shortened),
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
        case .campaign_allocated: "Campaign allocated"
        case .specification_approved: "Specification approved"
        case .pr_opened: "PR opened"
        case .pr_merged: "PR merged"
        }
    }

    /// The event's reference, when it carries one: the PR, the approved
    /// specification's digest, or the run that supplied it.
    static func detail(_ event: Components.Schemas.TaskEvent) -> String? {
        if let number = event.pr_number {
            return "PR #\(number)"
        }
        if let digest = event.approved_spec_digest?.value1 {
            return digest
        }
        if let runID = event.specification_run_id {
            return runID
        }
        return nil
    }
}
