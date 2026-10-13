#if os(macOS)
    import Foundation
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct DaemonLifecycleCommandTests {
        @Test func parsesOnlyDocumentedCommands() {
            for action in ["start", "stop", "status"] {
                #expect(DaemonLifecycleCommand(arguments: ["daemon", action])?.timeoutSeconds == 30)
                #expect(DaemonLifecycleCommand(arguments: ["daemon", action, "--timeout", "7"])?.timeoutSeconds == 7)
            }
            for arguments in [
                [], ["daemon"], ["daemon", "restart"], ["other", "start"],
                ["daemon", "start", "extra"], ["daemon", "start", "--wrong", "1"],
                ["daemon", "stop", "--timeout", "0"], ["daemon", "stop", "--timeout", "-1"],
                ["daemon", "stop", "--timeout", "3601"], ["daemon", "stop", "--timeout", "nan"],
                ["daemon", "stop", "--timeout", "999999999999999999999"],
                ["daemon", "stop", "--timeout", "1", "extra"],
            ] {
                #expect(DaemonLifecycleCommand(arguments: arguments) == nil)
            }
            #expect(DaemonLifecycleCommand.usage.exitCode == 64)
        }

        @Test func startAndStopUseTheServiceAndWaitForHealth() async throws {
            for action in ["start", "stop"] {
                let service = CommandService(status: action == "start" ? .notRegistered : .enabled)
                let health = CommandHealth(values: action == "start" ? [false, true] : [true, false])
                let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", action]))
                let result = await command.run(
                    environment: .prod,
                    makeService: { plist in
                        #expect(plist == "ai.freeside.daemon.plist")
                        return service
                    }, healthChecker: health, clock: FastPollClock())
                #expect(result.exitCode == 0)
                #expect(service.calls == [action])
                #expect(await health.calls == 2)
                #expect(await health.lastURL == URL(string: "http://127.0.0.1:7331"))
                #expect(
                    result.line
                        == (action == "start"
                            ? "action=start registration=enabled health=reachable"
                            : "action=stop registration=not_registered health=unreachable"))
            }
        }

        @Test func statusCoversEveryRegistrationAndHealthPair() async throws {
            let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", "status"]))
            for status: DaemonServiceStatus in [.enabled, .notRegistered, .notFound, .requiresApproval] {
                for healthy in [true, false] {
                    let service = CommandService(status: status)
                    let health = CommandHealth(values: [healthy])
                    let result = await command.run(
                        environment: .dev,
                        makeService: { plist in
                            #expect(plist == "ai.freeside.daemon.dev.plist")
                            return service
                        }, healthChecker: health)
                    let agrees = (status == .enabled && healthy) || (status == .notRegistered && !healthy)
                    #expect(result.exitCode == (agrees ? 0 : (status == .requiresApproval ? 3 : 1)))
                    #expect(service.calls.isEmpty)
                    #expect(result.line.contains("health=\(healthy ? "reachable" : "unreachable")"))
                    #expect(await health.lastURL == URL(string: "http://127.0.0.1:7332"))
                }
            }
        }

        @Test func approvalIsReportedWhetherStartThrowsOrReturns() async throws {
            let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", "start"]))
            for throwsError in [false, true] {
                let service = CommandService(status: .requiresApproval)
                service.failure = throwsError
                let health = CommandHealth(values: [true])
                let result = await command.run(environment: .prod, makeService: { _ in service }, healthChecker: health)
                #expect(result.exitCode == 3)
                #expect(result.line.contains("error=login_items_approval_required"))
                #expect(await health.calls == 0)
            }
        }

        @Test func serviceFailureHasItsOwnOutcome() async throws {
            for action in ["start", "stop"] {
                let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", action]))
                let service = CommandService(status: .notFound)
                service.failure = true
                let result = await command.run(environment: .prod, makeService: { _ in service })
                #expect(result.exitCode == 1)
                #expect(result.line.contains("error=service_operation_failed"))
            }
        }

        @Test func eachActionReturnsWhenItsHealthProbeNeverFinishes() async throws {
            for action in ["start", "stop", "status"] {
                let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", action, "--timeout", "1"]))
                let service = CommandService(status: .enabled)
                let gate = ProbeGate()
                let result = await command.run(
                    environment: .prod, makeService: { _ in service }, healthChecker: gate,
                    clock: gate)
                #expect(result.exitCode == 4)
                #expect(result.line.contains("health=unknown error=timeout"))
                await gate.release()
            }
        }

        @Test func deadlineDoesNotWaitForUnregisterCancellation() async throws {
            let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", "stop", "--timeout", "1"]))
            let service = CommandService(status: .enabled)
            let gate = ProbeGate()
            service.stopGate = gate
            let result = await command.run(
                environment: .prod, makeService: { _ in service },
                clock: gate)
            #expect(result.exitCode == 4)
            #expect(service.calls == ["stop"])
            await gate.release()
        }

        @Test func ephemeralRefusesBeforeConstructingTheService() async throws {
            for action in ["start", "stop", "status"] {
                let command = try #require(DaemonLifecycleCommand(arguments: ["daemon", action]))
                let result = await command.run(
                    environment: .ephemeral,
                    makeService: { _ in
                        Issue.record("Constructed a service for an ephemeral command")
                        return CommandService(status: .enabled)
                    })
                #expect(result.exitCode == 78)
                #expect(result.line == "error=unsupervised tier=ephemeral")
            }
        }
    }

    private struct FastPollClock: DaemonLifecycleClock {
        func sleep(for duration: Duration) async throws {
            if duration >= .seconds(1) { try await Task.sleep(for: duration) } else { await Task.yield() }
        }
    }

    private struct CommandFailure: Error {}

    @MainActor private final class CommandService: DaemonServiceControlling {
        var status: DaemonServiceStatus
        var needsAutomaticStart = false
        var failure = false
        var calls: [String] = []
        var stopGate: ProbeGate?
        init(status: DaemonServiceStatus) { self.status = status }
        func start() async throws {
            calls.append("start")
            if failure { throw CommandFailure() }
            if status != .requiresApproval { status = .enabled }
        }
        func stop() async throws {
            calls.append("stop")
            if failure { throw CommandFailure() }
            if let stopGate {
                let url = try #require(URL(string: "http://localhost"))
                _ = try await stopGate.health(at: url)
            }
            status = .notRegistered
        }
        func openApprovalSettings() { Issue.record("Command opened approval UI") }
    }

    private actor CommandHealth: DaemonHealthChecking {
        var values: [Bool]
        var calls = 0
        var lastURL: URL?
        init(values: [Bool]) { self.values = values }
        func health(at serverURL: URL) async throws -> DaemonHealth {
            lastURL = serverURL
            calls += 1
            let value = values.count > 1 ? values.removeFirst() : values[0]
            guard value else { throw CommandFailure() }
            return DaemonHealth(version: "test", startedAt: .distantPast)
        }
    }

    /// Holds a dependency past cancellation, then releases it after the
    /// command has returned. No wall-clock race determines the test outcome.
    private actor ProbeGate: DaemonHealthChecking, DaemonLifecycleClock {
        var probe: CheckedContinuation<Void, Never>?
        var waiter: CheckedContinuation<Void, Never>?
        func health(at serverURL: URL) async throws -> DaemonHealth {
            await withCheckedContinuation { continuation in
                probe = continuation
                waiter?.resume()
                waiter = nil
            }
            throw CommandFailure()
        }
        func sleep(for duration: Duration) async throws {
            if probe != nil { return }
            await withCheckedContinuation { waiter = $0 }
        }
        func release() {
            probe?.resume()
            probe = nil
        }
    }
#endif
