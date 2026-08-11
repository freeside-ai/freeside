#if os(macOS)
    import Foundation
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct DaemonServiceTests {
        @Test func anUpdatedEnabledServiceReregistersExactlyOnce() async throws {
            let (defaults, suite) = isolatedDefaults()
            defer { defaults.removePersistentDomain(forName: suite) }
            let registration = FakeServiceRegistration(status: .enabled)
            let service = SMAppDaemonService(service: registration, defaults: defaults)

            try await service.start()
            #expect(registration.unregisterCount == 1)
            #expect(registration.registerCount == 1)
            #expect(defaults.bool(forKey: SMAppDaemonService.registrationCurrentKey))

            try await service.start()
            #expect(registration.unregisterCount == 1)
            #expect(registration.registerCount == 1)
        }

        @Test func stoppingClearsTheRegistrationGeneration() async throws {
            let (defaults, suite) = isolatedDefaults()
            defer { defaults.removePersistentDomain(forName: suite) }
            defaults.set(true, forKey: SMAppDaemonService.registrationCurrentKey)
            let registration = FakeServiceRegistration(status: .enabled)
            let service = SMAppDaemonService(service: registration, defaults: defaults)

            try await service.stop()

            #expect(registration.unregisterCount == 1)
            #expect(!defaults.bool(forKey: SMAppDaemonService.registrationCurrentKey))
            #expect(defaults.bool(forKey: SMAppDaemonService.operatorDisabledKey))
            #expect(!service.needsAutomaticStart)
        }

        @Test func anExplicitStopSurvivesRelaunchUntilAnExplicitStart() async throws {
            let (defaults, suite) = isolatedDefaults()
            defer { defaults.removePersistentDomain(forName: suite) }
            defaults.set(true, forKey: SMAppDaemonService.registrationCurrentKey)
            let enabled = FakeServiceRegistration(status: .enabled)
            let service = SMAppDaemonService(service: enabled, defaults: defaults)

            try await service.stop()

            let stopped = FakeServiceRegistration(status: .notRegistered)
            let relaunched = SMAppDaemonService(service: stopped, defaults: defaults)
            #expect(!relaunched.needsAutomaticStart)

            try await relaunched.start()

            #expect(stopped.registerCount == 1)
            #expect(defaults.bool(forKey: SMAppDaemonService.registrationCurrentKey))
            #expect(!defaults.bool(forKey: SMAppDaemonService.operatorDisabledKey))
        }

        @Test func onlyAnUnregisteredGenerationNeedsAutomaticStart() {
            let (defaults, suite) = isolatedDefaults()
            defer { defaults.removePersistentDomain(forName: suite) }
            let registration = FakeServiceRegistration(status: .notRegistered)
            let service = SMAppDaemonService(service: registration, defaults: defaults)

            #expect(service.needsAutomaticStart)

            defaults.set(true, forKey: SMAppDaemonService.registrationCurrentKey)
            #expect(!service.needsAutomaticStart)
        }

        @Test func approvalPendingRemembersTheCurrentGeneration() async {
            let (defaults, suite) = isolatedDefaults()
            defer { defaults.removePersistentDomain(forName: suite) }
            let registration = FakeServiceRegistration(status: .notRegistered)
            registration.registerError = TestError.denied
            let service = SMAppDaemonService(service: registration, defaults: defaults)

            await #expect(throws: TestError.self) {
                try await service.start()
            }

            #expect(registration.status == .requiresApproval)
            #expect(defaults.bool(forKey: SMAppDaemonService.registrationCurrentKey))
        }

        private func isolatedDefaults() -> (UserDefaults, String) {
            let suite = "DaemonServiceTests.\(UUID().uuidString)"
            guard let defaults = UserDefaults(suiteName: suite) else {
                preconditionFailure("could not create isolated test defaults")
            }
            return (defaults, suite)
        }
    }

    @MainActor
    private final class FakeServiceRegistration: DaemonServiceRegistering {
        var status: DaemonServiceStatus
        var registerError: TestError?
        private(set) var registerCount = 0
        private(set) var unregisterCount = 0

        init(status: DaemonServiceStatus) {
            self.status = status
        }

        func register() throws {
            registerCount += 1
            if let registerError {
                status = .requiresApproval
                throw registerError
            }
            status = .enabled
        }

        func unregister() async {
            unregisterCount += 1
            status = .notRegistered
        }
    }

    private enum TestError: Error {
        case denied
    }
#endif
