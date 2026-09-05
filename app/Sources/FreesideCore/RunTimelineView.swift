import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

struct RunTimelineView: View {
    private struct RequestID: Hashable {
        let runID: String
        let syncEpoch: String?
        let revision: Int64
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.RunSnapshot

    private var timeline: Components.Schemas.RunTimeline? {
        coordinator.timelinesByRunID[snapshot.run.id]
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                if let timeline {
                    if let hold = timeline.hold?.value1 {
                        holdCard(hold)
                    }
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
        // Every canonical replacement clears cached timelines because they
        // are not part of bootstrap. Epoch and revision both participate:
        // restored revisions are incomparable and may equal the old value.
        .task(
            id: RequestID(
                runID: snapshot.run.id,
                syncEpoch: coordinator.cursors?.syncEpoch,
                revision: snapshot.as_of_revision)
        ) {
            await coordinator.refreshTimeline(for: snapshot.run.id)
        }
    }

    /// The project-owned timeline composition with fixture data supplied
    /// directly because ImageRenderer never executes the loading task.
    @ViewBuilder
    func screenshotContent(_ timeline: Components.Schemas.RunTimeline) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            header
            if let hold = timeline.hold?.value1 {
                holdCard(hold)
            }
            timelineSection(timeline)
            invocationSection(timeline)
        }
        .padding(24)
        .frame(maxWidth: 820, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    eyebrow
                    Text(RunDisplay.timelineTitle(snapshot.run))
                        .font(FreesideFont.largeTitle)
                }
                Spacer()
                RunOutcomeBadge(outcome: snapshot.run.outcome)
            }
            HStack(spacing: 14) {
                if let stage = snapshot.run.stages.last {
                    Label(RunDisplay.stageLabel(stage.name), systemImage: "square.stack.3d.up")
                    if let round = RunDisplay.round(stage) {
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
                if let parent = snapshot.run.parent_run_id {
                    Text("Parent run: \(parent)")
                        .font(FreesideFont.monoCaption)
                }
                Text("\(RunDisplay.specificationLabel(snapshot.run)): \(snapshot.run.spec_digest)")
                    .font(FreesideFont.monoCaption)
            }
            .foregroundStyle(Color.inkDim)
            KeywordLabel(text: "Daemon observations")
        }
    }

    /// The eyebrow names the screen and the run. The id sits beside the
    /// keyword in the same face but outside its uppercase transform, so a
    /// selection copies the id as the daemon spells it. The separator
    /// travels with the id so its spacing scales with the type size.
    private var eyebrow: some View {
        HStack(spacing: 0) {
            KeywordLabel(text: "Run timeline")
            Text(" · \(snapshot.run.id)")
                .font(FreesideFont.keyword)
                .tracking(0.8)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
        }
        .contextMenu {
            Button("Copy run ID") {
                copyRunID()
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
            KeywordLabel(text: "Current hold", color: holdIsFailure ? .waxText : .accentText)
            Text(RunDisplay.label(hold.reason))
                .font(FreesideFont.sectionTitle)
            Text(
                "Observed \(hold.first_observed_at.formatted(date: .abbreviated, time: .shortened)) to \(hold.last_observed_at.formatted(date: .omitted, time: .shortened))"
            )
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
        let entries = timeline.milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: milestoneDetail(milestone),
                context: attemptContext(invocationID: milestone.invocation_id),
                timestamp: milestone.recorded_at.formatted(
                    date: .abbreviated, time: .shortened),
                state: index == timeline.milestones.count - 1 ? .current : .completed)
        }
        return StageRail(
            title: "Stage, Round & Decision History",
            presentation: .timeline(entries: entries),
            axis: .vertical,
            showsSummaryText: false,
            accessibilityStyle: .entries)
    }

    private func invocationSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        let groups = RunTimelineGrouping.groups(
            invocations: timeline.invocations, stages: snapshot.run.stages)
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
                Text(invocation.observed_at.formatted(date: .abbreviated, time: .shortened))
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
        guard let invocationID else { return nil }
        for stage in snapshot.run.stages {
            if let attempt = stage.attempts.first(where: { $0.invocation_id == invocationID }) {
                return "\(RunDisplay.stageLabel(stage.name)) · Round \(attempt.number)"
            }
        }
        return nil
    }

    private func copyRunID() {
        #if os(macOS)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(snapshot.run.id, forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string = snapshot.run.id
        #endif
    }

    private func milestoneDetail(_ milestone: Components.Schemas.RunMilestone) -> String? {
        if let terminal = milestone.terminal?.value1 {
            return terminal.rawValue.capitalized
        }
        if let outcome = milestone.outcome?.value1 {
            return outcome.rawValue.capitalized
        }
        if let reason = milestone.reason?.value1 {
            return RunDisplay.label(reason)
        }
        return nil
    }
}

/// Invocation observations grouped under the stage whose attempt produced
/// them, newest first at both levels, so the latest observation of the
/// latest stage leads.
enum RunTimelineGrouping {
    static let unattributedLabel = "Unattributed"

    struct Group: Equatable, Identifiable {
        let id: String
        let label: String
        /// Newest observation first.
        let invocations: [Components.Schemas.InvocationObservation]

        var newestObservedAt: Date? { invocations.first?.observed_at }
    }

    /// Groups key on the canonical stage name, not the stage record: the
    /// daemon appends a further `implement` stage for each remediation
    /// round and operator-feedback pass, and those belong under one
    /// Implementation heading. An observation whose invocation matches no
    /// recorded attempt lands under `Unattributed`, a group ordered by its
    /// newest observation like any other rather than pinned last.
    static func groups(
        invocations: [Components.Schemas.InvocationObservation],
        stages: [Components.Schemas.Stage]
    ) -> [Group] {
        var membership: [String: [Components.Schemas.InvocationObservation]] = [:]
        var order: [(id: String, label: String)] = []
        for invocation in invocations {
            let owner = stages.first { stage in
                stage.attempts.contains { $0.invocation_id == invocation.invocation_id }
            }
            let key = owner.map { "stage:\(RunDisplay.canonicalStageName($0.name))" } ?? "unattributed"
            if membership[key] == nil {
                order.append(
                    (id: key, label: owner.map { RunDisplay.stageLabel($0.name) } ?? unattributedLabel))
            }
            membership[key, default: []].append(invocation)
        }
        return order.map { entry in
            Group(
                id: entry.id, label: entry.label,
                invocations: (membership[entry.id] ?? []).sorted {
                    if $0.observed_at != $1.observed_at { return $0.observed_at > $1.observed_at }
                    return $0.invocation_id < $1.invocation_id
                })
        }
        .sorted {
            let lhs = $0.newestObservedAt ?? .distantPast
            let rhs = $1.newestObservedAt ?? .distantPast
            if lhs != rhs { return lhs > rhs }
            return $0.label < $1.label
        }
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
