import FreesideModel
import SwiftUI

/// Current Run's `Transcript` chip (#3560): the document row's chip, led by
/// a water dot that says the writer is live, ending the section's heading
/// line while the run's writer runs. A tap asks the task page to open the
/// running sheet (#1926). The page holds the sheet, not the chip, because
/// the chip leaves when the writer ends, and the sheet must stay to become
/// the finished or failed one. It draws no `unverified`: the sheet's head
/// says so, and the run page's `Open Transcript` keeps its own label.
struct CurrentRunTranscriptChip: View {
    let transcript: TaskTimelinePresentation.LiveTranscript
    /// Nil in screenshots: the chip draws without its action.
    var open: (() -> Void)?

    var body: some View {
        if let open {
            Button(action: open) {
                label
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: PinnedRowButtonFrame.cornerRadius)
            .accessibilityLabel(spokenLabel)
        } else {
            label
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(spokenLabel)
                .accessibilityAddTraits(.isButton)
        }
    }

    /// `Transcript, Implementation · Round 1`: what it opens and whose.
    var spokenLabel: String { "Transcript, \(transcript.title)" }

    private var label: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(Color.waterText)
                .frame(width: 7, height: 7)
                .accessibilityHidden(true)
            Text("Transcript")
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
            Text("›")
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.accentText)
        }
        .modifier(PinnedRowButtonFrame())
    }
}

/// The running sheet Current Run's chip opened, for the writer it named at
/// the tap. It reads the run as it moves on, as the run page's link does:
/// once the writer ends it passes the ended outcome and working time, so
/// the sheet becomes the finished or failed one with what the run page
/// would show.
struct CurrentRunTranscriptSheet: View {
    struct Opened: Identifiable {
        let runID: String
        let transcript: TaskTimelinePresentation.LiveTranscript
        var id: String { transcript.invocationID }
    }

    let coordinator: SyncCoordinator
    let opened: Opened

    var body: some View {
        let id = opened.transcript.invocationID
        let timeline = coordinator.timelinesByRunID[opened.runID]
        let stages = coordinator.runs.first { $0.run.id == opened.runID }?.run.stages ?? []
        RunningTranscriptSheet(
            coordinator: coordinator, runID: opened.runID, invocationID: id,
            title: opened.transcript.title, startedAt: opened.transcript.startedAt,
            endedOutcome: timeline.flatMap {
                RunHistoryPresentation.endedTranscriptOutcome(of: id, in: $0, stages: stages)
            },
            workingTime: timeline.flatMap {
                RunTimeStrip.invocationDuration(id, milestones: $0.milestones, clocks: $0.clocks?.value1)
            })
    }
}
