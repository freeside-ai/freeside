#if os(macOS)
    import Foundation
    import FreesideAPI

    @testable import FreesideCore

    /// The Agents pane's states for the screenshot suite, each reached
    /// through the mock daemon the way the app reaches it.
    @MainActor enum RoutingPolicyScreenshots {
        enum State: CaseIterable {
            /// The canvas's policy, set from the other paired device: the
            /// three writer roles ordered alike, a reviewer, and no
            /// drain-first account.
            case policy
            /// The mock's default policy, which orders only the implementer,
            /// so Writer reads `Differs by role`.
            case differs
            /// The default orders with a drain-first account.
            case drainFirst
            /// No policy and no agent tree: every role reads "Lineup", and
            /// the view says why it can't change the policy.
            case noTree
            /// An edit in progress, with an agent added to two roles.
            case editing
            /// The same edit, also picking a drain-first account.
            case editingDrainFirst
            /// A change the daemon never answered, waiting for Retry.
            case unconfirmed
            /// A change refused because another device changed the policy.
            case failed

            var surface: String {
                switch self {
                case .policy: "routing"
                case .differs: "routing-differs"
                case .drainFirst: "routing-drain-first"
                case .noTree: "routing-no-tree"
                case .editing: "routing-editing"
                case .editingDrainFirst: "routing-editing-drain-first"
                case .unconfirmed: "routing-unconfirmed"
                case .failed: "routing-failed"
                }
            }

            /// An edit in progress, which the app draws in the `Change
            /// order…` sheet; the suite draws no sheet, so it renders the
            /// editor itself.
            var isEditing: Bool {
                switch self {
                case .editing, .editingDrainFirst: true
                case .policy, .differs, .drainFirst, .noTree, .unconfirmed, .failed: false
                }
            }
        }

        /// The policy the canvas frame draws: the writer roles share one
        /// order and the reviewer has its own.
        static func canvasPolicy() -> Components.Schemas.RoutingPolicy {
            var policy = UsageFixtures.defaultRoutingPolicy()
            let writers = ["claude-primary", "codex-primary"]
            policy.roles = [
                .init(role: .specifier, agents: writers), .init(role: .implementer, agents: writers),
                .init(role: .remediator, agents: writers), .init(role: .reviewer, agents: ["codex-primary"]),
            ]
            return policy
        }

        static func model(_ state: State) async -> RoutingPolicyModel {
            let server =
                switch state {
                case .policy: MockServer(routingPolicy: canvasPolicy())
                case .noTree: MockServer(routingPolicy: nil, agentCatalog: nil)
                case .drainFirst: MockServer(routingPolicy: drainFirstPolicy())
                default: MockServer()
                }
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.refresh()
            let model = RoutingPolicyModel(coordinator: coordinator)
            switch state {
            case .policy, .differs, .noTree, .drainFirst:
                break
            case .editing:
                edit(model)
            case .editingDrainFirst:
                edit(model)
                model.draft?.drainFirstAccount = UsageFixtures.unsupportedPoolID
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

        private static func drainFirstPolicy() -> Components.Schemas.RoutingPolicy {
            var policy = UsageFixtures.defaultRoutingPolicy()
            policy.drain_first_account = UsageFixtures.readingPoolID
            return policy
        }

        private static func edit(_ model: RoutingPolicyModel) {
            model.beginEditing()
            model.draft?.add("claude-secondary", to: .implementer)
            model.draft?.move("claude-secondary", in: .implementer, by: -1)
            model.draft?.add("claude-primary", to: .reviewer)
        }
    }
#endif
