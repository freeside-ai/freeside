#if os(macOS)
    import FreesideAPI

    @testable import FreesideCore

    @MainActor enum DevicesScreenshots {
        /// The Devices model loaded from the permissive mock's seed: this
        /// device, another active one, and a revoked one never seen.
        /// The phone surfaces pass the subscription the iPhone app shows on
        /// this device's card; the Mac app passes none.
        static func model(notificationSubscription: DeviceNtfySubscription? = nil) async -> DevicesModel {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            let model = DevicesModel(
                coordinator: coordinator, notificationSubscription: notificationSubscription, signOut: {})
            await model.load()
            return model
        }
    }
#endif
