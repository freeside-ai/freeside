import FreesideAPI

/// A command the daemon refused with 400 (api/openapi.yaml, POST /commands).
/// The daemon applied and recorded nothing, and a retry meets the same
/// refusal, so every caller drops the command instead of keeping it for retry.
enum CommandRefusal {
    /// The operator message for `refusal`: the update prompt when the daemon's
    /// build serves no command of this kind, as when this app is newer than the
    /// daemon, and otherwise `fallback`, the caller's message for an invalid
    /// command. A reason this app doesn't know reads as `fallback`.
    static func message(
        _ refusal: Operations.submitCommand.Output.BadRequest, otherwise fallback: String
    ) -> String {
        guard let rejection = try? refusal.body.json, rejection.reason == .unknown_command_kind else {
            return fallback
        }
        return unservedKind
    }

    static let unservedKind =
        "This daemon doesn't support this command yet. Update the daemon to use it; nothing was changed."
}
