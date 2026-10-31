#if os(macOS)
    import Foundation
    import FreesideAPI
    import Testing

    @testable import FreesideCore

    /// What each Settings row states. The rows only report what the app
    /// already holds, so these are the sheet's whole behavior but Devices.
    @MainActor
    @Suite struct SettingsSheetTests {
        @Test func theDevicesRowCountsTheListOnceLoaded() async {
            let devices = await DevicesScreenshots.model()
            #expect(SettingsSheet.devicesSummary(devices, client: .mac) == "This Mac · 1 other · 1 revoked")
            #expect(SettingsSheet.devicesSummary(devices, client: .iPhone) == "This iPhone · 1 other · 1 revoked")
            #expect(SettingsSheet.devicesSummary(devices, client: .iPad) == "This iPad · 1 other · 1 revoked")
        }

        @Test func theDevicesRowSaysSoBeforeTheListLoads() {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            let devices = DevicesModel(coordinator: coordinator, signOut: {})
            #expect(SettingsSheet.devicesSummary(devices, client: .mac) == "Loading…")
        }

        @Test func notificationsNameTheIPhoneOnTheMac() {
            #expect(SettingsSheet.notificationsSummary(nil, client: .mac) == "On iPhone")
            #expect(SettingsSheet.notificationsSummary(.mock, client: .mac) == "On iPhone")
            #expect(SettingsSheet.notificationsSummary(nil, client: .iPhone) == "Not set up")
            #expect(SettingsSheet.notificationsSummary(.mock, client: .iPhone) == "ntfy · topic set")
            #expect(SettingsSheet.notificationsSummary(nil, client: .iPad) == "Not set up")
        }

        @Test func projectsNameTheFilterOnlyWhenOneIsChosen() {
            let known = ["freeside", "teloleo"]
            #expect(SettingsSheet.projectsSummary(known: known, filter: nil) == "2 known")
            #expect(SettingsSheet.projectsSummary(known: known, filter: "freeside") == "2 known · freeside selected")
            #expect(
                SettingsSheet.projectsSummary(known: known, filter: UnifiedList.noProject)
                    == "2 known · No project selected")
            #expect(SettingsSheet.projectsSummary(known: [], filter: nil) == "None known")
        }

        @Test func theDaemonRowReadsFreshness() {
            #expect(SettingsSheet.daemonSummary(.fresh) == "Connected")
            #expect(SettingsSheet.daemonSummary(.unvalidated) == "Checking")
            #expect(SettingsSheet.daemonSummary(.unreachable) == "Unreachable")
            #expect(SettingsSheet.daemonSummary(.syncFailing) == "Sync failing")
            #expect(SettingsSheet.daemonSummary(.contractMismatch(daemonContract: "sha256:a")) == "Mismatch")
            #expect(SettingsSheet.daemonSummary(.unauthenticated) == "Revoked")
        }
    }
#endif
