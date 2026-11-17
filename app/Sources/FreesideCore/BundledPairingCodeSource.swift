#if os(macOS)
    import Foundation

    /// Gets a pairing code by running the `freesided` this app bundles, the
    /// same file its LaunchAgent runs: `freesided pairing-code -state-dir
    /// <this tier's daemon state directory>`. The CLI finds the daemon's
    /// pairing control socket, checks the server's peer UID, and validates
    /// the answer, so the app reimplements none of that and can't drift from
    /// the daemon it supervises (devlog 2026-11-16-1732-pairing-code-via-bundled-cli).
    ///
    /// The CLI's stdout holds the code, so it is parsed and dropped: never
    /// logged, kept, or put in an error. Stderr is discarded unread.
    public struct BundledPairingCodeSource: PairingCodeSource {
        /// Nil when the bundle has no `freesided`, as in an Xcode build.
        let executableURL: URL?
        let arguments: [String]
        let timeout: Duration

        init(executableURL: URL?, arguments: [String], timeout: Duration = .seconds(10)) {
            self.executableURL = executableURL
            self.arguments = arguments
            self.timeout = timeout
        }

        /// The source for `environment`'s supervised daemon, or nil for a
        /// tier with none (`ephemeral`).
        public init?(environment: FreesideEnvironment, bundle: Bundle = .main) {
            guard environment.isSupervised, let stateDirectory = environment.daemonStateDirectory() else {
                return nil
            }
            self.init(
                executableURL: bundle.url(forResource: "freesided", withExtension: nil),
                arguments: ["pairing-code", "-state-dir", stateDirectory.path])
        }

        /// Longer than the CLI's own five-second request timeout, so the
        /// CLI's refusal normally arrives first; this bounds a CLI that hangs.
        public func mint() async throws(PairingCodeFailure) -> MintedPairingCode {
            guard let executableURL else { throw .unavailable }
            let process = Process()
            process.executableURL = executableURL
            process.arguments = arguments
            let stdout = Pipe()
            process.standardOutput = stdout
            process.standardError = FileHandle.nullDevice
            process.standardInput = FileHandle.nullDevice

            // Exit, output, and the deadline race on one stream. Unstructured
            // tasks, because a blocked read can't be cancelled and a task
            // group would wait for it.
            let (events, continuation) = AsyncStream.makeStream(of: Event.self)
            process.terminationHandler = { finished in
                continuation.yield(.exited(finished.terminationStatus, finished.terminationReason))
            }
            do {
                try process.run()
            } catch {
                throw .unavailable
            }
            let reader = stdout.fileHandleForReading
            Thread.detachNewThread {
                continuation.yield(.output(reader.readDataToEndOfFile()))
            }
            let deadline = Task {
                try? await Task.sleep(for: timeout)
                continuation.yield(.timedOut)
            }
            // On every way out, including the caller's cancellation, which
            // ends the stream: a CLI left running would still mint a code
            // nobody sees.
            defer {
                deadline.cancel()
                if process.isRunning { process.terminate() }
            }

            var exit: (status: Int32, reason: Process.TerminationReason)?
            var output: Data?
            for await event in events {
                switch event {
                case .exited(let status, let reason): exit = (status, reason)
                case .output(let data): output = data
                case .timedOut:
                    if process.isRunning { process.terminate() }
                    throw .timedOut
                }
                guard let exit, let output else { continue }
                guard exit.reason == .exit, exit.status == 0 else {
                    throw .refused(exitStatus: exit.status)
                }
                guard let minted = MintedPairingCode.parse(output) else { throw .unreadableOutput }
                return minted
            }
            // The stream ends early only when the calling task is cancelled.
            // Its caller has stopped listening, so this error is never shown.
            throw .unreadableOutput
        }

        private enum Event: Sendable {
            case exited(Int32, Process.TerminationReason)
            case output(Data)
            case timedOut
        }
    }
#endif
