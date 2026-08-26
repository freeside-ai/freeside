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
                    ContentUnavailableView {
                        Label {
                            Text("Timeline unavailable").font(FreesideFont.title)
                        } icon: {
                            Image(systemName: "exclamationmark.triangle")
                        }
                    } description: {
                        Text("Freeside could not load daemon observations for this run.")
                            .font(FreesideFont.callout)
                    }
                    .foregroundStyle(Color.inkDim)
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
                    Text("Run timeline")
                        .font(FreesideFont.largeTitle)
                    Text(snapshot.run.id)
                        .font(FreesideFont.monoCallout)
                        .foregroundStyle(Color.inkDim)
                        .textSelection(.enabled)
                        .contextMenu {
                            Button("Copy run ID") {
                                copyRunID()
                            }
                        }
                }
                Spacer()
                RunOutcomeBadge(outcome: snapshot.run.outcome)
            }
            HStack(spacing: 14) {
                if let stage = snapshot.run.stages.last {
                    Label(stage.name.capitalized, systemImage: "square.stack.3d.up")
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
            if let campaign = RunDisplay.campaign(snapshot.run) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(campaign)
                        .font(FreesideFont.mono(.callout, weight: .semibold))
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
            }
            KeywordLabel(text: "Daemon observations")
        }
    }

    /// Mirrors the run list: a hold is attention, but on a failed or
    /// lost run it reads as part of the failure and keeps wax.
    private var holdIsFailure: Bool {
        switch snapshot.run.outcome {
        case .failed, .lost: true
        case .unobserved, .pending, .published, .blocked: false
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
        VStack(alignment: .leading, spacing: 14) {
            Text("Stage, Round & Decision History")
                .font(FreesideFont.title)
            ForEach(Array(timeline.milestones.enumerated()), id: \.offset) { index, milestone in
                HStack(alignment: .top, spacing: 12) {
                    VStack(spacing: 0) {
                        Circle()
                            .fill(index == timeline.milestones.count - 1 ? Color.accentBorder : Color.milestonePrior)
                            .frame(width: 10, height: 10)
                        if index < timeline.milestones.count - 1 {
                            Rectangle()
                                .fill(Color.milestoneConnector)
                                .frame(width: 2, height: 44)
                        }
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        Text(RunDisplay.label(milestone.kind))
                            .font(FreesideFont.sans(.headline, weight: .semibold))
                        if let detail = milestoneDetail(milestone) {
                            Text(detail)
                                .font(FreesideFont.sans(.subheadline, weight: .medium))
                        }
                        if let context = attemptContext(invocationID: milestone.invocation_id) {
                            Text(context)
                                .font(FreesideFont.subheadline)
                                .foregroundStyle(Color.inkDim)
                        }
                        Text(milestone.recorded_at.formatted(date: .abbreviated, time: .shortened))
                            .font(FreesideFont.monoCaption)
                            .foregroundStyle(Color.inkDim)
                    }
                }
            }
        }
    }

    private func invocationSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Latest Invocation Observations")
                .font(FreesideFont.title)
            ForEach(Array(timeline.invocations.enumerated()), id: \.element.invocation_id) { index, invocation in
                if index > 0 {
                    Divider().overlay(Color.rule)
                }
                HStack {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(attemptContext(invocationID: invocation.invocation_id) ?? invocation.invocation_id)
                            .font(FreesideFont.sans(.headline, weight: .semibold))
                        Text(invocation.observed_at.formatted(date: .abbreviated, time: .shortened))
                            .font(FreesideFont.monoCaption)
                            .foregroundStyle(Color.inkDim)
                    }
                    Spacer()
                    let presentation = InvocationPresentation(invocation, asOf: timeline.as_of)
                    StateChip(label: presentation.label, color: presentation.color, glyph: presentation.glyph)
                }
                .padding(.vertical, 6)
            }
        }
    }

    private func attemptContext(invocationID: String?) -> String? {
        guard let invocationID else { return nil }
        for stage in snapshot.run.stages {
            if let attempt = stage.attempts.first(where: { $0.invocation_id == invocationID }) {
                return "\(stage.name.capitalized) · Round \(attempt.number)"
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
        case .completed, .failed, .canceled:
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
            case .pending, .gone:
                color = .inkDim
                glyph = invocation.live ? "●" : "○"
            }
        }
    }
}
