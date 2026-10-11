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
        static func model(
            notificationSubscription: DeviceNtfySubscription? = nil, openedAt: Date? = nil
        ) async -> DevicesModel {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            let opens = NotificationOpenLog(defaults: nil)
            if let openedAt {
                opens.record(deviceID: coordinator.store.device.deviceID, at: openedAt)
            }
            let model = DevicesModel(
                coordinator: coordinator, notificationSubscription: notificationSubscription,
                notificationOpens: opens, signOut: {})
            await model.load()
            return model
        }
    }
#endif
