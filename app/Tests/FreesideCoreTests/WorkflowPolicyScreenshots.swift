#if os(macOS)
    import Foundation
    import FreesideAPI

    @testable import FreesideCore

    /// The Workflow pane's states for the screenshot suite, each reached
    /// through the mock daemon the way the app reaches it.
    @MainActor enum WorkflowPolicyScreenshots {
        enum State: CaseIterable {
            /// The mock's layers: the closure gate on for every project from
            /// the other paired device, and a lower daily cap for the served
            /// project, so the rows show each origin and one reset.
            case layers
            /// A change the daemon never answered, waiting for Retry.
            case unconfirmed
            /// A change refused because another device changed the policy.
            case failed

            var surface: String {
                switch self {
                case .layers: "workflow"
                case .unconfirmed: "workflow-unconfirmed"
                case .failed: "workflow-failed"
                }
            }
        }

        static func model(_ state: State) async -> WorkflowPolicyModel {
            let server = MockServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.refresh()
            let model = WorkflowPolicyModel(coordinator: coordinator)
            switch state {
            case .layers:
                break
            case .unconfirmed:
                await server.setBeforeRespond { operation in
                    if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
                }
                await model.set(.gates_period_issue_comment, to: "true", in: .general)
            case .failed:
                let other = Components.Schemas.ClientCommand(
                    command_id: "cmd-workflow-other", device_id: DeviceFixtures.otherDeviceID,
                    payload: .set_workflow_policy(
                        .init(
                            kind: .set_workflow_policy, scope: .general, project_id: nil,
                            key: .follow_up_filing_period_max_per_run, value: "3", expected_revision: 2)))
                _ = try? await coordinator.store.client.submitCommand(body: .json(other))
                await model.set(.gates_period_issue_comment, to: "true", in: .general)
            }
            return model
        }
    }
#endif
