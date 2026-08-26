#if os(macOS)
    import Foundation
    import FreesideAPI
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct DaemonMenuModelTests {
        @Test func generatedHealthClientDrivesRunningRestartAndOutage() async throws {
            let server = MockServer(authMode: .enforcing)
            let checker = APIDaemonHealthChecker { _ in APIClientFactory.mock(server: server) }
            let serverURL = try #require(URL(string: "http://127.0.0.1:7331"))

            let first = try await checker.health(at: serverURL)
            #expect(first.version == "mock")

            let restartedAt = Date(timeIntervalSince1970: 1_725_184_860)
            await server.restart(version: "mock-2", startedAt: restartedAt)
            let restarted = try await checker.health(at: serverURL)
            #expect(restarted == DaemonHealth(version: "mock-2", startedAt: restartedAt))

            await server.setHealthAvailable(false)
            await #expect(throws: (any Error).self) {
                _ = try await checker.health(at: serverURL)
            }
        }

        @Test func generatedHealthClientBoundsAStalledProbe() async throws {
            let server = MockServer(authMode: .enforcing)
            await server.setBeforeRespond { operationID in
                if operationID == "getHealth" {
                    try await Task.sleep(for: .seconds(1))
                }
            }
            let checker = APIDaemonHealthChecker(timeout: .milliseconds(20)) { _ in
                APIClientFactory.mock(server: server)
            }
            let serverURL = try #require(URL(string: "http://127.0.0.1:7331"))

            await #expect(throws: DaemonHealthProbeTimeout.self) {
                _ = try await checker.health(at: serverURL)
            }
        }

        @Test func serviceAndHealthMapToEveryMenuState() async {
            let service = FakeDaemonService(status: .notRegistered)
            let health = ScriptedDaemonHealth()
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { nil })

            await model.refresh()
            #expect(model.state == .stopped)

            service.status = .requiresApproval
            await model.refresh()
            #expect(model.state == .needsApproval)

            service.status = .notFound
            await model.refresh()
            #expect(model.state == .unavailable)

            service.status = .enabled
            await health.append(.failure)
            await model.refresh()
            #expect(model.state == .unreachable)

            let first = DaemonHealth(
                version: "1.0.0", startedAt: Date(timeIntervalSince1970: 1_725_184_800))
            await health.append(.health(first))
            await model.refresh()
            #expect(model.state == .running(first, restartObserved: false))

            let restarted = DaemonHealth(
                version: "1.0.1", startedAt: Date(timeIntervalSince1970: 1_725_184_860))
            await health.append(.health(restarted))
            await model.refresh()
            #expect(model.state == .running(restarted, restartObserved: true))
        }

        @Test func startStopAndApprovalActionsUseOnlyTheFacade() async {
            let service = FakeDaemonService(status: .notRegistered)
            let health = ScriptedDaemonHealth([.failure])
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { nil })

            await model.start()
            #expect(service.startCount == 1)
            #expect(model.state == .unreachable)

            await model.stop()
            #expect(service.stopCount == 1)
            #expect(model.state == .stopped)

            model.openApprovalSettings()
            #expect(service.openSettingsCount == 1)
        }

        @Test func healthyPollingClearsARecoveredStartError() async {
            let service = FakeDaemonService(status: .notRegistered)
            service.startError = .denied
            let recovered = DaemonHealth(
                version: "1.0.0", startedAt: Date(timeIntervalSince1970: 1_725_184_800))
            let health = ScriptedDaemonHealth([.health(recovered)])
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { nil })

            await model.start()
            #expect(model.state == .needsApproval)
            #expect(model.actionError != nil)

            service.startError = nil
            service.status = .enabled
            await model.refresh()

            #expect(model.state == .running(recovered, restartObserved: false))
            #expect(model.actionError == nil)
        }

        @Test func healthyPollingPreservesAStopErrorUntilTheServiceDisables() async {
            let service = FakeDaemonService(status: .enabled)
            service.stopError = .denied
            let stillRunning = DaemonHealth(
                version: "1.0.0", startedAt: Date(timeIntervalSince1970: 1_725_184_800))
            let health = ScriptedDaemonHealth([.health(stillRunning)])
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { nil })

            await model.stop()

            #expect(model.state == .running(stillRunning, restartObserved: false))
            #expect(model.actionError != nil)

            service.status = .notRegistered
            await model.refresh()
            #expect(model.state == .stopped)
            #expect(model.actionError == nil)
        }

        @Test func nonStoppedStatesPreserveAStopErrorUntilTheServiceDisables() async {
            for (status, expectedState) in [
                (DaemonServiceStatus.requiresApproval, DaemonMenuState.needsApproval),
                (DaemonServiceStatus.notFound, DaemonMenuState.unavailable),
            ] {
                let service = FakeDaemonService(status: status)
                service.stopError = .denied
                let model = DaemonMenuModel(
                    service: service,
                    healthChecker: ScriptedDaemonHealth(),
                    registerOnFirstRun: false,
                    readReadiness: { nil })

                await model.stop()

                #expect(model.state == expectedState)
                #expect(model.actionError != nil)

                service.stopError = nil
                await model.stop()
                #expect(model.state == .stopped)
                #expect(model.actionError == nil)
            }
        }

        @Test func aCompletedStopDiscardsAnOlderHealthResponse() async {
            let service = FakeDaemonService(status: .enabled)
            let health = SuspendedDaemonHealth()
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { nil })
            let olderRefresh = Task { await model.refresh() }
            await health.waitUntilRequested()

            await model.stop()
            #expect(model.state == .stopped)

            await health.succeed(
                DaemonHealth(
                    version: "late", startedAt: Date(timeIntervalSince1970: 1_725_184_800)))
            await olderRefresh.value
            #expect(model.state == .stopped)
        }

        @Test func supervisedReadinessIsPublishedForPairing() async throws {
            let service = FakeDaemonService(status: .enabled)
            let health = ScriptedDaemonHealth([
                .health(
                    DaemonHealth(
                        version: "1.0.0",
                        startedAt: Date(timeIntervalSince1970: 1_725_184_800)))
            ])
            let readiness = try DaemonReadiness.parse(
                Data(#"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#.utf8))
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { readiness })

            await model.refresh()

            #expect(model.readiness == readiness)
            #expect(await health.lastURL == readiness.apiURL)
        }

        @Test func nonSupervisedReadinessIsIgnoredForHealth() async throws {
            let service = FakeDaemonService(status: .enabled)
            let health = ScriptedDaemonHealth([
                .health(
                    DaemonHealth(
                        version: "1.0.0",
                        startedAt: Date(timeIntervalSince1970: 1_725_184_800)))
            ])
            let staleReadiness = try DaemonReadiness.parse(
                Data(#"{"api_url":"http://127.0.0.1:49152","pairing_code":"stale"}"#.utf8))
            let model = DaemonMenuModel(
                service: service,
                healthChecker: health,
                registerOnFirstRun: false,
                readReadiness: { staleReadiness })

            await model.refresh()

            #expect(model.readiness == nil)
            #expect(await health.lastURL == DaemonReadinessReader.supervisedAPIURL)
        }
    }

    @MainActor
    private final class FakeDaemonService: DaemonServiceControlling {
        var status: DaemonServiceStatus
        var needsAutomaticStart = true
        var startError: TestError?
        var stopError: TestError?
        private(set) var startCount = 0
        private(set) var stopCount = 0
        private(set) var openSettingsCount = 0

        init(status: DaemonServiceStatus) {
            self.status = status
        }

        func start() async throws {
            startCount += 1
            if let startError {
                status = .requiresApproval
                throw startError
            }
            status = .enabled
        }

        func stop() async throws {
            stopCount += 1
            if let stopError {
                throw stopError
            }
            status = .notRegistered
        }

        func openApprovalSettings() {
            openSettingsCount += 1
        }
    }

    private enum TestError: Error {
        case denied
    }

    private actor ScriptedDaemonHealth: DaemonHealthChecking {
        enum Step: Sendable {
            case health(DaemonHealth)
            case failure
        }

        private var steps: [Step]
        private(set) var lastURL: URL?

        init(_ steps: [Step] = []) {
            self.steps = steps
        }

        func append(_ step: Step) {
            steps.append(step)
        }

        func health(at serverURL: URL) async throws -> DaemonHealth {
            lastURL = serverURL
            guard !steps.isEmpty else { throw URLError(.cannotConnectToHost) }
            switch steps.removeFirst() {
            case .health(let health):
                return health
            case .failure:
                throw URLError(.cannotConnectToHost)
            }
        }
    }

    private actor SuspendedDaemonHealth: DaemonHealthChecking {
        private var requested = false
        private var continuation: CheckedContinuation<DaemonHealth, Never>?

        func health(at serverURL: URL) async throws -> DaemonHealth {
            requested = true
            return await withCheckedContinuation { continuation = $0 }
        }

        func waitUntilRequested() async {
            while !requested {
                await Task.yield()
            }
        }

        func succeed(_ health: DaemonHealth) {
            continuation?.resume(returning: health)
            continuation = nil
        }
    }
#endif
