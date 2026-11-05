import FreesideAPI

/// Runs parked on a usage pool, for the usage page's parked count (#2583).
/// `RunFixtures` keeps its parked-run builder private, so each is a copy of
/// the default specification run (no campaign, no hold) under its own run,
/// task, stage, attempt, and invocation ids, held on `pool` the way the
/// daemon projects a live `usage_pool_exhausted` hold.
enum ParkedRunFixtures {
    static func run(_ id: String, on pool: String) -> Components.Schemas.RunSnapshot {
        guard var snapshot = RunFixtures.defaultRuns().first(where: { $0.run.id == "run-freeside-specification" })
        else { preconditionFailure("the default runs lost the specification run") }
        snapshot.run.id = id
        snapshot.run.task_id = "task-\(id)"
        snapshot.run.stages = snapshot.run.stages.map { stage in
            var stage = stage
            stage.id = "stage-\(id)-\(stage.name)"
            stage.run_id = id
            stage.attempts = stage.attempts.map { attempt in
                var attempt = attempt
                attempt.id = "attempt-\(stage.id)-\(attempt.number)"
                attempt.stage_id = stage.id
                attempt.invocation_id = "inv-\(stage.id)-\(attempt.number)"
                return attempt
            }
            return stage
        }
        snapshot.run.hold_reason = .init(value1: .usage_pool_exhausted)
        snapshot.run.hold_usage_pool = pool
        return snapshot
    }
}
