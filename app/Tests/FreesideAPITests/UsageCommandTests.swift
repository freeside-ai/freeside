import Foundation
import FreesideAPI
import Testing

@Suite struct UsageCommandTests {
    private func command(
        _ id: String, _ payload: Components.Schemas.ClientCommand.payloadPayload
    ) -> Components.Schemas.ClientCommand {
        .init(command_id: id, device_id: "device-1", payload: payload)
    }

    private func refresh(_ id: String, pool: String) -> Components.Schemas.ClientCommand {
        command(id, .refresh_usage(.init(kind: .refresh_usage, pool: pool)))
    }

    private func reassign(_ id: String, pool: String, enrollment: String) -> Components.Schemas.ClientCommand {
        command(id, .reassign_collector(.init(kind: .reassign_collector, pool: pool, enrollment_id: enrollment)))
    }

    private func line(
        _ id: String, task: String, selection: Components.Schemas.TaskLineSelection?
    ) -> Components.Schemas.ClientCommand {
        command(
            id,
            .change_task_line(
                .init(
                    kind: .change_task_line, task_id: task, role: .implementer,
                    selection: selection.map { .init(value1: $0) })))
    }

    private func policy(
        _ id: String, expected: Int?, enrollments: [String], drainFirst: String? = nil
    ) -> Components.Schemas.ClientCommand {
        command(
            id,
            .set_routing_policy(
                .init(
                    kind: .set_routing_policy, roles: [.init(role: .implementer, enrollments: enrollments)],
                    drain_first_account: drainFirst, expected_revision: expected)))
    }

    private func status(_ output: Operations.submitCommand.Output) -> Int {
        switch output {
        case .ok: 200
        case .badRequest: 400
        case .conflict: 409
        case .undocumented(let status, _): status
        }
    }

    /// #1266: the mock lists the features it serves on `/health`, omits the
    /// list when told to, as an older daemon does, and answers a withheld
    /// feature's command with the typed refusal, recording nothing.
    @Test func aWithheldFeaturesCommandIsRefusedAsAnUnservedKind() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        #expect(try await client.getHealth().ok.body.json.features == MockServer.servedFeatures)
        await server.setHealthFeatures(nil)
        #expect(try await client.getHealth().ok.body.json.features == nil)

        let before = try await client.getSyncRevision().ok.body.json.revision
        let move = Components.Schemas.ClientCommand(
            command_id: "cmd-move", device_id: "device-1",
            payload: .move_parked_task(
                .init(kind: .move_parked_task, task_id: "task-1", choice: .init(enrollment_id: "enrollment-1"))))
        guard case .badRequest(let refusal) = try await client.submitCommand(body: .json(move)) else {
            Issue.record("a withheld feature's command was not refused")
            return
        }
        let rejection = try refusal.body.json
        #expect(rejection.reason == .unknown_command_kind && !rejection.message.isEmpty)
        #expect(try await client.getSyncRevision().ok.body.json.revision == before)
    }

    @Test func usageCommandsRecordAReceiptAndReplay() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getSyncRevision().ok.body.json.revision
        let refreshCommand = refresh("refresh", pool: UsageFixtures.readingPoolID)
        let refreshed = try await client.submitCommand(body: .json(refreshCommand)).ok.body.json
        #expect(CommandResultTrust.accepts(refreshed, for: refreshCommand))
        #expect(refreshed.revision == before + 1)
        #expect(try await client.submitCommand(body: .json(refreshCommand)).ok.body.json == refreshed)
        #expect(try await client.getSyncRevision().ok.body.json.revision == before + 1)

        let reassignCommand = reassign(
            "reassign", pool: UsageFixtures.readingPoolID, enrollment: UsageFixtures.readingSecondEnrollmentID)
        let reassigned = try await client.submitCommand(body: .json(reassignCommand)).ok.body.json
        guard case .reassign_collector(let record) = reassigned.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.collector == UsageFixtures.readingSecondEnrollmentID)
        #expect(CommandResultTrust.accepts(reassigned, for: reassignCommand))
        #expect(!CommandResultTrust.accepts(reassigned, for: refreshCommand))

        // The same command_id with another body, or another kind, conflicts.
        let changed = refresh("refresh", pool: UsageFixtures.unsupportedPoolID)
        #expect(status(try await client.submitCommand(body: .json(changed))) == 400)
        let crossKind = line("refresh", task: "task-1", selection: UsageFixtures.agentSelection("codex-primary"))
        #expect(status(try await client.submitCommand(body: .json(crossKind))) == 400)
    }

    /// The mock commits the reassignment's record as the daemon's
    /// usagecollect.reassigned does, in the receipt's revision.
    @Test func reassignmentRecordsTheNewCollectorAndClearsTheOldReadings() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        func readingPool() async throws -> Components.Schemas.UsagePoolSnapshot {
            let bootstrap = try await client.getSyncBootstrap().ok.body.json
            return try #require(bootstrap.usage_pools.first { $0.pool.id == UsageFixtures.readingPoolID })
        }
        let before = try await readingPool()

        let moved = reassign(
            "move", pool: UsageFixtures.readingPoolID, enrollment: UsageFixtures.readingSecondEnrollmentID)
        let receipt = try await client.submitCommand(body: .json(moved)).ok.body.json
        let after = try await readingPool()
        #expect(after.as_of_revision == receipt.revision)
        #expect(after.entity_version == before.entity_version + 1)
        #expect(after.pool.collector == UsageFixtures.readingSecondEnrollmentID)
        #expect(after.pool.plan == nil)
        #expect(after.pool.last_attempt == nil)
        #expect(after.pool.model_windows.isEmpty)
        #expect(after.pool.windows.isEmpty)
        #expect(after.pool.enrollments == before.pool.enrollments)
        #expect(
            after.pool.recorded_exhausted_marks
                == before.pool.recorded_exhausted_marks.filter { $0.source == .provider_refusal })

        // Naming the current collector records the receipt and no record.
        let again = reassign(
            "again", pool: UsageFixtures.readingPoolID, enrollment: UsageFixtures.readingSecondEnrollmentID)
        let unchanged = try await client.submitCommand(body: .json(again)).ok.body.json
        #expect(unchanged.revision == receipt.revision + 1)
        #expect(try await readingPool() == after)
    }

    @Test func usageCommandsRefuseAnUnknownPoolAndAForeignEnrollment() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getSyncRevision().ok.body.json.revision
        #expect(status(try await client.submitCommand(body: .json(refresh("a", pool: "missing")))) == 404)
        let foreign = reassign(
            "b", pool: UsageFixtures.unsupportedPoolID, enrollment: UsageFixtures.readingSecondEnrollmentID)
        #expect(status(try await client.submitCommand(body: .json(foreign))) == 400)
        var enveloped = refresh("c", pool: UsageFixtures.readingPoolID)
        enveloped.expected_entity_version = 1
        #expect(status(try await client.submitCommand(body: .json(enveloped))) == 400)
        #expect(try await client.getSyncRevision().ok.body.json.revision == before)
    }

    @Test func taskLineChangeRecordsRepeatsClearsAndRefusesARunningRole() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let task = try #require(try await client.getSyncBootstrap().ok.body.json.tasks.first)
        let changeCommand = line("line-1", task: task.task.id, selection: UsageFixtures.agentSelection("codex-primary"))
        let changed = try await client.submitCommand(body: .json(changeCommand)).ok.body.json
        guard case .change_task_line(let record) = changed.record, let first = record.line?.value1 else {
            Issue.record("missing line receipt")
            return
        }
        #expect(record.recorded)
        #expect(record.agent == nil && record.selection?.value1 == UsageFixtures.agentSelection("codex-primary"))
        #expect(first.agent == nil && first.selection?.value1 == UsageFixtures.agentSelection("codex-primary"))
        #expect(first.version == 1 && first.source == .client_line_change)
        #expect(CommandResultTrust.accepts(changed, for: changeCommand))
        let after = try await client.getSyncBootstrap().ok.body.json
        let current = try #require(after.tasks.first { $0.task.id == task.task.id })
        #expect(current.task.task_lines == [first])
        #expect(current.entity_version == changed.revision)

        let repeated = try await client.submitCommand(
            body: .json(line("line-2", task: task.task.id, selection: UsageFixtures.agentSelection("codex-primary")))
        ).ok.body.json
        guard case .change_task_line(let repeatRecord) = repeated.record else {
            Issue.record("missing repeat receipt")
            return
        }
        #expect(!repeatRecord.recorded && repeatRecord.line?.value1 == first)

        let clearCommand = line("line-3", task: task.task.id, selection: nil)
        let cleared = try await client.submitCommand(body: .json(clearCommand)).ok.body.json
        guard case .change_task_line(let clear) = cleared.record else {
            Issue.record("missing clear receipt")
            return
        }
        #expect(clear.recorded && clear.line == nil && clear.agent == nil && clear.selection == nil)
        #expect(CommandResultTrust.accepts(cleared, for: clearCommand))
        #expect(!CommandResultTrust.accepts(cleared, for: changeCommand))
        let afterClear = try await client.getSyncBootstrap().ok.body.json
        #expect(afterClear.tasks.first { $0.task.id == task.task.id }?.task.task_lines == [])

        let missing = line("line-4", task: "task-missing", selection: UsageFixtures.agentSelection("codex-primary"))
        #expect(status(try await client.submitCommand(body: .json(missing))) == 404)

        var malformed = UsageFixtures.agentSelection("codex-primary")
        malformed.model_id = "gpt codex"
        let invalid = line("line-invalid", task: task.task.id, selection: malformed)
        #expect(status(try await client.submitCommand(body: .json(invalid))) == 400)

        await server.setTaskLineRoleRunning(taskID: task.task.id, role: .implementer, running: true)
        // The line the role already has answers before the running check.
        let held = try await client.submitCommand(
            body: .json(line("line-held", task: task.task.id, selection: nil))
        ).ok.body.json
        guard case .change_task_line(let heldRecord) = held.record else {
            Issue.record("missing held receipt")
            return
        }
        #expect(!heldRecord.recorded)
        let revision = try await client.getSyncRevision().ok.body.json.revision
        let refused = try await client.submitCommand(
            body: .json(line("line-5", task: task.task.id, selection: UsageFixtures.agentSelection("claude-primary")))
        ).conflict.body.json
        guard case .TaskLineChangeRejection(let rejection) = refused else {
            Issue.record("wrong conflict arm")
            return
        }
        #expect(rejection.reason == .role_running)
        #expect(rejection.replacement_task.task.id == task.task.id)
        #expect(rejection.sync_epoch == afterClear.sync_epoch)
        #expect(try await client.getSyncRevision().ok.body.json.revision == revision)
    }

    @Test func taskLineChangeContinuesASeededLine() async throws {
        var tasks = TaskFixtures.defaultTasks()
        let task = try #require(tasks.first)
        let seeded = Components.Schemas.TaskLine(
            id: "sha256:" + String(repeating: "b", count: 64), task_id: task.task.id, role: .reviewer,
            selection: .init(value1: UsageFixtures.agentSelection("claude-primary")), version: 2,
            predecessor_id: .init(value1: "sha256:" + String(repeating: "c", count: 64)),
            source: .submit_task, set_by: "command-seeded", set_at: Date(timeIntervalSince1970: 0))
        tasks[0].task.task_lines = [seeded]
        let client = APIClientFactory.mock(server: MockServer(tasks: tasks))

        let held = try await client.submitCommand(
            body: .json(
                command(
                    "held",
                    .change_task_line(
                        .init(
                            kind: .change_task_line, task_id: task.task.id, role: .reviewer,
                            selection: .init(value1: UsageFixtures.agentSelection("claude-primary"))))
                ))
        ).ok.body.json
        guard case .change_task_line(let heldRecord) = held.record else {
            Issue.record("missing receipt")
            return
        }
        #expect(!heldRecord.recorded && heldRecord.line?.value1 == seeded)

        let changed = try await client.submitCommand(
            body: .json(
                line("implementer", task: task.task.id, selection: UsageFixtures.agentSelection("codex-primary")))
        ).ok.body.json
        guard case .change_task_line(let record) = changed.record, let added = record.line?.value1 else {
            Issue.record("missing receipt")
            return
        }
        #expect(added.version == 1)
        let current = try await client.getSyncBootstrap().ok.body.json.tasks.first { $0.task.id == task.task.id }
        #expect(current?.task.task_lines == [added, seeded])

        let moved = try await client.submitCommand(
            body: .json(
                command(
                    "moved",
                    .change_task_line(
                        .init(
                            kind: .change_task_line, task_id: task.task.id, role: .reviewer,
                            selection: .init(value1: UsageFixtures.agentSelection("codex-primary")))))
            )
        ).ok.body.json
        guard case .change_task_line(let movedRecord) = moved.record, let next = movedRecord.line?.value1 else {
            Issue.record("missing receipt")
            return
        }
        #expect(next.version == 3 && next.predecessor_id?.value1 == seeded.id)
    }

    @Test func routingPolicyChangeHoldsTheExpectedRevision() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        let current = try #require(bootstrap.routing_policy?.value1.policy)

        let stale = try await client.submitCommand(
            body: .json(
                policy("stale", expected: current.revision - 1, enrollments: [UsageFixtures.readingCollectorID]))
        ).conflict.body.json
        guard case .RoutingPolicyRejection(let rejection) = stale else {
            Issue.record("wrong conflict arm")
            return
        }
        #expect(rejection.current_policy?.value1 == current)

        let unknown = policy("unknown", expected: current.revision, enrollments: ["enrollment-gemini"])
        #expect(status(try await client.submitCommand(body: .json(unknown))) == 400)
        let repeated = policy(
            "repeated", expected: current.revision,
            enrollments: [UsageFixtures.unsupportedCollectorID, UsageFixtures.unsupportedCollectorID])
        #expect(status(try await client.submitCommand(body: .json(repeated))) == 400)

        let same = policy(
            "same", expected: current.revision,
            enrollments: current.roles.first { $0.role == .implementer }?.enrollments ?? [])
        let held = try await client.submitCommand(body: .json(same)).ok.body.json
        guard case .set_routing_policy(let heldRecord) = held.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(!heldRecord.recorded && heldRecord.policy == current)
        #expect(CommandResultTrust.accepts(held, for: same))

        let setCommand = policy(
            "set", expected: current.revision, enrollments: [UsageFixtures.readingSecondEnrollmentID])
        let set = try await client.submitCommand(body: .json(setCommand)).ok.body.json
        guard case .set_routing_policy(let record) = set.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.recorded && record.policy.revision == current.revision + 1)
        #expect(record.policy.set_by == "device:device-1" && record.policy.source == .client_routing_set)
        #expect(CommandResultTrust.accepts(set, for: setCommand))
        #expect(try await client.submitCommand(body: .json(setCommand)).ok.body.json == set)
        let after = try await client.getSyncBootstrap().ok.body.json
        #expect(after.routing_policy?.value1.policy == record.policy)

        var forged = record
        forged.policy.revision += 1
        #expect(
            !CommandResultTrust.accepts(
                .init(record: .set_routing_policy(forged), revision: set.revision), for: setCommand))
    }

    @Test func routingPolicyChangeChecksEnrollmentsAgainstTheCatalog() async throws {
        let server = MockServer(routingPolicy: nil)
        let client = APIClientFactory.mock(server: server)
        var changed = UsageFixtures.defaultAgentCatalog()
        changed.lineup_revision = .init(value1: "sha256:" + String(repeating: "e", count: 64))
        changed.enrollments.removeAll { $0.enrollment_id == UsageFixtures.unsupportedCollectorID }
        await server.setAgentCatalog(changed)

        // An enrollment the default fixtures carry but the served catalog does not.
        let dropped = policy("dropped", expected: nil, enrollments: [UsageFixtures.unsupportedCollectorID])
        #expect(status(try await client.submitCommand(body: .json(dropped))) == 400)
        // The dropped enrollment's pool is no ordered enrollment's pool.
        let droppedPool = policy(
            "dropped-pool", expected: nil, enrollments: [UsageFixtures.readingSecondEnrollmentID],
            drainFirst: UsageFixtures.unsupportedPoolID)
        #expect(status(try await client.submitCommand(body: .json(droppedPool))) == 400)

        let kept = policy("kept", expected: nil, enrollments: [UsageFixtures.readingSecondEnrollmentID])
        let set = try await client.submitCommand(body: .json(kept)).ok.body.json
        guard case .set_routing_policy(let record) = set.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.policy.lineup_revision == changed.lineup_revision?.value1)
    }

    @Test func routingPolicyChangeRecordsADrainFirstAccount() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let current = try #require(try await client.getSyncBootstrap().ok.body.json.routing_policy?.value1.policy)
        let enrollments = current.roles.first { $0.role == .implementer }?.enrollments ?? []

        // codex-b is the codex enrollment's pool; spare holds no ordered enrollment.
        for (id, account) in [
            ("unknown-pool", "nope"), ("agentless-pool", UsageFixtures.unrecordedPoolID), ("empty", ""),
        ] {
            let refused = policy(id, expected: current.revision, enrollments: enrollments, drainFirst: account)
            #expect(status(try await client.submitCommand(body: .json(refused))) == 400)
        }

        let nameCommand = policy(
            "name", expected: current.revision, enrollments: enrollments, drainFirst: UsageFixtures.unsupportedPoolID)
        let named = try await client.submitCommand(body: .json(nameCommand)).ok.body.json
        guard case .set_routing_policy(let record) = named.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.recorded && record.policy.revision == current.revision + 1)
        #expect(record.policy.drain_first_account == UsageFixtures.unsupportedPoolID)
        #expect(CommandResultTrust.accepts(named, for: nameCommand))
        var forged = record
        forged.policy.drain_first_account = nil
        #expect(
            !CommandResultTrust.accepts(
                .init(record: .set_routing_policy(forged), revision: named.revision), for: nameCommand))

        let repeatCommand = policy(
            "repeat", expected: record.policy.revision, enrollments: enrollments,
            drainFirst: UsageFixtures.unsupportedPoolID)
        let repeated = try await client.submitCommand(body: .json(repeatCommand)).ok.body.json
        guard case .set_routing_policy(let repeatRecord) = repeated.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(!repeatRecord.recorded && repeatRecord.policy == record.policy)

        // The same orders with no account clear it, as a new revision.
        let clearCommand = policy("clear", expected: record.policy.revision, enrollments: enrollments)
        let cleared = try await client.submitCommand(body: .json(clearCommand)).ok.body.json
        guard case .set_routing_policy(let clearRecord) = cleared.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(clearRecord.recorded && clearRecord.policy.revision == record.policy.revision + 1)
        #expect(clearRecord.policy.drain_first_account == nil)
        #expect(CommandResultTrust.accepts(cleared, for: clearCommand))
    }

    @Test func routingPolicyChangeFailsClosedWithoutATree() async throws {
        let client = APIClientFactory.mock(
            server: MockServer(routingPolicy: nil, agentCatalog: UsageFixtures.treelessAgentCatalog()))
        let first = policy("first", expected: nil, enrollments: [UsageFixtures.readingCollectorID])
        #expect(status(try await client.submitCommand(body: .json(first))) == 500)
        #expect(try await client.getSyncBootstrap().ok.body.json.routing_policy == nil)
    }

    @Test func routingPolicyChangeFromNoPolicy() async throws {
        let client = APIClientFactory.mock(server: MockServer(routingPolicy: nil))
        let first = policy("first", expected: nil, enrollments: [UsageFixtures.unsupportedCollectorID])
        let set = try await client.submitCommand(body: .json(first)).ok.body.json
        guard case .set_routing_policy(let record) = set.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.recorded && record.policy.revision == 1 && record.expected_revision == nil)
        #expect(CommandResultTrust.accepts(set, for: first))
        let again = policy("again", expected: nil, enrollments: [UsageFixtures.readingCollectorID])
        let conflict = try await client.submitCommand(body: .json(again)).conflict.body.json
        guard case .RoutingPolicyRejection(let rejection) = conflict else {
            Issue.record("wrong conflict arm")
            return
        }
        #expect(rejection.current_policy?.value1 == record.policy)
    }
}
