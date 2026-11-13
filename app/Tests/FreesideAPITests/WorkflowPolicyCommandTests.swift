import Foundation
import FreesideAPI
import Testing

/// #3145: the mock daemon's set_workflow_policy, its bootstrap projection,
/// and the trust gate on its receipt.
@Suite struct WorkflowPolicyCommandTests {
    private func change(
        _ id: String, scope: Components.Schemas.WorkflowPolicyScope = .general, project: String? = nil,
        key: Components.Schemas.WorkflowPolicyKey = .gates_period_issue_comment, value: String?, expected: Int?
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: id, device_id: "device-1",
            payload: .set_workflow_policy(
                .init(
                    kind: .set_workflow_policy, scope: scope, project_id: project, key: key, value: value,
                    expected_revision: expected)))
    }

    private func status(_ output: Operations.submitCommand.Output) -> Int {
        switch output {
        case .ok: 200
        case .badRequest: 400
        case .conflict: 409
        case .undocumented(let status, _): status
        }
    }

    private func setting(
        _ snapshot: Components.Schemas.WorkflowPolicySnapshot, project: String?,
        _ key: Components.Schemas.WorkflowPolicyKey
    ) -> Components.Schemas.WorkflowPolicySetting? {
        let settings =
            project.map { id in snapshot.projects.first { $0.project_id == id }?.settings ?? [] } ?? snapshot.general
        return settings.first { $0.key == key }
    }

    @Test func theBootstrapResolvesProjectOverGeneralOverDefault() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let policy = try await client.getSyncBootstrap().ok.body.json.workflow_policy
        let project = WorkflowPolicyFixtures.projectID
        #expect(policy.revision == 2)
        #expect(policy.projects.map(\.project_id) == [project])
        #expect(policy.general.map(\.key) == WorkflowPolicyFixtures.paneKeys)
        let closure = try #require(setting(policy, project: project, .gates_period_source_issue_closure))
        #expect(closure.value == "true" && closure.origin == .general)
        let perDay = try #require(setting(policy, project: project, .follow_up_filing_period_max_per_day))
        #expect(perDay.value == "3" && perDay.origin == .project)
        #expect(setting(policy, project: nil, .follow_up_filing_period_max_per_day)?.value == "10")
        let depth = try #require(setting(policy, project: project, .follow_up_filing_period_max_depth))
        #expect(depth.value == "2" && depth.origin == ._default && depth.set_by == nil && depth.set_at == nil)
    }

    @Test func aChangeIsRecordedOnceAndReplayed() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let command = change("set", value: "true", expected: 2)
        let result = try await client.submitCommand(body: .json(command)).ok.body.json
        guard case .set_workflow_policy(let record) = result.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(record.recorded && record.revision == 3)
        #expect(CommandResultTrust.accepts(result, for: command))
        #expect(try await client.submitCommand(body: .json(command)).ok.body.json == result)
        let policy = try await client.getSyncBootstrap().ok.body.json.workflow_policy
        let comment = try #require(setting(policy, project: nil, .gates_period_issue_comment))
        #expect(comment.value == "true" && comment.origin == .general && comment.set_by == "device:device-1")

        // The value the layer already holds records nothing.
        let same = change("same", value: "true", expected: 3)
        let held = try await client.submitCommand(body: .json(same)).ok.body.json
        guard case .set_workflow_policy(let heldRecord) = held.record else {
            Issue.record("wrong receipt arm")
            return
        }
        #expect(!heldRecord.recorded && heldRecord.revision == 3)
        #expect(CommandResultTrust.accepts(held, for: same))

        var forged = record
        forged.revision += 1
        #expect(
            !CommandResultTrust.accepts(
                .init(record: .set_workflow_policy(forged), revision: result.revision), for: command))
        forged = record
        forged.value = "false"
        #expect(
            !CommandResultTrust.accepts(
                .init(record: .set_workflow_policy(forged), revision: result.revision), for: command))
    }

    @Test func clearingAProjectLayerFallsBackToGeneral() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let project = WorkflowPolicyFixtures.projectID
        let clear = change(
            "clear", scope: .project, project: project, key: .follow_up_filing_period_max_per_day, value: nil,
            expected: 2)
        #expect(status(try await client.submitCommand(body: .json(clear))) == 200)
        let policy = try await client.getSyncBootstrap().ok.body.json.workflow_policy
        let perDay = try #require(setting(policy, project: project, .follow_up_filing_period_max_per_day))
        #expect(perDay.value == "10" && perDay.origin == ._default)
    }

    @Test func aStaleRevisionIsRefusedWithTheCurrentLayers() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getSyncRevision().ok.body.json.revision
        let stale = try await client.submitCommand(body: .json(change("stale", value: "true", expected: 1)))
            .conflict.body.json
        guard case .WorkflowPolicyRejection(let rejection) = stale else {
            Issue.record("wrong conflict arm: \(stale)")
            return
        }
        #expect(rejection.current_workflow_policy.revision == 2)
        #expect(try await client.getSyncRevision().ok.body.json.revision == before)
    }

    /// An unknown scope or key never leaves the client, which refuses to
    /// encode one; the daemon's own tests cover its refusal.
    @Test(arguments: [
        (
            "general-with-project", Components.Schemas.WorkflowPolicyScope.general,
            WorkflowPolicyFixtures.projectID as String?, "true"
        ),
        ("project-without-id", .project, nil, "true"),
        ("unserved", .project, "other-project", "true"),
        ("bad-gate", .general, nil, "yes"),
    ])
    func anInvalidChangeIsRefusedAndRecordsNothing(
        id: String, scope: Components.Schemas.WorkflowPolicyScope, project: String?, value: String
    ) async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getSyncRevision().ok.body.json.revision
        let command = change(id, scope: scope, project: project, value: value, expected: 2)
        #expect(status(try await client.submitCommand(body: .json(command))) == 400)
        #expect(try await client.getSyncRevision().ok.body.json.revision == before)
    }

    @Test(arguments: ["0", "02", "+3", "-1", "three", ""])
    func aCapTakesOnlyACanonicalPositiveDecimal(value: String) {
        #expect(!Components.Schemas.WorkflowPolicyKey.follow_up_filing_period_max_per_run.accepts(value))
    }

    /// The 409 body's arms carry no discriminator: a client takes the first
    /// that decodes, and RoutingPolicyRejection's nullable current_policy
    /// decodes as null when absent, so a workflow refusal must come first.
    @Test func aWorkflowRefusalDecodesAsItsOwnArm() throws {
        let rejection = Components.Schemas.WorkflowPolicyRejection(
            message: "the workflow policy changed",
            current_workflow_policy: WorkflowPolicyFixtures.defaultLayers().snapshot(
                projects: [WorkflowPolicyFixtures.projectID], asOfRevision: 9))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let decoded = try decoder.decode(
            Components.Schemas.CommandRejection.self, from: encoder.encode(rejection))
        #expect(decoded == .WorkflowPolicyRejection(rejection))
    }
}
