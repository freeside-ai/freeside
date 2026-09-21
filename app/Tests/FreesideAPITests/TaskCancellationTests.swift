import Foundation
import FreesideAPI
import Testing

@Suite struct TaskCancellationTests {
    private func stop(
        _ snapshot: Components.Schemas.TaskSnapshot, epoch: String, id: String = "stop"
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: id, device_id: "device-1", expected_entity_version: snapshot.entity_version,
            payload: .stop_task(
                .init(
                    kind: .stop_task, task_id: snapshot.task.id,
                    project_id: snapshot.task.project_id, expected_sync_epoch: epoch)))
    }

    @Test func acceptanceReplayConflictAndResultTrust() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getSyncBootstrap().ok.body.json
        let task = try #require(before.tasks.first)
        #expect(task.task.cancellation == nil)
        let command = stop(task, epoch: before.sync_epoch)
        let result = try await client.submitCommand(body: .json(command)).ok.body.json
        #expect(CommandResultTrust.accepts(result, for: command))
        guard case .stop_task(let receipt) = result.record else {
            Issue.record("missing Stop receipt")
            return
        }
        #expect(receipt.cancellation.state == .requested)
        #expect(receipt.cancellation.acknowledgement == nil)
        #expect(receipt.cancellation.target.runs.map(\.run_id) == task.task.run_ids)
        let after = try await client.getSyncBootstrap().ok.body.json
        let current = try #require(after.tasks.first { $0.task.id == task.task.id })
        #expect(current.task.wip == task.task.wip)
        #expect(current.task.lifecycle == task.task.lifecycle)
        #expect(current.task.lifecycle_facts == task.task.lifecycle_facts)
        #expect(current.task.cancellation?.value1 == receipt.cancellation)
        #expect(current.entity_version == result.revision)
        #expect(try await client.submitCommand(body: .json(command)).ok.body.json == result)
        #expect(try await client.getSyncRevision().ok.body.json.revision == after.revision)
        // Only a version above the current revision is rejected now; the older
        // task.entity_version, behind after the first Stop, would be accepted.
        var tooNew = stop(current, epoch: after.sync_epoch, id: "too-new")
        tooNew.expected_entity_version = after.revision + 1
        let rejected = try await client.submitCommand(body: .json(tooNew)).conflict.body.json
        guard case .StaleTaskRejection(let conflict) = rejected else {
            Issue.record("wrong conflict arm")
            return
        }
        #expect(conflict.replacement_task == current)
        #expect(conflict.sync_epoch == before.sync_epoch)
        let distinct = stop(current, epoch: after.sync_epoch, id: "another")
        let another = try await client.submitCommand(body: .json(distinct)).ok.body.json
        guard case .stop_task(let duplicate) = another.record else {
            Issue.record("wrong receipt")
            return
        }
        #expect(duplicate.cancellation == receipt.cancellation)
        for change in 0..<7 {
            var changed = receipt
            switch change {
            case 0: changed.device_id = "other"
            case 1: changed.task_id = "other"
            case 2: changed.expected_entity_version += 1
            case 3: changed.expected_sync_epoch = "other"
            case 4: changed.cancellation.target.runs = []
            case 5: changed.cancellation.state = .confirmed
            default: changed.cancellation.fence_revision = result.revision + 1
            }
            #expect(
                !CommandResultTrust.accepts(.init(record: .stop_task(changed), revision: result.revision), for: command)
            )
        }
        // The receipt's version is valid strictly below the accepting revision:
        // equal or greater is rejected, but any distance below is accepted (a live
        // task's revision advances between the client's read and the Stop).
        #expect(
            CommandResultTrust.accepts(
                .init(record: result.record, revision: receipt.expected_entity_version + 50), for: command))
        #expect(
            !CommandResultTrust.accepts(
                .init(record: result.record, revision: receipt.expected_entity_version), for: command))
        #expect(
            !CommandResultTrust.accepts(
                .init(record: result.record, revision: receipt.expected_entity_version - 1), for: command))
        // An inflated revision is no longer rejected here: the loosened rule bounds
        // the revision only from below, so this now passes the gate. The
        // stuck-pending risk it reopens is analysed in the decision note; a
        // pending entry settles on the next epoch change regardless.
        #expect(
            CommandResultTrust.accepts(.init(record: result.record, revision: result.revision + 100), for: command))
        var changed = command
        changed.expected_entity_version = after.revision
        let collision = try await client.submitCommand(body: .json(changed))
        if case .ok = collision { Issue.record("accepted changed replay") }
        let submission = Components.Schemas.ClientCommand(
            command_id: command.command_id, device_id: command.device_id,
            payload: .submit_task(.init(kind: .submit_task, project_id: task.task.project_id, source: "source")))
        if case .ok = try await client.submitCommand(body: .json(submission)) {
            Issue.record("accepted cross-kind replay")
        }
    }

    @Test func revokedDeviceCannotReplayStop() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["123456": .valid])
        let anonymous = APIClientFactory.mock(server: server)
        let grant = try await anonymous.pairDevice(
            body: .json(.init(pairing_code: "123456", display_name: "Stop device"))
        ).created.body.json
        guard case .active(let device) = grant.device.device else {
            Issue.record("pairing failed")
            return
        }
        let client = APIClientFactory.mock(server: server, token: { grant.device_token })
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        var command = stop(try #require(bootstrap.tasks.first), epoch: bootstrap.sync_epoch)
        command.device_id = device.id
        _ = try await client.submitCommand(body: .json(command)).ok.body.json
        _ = try await client.revokeDevice(path: .init(device_id: device.id))
        if case .ok = try await client.submitCommand(body: .json(command)) { Issue.record("revoked replay accepted") }
    }

    @Test func confirmedFixtureConvergesWithoutRestartingCancellation() async throws {
        let initial = APIClientFactory.mock(server: MockServer())
        let before = try await initial.getSyncBootstrap().ok.body.json
        let task = try #require(before.tasks.first)
        let result = try await initial.submitCommand(body: .json(stop(task, epoch: before.sync_epoch))).ok.body.json
        guard case .stop_task(let receipt) = result.record else {
            Issue.record("wrong receipt")
            return
        }
        var seeded = task
        var cancellation = receipt.cancellation
        cancellation.state = .confirmed
        cancellation.acknowledgement = .init(
            value1: .init(
                id: "explicit-test-evidence", request_id: cancellation.request_id,
                target_digest: cancellation.target_digest, state: .confirmed,
                evidence_digest: cancellation.target_digest,
                recorded_at: cancellation.requested_at))
        seeded.task.cancellation = .init(value1: cancellation)
        seeded.as_of_revision = result.revision
        let client = APIClientFactory.mock(server: MockServer(tasks: [seeded]))
        let snapshot = try await client.getSyncBootstrap().ok.body.json
        let command = stop(try #require(snapshot.tasks.first), epoch: snapshot.sync_epoch, id: "confirmed-target")
        let repeated = try await client.submitCommand(body: .json(command)).ok.body.json
        guard case .stop_task(let repeatReceipt) = repeated.record else {
            Issue.record("wrong receipt")
            return
        }
        #expect(repeatReceipt.cancellation == cancellation)
        #expect(CommandResultTrust.accepts(repeated, for: command))
    }
}
