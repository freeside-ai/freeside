#if os(macOS)
    import FreesideAPI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    @MainActor enum TaskSubmissionRecoveryScreenshots {
        static func model(client: any APIProtocol) throws -> TaskSubmissionModel {
            let coordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
            // The second request chose agents, so its item shows its lines.
            for (id, lines) in [
                ("saved-first", nil),
                ("saved-second", AgentChoiceDisplay.taskLines(writer: "claude-secondary", reviewer: "codex-primary")),
            ] {
                let command = Components.Schemas.ClientCommand(
                    command_id: id, device_id: "device-mock",
                    payload: .submit_task(
                        .init(
                            kind: .submit_task, project_id: "example-project",
                            source: "Add a health endpoint and a focused test.", name: "Health endpoint",
                            task_lines: lines)))
                try #require(coordinator.retainTaskSubmission(command))
            }
            return TaskSubmissionModel(coordinator: coordinator)
        }
    }
#endif
