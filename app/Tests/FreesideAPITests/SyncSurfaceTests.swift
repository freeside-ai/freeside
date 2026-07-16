import Foundation
import FreesideAPI
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

        // The full-cache cursor pair matches the heartbeat's ServerState
        // read, and the item collection is the same canonical list the
        // list endpoint serves.
        #expect(bootstrap.sync_epoch == heartbeat.sync_epoch)
        #expect(bootstrap.revision == heartbeat.revision)
        #expect(bootstrap.attention_items == listed)
        // The other collections are present-but-empty until their units
        // seed them; the envelope shape is the real contract's.
        #expect(bootstrap.attention_deliveries.isEmpty)
        #expect(bootstrap.runs.isEmpty)
        #expect(bootstrap.conversations.isEmpty)
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
