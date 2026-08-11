#if os(macOS)
    import Foundation
    import FreesideAPI
    import Observation

    public struct DaemonHealth: Equatable, Sendable {
        public let version: String
        public let startedAt: Date

        public init(version: String, startedAt: Date) {
            self.version = version
            self.startedAt = startedAt
        }
    }

    public protocol DaemonHealthChecking: Sendable {
        func health(at serverURL: URL) async throws -> DaemonHealth
    }

    public struct APIDaemonHealthChecker: DaemonHealthChecking {
        private let makeClient: @Sendable (URL) -> any APIProtocol
        private let timeout: Duration

        public init(
            timeout: Duration = .seconds(3),
            makeClient: @escaping @Sendable (URL) -> any APIProtocol = {
                APIClientFactory.live(serverURL: $0)
            }
        ) {
            self.timeout = timeout
            self.makeClient = makeClient
        }

        public func health(at serverURL: URL) async throws -> DaemonHealth {
            let makeClient = self.makeClient
            let timeout = self.timeout
            return try await withThrowingTaskGroup(of: DaemonHealth.self) { group in
                group.addTask {
                    let body = try await makeClient(serverURL).getHealth().ok.body.json
                    return DaemonHealth(version: body.version, startedAt: body.started_at)
                }
                group.addTask {
                    try await Task.sleep(for: timeout)
                    throw DaemonHealthProbeTimeout()
                }
                defer { group.cancelAll() }
                guard let result = try await group.next() else {
                    throw DaemonHealthProbeTimeout()
                }
                return result
            }
        }
    }

    private struct DaemonHealthProbeTimeout: Error {}

    public enum DaemonMenuState: Equatable, Sendable {
        case checking
        case stopped
        case needsApproval
        case unavailable
        case unreachable
        case running(DaemonHealth, restartObserved: Bool)

    }

    @MainActor
    @Observable
    public final class DaemonMenuModel {
        public private(set) var state: DaemonMenuState
        public private(set) var readiness: DaemonReadiness?
        public private(set) var actionError: String?

        private let service: any DaemonServiceControlling
        private let healthChecker: any DaemonHealthChecking
        private let readReadiness: () -> DaemonReadiness?
        private let pollingInterval: Duration
        private let registerOnFirstRun: Bool
        private var lastStartedAt: Date?
        private var restartObserved = false
        private var monitoringTask: Task<Void, Never>?
        private var refreshGeneration = 0
        private var actionErrorKind: ActionErrorKind?

        private enum ActionErrorKind {
            case start
            case stop
        }

        public convenience init() {
            self.init(
                service: SMAppDaemonService(),
                healthChecker: APIDaemonHealthChecker(),
                readReadiness: {
                    DaemonReadinessReader.defaultFileURL()
                        .flatMap { DaemonReadinessReader().read(at: $0) }
                })
        }

        public init(
            service: any DaemonServiceControlling,
            healthChecker: any DaemonHealthChecking,
            pollingInterval: Duration = .seconds(5),
            registerOnFirstRun: Bool = true,
            readReadiness: @escaping () -> DaemonReadiness?
        ) {
            self.service = service
            self.healthChecker = healthChecker
            self.pollingInterval = pollingInterval
            self.registerOnFirstRun = registerOnFirstRun
            self.readReadiness = readReadiness
            state = Self.serviceState(service.status)
        }

        isolated deinit {
            monitoringTask?.cancel()
        }

        public func startMonitoring() {
            guard monitoringTask == nil else { return }
            monitoringTask = Task { [weak self] in
                if self?.registerOnFirstRun == true, self?.service.needsAutomaticStart == true {
                    await self?.start()
                }
                while !Task.isCancelled {
                    await self?.refresh()
                    guard let interval = self?.pollingInterval else { return }
                    do {
                        try await Task.sleep(for: interval)
                    } catch {
                        return
                    }
                }
            }
        }

        public func stopMonitoring() {
            monitoringTask?.cancel()
            monitoringTask = nil
        }

        public func start() async {
            actionError = nil
            actionErrorKind = nil
            do {
                try await service.start()
            } catch {
                actionErrorKind = .start
                actionError = "The daemon could not be started: \(error.localizedDescription)"
            }
            await refresh()
        }

        public func stop() async {
            actionError = nil
            actionErrorKind = nil
            do {
                try await service.stop()
            } catch {
                actionErrorKind = .stop
                actionError = "The daemon could not be stopped: \(error.localizedDescription)"
            }
            await refresh()
        }

        public func openApprovalSettings() {
            service.openApprovalSettings()
        }

        func refresh() async {
            refreshGeneration += 1
            let generation = refreshGeneration
            switch service.status {
            case .notRegistered:
                clearRecoveredStopError()
                readiness = nil
                state = .stopped
            case .requiresApproval:
                readiness = nil
                state = .needsApproval
            case .notFound:
                readiness = nil
                state = .unavailable
            case .enabled:
                let currentReadiness = readReadiness().flatMap { readiness in
                    readiness.apiURL == DaemonReadinessReader.supervisedAPIURL
                        ? readiness : nil
                }
                readiness = currentReadiness
                let serverURL =
                    currentReadiness?.apiURL
                    ?? DaemonReadinessReader.supervisedAPIURL
                do {
                    let health = try await healthChecker.health(at: serverURL)
                    guard generation == refreshGeneration, service.status == .enabled else {
                        return
                    }
                    if let lastStartedAt, lastStartedAt != health.startedAt {
                        restartObserved = true
                    }
                    lastStartedAt = health.startedAt
                    if actionErrorKind == .start {
                        actionError = nil
                        actionErrorKind = nil
                    }
                    state = .running(health, restartObserved: restartObserved)
                } catch {
                    guard generation == refreshGeneration, service.status == .enabled else {
                        return
                    }
                    state = .unreachable
                }
            }
        }

        private func clearRecoveredStopError() {
            if actionErrorKind == .stop {
                actionError = nil
                actionErrorKind = nil
            }
        }

        private static func serviceState(_ status: DaemonServiceStatus) -> DaemonMenuState {
            switch status {
            case .notRegistered:
                return .stopped
            case .enabled:
                return .checking
            case .requiresApproval:
                return .needsApproval
            case .notFound:
                return .unavailable
            }
        }
    }
#endif
