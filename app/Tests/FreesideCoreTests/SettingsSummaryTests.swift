#if os(macOS)
    import Foundation
    import FreesideAPI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    /// What each row of the iPhone's Settings list trails. The summaries only
    /// report what the app already holds; Agents and Usage are covered with
    /// their models (`RoutingPolicyModelTests`, `AccountUsageModelTests`).
    @MainActor
    @Suite struct SettingsSummaryTests {
        @Test func generalReadsTheDecisionFlowToggle() throws {
            let suite = "SettingsSummaryTests-\(UUID().uuidString)"
            let defaults = try #require(UserDefaults(suiteName: suite))
            defer { defaults.removePersistentDomain(forName: suite) }
            let preferences = DecisionFlowPreferences(defaults: defaults)
            #expect(SettingsSummary.general(preferences) == "Advance off")
            preferences.advancesToNextItem = true
            #expect(SettingsSummary.general(preferences) == "Advance on")
        }

        @Test func theDevicesRowCountsTheListOnceLoaded() async {
            let devices = await DevicesScreenshots.model()
            #expect(SettingsSummary.devices(devices, client: .mac) == "This Mac · 1 other · 1 revoked")
            #expect(SettingsSummary.devices(devices, client: .iPhone) == "This iPhone · 1 other · 1 revoked")
            #expect(SettingsSummary.devices(devices, client: .iPad) == "This iPad · 1 other · 1 revoked")
        }

        @Test func theDevicesRowSaysSoBeforeTheListLoads() {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            let devices = DevicesModel(coordinator: coordinator, signOut: {})
            #expect(SettingsSummary.devices(devices, client: .mac) == "Loading…")
        }

        @Test func notificationsReadThisDevicesSwitch() async {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            #expect(SettingsSummary.notifications(DevicesModel(coordinator: coordinator, signOut: {})) == "Loading…")

            let devices = await DevicesScreenshots.model()
            #expect(SettingsSummary.notifications(devices) == "Off")
            await devices.setNotifications(DeviceFixtures.currentDeviceID, enabled: true)
            #expect(SettingsSummary.notifications(devices) == "On")
        }

        @Test func theDaemonRowReadsFreshness() {
            #expect(SettingsSummary.daemon(.fresh) == "Connected")
            #expect(SettingsSummary.daemon(.unvalidated) == "Checking")
            #expect(SettingsSummary.daemon(.unreachable) == "Unreachable")
            #expect(SettingsSummary.daemon(.syncFailing) == "Sync failing")
            #expect(
                SettingsSummary.daemon(.incompatible(.appBelowFloor(floor: 1, daemonVersion: "1.0.0")))
                    == "Update needed")
            #expect(
                SettingsSummary.daemon(.incompatible(.daemonBelowLevel(level: 0, daemonVersion: "1.0.0")))
                    == "Update needed")
            #expect(SettingsSummary.daemon(.unauthenticated) == "Revoked")
        }

        @Test func systemHealthCountsOpenAndBlockingFindings() {
            func finding(
                _ id: String, posture: Components.Schemas.HealthPosture
            ) -> Components.Schemas.AttentionItemSnapshot {
                var snapshot = AttentionFixtures.fixture(type: .system_health)
                snapshot.item.id = id
                snapshot.item.posture = .init(value1: posture)
                return snapshot
            }
            let closed = AttentionFixtures.resolvedSystemHealth()
            let two = SystemHealthFindings(
                open: [finding("a", posture: .advisory), finding("b", posture: .blocking)], closed: [closed])
            let one = SystemHealthFindings(open: [finding("a", posture: .advisory)], closed: [])
            let none = SystemHealthFindings(open: [], closed: [closed])

            #expect(SettingsSummary.systemHealth(two) == "2 open · 1 blocking")
            #expect(SettingsSummary.systemHealth(one) == "1 open")
            #expect(SettingsSummary.systemHealth(none) == "Nothing open")
            // The Mac's source list trails the open count alone, or nothing.
            #expect(SettingsSummary.systemHealthCount(two) == "2")
            #expect(SettingsSummary.systemHealthCount(one) == "1")
            #expect(SettingsSummary.systemHealthCount(none) == nil)
        }

        @Test func theHostCarriesItsPortWhenTheAddressHasOne() {
            #expect(SettingsHost.line(URL(string: "http://127.0.0.1:7342")) == "127.0.0.1:7342")
            #expect(SettingsHost.line(URL(string: "https://studio.local")) == "studio.local")
            #expect(SettingsHost.line(nil) == nil)
        }
    }
#endif
