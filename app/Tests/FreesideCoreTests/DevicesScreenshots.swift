#if os(macOS)
    import FreesideAPI

    @testable import FreesideCore

    @MainActor enum DevicesScreenshots {
        /// The Devices model loaded from the permissive mock's seed: this
        /// device, another active one, and a revoked one never seen.
        static func model() async -> DevicesModel {
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
            let model = DevicesModel(coordinator: coordinator, signOut: {})
            await model.load()
            return model
        }
    }
#endif
