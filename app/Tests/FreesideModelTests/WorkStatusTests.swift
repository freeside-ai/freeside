import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

@Suite struct WorkStatusTests {
    private typealias Position = WorkStatus.Position

    private func groups(projectFilter: String? = nil) -> [(Position, [String])] {
        WorkStatus(
            open: WorkStatusFixtures.openItems(), tasks: WorkStatusFixtures.tasks(), projectFilter: projectFilter
        ).groups.map { ($0.position, $0.taskIDs) }
    }

    @Test func noTasksCountNothing() {
        #expect(WorkStatus(open: [], tasks: [], projectFilter: nil).groups.isEmpty)
        // Scoped items alone aren't tasks.
        let scoped = WorkStatusFixtures.openItems().filter { !$0.item.id.hasSuffix("card") }
        #expect(WorkStatus(open: scoped, tasks: [], projectFilter: nil).groups.isEmpty)
    }

    /// Each task lands in at most one group: an open card first, then a
    /// hold, then a stage. `implement` counts as Implementation, an unknown
    /// stage is its own group after the pipeline, `identity_parallelism`
    /// counts with `usage_pool_exhausted`, and finished, stopped, and
    /// scoped work isn't counted.
    @Test func eachTaskCountsOnceAtItsPositionInOrder() {
        let all = groups()
        #expect(
            all.map(\.0) == [
                .waitingOnYou,
                .stage("specification"),
                .stage("implementation"),
                .stage("review"),
                .stage("triage"),
                .notStarted,
                .held(.base_advanced),
                .held(.usage_pool_exhausted),
            ])
        #expect(
            all.map(\.1) == [
                ["task-card", "task-finished-card"],
                ["task-spec"],
                ["task-implement", "task-implementation"],
                ["task-other-project", "task-review"],
                ["task-triage"],
                ["task-unpositioned", "task-unstaged"],
                ["task-base"],
                ["task-capacity", "task-parallel"],
            ])
        let counted = all.flatMap(\.1)
        #expect(Set(counted).count == counted.count)
        #expect(!counted.contains("task-finished"))
        #expect(!counted.contains("task-stopped"))
    }

    @Test func labelsReadTheStageAndHoldVocabulary() {
        let labels = WorkStatus(
            open: WorkStatusFixtures.openItems(), tasks: WorkStatusFixtures.tasks(), projectFilter: nil
        ).groups.map(\.label)
        #expect(
            labels == [
                "Waiting on you", "Specification", "Implementation", "Review", "Triage", "Not started",
                "Base branch changed", "Waiting for agent capacity",
            ])
    }

    @Test func theProjectFilterNarrowsEveryGroup() {
        let freeside = groups(projectFilter: WorkStatusFixtures.project)
        #expect(freeside.first { $0.0 == .stage("review") }?.1 == ["task-review"])
        let atlas = groups(projectFilter: WorkStatusFixtures.otherProject)
        #expect(atlas.map(\.0) == [.stage("review")])
        #expect(atlas.map(\.1) == [["task-other-project"]])
    }

    /// Every row's bar reads one scale: the largest count, held groups
    /// included. An all-zero tab fills nothing, and one dominant count
    /// fills its track while a small count fills its share.
    @Test func barsShareTheLargestCountAsTheirScale() {
        #expect(WorkStatus(open: [], tasks: [], projectFilter: nil).largestCount == 0)
        #expect(WorkStatus.barFraction(0, largest: 0) == 0)

        #expect(WorkStatus.barFraction(9, largest: 9) == 1)
        #expect(WorkStatus.barFraction(1, largest: 9) == 1.0 / 9)

        let status = WorkStatus(
            open: WorkStatusFixtures.openItems(), tasks: WorkStatusFixtures.tasks(), projectFilter: nil)
        #expect(status.largestCount == 2)
        // Held groups alone still set the scale.
        let heldIDs: Set = ["task-base", "task-capacity", "task-parallel"]
        let held = WorkStatus(
            open: [], tasks: WorkStatusFixtures.tasks().filter { heldIDs.contains($0.task.id) }, projectFilter: nil)
        #expect(held.groups.map(\.position) == [.held(.base_advanced), .held(.usage_pool_exhausted)])
        #expect(held.largestCount == 2)
    }

    @Test func aQuietTaskSitsUnderItsHoldBeforeItsStage() {
        let tasks = WorkStatusFixtures.tasks()
        func position(_ id: String) -> Position? {
            tasks.first { $0.task.id == id }.flatMap { WorkStatus.position(ofQuiet: $0.task) }
        }
        #expect(position("task-card") == .held(.scope_conflict))
        #expect(position("task-parallel") == .held(.usage_pool_exhausted))
        #expect(position("task-implement") == .stage("implementation"))
        #expect(position("task-unstaged") == .notStarted)
        #expect(position("task-finished") == nil)
        #expect(position("task-stopped") == nil)
    }
}
