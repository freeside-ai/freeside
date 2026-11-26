#if os(macOS)
    import Foundation
    import FreesideAPI

    @testable import FreesideCore

    @MainActor enum DevicesScreenshots {
        /// The Devices model loaded from the permissive mock's seed: this
        /// device, another active one, and a revoked one never seen.
        /// The phone surfaces pass the subscription the iPhone app shows on
        /// this device's card; the Mac app passes none. `openedAt` is when
        /// this device last opened a notification, or nil for never.
        /// `notificationsOn` turns this device's switch on, which the seed
        /// leaves off.
        static func model(
            notificationSubscription: DeviceNtfySubscription? = nil, openedAt: Date? = nil,
            notificationsOn: Bool = false,
            testNotificationAnswer: MockServer.TestNotificationAnswer? = nil
        ) async -> DevicesModel {
            let server = MockServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.refresh()
            let opens = NotificationOpenLog(defaults: nil)
            if let openedAt {
                opens.record(deviceID: coordinator.store.device.deviceID, at: openedAt)
            }
            let model = DevicesModel(
                coordinator: coordinator, notificationSubscription: notificationSubscription,
                notificationOpens: opens, signOut: {})
            await model.load()
            if notificationsOn {
                await model.setNotifications(coordinator.store.device.deviceID, enabled: true)
            }
            if let testNotificationAnswer {
                await server.setTestNotificationAnswer(testNotificationAnswer)
                await model.sendTestNotification()
            }
            return model
        }
    }
#endif
