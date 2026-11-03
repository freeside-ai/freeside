#if os(macOS)
    import Foundation
    import FreesideAPI

    @testable import FreesideCore

    /// The routing policy view's states for the screenshot suite, each
    /// reached through the mock daemon the way the app reaches it.
    @MainActor enum RoutingPolicyScreenshots {
        enum State: CaseIterable {
            /// The default policy, set from the other paired device.
            case policy
            /// No policy and no agent tree: every role reads "Lineup", and
            /// the view says why it can't change the policy.
            case noTree
            /// An edit in progress, with an agent added to two roles.
            case editing
            /// A change the daemon never answered, waiting for Retry.
            case unconfirmed
            /// A change refused because another device changed the policy.
            case failed

            var surface: String {
                switch self {
                case .policy: "routing"
                case .noTree: "routing-no-tree"
                case .editing: "routing-editing"
                case .unconfirmed: "routing-unconfirmed"
                case .failed: "routing-failed"
                }
            }
        }

        static func model(_ state: State) async -> RoutingPolicyModel {
            let server =
                state == .noTree ? MockServer(routingPolicy: nil, agentCatalog: nil) : MockServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.refresh()
            let model = RoutingPolicyModel(coordinator: coordinator)
            switch state {
            case .policy, .noTree:
                break
            case .editing:
                edit(model)
            case .unconfirmed:
                await server.setBeforeRespond { operation in
                    if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
                }
                edit(model)
                await model.save()
            case .failed:
                edit(model)
                let other = Components.Schemas.ClientCommand(
                    command_id: "cmd-routing-other", device_id: DeviceFixtures.otherDeviceID,
                    payload: .set_routing_policy(
                        .init(
                            kind: .set_routing_policy,
                            roles: [.init(role: .implementer, agents: ["codex-primary"])],
                            expected_revision: 2)))
                _ = try? await coordinator.store.client.submitCommand(body: .json(other))
                await model.save()
            }
            return model
        }

        private static func edit(_ model: RoutingPolicyModel) {
            model.beginEditing()
            model.draft?.add("claude-secondary", to: .implementer)
            model.draft?.move("claude-secondary", in: .implementer, by: -1)
            model.draft?.add("claude-primary", to: .reviewer)
        }
    }
#endif
