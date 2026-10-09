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
    /// Supplies the screenshot composition without the scroll viewport or
    /// the loading tasks. It renders through `body`, as the task timeline's
    /// does, because the helpers below read locale, time zone, and the
    /// pinned clock from the environment, which only a mounted view has.
    var screenshotTimeline: Components.Schemas.RunTimeline?
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow

    private func shortTime(_ date: Date) -> String {
        FreesideFormat.shortTime(date, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
    }

    private var timeline: Components.Schemas.RunTimeline? {
        coordinator.timelinesByRunID[snapshot.run.id]
    }

    private var task: Components.Schemas.Task? {
        coordinator.tasks.first { $0.task.id == snapshot.run.task_id }?.task
    }

    @ViewBuilder var body: some View {
        if let screenshotTimeline {
            composition(screenshotTimeline)
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
                    sections(timeline)
                } else if coordinator.timelineLoadStates[snapshot.run.id] == .unavailable {
                    UnavailableStateView(
                        title: "Timeline unavailable",
                        description: "Freeside could not load daemon observations for this run."
                    )
                    .frame(maxWidth: .infinity, minHeight: 180)
                } else {
                    ProgressView("Loading timeline…")
                        .tint(.waterText)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .frame(maxWidth: .infinity, minHeight: 180)
                }
                folds(hold: timeline?.hold?.value1)
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
    func screenshotContent(_ timeline: Components.Schemas.RunTimeline) -> some View {
        RunTimelineView(
            coordinator: coordinator, snapshot: snapshot, expandsTechnicalDetails: expandsTechnicalDetails,
            screenshotTimeline: timeline)
    }

    private func composition(_ timeline: Components.Schemas.RunTimeline) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            header
            sections(timeline, persistsFolds: false)
            folds(hold: timeline.hold?.value1)
        }
    }

    /// What the daemon recorded for the run, each under its own keyword:
    /// the hold, the review rounds, the milestones, and the invocations.
    @ViewBuilder
    private func sections(
        _ timeline: Components.Schemas.RunTimeline, persistsFolds: Bool = true
    ) -> some View {
        if let hold = timeline.hold?.value1 {
            holdCallout(hold)
        }
        reviewSection(timeline, persistsFolds: persistsFolds)
        timelineSection(timeline)
        invocationSection(timeline)
    }

    /// The page's one hairline, then its fold (R26): the exact identifiers
    /// sit under everything the page says in words.
    private func folds(hold: Components.Schemas.RunHold?) -> some View {
        TechnicalDetailsSection(
            rows: RunTimelineView.technicalRows(
                run: snapshot.run, specificationLabel: specificationLabel, hold: hold),
            summary: RunTimelineView.technicalSummary(run: snapshot.run, hold: hold),
            startsExpanded: expandsTechnicalDetails
        )
        .padding(.top, 18)
        .overlay(alignment: .top) {
            Color.rule.frame(height: 1)
        }
    }

    /// The run's name and where it stands (6.2): the screen and its task
    /// with the outcome beside them, the attempt, and one identity line.
    var header: some View {
        VStack(alignment: .leading, spacing: 12) {
            eyebrow
            // One line cut in the middle at the standard sizes; an
            // accessibility size wraps it, so larger text hides no part of
            // the run's name (R22).
            Text(RunDisplay.timelineTitle(snapshot.run))
                .font(FreesideFont.ask)
                .lineLimit(dynamicTypeSize.isAccessibilitySize ? nil : 1)
                .truncationMode(.middle)
            if let identity = RunTimelineView.identityLine(
                snapshot.run, task: task, attentionItems: coordinator.store.orderedSnapshots)
            {
                Text(identity)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
            VStack(alignment: .leading, spacing: 4) {
                if let reason = snapshot.run.attempt_reason {
                    Text("Reason: \(reason)")
                }
                Text(RunDisplay.specificationHeaderLabel(snapshot.run, approval: specificationApproval))
                if let qualification = specificationApproval.qualification {
                    Text(qualification)
                }
            }
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
        }
    }

    /// The header's identity line: the phase, its round, and the newest
    /// milestone, the three facts the header used to print as icon labels.
    /// Nil when the run has recorded none of them.
    static func identityLine(
        _ run: Components.Schemas.Run, task: Components.Schemas.Task? = nil,
        attentionItems: [Components.Schemas.AttentionItemSnapshot] = []
    ) -> String? {
        var parts: [String] = []
        if let heading = RunDisplay.stageHeading(run, task: task, attentionItems: attentionItems) {
            parts.append(heading.label)
            if let round = heading.round { parts.append(round) }
        }
        if let milestone = run.latest_milestone?.value1 {
            parts.append(RunDisplay.label(milestone))
        }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
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
            stages: snapshot.run.stages,
            folds: !persistsFolds
                ? nil
                : .init(
                    facts: { folds.binding(.roundFacts($0), taskID: taskID) },
                    priorRounds: folds.binding(.priorRounds(snapshot.run.id), taskID: taskID)))
    }

    /// The page's technical details: the exact run, task, campaign, and
    /// parent ids, the specification digest under its own label, and the
    /// recorded hold's code. The primary text names the specification and
    /// the hold; the exact values stay here to compare or paste. Pure, so
    /// a test can check each value against its source field.
    static func technicalRows(
        run: Components.Schemas.Run, specificationLabel: String, hold: Components.Schemas.RunHold? = nil
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [
            .init(label: "Run ID", value: run.id),
            .init(label: "Task ID", value: run.task_id),
        ]
        if let campaignID = run.campaign_id {
            rows.append(.init(label: "Campaign ID", value: campaignID))
        }
        if let parent = run.parent_run_id {
            rows.append(.init(label: "Parent Run ID", value: parent))
        }
        rows.append(.init(label: specificationLabel, value: run.spec_digest))
        if let hold {
            rows.append(.init(label: "Hold Code", value: hold.reason.rawValue))
        }
        return rows
    }

    /// What the closed Technical Details says it holds: one word per row
    /// of `technicalRows`.
    static func technicalSummary(run: Components.Schemas.Run, hold: Components.Schemas.RunHold? = nil) -> String {
        var parts = ["run", "task"]
        if run.campaign_id != nil { parts.append("campaign") }
        if run.parent_run_id != nil { parts.append("parent") }
        parts.append("digest")
        if hold != nil { parts.append("hold code") }
        return parts.joined(separator: " · ")
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
        RunDisplay.specificationRowLabel(snapshot.run, approval: specificationApproval)
    }

    /// The eyebrow names the screen and the task the run belongs to, in the
    /// task's own label style (mono for an identifier fallback, the Agent
    /// keyword after an agent-proposed name), with the run's outcome
    /// trailing as a card's state does (R27). The chip drops under the
    /// keyword at an accessibility size. The run id stays in the copy
    /// context menu and, for a run outside a campaign, in the title.
    private var eyebrow: some View {
        let keyword = KeywordLabel(text: "Run timeline")
            .accessibilityAddTraits(.isHeader)
        let chip = RunOutcomeBadge(outcome: snapshot.run.outcome)
        // One line where the keyword, the whole name, and the chip fit; a
        // phone's width or an accessibility size gives the name its own line
        // instead of truncating it beside a wrapped keyword.
        return ViewThatFits(in: .horizontal) {
            HStack(alignment: .center, spacing: 12) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    keyword
                        .fixedSize()
                    Text("·")
                        .font(FreesideFont.keyword)
                        .foregroundStyle(Color.inkDim)
                        .accessibilityHidden(true)
                    eyebrowName(lineLimit: 1)
                }
                Spacer(minLength: 0)
                chip
            }
            VStack(alignment: .leading, spacing: 4) {
                if dynamicTypeSize.isAccessibilitySize {
                    keyword
                    chip
                } else {
                    HStack(alignment: .center, spacing: 12) {
                        keyword
                        Spacer(minLength: 0)
                        chip
                    }
                }
                eyebrowName(lineLimit: 2)
            }
        }
        .contextMenu {
            Button("Copy run ID") {
                Clipboard.copy(snapshot.run.id)
            }
        }
    }

    private func eyebrowName(lineLimit: Int) -> some View {
        TaskNameLabel(
            name: TaskDisplay.name(for: snapshot.run, tasks: coordinator.tasks),
            font: FreesideFont.cardBody,
            monoFont: FreesideFont.trailingSummary,
            color: .inkDim,
            lineLimit: lineLimit)
    }

    /// The recorded hold in the one system callout, the shape the task
    /// page gives its hold: the round, the hold in words, and when the
    /// daemon observed it. A finished run's hold is a past fact and says
    /// so. The exact code sits in Technical Details.
    private func holdCallout(_ hold: Components.Schemas.RunHold) -> some View {
        SystemCallout {
            if let round = RunDisplay.stageHeading(
                snapshot.run, task: task, attentionItems: coordinator.store.orderedSnapshots)?.round
            {
                Text(round)
                    .font(FreesideFont.cardBody)
            }
            Text(RunTimelineView.holdSentence(hold, run: snapshot.run))
                .font(FreesideFont.statement)
            Text(
                "Observed \(shortTime(hold.first_observed_at)) to "
                    + hold.last_observed_at.formatted(
                        Date.FormatStyle(date: .omitted, time: .shortened, locale: locale, timeZone: timeZone))
            )
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .exactInstants(hold.first_observed_at, to: hold.last_observed_at)
        }
        .foregroundStyle(Color.ink)
        .fixedSize(horizontal: false, vertical: true)
    }

    static func holdSentence(_ hold: Components.Schemas.RunHold, run: Components.Schemas.Run) -> String {
        "\(run.lifecycle == .finished ? "Recorded hold" : "Hold"): \(RunDisplay.label(hold.reason))"
    }

    private func timelineSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        let entries = RunHistoryPresentation.entries(
            milestones: timeline.milestones,
            detail: milestoneDetail,
            context: { attemptContext(invocationID: $0, in: timeline) },
            reviewRounds: timeline.review?.value1.rounds ?? [],
            now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
        return VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Milestones")
            StageRail(
                title: nil,
                presentation: .timeline(entries: entries),
                axis: .vertical,
                showsSummaryText: false,
                accessibilityStyle: .entries)
        }
    }

    private func invocationSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        let groups = RunTimelineGrouping.groups(
            invocations: timeline.invocations, stages: snapshot.run.stages,
            reviewRounds: timeline.review?.value1.rounds ?? [],
            milestones: timeline.milestones)
        return VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Latest invocation observations")
            ForEach(groups) { group in
                // Spacing alone sets the groups and their rows apart: the
                // page's one hairline belongs to its folds (R26).
                Text(group.label)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                ForEach(group.invocations, id: \.invocation_id) { invocation in
                    invocationRow(invocation, in: timeline)
                }
            }
        }
    }

    private func invocationRow(
        _ invocation: Components.Schemas.InvocationObservation, in timeline: Components.Schemas.RunTimeline
    ) -> some View {
        let presentation = InvocationPresentation(invocation, asOf: timeline.as_of)
        let chip = StateChip(label: presentation.label, color: presentation.color, glyph: presentation.glyph)
        let lines = VStack(alignment: .leading, spacing: 3) {
            Text(
                attemptContext(invocationID: invocation.invocation_id, in: timeline)
                    ?? invocation.invocation_id
            )
            .font(FreesideFont.sans(.headline, weight: .semibold))
            // The observed time is freshness (the daemon's last look), not
            // the attempt's place in history; the "Observed" prefix says so.
            Text("Observed \(shortTime(invocation.observed_at))")
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .exactInstant(invocation.observed_at)
        }
        // Beside its chip an accessibility-size title breaks mid-word.
        return Group {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 6) {
                    lines
                    chip
                }
            } else {
                HStack {
                    lines
                    Spacer()
                    chip
                }
            }
        }
    }

    /// Labels from the timeline being rendered, not the coordinator's copy,
    /// so a screenshot's supplied timeline labels its own milestones.
    private func attemptContext(
        invocationID: String?, in timeline: Components.Schemas.RunTimeline
    ) -> String? {
        RunHistoryPresentation.attemptContext(
            invocationID: invocationID,
            stages: snapshot.run.stages,
            reviewRounds: timeline.review?.value1.rounds ?? [])
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
    /// `.current`), adds the findings adjudications that started a
    /// remediation, then returns them reversed so the newest leads. `detail`
    /// and `context` stay the view's own closures over run state.
    static func entries(
        milestones: [Components.Schemas.RunMilestone],
        detail: (Components.Schemas.RunMilestone) -> String?,
        context: (String?) -> String?,
        reviewRounds: [Components.Schemas.RunReviewRound] = [],
        now: Date = Date(), locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [DecisionStageRailPresentation.Entry] {
        let ordered = milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: detail(milestone),
                context: context(milestone.invocation_id),
                timestamp: FreesideFormat.shortTime(
                    milestone.recorded_at, now: now, locale: locale, timeZone: timeZone),
                instant: milestone.recorded_at,
                state: index == milestones.count - 1 ? .current : .completed)
        }
        return Array(
            insertingAdjudications(
                into: ordered, invocationIDs: milestones.map(\.invocation_id), reviewRounds: reviewRounds,
                now: now, locale: locale, timeZone: timeZone
            ).reversed())
    }

    /// Adds one `Findings Adjudicated` entry for each review round whose
    /// findings started a remediation, to entries in daemon record order
    /// (oldest first) whose milestones name `invocationIDs`. The entry goes
    /// just before the remediator's first milestone, or at the newest end
    /// while the remediator has none. It is placed by record order, never by
    /// time, and is never `.current`.
    static func insertingAdjudications(
        into entries: [DecisionStageRailPresentation.Entry], invocationIDs: [String?],
        reviewRounds: [Components.Schemas.RunReviewRound],
        now: Date = Date(), locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [DecisionStageRailPresentation.Entry] {
        typealias Placed = (position: Int, entry: DecisionStageRailPresentation.Entry)
        let adjudications = reviewRounds.compactMap { round -> Placed? in
            guard let remediation = round.remediation?.value1 else { return nil }
            let entry = DecisionStageRailPresentation.Entry(
                id: "adjudication-\(round.round)",
                title: "Findings Adjudicated",
                detail: "Remediate \(ReviewRoundPresentation.findingsPhrase(remediation.finding_ids.count)) "
                    + "from Review \(round.round)",
                timestamp: remediation.decided_at.map {
                    FreesideFormat.shortTime($0, now: now, locale: locale, timeZone: timeZone)
                },
                instant: remediation.decided_at,
                state: .completed)
            let position = invocationIDs.firstIndex(of: remediation.invocation_id) ?? entries.count
            return (position, entry)
        }
        guard !adjudications.isEmpty else { return entries }
        var result: [DecisionStageRailPresentation.Entry] = []
        for index in 0...entries.count {
            result += adjudications.filter { $0.position == index }.map(\.entry)
            if index < entries.count { result.append(entries[index]) }
        }
        return result
    }

    /// `Remediation for Review <n>` for the remediator a review round's
    /// findings started. The daemon runs it in a stage named like the
    /// implementer's, so only the review facts tell the two apart.
    static func remediationContext(
        invocationID: String?, reviewRounds: [Components.Schemas.RunReviewRound]
    ) -> String? {
        guard let invocationID,
            let round = reviewRounds.first(where: { $0.remediation?.value1.invocation_id == invocationID })
        else { return nil }
        return "Remediation for Review \(round.round)"
    }

    /// The attempt the review facts prove for an invocation: a remediator,
    /// or the producer a round's subject names. Nil when no round names it.
    static func roleContext(
        invocationID: String?, reviewRounds: [Components.Schemas.RunReviewRound],
        stages: [Components.Schemas.Stage] = []
    ) -> String? {
        if let remediation = remediationContext(invocationID: invocationID, reviewRounds: reviewRounds) {
            return remediation
        }
        guard let invocationID,
            let subject = reviewRounds.lazy.compactMap({ $0.subject?.value1 })
                .first(where: { $0.invocation_id == invocationID })
        else { return nil }
        return attemptLabel(for: subject, reviewRounds: reviewRounds, stages: stages)
    }

    /// The label for the attempt a review subject names, as the run rail
    /// labels that attempt's milestones (`Implementation · Pass 1 · Round 2`,
    /// `Remediation for Review 1`), else by its kind when the run's stages
    /// don't hold it.
    static func attemptLabel(
        for subject: Components.Schemas.ReviewRoundSubject, reviewRounds: [Components.Schemas.RunReviewRound],
        stages: [Components.Schemas.Stage]
    ) -> String {
        switch subject.kind {
        case .implementation:
            return attemptContext(invocationID: subject.invocation_id, stages: stages, reviewRounds: reviewRounds)
                ?? "Implementation"
        case .operator_feedback:
            return attemptContext(invocationID: subject.invocation_id, stages: stages, reviewRounds: reviewRounds)
                ?? "Operator feedback"
        case .remediation:
            return subject.remediates_round.map { "Remediation for Review \($0)" } ?? "Remediation"
        }
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
    /// A review round reads `Review · Round <n>` and takes precedence, then
    /// a remediator reads `Remediation for Review <n>`. Every
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
        if let remediation = remediationContext(invocationID: invocationID, reviewRounds: reviewRounds) {
            return remediation
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
            label = "Observation Gap"
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
