import FreesideModel
import Testing

/// The four contract cases of #3109, decided against an explicit app level
/// so each holds whatever level this build carries.
@Suite struct DaemonIncompatibilityTests {
    private func verdict(level: Int?, floor: Int?, app: Int) -> DaemonIncompatibility? {
        DaemonIncompatibility.verdict(daemonLevel: level, daemonFloor: floor, daemonVersion: "1.2.3", appLevel: app)
    }

    @Test func aSameLevelDaemonIsCompatible() {
        #expect(verdict(level: 3, floor: 1, app: 3) == nil)
    }

    @Test func aNewerDaemonWhoseFloorAdmitsThisAppIsCompatible() {
        #expect(verdict(level: 5, floor: 3, app: 3) == nil)
        #expect(verdict(level: 5, floor: 2, app: 3) == nil)
    }

    @Test func aDaemonWhoseFloorIsAboveThisAppRefusesTheApp() {
        #expect(verdict(level: 5, floor: 4, app: 3) == .appBelowFloor(floor: 4, daemonVersion: "1.2.3"))
    }

    @Test func aDaemonBelowThisAppsLevelNeedsAnUpdate() {
        #expect(verdict(level: 2, floor: 0, app: 3) == .daemonBelowLevel(level: 2, daemonVersion: "1.2.3"))
    }

    @Test func omittedFieldsReadAsZero() {
        #expect(verdict(level: nil, floor: nil, app: 0) == nil)
        #expect(verdict(level: nil, floor: nil, app: 1) == .daemonBelowLevel(level: 0, daemonVersion: "1.2.3"))
        #expect(verdict(level: 1, floor: nil, app: 1) == nil)
    }

    @Test func theFloorIsJudgedBeforeTheLevel() {
        // A malformed answer (floor above level) still reads as the daemon
        // refusing this app, the side that tells the user which to update.
        #expect(verdict(level: 1, floor: 4, app: 3) == .appBelowFloor(floor: 4, daemonVersion: "1.2.3"))
    }
}
