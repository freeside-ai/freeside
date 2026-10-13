#if os(macOS)
    import Foundation

    public protocol DaemonLifecycleClock: Sendable {
        func sleep(for duration: Duration) async throws
    }

    public struct SystemDaemonLifecycleClock: DaemonLifecycleClock {
        public init() {}
        public func sleep(for duration: Duration) async throws {
            try await Task.sleep(for: duration)
        }
    }

    /// The installed app's non-UI lifecycle front end. Its result is one line
    /// of stable key=value fields for shell callers; diagnostics use tokens.
    public struct DaemonLifecycleCommand: Equatable, Sendable {
        public enum Action: String, Sendable { case start, stop, status }
        public let action: Action
        public let timeoutSeconds: Int

        public struct Result: Equatable, Sendable {
            public let exitCode: Int32
            public let line: String
        }

        public static let usage = Result(
            exitCode: 64,
            line: "error=usage expected=daemon_start|stop|status_[--timeout_SECONDS]")

        public init?(arguments: [String]) {
            guard arguments.count == 2 || arguments.count == 4,
                arguments[0] == "daemon", let action = Action(rawValue: arguments[1])
            else { return nil }
            var timeout = 30
            if arguments.count == 4 {
                guard arguments[2] == "--timeout", let value = Int(arguments[3]),
                    (1...3600).contains(value)
                else { return nil }
                timeout = value
            }
            self.action = action
            timeoutSeconds = timeout
        }

        /// Refuse an unsupervised tier before constructing a registration
        /// boundary. The factories also let tests prove that refusal is inert.
        @MainActor
        public func run(
            environment: FreesideEnvironment,
            makeService: (String) -> any DaemonServiceControlling = {
                SMAppDaemonService(plistName: $0)
            },
            healthChecker: any DaemonHealthChecking = APIDaemonHealthChecker(),
            clock: any DaemonLifecycleClock = SystemDaemonLifecycleClock()
        ) async -> Result {
            guard let plist = environment.launchAgentPlistName,
                let url = environment.supervisedAPIURL
            else {
                return Result(exitCode: 78, line: "error=unsupervised tier=ephemeral")
            }
            let service = makeService(plist)
            let completion = Completion(action: action)
            completion.registration = service.status
            // Do not join a task group here: SMAppService.unregister can stay
            // suspended after cancellation. The CLI must still return on time.
            return await withCheckedContinuation { continuation in
                completion.continuation = continuation
                completion.operation = Task { @MainActor in
                    do {
                        switch action {
                        case .start: try await service.start()
                        case .stop: try await service.stop()
                        case .status: break
                        }
                    } catch {
                        completion.registration = service.status
                        let approval = action == .start && service.status == .requiresApproval
                        completion.finish(
                            code: approval ? 3 : 1,
                            error: approval ? "login_items_approval_required" : "service_operation_failed")
                        return
                    }
                    while !Task.isCancelled {
                        completion.registration = service.status
                        if action == .start && service.status == .requiresApproval {
                            completion.finish(code: 3, error: "login_items_approval_required")
                            return
                        }
                        completion.healthy = (try? await healthChecker.health(at: url)) != nil
                        guard !Task.isCancelled else { return }
                        completion.registration = service.status
                        let running = service.status == .enabled && completion.healthy == true
                        let stopped = service.status == .notRegistered && completion.healthy == false
                        switch action {
                        case .start where running, .stop where stopped:
                            completion.finish(code: 0)
                            return
                        case .status:
                            let approval = service.status == .requiresApproval
                            completion.finish(
                                code: running || stopped ? 0 : (approval ? 3 : 1),
                                error: running || stopped
                                    ? nil : (approval ? "login_items_approval_required" : "state_mismatch"))
                            return
                        default: break
                        }
                        do { try await clock.sleep(for: .milliseconds(250)) } catch { return }
                    }
                }
                completion.timer = Task { @MainActor in
                    do { try await clock.sleep(for: .seconds(timeoutSeconds)) } catch { return }
                    completion.finish(code: 4, error: "timeout")
                }
            }
        }

        @MainActor
        private final class Completion {
            let action: Action
            var registration: DaemonServiceStatus?
            var healthy: Bool?
            var continuation: CheckedContinuation<Result, Never>?
            var operation: Task<Void, Never>?
            var timer: Task<Void, Never>?

            init(action: Action) { self.action = action }

            func finish(code: Int32, error: String? = nil) {
                guard let continuation else { return }
                self.continuation = nil
                operation?.cancel()
                timer?.cancel()
                let state: String
                switch registration {
                case .enabled: state = "enabled"
                case .notRegistered: state = "not_registered"
                case .requiresApproval: state = "requires_approval"
                case .notFound: state = "not_found"
                case nil: state = "unknown"
                }
                let health = healthy.map { $0 ? "reachable" : "unreachable" } ?? "unknown"
                let diagnostic = error.map { " error=\($0)" } ?? ""
                continuation.resume(
                    returning: Result(
                        exitCode: code,
                        line: "action=\(action.rawValue) registration=\(state) health=\(health)\(diagnostic)"))
            }
        }
    }
#endif
