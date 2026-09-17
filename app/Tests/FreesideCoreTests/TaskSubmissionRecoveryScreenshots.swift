#if os(macOS)
    import FreesideAPI
    import Testing

    @testable import FreesideCore

    @MainActor enum TaskSubmissionRecoveryScreenshots {
        static func model(client: any APIProtocol) throws -> TaskSubmissionModel {
            let coordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
            for id in ["saved-first", "saved-second"] {
                let command = Components.Schemas.ClientCommand(
                    command_id: id, device_id: "device-mock",
                    payload: .submit_task(
                        .init(
                            kind: .submit_task, project_id: "example-project",
                            source: "Add a health endpoint and a focused test.", name: "Health endpoint")))
                try #require(coordinator.retainTaskSubmission(command))
            }
            return TaskSubmissionModel(coordinator: coordinator)
        }
    }
#endif
