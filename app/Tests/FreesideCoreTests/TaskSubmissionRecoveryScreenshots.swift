#if os(macOS)
    import FreesideAPI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    @MainActor enum TaskSubmissionRecoveryScreenshots {
        static func model(client: any APIProtocol) throws -> TaskSubmissionModel {
            let coordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
            // The first request attached files, so its item lists their
            // names; the second chose agents, so its item shows its lines.
            let attachments: [Components.Schemas.TaskAttachmentInput] = [
                .init(
                    digest: "sha256:" + String(repeating: "a", count: 64), name: "Layout bug.png",
                    media_type: "image/png"),
                .init(
                    digest: "sha256:" + String(repeating: "b", count: 64), name: "crash.log", media_type: "text/plain"),
            ]
            let requests: [(String, [Components.Schemas.TaskLineInput]?, [Components.Schemas.TaskAttachmentInput]?)] = [
                ("saved-first", nil, attachments),
                (
                    "saved-second",
                    AgentChoiceDisplay.taskLines(choices: [
                        .specifier: "claude-secondary", .implementer: "claude-secondary",
                        .remediator: "claude-secondary", .reviewer: "codex-primary",
                    ]),
                    nil
                ),
            ]
            for (id, lines, attached) in requests {
                let command = Components.Schemas.ClientCommand(
                    command_id: id, device_id: "device-mock",
                    payload: .submit_task(
                        .init(
                            kind: .submit_task, project_id: "example-project",
                            source: "Add a health endpoint and a focused test.", name: "Health endpoint",
                            task_lines: lines, attachments: attached)))
                try #require(coordinator.retainTaskSubmission(command))
            }
            return TaskSubmissionModel(coordinator: coordinator)
        }
    }
#endif
