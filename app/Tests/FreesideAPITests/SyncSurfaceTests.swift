import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// The mock's sync envelope (plan §5.14): bootstrap is the one canonical
/// full-cache read, the heartbeat is the loss detector, and an epoch
/// rotation simulates a daemon restore.
@Suite struct SyncSurfaceTests {
    @Test func bootstrapCarriesTheCursorAndTheWholeInbox() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        let heartbeat = try await client.getSyncRevision().ok.body.json
        let listed = try await client.listAttentionItems().ok.body.json
        let runs = try await client.listRuns().ok.body.json
        let schedules = try await client.listSchedules().ok.body.json

        // The full-cache cursor pair matches the heartbeat's ServerState
        // read, and the item collection is the same canonical list the
        // list endpoint serves.
        #expect(bootstrap.sync_epoch == heartbeat.sync_epoch)
        #expect(bootstrap.revision == heartbeat.revision)
        #expect(bootstrap.attention_items == listed)
        #expect(bootstrap.attention_deliveries.isEmpty)
        #expect(bootstrap.runs == runs)
        #expect(bootstrap.schedules == schedules)
        #expect(bootstrap.conversations == AttentionFixtures.defaultConversations())

        let timeline = try await client.getRunTimeline(
            path: .init(run_id: RunFixtures.activeRunID)
        ).ok.body.json
        #expect(timeline.run_id == RunFixtures.activeRunID)
        #expect(timeline.as_of_revision == heartbeat.revision)
        #expect(!timeline.milestones.isEmpty)
    }

    @Test func runReadsDeriveTimestampsFromTimelineFacts() async throws {
        let observedID = RunFixtures.activeRunID
        let legacyID = RunFixtures.legacyRunID
        let submittedAt = Date(timeIntervalSince1970: 1_786_600_000)
        let milestoneAt = submittedAt.addingTimeInterval(60)
        let holdAt = submittedAt.addingTimeInterval(120)
        let invocationAt = submittedAt.addingTimeInterval(180)
        let forgedAt = submittedAt.addingTimeInterval(3_600)

        var observed = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == observedID })
        observed.run.created_at = forgedAt
        observed.run.last_activity_at = forgedAt
        var legacy = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == legacyID })
        legacy.run.created_at = forgedAt
        legacy.run.last_activity_at = forgedAt

        let observedTimeline = Components.Schemas.RunTimeline(
            as_of_revision: 12,
            as_of: invocationAt,
            run_id: observedID,
            milestones: [
                .init(
                    run_id: observedID,
                    kind: .run_submitted,
                    invocation_id: "inv-observed-1",
                    recorded_at: submittedAt),
                .init(
                    run_id: observedID,
                    kind: .invocation_started,
                    invocation_id: "inv-observed-1",
                    recorded_at: milestoneAt),
            ],
            hold: .init(
                value1: .init(
                    run_id: observedID,
                    invocation_id: "inv-observed-1",
                    reason: .verification_findings,
                    first_observed_at: holdAt,
                    last_observed_at: holdAt)),
            invocations: [
                .init(
                    invocation_id: "inv-observed-1",
                    run_id: observedID,
                    status: .running,
                    live: true,
                    observed_at: invocationAt)
            ],
            completion: nil,
            billable_cost_so_far: nil)
        let legacyTimeline = Components.Schemas.RunTimeline(
            as_of_revision: 12,
            as_of: submittedAt,
            run_id: legacyID,
            milestones: [],
            invocations: [],
            completion: nil,
            billable_cost_so_far: nil)
        let server = MockServer(
            runs: [observed, legacy],
            timelines: [observedTimeline, legacyTimeline])
        let client = APIClientFactory.mock(server: server)

        let listed = try await client.listRuns().ok.body.json
        let fetched = try await client.getRun(path: .init(run_id: observedID)).ok.body.json
        let bootstrap = try await client.getSyncBootstrap().ok.body.json

        for runs in [listed, bootstrap.runs] {
            let projected = try #require(runs.first { $0.run.id == observedID })
            #expect(projected.run.created_at == submittedAt)
            #expect(projected.run.last_activity_at == invocationAt)
            let unobserved = try #require(runs.first { $0.run.id == legacyID })
            #expect(unobserved.run.created_at == nil)
            #expect(unobserved.run.last_activity_at == nil)
        }
        #expect(fetched.run.created_at == submittedAt)
        #expect(fetched.run.last_activity_at == invocationAt)
    }

    @Test func legacyRunTimestampsArePresentAsExplicitNullsOnTheMockWire() async throws {
        let transport = MockServerTransport(server: MockServer())
        let request = HTTPRequest(
            method: .get,
            scheme: "https",
            authority: "freeside.invalid",
            path: "/runs")
        let (_, body) = try await transport.send(
            request,
            body: nil,
            baseURL: try #require(URL(string: "https://freeside.invalid")),
            operationID: "listRuns")
        let data = try await Data(collecting: #require(body), upTo: 1 << 20)
        let rows = try #require(
            JSONSerialization.jsonObject(with: data) as? [[String: Any]])
        let snapshot = try #require(
            rows.first { ($0["run"] as? [String: Any])?["id"] as? String == RunFixtures.legacyRunID })
        let run = try #require(snapshot["run"] as? [String: Any])

        #expect(run.keys.contains("created_at"))
        #expect(run["created_at"] is NSNull)
        #expect(run.keys.contains("last_activity_at"))
        #expect(run["last_activity_at"] is NSNull)
        for key in ["campaign_id", "attempt_number", "attempt_reason", "parent_run_id"] {
            #expect(run.keys.contains(key))
            #expect(run[key] is NSNull)
        }
    }

    @Test func legacyReadinessIsPresentAsExplicitNullOnTheMockWire() async throws {
        var legacy = AttentionFixtures.fixture(type: .ready_for_final_review)
        legacy.item.readiness = nil
        legacy.item.readiness_detail = nil
        legacy.item.yield_history = nil
        let transport = MockServerTransport(server: MockServer(items: [legacy]))
        let request = HTTPRequest(
            method: .get,
            scheme: "https",
            authority: "freeside.invalid",
            path: "/attention/items")
        let (_, body) = try await transport.send(
            request,
            body: nil,
            baseURL: try #require(URL(string: "https://freeside.invalid")),
            operationID: "listAttentionItems")
        let data = try await Data(collecting: #require(body), upTo: 1 << 20)
        let rows = try #require(
            JSONSerialization.jsonObject(with: data) as? [[String: Any]])
        let row = try #require(rows.first)
        let item = try #require(row["item"] as? [String: Any])

        #expect(item.keys.contains("readiness"))
        #expect(item["readiness"] is NSNull)
        #expect(item.keys.contains("readiness_detail"))
        #expect(item["readiness_detail"] is NSNull)
        #expect(item.keys.contains("yield_history"))
        #expect(item["yield_history"] is NSNull)
    }

    @Test func bootstrapFailsClosedOnOneInvalidRow() async throws {
        // One row the daemon could never serve fails the whole bootstrap
        // (the single-read gate), never a partial snapshot that would
        // advance a client's full-cache cursor over a hole.
        var forged = AttentionFixtures.fixture(type: .spec_approval)
        forged.item.artifact_digests.removeLast()
        let valid = AttentionFixtures.fixture(type: .agent_question)
        let client = APIClientFactory.mock(server: MockServer(items: [forged, valid]))

        let output = try await client.getSyncBootstrap()
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected a failed bootstrap, got \(output)")
            return
        }
        #expect(statusCode == 500)
    }

    @Test func invalidRunReadsAreAuthoritativeServerFailures() async throws {
        var forged = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID })
        forged.run.attempt_reason = nil
        let client = APIClientFactory.mock(server: MockServer(runs: [forged]))

        let list = try await client.listRuns()
        guard case .undocumented(let listStatus, _) = list else {
            Issue.record("expected list reconstruction failure, got \(list)")
            return
        }
        #expect(listStatus == 500)
        let get = try await client.getRun(path: .init(run_id: forged.run.id))
        guard case .undocumented(let getStatus, _) = get else {
            Issue.record("expected get reconstruction failure, got \(get)")
            return
        }
        #expect(getStatus == 500)
        let bootstrap = try await client.getSyncBootstrap()
        guard case .undocumented(let bootstrapStatus, _) = bootstrap else {
            Issue.record("expected bootstrap reconstruction failure, got \(bootstrap)")
            return
        }
        #expect(bootstrapStatus == 500)
    }

    @Test func rotatedEpochReachesBothSyncReadsWithoutTouchingRows() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getSyncRevision().ok.body.json
        let rowsBefore = try await client.listAttentionItems().ok.body.json

        await server.rotateEpoch()

        let heartbeat = try await client.getSyncRevision().ok.body.json
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        #expect(heartbeat.sync_epoch != before.sync_epoch)
        #expect(bootstrap.sync_epoch == heartbeat.sync_epoch)
        // A restore replaces the epoch, not the data a client refetches.
        #expect(bootstrap.attention_items == rowsBefore)
    }

    @Test func restoreCanRewindTheRevisionUnderTheNewEpoch() async throws {
        // A restored daemon resumes from the restored state's revision,
        // which may sit behind a client's cached cursors (test 8's
        // "discard newer cursors" half); the mock can express that.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        await server.advance(itemID: "item-spec_approval")
        await server.advance(itemID: "item-spec_approval")
        let advanced = try await client.getSyncRevision().ok.body.json

        await server.rotateEpoch(revision: 1)

        let restored = try await client.getSyncRevision().ok.body.json
        #expect(restored.sync_epoch != advanced.sync_epoch)
        #expect(restored.revision < advanced.revision)
    }

    @Test func advanceOpensAGapBetweenHeartbeatAndAFullSnapshot() async throws {
        // The raw material of the revision-gap rule (test 11): after a
        // full snapshot, a concurrent write moves the heartbeat past the
        // client's last_full_snapshot_revision.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let bootstrap = try await client.getSyncBootstrap().ok.body.json

        await server.advance(itemID: "item-spec_approval")

        let heartbeat = try await client.getSyncRevision().ok.body.json
        #expect(heartbeat.sync_epoch == bootstrap.sync_epoch)
        #expect(heartbeat.revision > bootstrap.revision)
    }
}
