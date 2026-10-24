import FreesideAPI

// Beside the task timeline it reads, which stays with the views.
extension TaskDisplay {
    /// The stage the task detail page names on its chip and its hold callout
    /// (#1923), from the first of these that says one:
    ///
    /// 1. Verification, when the current run's publication position says so
    ///    (`RunDisplay.workflowPhase`). The event rail draws Verification's
    ///    start only under a verification or publication event, so the
    ///    position names it before the timeline loads and when a publication
    ///    hold has no such event yet.
    /// 2. The newest start point on the task's event rail, so the header and
    ///    the rail under it name the same stage. `reviewRounds` is the
    ///    rail's, so a remediation reads as Implementation here too.
    /// 3. The stage in the task's position, which is all there is until the
    ///    timeline loads.
    ///
    /// Nil when none does: the page does not guess a stage.
    static func detailStage(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot],
        history: Components.Schemas.TaskTimeline?,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> Components.Schemas.StageName? {
        guard let current = task.current_position?.value1 else { return nil }
        if let run = runs.first(where: { $0.run.id == current.run_id })?.run,
            RunDisplay.workflowPhase(run)?.stage == .verification
        {
            return .verification
        }
        if let history,
            let stage = TaskTimelinePresentation.newestStage(
                TaskTimelinePresentation.rows(history, reviewRounds: reviewRounds))
        {
            return stage
        }
        return current.stage.flatMap { Components.Schemas.StageName(rawValue: RunDisplay.canonicalStageName($0)) }
    }
}
