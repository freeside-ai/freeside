import Foundation
import FreesideAPI

// Beside the task timeline it reads, which stays with the views.
extension TaskDisplay {
    /// The stage the task detail page names on its chip and its hold callout
    /// (#1923), from the first of these that says one:
    ///
    /// 1. The stage the task's timeline names (`currentStage`), so the
    ///    header, the task row, and the rail under the header agree.
    /// 2. Verification, when the current run's publication position says so
    ///    (`RunDisplay.workflowPhase`), which is all there is before the
    ///    timeline loads.
    /// 3. The stage in the task's position.
    ///
    /// Nil when none does: the page does not guess a stage.
    static func detailStage(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot],
        history: Components.Schemas.TaskTimeline?,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> Components.Schemas.StageName? {
        guard let current = task.current_position?.value1 else { return nil }
        if let stage = currentStage(task, runs: runs, history: history, reviewRounds: reviewRounds) {
            return stage.stage
        }
        if let run = runs.first(where: { $0.run.id == current.run_id })?.run,
            RunDisplay.workflowPhase(run)?.stage == .verification
        {
            return .verification
        }
        return current.stage.flatMap { Components.Schemas.StageName(rawValue: RunDisplay.canonicalStageName($0)) }
    }

    /// Where the task stands by its timeline: the stage and round that the
    /// task row's context line and the task detail header's chip both name,
    /// so neither can lag the other (#2350).
    ///
    /// The rail's newest start point names the stage. `reviewRounds` is the
    /// rail's, so a remediation reads as Implementation. Review's round is
    /// the newest round-opening event's: an external review re-entry or a
    /// round request. Any other stage keeps the round its run names for it.
    ///
    /// The current run's publication position (`RunDisplay.workflowPhase`)
    /// says Verification over that start unless the start is newer. The
    /// rail draws no event for a publication hold, so the position names
    /// it; a publication that a later re-entry withdrew is older than the
    /// Review start the re-entry drew, so it no longer does.
    ///
    /// Nil when the timeline is not loaded or has no start point; each
    /// caller then keeps its own fallback.
    static func currentStage(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot],
        history: Components.Schemas.TaskTimeline?,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> CurrentStage? {
        guard let current = task.current_position?.value1, let history,
            let start = TaskTimelinePresentation.newestStageStart(
                TaskTimelinePresentation.rows(history, reviewRounds: reviewRounds))
        else { return nil }
        let run = runs.first { $0.run.id == current.run_id }?.run
        let heading = run.flatMap { RunDisplay.stageHeading($0) }
        if let run, RunDisplay.workflowPhase(run)?.stage == .verification, start.stage != .verification {
            // With no instant on either side, the position stands.
            let startIsNewer =
                publicationInstant(history, runID: run.id).flatMap { published in
                    start.recordedAt.map { published < $0 }
                } ?? false
            if !startIsNewer { return CurrentStage(stage: .verification, round: heading?.round) }
        }
        if start.stage == .review {
            return CurrentStage(
                stage: .review, round: TaskTimelinePresentation.newestReviewRound(history).map { "Round \($0)" })
        }
        let round = heading?.label == AttentionDisplay.label(start.stage) ? heading?.round : nil
        return CurrentStage(stage: start.stage, round: round)
    }

    /// When the task's timeline last recorded the run's publication
    /// position: its newest publication milestone, or its publication hold
    /// when that was observed later. Any other hold (an attended-mode wait
    /// on the re-entered cycle's publication) says nothing about it.
    private static func publicationInstant(
        _ history: Components.Schemas.TaskTimeline, runID: String
    ) -> Date? {
        guard let run = history.sections.flatMap(\.runs).first(where: { $0.run_id == runID }) else { return nil }
        let milestone = run.milestones.first {
            $0.kind == .publication_ready || $0.kind == .publication_blocked || $0.kind == .work_unit_completed
        }?.recorded_at
        let hold = run.hold?.value1
        let held = hold.flatMap { RunDisplay.publicationHoldReasons.contains($0.reason) ? $0.first_observed_at : nil }
        return [milestone, held].compactMap { $0 }.max()
    }
}
