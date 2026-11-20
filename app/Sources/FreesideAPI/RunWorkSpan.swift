import Foundation

/// Which milestones open and end a run's working spans, as the daemon's
/// `domain.RunClocksOf` defines them
/// (`devlog/2026-11-16-1845-run-clock-definition.md`, extended for
/// verification by #3446). The run time strip and the mock's clocks both
/// read it, so the client and the mock can't disagree on the rule.
///
/// Milestones come in daemon record order (`ORDER BY id`), which pairs each
/// start with its own later ending.
public enum RunWorkSpan {
    /// Whether a milestone opens a working span: an agent invocation's
    /// start, or a verification's.
    public static func startsWork(_ kind: Components.Schemas.RunMilestoneKind) -> Bool {
        kind == .invocation_started || kind == .verification_started
    }

    /// The milestone that ends the span `milestones[index]` opens: its own
    /// first ending, else the next start of either kind; nil while the span
    /// is open.
    public static func ending(
        of index: Int, in milestones: [Components.Schemas.RunMilestone]
    ) -> Components.Schemas.RunMilestone? {
        ownEnding(of: index, in: milestones)
            ?? milestones[(index + 1)...].first { startsWork($0.kind) }
    }

    /// The first milestone after `index` on the same invocation that ends
    /// the span `milestones[index]` opens: an export, outcome, or terminal
    /// for an invocation, a pass or failure for a verification.
    public static func ownEnding(
        of index: Int, in milestones: [Components.Schemas.RunMilestone]
    ) -> Components.Schemas.RunMilestone? {
        let endings: Set<Components.Schemas.RunMilestoneKind> =
            milestones[index].kind == .verification_started
            ? [.verification_passed, .verification_failed]
            : [.execution_export_recorded, .execution_outcome_recorded, .terminal_recorded]
        return milestones[index].invocation_id.flatMap { invocation in
            milestones[(index + 1)...].first { $0.invocation_id == invocation && endings.contains($0.kind) }
        }
    }
}
