import FreesideAPI

/// Why this app and a daemon can't talk (#3109), decided from the
/// compatibility level and floor the daemon reports on `/health` against
/// this app's compiled-in `APIContract.compatibilityLevel`. The daemon never
/// refuses a client on version; the app refuses itself, and a different
/// contract digest alone is never a reason (api/README.md, Compatibility
/// Level). Carries the daemon's build label so the refusal can name it.
public enum DaemonIncompatibility: Equatable, Sendable {
    /// The daemon's floor is above this app's level: this app is too old
    /// for the daemon and must be updated.
    case appBelowFloor(floor: Int, daemonVersion: String)
    /// The daemon's level is below this app's level: the daemon is too old
    /// for this app and must be updated. An app's minimum daemon level is
    /// its own level.
    case daemonBelowLevel(level: Int, daemonVersion: String)

    /// The verdict for a daemon's reported level and floor, or nil when the
    /// app may sync. A daemon built before the fields existed omits them,
    /// and an omitted field reads as 0.
    public static func verdict(
        daemonLevel: Int?, daemonFloor: Int?, daemonVersion: String,
        appLevel: Int = APIContract.compatibilityLevel
    ) -> DaemonIncompatibility? {
        let floor = daemonFloor ?? 0
        let level = daemonLevel ?? 0
        if floor > appLevel { return .appBelowFloor(floor: floor, daemonVersion: daemonVersion) }
        if level < appLevel { return .daemonBelowLevel(level: level, daemonVersion: daemonVersion) }
        return nil
    }

    /// The verdict for a `/health` answer.
    public static func verdict(
        for health: Components.Schemas.HealthStatus, appLevel: Int = APIContract.compatibilityLevel
    ) -> DaemonIncompatibility? {
        verdict(
            daemonLevel: health.compatibility_level, daemonFloor: health.compatibility_floor,
            daemonVersion: health.version, appLevel: appLevel)
    }

    /// The daemon's build label.
    public var daemonVersion: String {
        switch self {
        case .appBelowFloor(_, let version), .daemonBelowLevel(_, let version): version
        }
    }
}
