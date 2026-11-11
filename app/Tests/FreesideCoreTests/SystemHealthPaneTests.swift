import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The System Health pane's Unattended Operation row (#2920): the gate as a
/// chip, wax only for a current stop, and a line naming the cause and age,
/// read from the same snapshot and freshness as the standing indicator.
@MainActor
struct UnattendedGatePresentationTests {
    private static let now = Date(timeIntervalSince1970: 1_786_502_645)

    private func make(
        _ stops: [Components.Schemas.UnattendedStop],
        admission: Components.Schemas.UnattendedAdmission = .stopped,
        freshness: InboxStore.Freshness = .fresh,
        age: TimeInterval = 5
    ) -> UnattendedGatePresentation {
        .make(
            operation: .init(admission: admission, stops: stops), freshness: freshness,
            lastUpdatedAt: Self.now.addingTimeInterval(-age), now: Self.now)
    }

    @Test func anOpenGateReadsNeutral() {
        let gate = make([], admission: .open)
        #expect(gate == .init(chip: .open, isWax: false, line: "New unattended work is admitted."))
        #expect(gate.chipLabel == "Open")
    }

    @Test func anOperatorStopNamesItsCauseAndAge() {
        let gate = make([
            .init(
                kind: .operator_stop, item_id: "notice", command_id: "cmd",
                since: Self.now.addingTimeInterval(-2 * 3_600))
        ])
        #expect(gate == .init(chip: .stopped, isWax: true, line: "Stopped by operator decision 2h ago."))
        #expect(gate.chipLabel == "Stopped")
    }

    @Test func aBlockingFindingNamesItselfAndCountsTheOtherStops() {
        let gate = make([
            .init(
                kind: .blocking_system_health, item_id: "finding-a", command_id: nil,
                since: Self.now.addingTimeInterval(-30 * 60)),
            .init(kind: .blocking_system_health, item_id: "finding-b", command_id: nil, since: nil),
        ])
        #expect(
            gate
                == .init(
                    chip: .stopped, isWax: true,
                    line: "Stopped by a system health finding 30m ago. 1 other stop is in force."))
    }

    @Test func aNonCurrentSnapshotReportsTheLastKnownStateInNeutral() {
        let stale = SyncCoordinator.stalenessThreshold + 1
        let stopped = make(
            [
                .init(
                    kind: .operator_stop, item_id: "notice", command_id: "cmd",
                    since: Self.now.addingTimeInterval(-3 * 86_400))
            ],
            age: stale)
        #expect(
            stopped
                == .init(
                    chip: .stopped, isWax: false,
                    line: "At the last successful refresh, stopped by operator decision 3d ago."))

        let open = make([], admission: .open, freshness: .unreachable)
        #expect(
            open
                == .init(
                    chip: .open, isWax: false,
                    line: "At the last successful refresh, new unattended work was admitted."))
    }

    @Test func noSnapshotYetReadsUnknown() {
        let gate = UnattendedGatePresentation.make(
            operation: nil, freshness: .fresh, lastUpdatedAt: Self.now, now: Self.now)
        #expect(gate == .init(chip: .unknown, isWax: false, line: "The daemon hasn't reported it yet."))
        #expect(gate.chipLabel == "Unknown")
    }
}

/// A finding row says what it is about (#3026): a task finding names its
/// task as the Unified List does, and any other names its project, in
/// place of the raw diagnostic code.
struct FindingRowContextTests {
    @Test func aTaskFindingNamesItsTask() {
        #expect(FindingRow.context(AttentionFixtures.heldWorkSystemHealth().item) == "Time grammar for inbox rows")
    }

    @Test func aSystemFindingNamesItsProject() {
        let item = AttentionFixtures.blockingSystemHealth().item
        #expect(FindingRow.context(item) == "owner/repo")
        #expect(FindingRow.context(item) != item.health_diagnostic?.value1.code)
    }
}
