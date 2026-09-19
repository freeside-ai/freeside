import FreesideAPI
import SwiftUI

struct RunTimelineView: View {
    /// Keys the timeline refetch task. It changes on the run's own revision
    /// and, through `lastFullSnapshotRevision`, on every same-epoch bootstrap:
    /// a live run bootstraps each round without its own row changing, and the
    /// detail must refetch then to keep the observations current. The epoch
    /// participates because restored revisions are incomparable and may equal
    /// an old value. Internal, not private, so a test can build it without a
    /// view.
    struct TimelineRequestKey: Hashable {
        let runID: String
        let syncEpoch: String?
        let lastFullSnapshotRevision: Int64?
        let revision: Int64

        init(snapshot: Components.Schemas.RunSnapshot, cursors: SyncCursors?) {
            runID = snapshot.run.id
            syncEpoch = cursors?.syncEpoch
            lastFullSnapshotRevision = cursors?.lastFullSnapshotRevision
            revision = snapshot.as_of_revision
        }
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.RunSnapshot
    /// Screenshot-only: start the header's technical-details disclosure
    /// expanded so a baseline can capture its rows and copy controls. Live use
    /// leaves it false.
    var expandsTechnicalDetails = false
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    private var timeline: Components.Schemas.RunTimeline? {
        coordinator.timelinesByRunID[snapshot.run.id]
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header(at: dynamicTypeSize)
                if let timeline {
                    if let hold = timeline.hold?.value1 {
                        holdCard(hold)
                    }
                    reviewSection(timeline)
                    timelineSection(timeline)
                    invocationSection(timeline)
                } else if coordinator.timelineLoadStates[snapshot.run.id] == .unavailable {
                    UnavailableStateView(
                        title: "Timeline unavailable",
                        systemImage: "exclamationmark.triangle",
                        description: "Freeside could not load daemon observations for this run."
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
        .navigationTitle(snapshot.run.project_id)
        // A bootstrap now keeps the cached timeline (SyncCoordinator.adopt);
        // this key still changes on every same-epoch bootstrap, so the view
        // refetches to replace the retained projection while cached content
        // stays on screen. The three body branches keep their order, so
        // cached content wins over a `.loading` spinner between rounds.
        .task(id: TimelineRequestKey(snapshot: snapshot, cursors: coordinator.cursors)) {
            await coordinator.refreshTimeline(for: snapshot.run.id)
        }
        .task(id: TimelineRequestKey(snapshot: snapshot, cursors: coordinator.cursors)) {
            await coordinator.refreshTaskTimeline(for: snapshot.run.task_id)
        }
    }

    /// The project-owned timeline composition with fixture data supplied
    /// directly because ImageRenderer never executes the loading task.
    @ViewBuilder
    func screenshotContent(_ timeline: Components.Schemas.RunTimeline, at size: DynamicTypeSize) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            header(at: size)
            if let hold = timeline.hold?.value1 {
                holdCard(hold)
            }
            reviewSection(timeline, persistsFolds: false)
            timelineSection(timeline)
            invocationSection(timeline)
        }
        .padding(24)
        .frame(maxWidth: 820, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    func header(at size: DynamicTypeSize) -> some View {
        let accessibilityLayout = size >= .accessibility1
        let layout =
            accessibilityLayout
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
            : AnyLayout(HStackLayout())
        return VStack(alignment: .leading, spacing: 10) {
            layout {
                VStack(alignment: .leading, spacing: 4) {
                    eyebrow
                    Text(RunDisplay.timelineTitle(snapshot.run))
                        .font(FreesideFont.largeTitle)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                if !accessibilityLayout {
                    Spacer()
                }
                RunOutcomeBadge(outcome: snapshot.run.outcome)
            }
            HStack(spacing: 14) {
                if let heading = RunDisplay.stageHeading(
                    snapshot.run,
                    task: coordinator.tasks.first { $0.task.id == snapshot.run.task_id }?.task,
                    attentionItems: coordinator.store.orderedSnapshots)
                {
                    Label(heading.label, systemImage: "square.stack.3d.up")
                    if let round = heading.round {
                        Label(round, systemImage: "arrow.triangle.2.circlepath")
                    }
                }
                if let milestone = snapshot.run.latest_milestone?.value1 {
                    Label(RunDisplay.label(milestone), systemImage: "point.topleft.down.to.point.bottomright.curvepath")
                }
            }
            .font(FreesideFont.subheadline)
            .foregroundStyle(Color.inkDim)
            VStack(alignment: .leading, spacing: 4) {
                if let reason = snapshot.run.attempt_reason {
                    Text("Reason: \(reason)")
                        .font(FreesideFont.callout)
                }
                Text(RunDisplay.specificationHeaderLabel(snapshot.run, approval: specificationApproval))
                    .font(FreesideFont.callout)
                if let qualification = specificationApproval.qualification {
                    Text(qualification)
                        .font(FreesideFont.caption)
                }
            }
            .foregroundStyle(Color.inkDim)
            TechnicalDetailsSection(rows: technicalRows, startsExpanded: expandsTechnicalDetails)
            KeywordLabel(text: "Daemon observations")
        }
    }

    /// The review folds persist with the task's other folds, so a round
    /// opened here is open on the task timeline too. A screenshot leaves
    /// them view-local.
    private func reviewSection(
        _ timeline: Components.Schemas.RunTimeline, persistsFolds: Bool = true
    ) -> some View {
        let folds = TaskTimelineDisclosurePreferences.shared
        let taskID = snapshot.run.task_id
        return RunReviewSection(
            coordinator: coordinator, runID: snapshot.run.id, facts: timeline.review?.value1,
            folds: !persistsFolds
                ? nil
                : .init(
                    facts: { folds.binding(.roundFacts($0), taskID: taskID) },
                    priorRounds: folds.binding(.priorRounds(snapshot.run.id), taskID: taskID)))
    }

    private var technicalRows: [AttentionDisplay.BindingRow] {
        RunTimelineView.technicalRows(run: snapshot.run, specificationLabel: specificationLabel)
    }

    /// The header's technical details: the exact run, task, campaign, and
    /// parent ids, and the specification digest under its own label. The
    /// primary text names the specification; the digest stays here to compare
    /// or paste. Pure, so a test can check each value against its source field.
    static func technicalRows(
        run: Components.Schemas.Run, specificationLabel: String
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [
            .init(label: "Run ID", value: run.id),
            .init(label: "Task ID", value: run.task_id),
        ]
        if let campaignID = run.campaign_id {
            rows.append(.init(label: "Campaign ID", value: campaignID))
        }
        if let parent = run.parent_run_id {
            rows.append(.init(label: "Parent run ID", value: parent))
        }
        rows.append(.init(label: specificationLabel, value: run.spec_digest))
        return rows
    }

    var specificationApproval: TaskDisplay.SpecificationApproval {
        guard let task = coordinator.tasks.first(where: { $0.task.id == snapshot.run.task_id })?.task else {
            return .unavailable
        }
        return TaskDisplay.specificationApproval(
            task, runID: snapshot.run.id, run: snapshot.run,
            history: coordinator.taskTimelinesByTaskID[task.id])
    }

    var specificationLabel: String {
        RunDisplay.specificationLabel(snapshot.run, approval: specificationApproval)
    }

    /// The eyebrow names the screen and the task the run belongs to, in the
    /// task's own label style (mono for an identifier fallback, the Agent
    /// keyword after an agent-proposed name). The run id stays in the copy
    /// context menu and, for a run outside a campaign, in the title.
    private var eyebrow: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            KeywordLabel(text: "Run timeline")
            Text("·")
                .font(FreesideFont.keyword)
                .foregroundStyle(Color.inkDim)
                .accessibilityHidden(true)
            TaskNameLabel(
                name: TaskDisplay.name(for: snapshot.run, tasks: coordinator.tasks),
                font: FreesideFont.subheadline,
                monoFont: FreesideFont.monoCaption,
                color: .inkDim,
                lineLimit: 1)
        }
        .contextMenu {
            Button("Copy run ID") {
                Clipboard.copy(snapshot.run.id)
            }
        }
    }

    /// Mirrors the run list: a hold is attention, but on a failed or
    /// lost run it reads as part of the failure and keeps wax.
    private var holdIsFailure: Bool {
        switch snapshot.run.outcome {
        case .failed, .lost: true
        case .unobserved, .pending, .published, .blocked, .completed: false
        }
    }

    private func holdCard(_ hold: Components.Schemas.RunHold) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: "Recorded hold", color: holdIsFailure ? .waxText : .accentText)
            Text(RunDisplay.label(hold.reason))
                .font(FreesideFont.sectionTitle)
            Text(
                "Observed \(hold.first_observed_at.formatted(date: .abbreviated, time: .shortened)) to \(hold.last_observed_at.formatted(date: .omitted, time: .shortened))"
            )
            .font(FreesideFont.monoCaption)
            .foregroundStyle(Color.inkDim)
            Text("Hold code: \(hold.reason.rawValue)")
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(holdIsFailure ? Color.waxWash : Color.accentWash))
        .overlay(
            RoundedRectangle(cornerRadius: 8).strokeBorder(
                holdIsFailure ? Color.waxText : Color.accentBorder, lineWidth: 1)
        )
    }

    private func timelineSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        let entries = RunHistoryPresentation.entries(
            milestones: timeline.milestones,
            detail: milestoneDetail,
            context: attemptContext)
        return StageRail(
            title: "Stage, Round & Decision History",
            presentation: .timeline(entries: entries),
            axis: .vertical,
            showsSummaryText: false,
            accessibilityStyle: .entries)
    }

    private func invocationSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        let groups = RunTimelineGrouping.groups(
            invocations: timeline.invocations, stages: snapshot.run.stages,
            reviewRounds: timeline.review?.value1.rounds ?? [],
            milestones: timeline.milestones)
        return VStack(alignment: .leading, spacing: 10) {
            Text("Latest Invocation Observations")
                .font(FreesideFont.title)
            ForEach(groups) { group in
                KeywordLabel(text: group.label)
                    .padding(.top, 4)
                ForEach(Array(group.invocations.enumerated()), id: \.element.invocation_id) { index, invocation in
                    if index > 0 {
                        Divider().overlay(Color.rule)
                    }
                    invocationRow(invocation, asOf: timeline.as_of)
                }
            }
        }
    }

    private func invocationRow(
        _ invocation: Components.Schemas.InvocationObservation, asOf: Date
    ) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 3) {
                Text(attemptContext(invocationID: invocation.invocation_id) ?? invocation.invocation_id)
                    .font(FreesideFont.sans(.headline, weight: .semibold))
                // The observed time is freshness (the daemon's last look), not
                // the attempt's place in history; the "Observed" prefix says so.
                Text("Observed \(invocation.observed_at.formatted(date: .abbreviated, time: .shortened))")
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
            Spacer()
            let presentation = InvocationPresentation(invocation, asOf: asOf)
            StateChip(label: presentation.label, color: presentation.color, glyph: presentation.glyph)
        }
        .padding(.vertical, 6)
    }

    private func attemptContext(invocationID: String?) -> String? {
        RunHistoryPresentation.attemptContext(
            invocationID: invocationID,
            stages: snapshot.run.stages,
            reviewRounds: timeline?.review?.value1.rounds ?? [])
    }

    private func milestoneDetail(_ milestone: Components.Schemas.RunMilestone) -> String? {
        RunHistoryPresentation.detail(milestone)
    }
}

/// Orders the run detail's history surfaces newest first without sorting by
/// timestamp. The daemon records milestones oldest first (`ORDER BY id`) and
/// review rounds in ascending round order, so reversing that record order
/// puts the latest entry on top while keeping equal-timestamp entries in a
/// deterministic, tie-stable order and keeping `.current` on the daemon's
/// last-recorded milestone.
enum RunHistoryPresentation {
    /// Builds the decision-history rail entries in daemon order (last one
    /// `.current`), then returns them reversed so the newest leads. `detail`
    /// and `context` stay the view's own closures over run state.
    static func entries(
        milestones: [Components.Schemas.RunMilestone],
        detail: (Components.Schemas.RunMilestone) -> String?,
        context: (String?) -> String?
    ) -> [DecisionStageRailPresentation.Entry] {
        let ordered = milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: detail(milestone),
                context: context(milestone.invocation_id),
                timestamp: milestone.recorded_at.formatted(
                    date: .abbreviated, time: .shortened),
                state: index == milestones.count - 1 ? .current : .completed)
        }
        return Array(ordered.reversed())
    }

    /// A milestone's detail: the terminal state, else the outcome, else
    /// the hold reason it recorded. Shared with the task timeline's run
    /// sections so both read a milestone the same way.
    static func detail(_ milestone: Components.Schemas.RunMilestone) -> String? {
        if let terminal = milestone.terminal?.value1 {
            return terminal.rawValue.capitalized
        }
        if let outcome = milestone.outcome?.value1 {
            return outcome.rawValue.capitalized
        }
        if let reason = milestone.reason?.value1 {
            return "Recorded hold: \(RunDisplay.label(reason)) (\(reason.rawValue))"
        }
        return nil
    }

    /// Review rounds newest round first. The daemon supplies them ascending,
    /// so reversing keeps ties in reverse record order without a timestamp
    /// sort. Missing facts yield no rounds.
    static func rounds(
        _ facts: Components.Schemas.RunReviewFacts?
    ) -> [Components.Schemas.RunReviewRound] {
        guard let facts else { return [] }
        return Array(facts.rounds.reversed())
    }

    /// The row label for an attempt or review round, shared by the decision
    /// history rail and the invocation observations.
    ///
    /// A review round reads `Review · Round <n>` and takes precedence. Every
    /// other invocation reads `<Stage> · Round <n>` with the daemon's
    /// per-stage `Attempt.number`. That number restarts at 1 for each stage,
    /// and the daemon appends a fresh `implement` stage for each remediation
    /// and operator-feedback pass, so a run that recorded the same stage more
    /// than once would show several rows all labelled "Round 1". When the
    /// owning stage's canonical name (folding `implement` into
    /// `implementation`) repeats across `run.stages`, the label also names the
    /// pass in stage creation order: `Implementation · Pass 2 · Round 1`. The
    /// label says "Pass" rather than "remediation" or "operator feedback"
    /// because the API `Stage` carries no field recording why a stage was
    /// added, so the reason can't be named without guessing.
    ///
    /// Returns nil when the invocation matches no attempt or review round, so
    /// the caller falls back to the invocation id.
    static func attemptContext(
        invocationID: String?,
        stages: [Components.Schemas.Stage],
        reviewRounds: [Components.Schemas.RunReviewRound]
    ) -> String? {
        guard let invocationID else { return nil }
        if let round = reviewRounds.first(where: { $0.invocation_id == invocationID }) {
            return "Review · Round \(round.round)"
        }
        for (index, stage) in stages.enumerated() {
            guard let attempt = stage.attempts.first(where: { $0.invocation_id == invocationID })
            else { continue }
            let label = RunDisplay.stageLabel(stage.name)
            let canonical = RunDisplay.canonicalStageName(stage.name)
            let sameName = stages.enumerated().filter {
                RunDisplay.canonicalStageName($0.element.name) == canonical
            }
            if sameName.count > 1,
                let pass = sameName.firstIndex(where: { $0.offset == index }).map({ $0 + 1 })
            {
                return "\(label) · Pass \(pass) · Round \(attempt.number)"
            }
            return "\(label) · Round \(attempt.number)"
        }
        return nil
    }
}

/// Invocation observations grouped under the stage or review round that
/// produced them, ordered by attempt with the newest attempt first, using
/// facts the daemon never rewrites. A stage attempt's position is its stage's
/// index in `run.stages` paired with the attempt number (both daemon creation
/// order, `daemon/internal/engine/invocation.go`), and its start instant is
/// its `invocation_started` milestone, falling back to `invocation_admitted`;
/// a review round's are its `round` and its `requested_at`. Observation time
/// is deliberately kept out of the ordering: a late re-observation of an old
/// failed or gone attempt updates only that row's freshness, never its place.
enum RunTimelineGrouping {
    static let unattributedLabel = "Unattributed"

    struct Group: Equatable, Identifiable {
        let id: String
        let label: String
        /// Newest attempt first.
        let invocations: [Components.Schemas.InvocationObservation]
    }

    private enum Kind { case stage, review, unattributed }

    /// Groups key on the canonical stage name, not the stage record: the
    /// daemon appends a further `implement` stage for each remediation
    /// round and operator-feedback pass, and those belong under one
    /// Implementation heading. An observation whose invocation matches no
    /// recorded attempt or review round lands under `Unattributed`, which
    /// always sorts last.
    static func groups(
        invocations: [Components.Schemas.InvocationObservation],
        stages: [Components.Schemas.Stage],
        reviewRounds: [Components.Schemas.RunReviewRound] = [],
        milestones: [Components.Schemas.RunMilestone] = []
    ) -> [Group] {
        // The durable attempt position: the daemon appends stages in creation
        // order and numbers attempts contiguously, so a re-observation cannot
        // move a row.
        func stagePosition(_ invocationID: String) -> (Int, Int)? {
            for (index, stage) in stages.enumerated()
            where stage.attempts.contains(where: { $0.invocation_id == invocationID }) {
                let number = stage.attempts.first { $0.invocation_id == invocationID }?.number ?? 0
                return (index, number)
            }
            return nil
        }
        func reviewNumber(_ invocationID: String) -> Int? {
            reviewRounds.first { $0.invocation_id == invocationID }?.round
        }
        // The append-only start fact that places a group relative to the
        // others, never an observation time.
        func startInstant(_ invocationID: String) -> Date? {
            if let round = reviewRounds.first(where: { $0.invocation_id == invocationID }) {
                return round.requested_at
            }
            if let started = milestones.first(where: {
                $0.kind == .invocation_started && $0.invocation_id == invocationID
            }) {
                return started.recorded_at
            }
            return milestones.first {
                $0.kind == .invocation_admitted && $0.invocation_id == invocationID
            }?.recorded_at
        }

        struct Entry {
            let id: String
            let label: String
            let kind: Kind
        }
        var membership: [String: [Components.Schemas.InvocationObservation]] = [:]
        var order: [Entry] = []
        for invocation in invocations {
            let owner = stages.first { stage in
                stage.attempts.contains { $0.invocation_id == invocation.invocation_id }
            }
            let isReview = reviewNumber(invocation.invocation_id) != nil
            let key =
                isReview
                ? "review" : (owner.map { "stage:\(RunDisplay.canonicalStageName($0.name))" } ?? "unattributed")
            if membership[key] == nil {
                let entry: Entry
                if isReview {
                    entry = Entry(id: key, label: "Review", kind: .review)
                } else if let owner {
                    entry = Entry(id: key, label: RunDisplay.stageLabel(owner.name), kind: .stage)
                } else {
                    entry = Entry(id: key, label: unattributedLabel, kind: .unattributed)
                }
                order.append(entry)
            }
            membership[key, default: []].append(invocation)
        }
        return order.map { entry -> (entry: Entry, group: Group) in
            let rows = membership[entry.id] ?? []
            let sorted: [Components.Schemas.InvocationObservation]
            switch entry.kind {
            case .stage:
                sorted = rows.sorted {
                    (stagePosition($0.invocation_id) ?? (-1, -1))
                        > (stagePosition($1.invocation_id) ?? (-1, -1))
                }
            case .review:
                sorted = rows.sorted {
                    (reviewNumber($0.invocation_id) ?? -1) > (reviewNumber($1.invocation_id) ?? -1)
                }
            case .unattributed:
                sorted = rows.sorted { $0.invocation_id < $1.invocation_id }
            }
            return (entry, Group(id: entry.id, label: entry.label, invocations: sorted))
        }
        .sorted { lhs, rhs in
            // Unattributed always sorts last, whatever its rows' start facts.
            if (lhs.entry.kind == .unattributed) != (rhs.entry.kind == .unattributed) {
                return rhs.entry.kind == .unattributed
            }
            let lhsStart = lhs.group.invocations.first.flatMap { startInstant($0.invocation_id) }
            let rhsStart = rhs.group.invocations.first.flatMap { startInstant($0.invocation_id) }
            switch (lhsStart, rhsStart) {
            case (let left?, let right?):
                if left != right { return left > right }
            case (.some, .none):
                return true  // A known start leads a group with none.
            case (.none, .some):
                return false
            case (.none, .none):
                break
            }
            return lhs.group.label < rhs.group.label
        }
        .map(\.group)
    }
}

struct InvocationPresentation {
    let label: String
    let symbol: String
    /// Running is water, completed is a quiet tick, an observation gap is
    /// the accent, failed and canceled are wax.
    let color: Color
    /// The chip's leading glyph: a tick when completed, and for a
    /// nonterminal status the live bit the symbol also carries (a filled
    /// dot when live, an open one when not).
    let glyph: String?

    init(_ invocation: Components.Schemas.InvocationObservation, asOf: Date) {
        let isTerminal: Bool
        switch invocation.status {
        case .completed, .failed, .canceled, .blocked:
            isTerminal = true
        case .pending, .running, .gone:
            isTerminal = false
        }
        let stale = invocation.observed_at > asOf || asOf.timeIntervalSince(invocation.observed_at) > 30
        if !isTerminal && stale {
            label = "Observation gap"
            symbol = "exclamationmark.triangle"
            color = .accentText
            glyph = nil
        } else {
            label = invocation.status.rawValue.capitalized
            symbol = invocation.live ? "wave.3.right.circle.fill" : "circle"
            switch invocation.status {
            case .running:
                color = .waterText
                glyph = invocation.live ? "●" : "○"
            case .completed:
                color = .ink
                glyph = "✓"
            case .failed, .canceled:
                color = .waxText
                glyph = nil
            case .blocked:
                // A typed stop: the stage ended on a question for the human,
                // not on a failure.
                color = .accentText
                glyph = "?"
            case .pending, .gone:
                color = .inkDim
                glyph = invocation.live ? "●" : "○"
            }
        }
    }
}

/// Ready is quiet in hue but full contrast (a neutral tick in the text
/// color, never green or the accent), in progress is water, blocked is
/// the accent, failed and lost are wax, not observed is a dashed faint.
struct RunOutcomeBadge: View {
    let outcome: Components.Schemas.RunOutcome

    var body: some View {
        StateChip(
            label: RunDisplay.label(outcome), color: color, dashed: outcome == .unobserved,
            glyph: outcome == .published ? "✓" : nil)
    }

    private var color: Color {
        switch outcome {
        case .unobserved: .inkDim
        case .pending: .waterText
        case .published, .completed: .ink
        case .blocked: .accentText
        case .failed, .lost: .waxText
        }
    }
}
