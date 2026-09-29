import Foundation

/// The Mac window's title-bar subtitle, which names the instance the window
/// shows; the app icon and menu bar mark show the tier. A `dev-instance.sh`
/// subtitle names the tier too, because a bare worktree name such as
/// "freeside" reads as the product name. Nil means no subtitle, so a prod
/// window on its own daemon keeps a plain title bar.
enum WindowSubtitle {
    /// `serverURL` is the session's selected deployment, set from the
    /// pairing screen on; `readinessDirectory` is the validated
    /// `-FreesideReadinessDir` whose run the session still shows.
    static func text(
        environment: FreesideEnvironment, serverURL: URL?, readinessDirectory: URL?
    ) -> String? {
        switch environment {
        case .prod:
            // A prod app on any port but its own is taken to be on an
            // attended real-work run. That holds because the ephemeral guard
            // keeps every such run off 7331 (plan §10 Rules). No field can
            // say so instead: a typed or persisted address has no readiness
            // stamp, and the API reports no tier, so a prod app pointed at
            // the dev daemon also reads as a real-work run.
            guard let port = serverURL?.port,
                port != FreesideEnvironment.prod.supervisedAPIURL?.port
            else { return nil }
            return "Real-work run on :\(port)"
        case .dev:
            return nil
        case .ephemeral:
            if let worktree = readinessDirectory.flatMap(devInstanceWorktreeName) {
                return "Ephemeral · \(worktree)"
            }
            if let port = serverURL?.port {
                return ":\(port)"
            }
            return "Ephemeral"
        }
    }

    /// `scripts/dev-instance.sh` passes `<worktree>/.dev-instance.XXXXXX/daemon`,
    /// so the worktree is the directory above the `.dev-instance.*` one. Any
    /// other shape names no worktree.
    private static func devInstanceWorktreeName(_ readinessDirectory: URL) -> String? {
        let instance = readinessDirectory.deletingLastPathComponent()
        guard instance.lastPathComponent.hasPrefix(".dev-instance.") else { return nil }
        let worktree = instance.deletingLastPathComponent().lastPathComponent
        return worktree.isEmpty || worktree == "/" ? nil : worktree
    }
}
