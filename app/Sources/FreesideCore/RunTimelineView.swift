import FreesideAPI
import SwiftUI

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
                    ContentUnavailableView(
                        "Timeline unavailable",
                        systemImage: "exclamationmark.triangle",
                        description: Text("Freeside could not load daemon observations for this run.")
                    )
                    .frame(maxWidth: .infinity, minHeight: 180)
                } else {
                    ProgressView("Loading timeline…")
                        .frame(maxWidth: .infinity, minHeight: 180)
                }
            }
            .padding(24)
            .frame(maxWidth: 820, alignment: .leading)
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

    private var header: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Run timeline")
                        .font(.largeTitle.bold())
                    Text(snapshot.run.id)
                        .font(.callout.monospaced())
                        .foregroundStyle(.secondary)
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
            .font(.subheadline)
            .foregroundStyle(.secondary)
            Label("Daemon observations", systemImage: "eye")
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
        }
    }

    private func holdCard(_ hold: Components.Schemas.RunHold) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("Current hold", systemImage: "pause.circle.fill")
                .font(.headline)
            Text(RunDisplay.label(hold.reason))
                .font(.title3.weight(.semibold))
            Text(
                "Observed \(hold.first_observed_at.formatted(date: .abbreviated, time: .shortened)) to \(hold.last_observed_at.formatted(date: .omitted, time: .shortened))"
            )
            .font(.caption)
            .foregroundStyle(.secondary)
        }
        .padding()
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 12))
    }

    private func timelineSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Stage, Round & Decision History")
                .font(.title2.bold())
            ForEach(Array(timeline.milestones.enumerated()), id: \.offset) { index, milestone in
                HStack(alignment: .top, spacing: 12) {
                    VStack(spacing: 0) {
                        Circle()
                            .fill(index == timeline.milestones.count - 1 ? Color.accentColor : .secondary)
                            .frame(width: 10, height: 10)
                        if index < timeline.milestones.count - 1 {
                            Rectangle()
                                .fill(.quaternary)
                                .frame(width: 2, height: 44)
                        }
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        Text(RunDisplay.label(milestone.kind))
                            .font(.headline)
                        if let detail = milestoneDetail(milestone) {
                            Text(detail)
                                .font(.subheadline.weight(.medium))
                        }
                        if let context = attemptContext(invocationID: milestone.invocation_id) {
                            Text(context)
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                        }
                        Text(milestone.recorded_at.formatted(date: .abbreviated, time: .shortened))
                            .font(.caption)
                            .foregroundStyle(.tertiary)
                    }
                }
            }
        }
    }

    private func invocationSection(_ timeline: Components.Schemas.RunTimeline) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Latest Invocation Observations")
                .font(.title2.bold())
            ForEach(timeline.invocations, id: \.invocation_id) { invocation in
                HStack {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(attemptContext(invocationID: invocation.invocation_id) ?? invocation.invocation_id)
                            .font(.headline)
                        Text(invocation.observed_at.formatted(date: .abbreviated, time: .shortened))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    Spacer()
                    let presentation = InvocationPresentation(invocation, asOf: timeline.as_of)
                    Label(
                        presentation.label,
                        systemImage: presentation.symbol
                    )
                    .font(.subheadline.weight(.medium))
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
        } else {
            label = invocation.status.rawValue.capitalized
            symbol = invocation.live ? "wave.3.right.circle.fill" : "circle"
        }
    }
}
