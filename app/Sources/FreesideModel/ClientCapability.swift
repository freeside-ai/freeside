import FreesideAPI

/// The device's decision-action capability contract (plan §8): the set of
/// actions this app build can present and submit. The daemon intersects it with
/// each item's requested decisions to derive the action surface.
public enum ClientCapability {
    /// Every action whose accepted effect this build can carry, i.e. every
    /// action `ActionOutcome.of` does not classify as `.pending`. A new
    /// `Action` member is force-classified in `ActionOutcome`, so it joins or
    /// stays out of the contract there, never silently.
    public static var presentableActions: [Components.Schemas.Action] {
        Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
    }

    /// A stable local fingerprint of an action set, so session start can skip
    /// re-registering an unchanged contract without needing the daemon's
    /// content-address digest. The build's set is static, so this changes only
    /// when a release adds or removes an action.
    public static func fingerprint(of actions: [Components.Schemas.Action]) -> String {
        actions.map(\.rawValue).sorted().joined(separator: ",")
    }
}
