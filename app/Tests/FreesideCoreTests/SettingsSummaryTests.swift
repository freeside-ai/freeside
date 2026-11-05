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

        @Test func notificationsNameTheIPhoneOnTheMac() {
            #expect(SettingsSummary.notifications(nil, client: .mac) == "On iPhone")
            #expect(SettingsSummary.notifications(.mock, client: .mac) == "On iPhone")
            #expect(SettingsSummary.notifications(nil, client: .iPhone) == "Not set up")
            #expect(SettingsSummary.notifications(.mock, client: .iPhone) == "ntfy · topic set")
            #expect(SettingsSummary.notifications(nil, client: .iPad) == "Not set up")
        }

        @Test func theDaemonRowReadsFreshness() {
            #expect(SettingsSummary.daemon(.fresh) == "Connected")
            #expect(SettingsSummary.daemon(.unvalidated) == "Checking")
            #expect(SettingsSummary.daemon(.unreachable) == "Unreachable")
            #expect(SettingsSummary.daemon(.syncFailing) == "Sync failing")
            #expect(SettingsSummary.daemon(.contractMismatch(daemonContract: "sha256:a")) == "Mismatch")
            #expect(SettingsSummary.daemon(.unauthenticated) == "Revoked")
        }

        @Test func theHostCarriesItsPortWhenTheAddressHasOne() {
            #expect(SettingsHost.line(URL(string: "http://127.0.0.1:7342")) == "127.0.0.1:7342")
            #expect(SettingsHost.line(URL(string: "https://studio.local")) == "studio.local")
            #expect(SettingsHost.line(nil) == nil)
        }
    }
#endif
