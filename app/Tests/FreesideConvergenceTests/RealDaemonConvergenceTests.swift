import Foundation
import FreesideAPI
import FreesideCore
import Testing

/// The §5.14 real-daemon convergence pass (issue #72): the client
/// halves of sync tests 1, 2, 8, 11, and 13–16, exercised against a
/// running freeside-signet-dev harness instead of the in-process mock.
/// The assertions mirror the mock suites (SyncCoordinatorTests,
/// DecisionRevocationTests, AppSessionTests); what changes is the
/// choreography, which here goes through real state: a second paired
/// device where the mock had an actor hook, the control listener where
/// it had seed/rotate methods, and a client-side failing transport
/// where it had setBeforeRespond.
///
/// Serialized: every test shares one daemon process, and test 8's
/// epoch rotation would read as a restore to any test in flight.
/// Isolation within the shared process comes from unique item IDs and
/// per-test paired devices. Swift Testing serializes only within a
/// suite, so any future suite added to this target must carry
/// `.serialized` too, or its tests race this one against the shared
/// daemon.
@Suite(.serialized, .enabled(if: ConvergenceEnvironment.isConfigured))
@MainActor
struct RealDaemonConvergenceTests {
    // MARK: - Cache semantics (tests 8 and 11)

    @Test func epochChangeDiscardsTheCacheAndBootstraps() async throws {
        // Test 8, client half: a restored daemon issues a new epoch and
        // the client discards cursors and cache and bootstraps fresh.
        // One deliberate difference from the mock test: the real
        // daemon's revision is monotonic across epochs by design
        // (store.NewEpoch never resets it), so the epoch change itself,
        // not a revision comparison, is what invalidates the cursors.
        let control = try ConvergenceHarness.control()
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t8")
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 8")
        let cache = InMemoryCacheStore()
        let coordinator = ConvergenceHarness.coordinator(for: device, cache: cache)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)
        #expect(coordinator.store.snapshotsByID[itemID] != nil)

        try await control.rotateEpoch()
        await coordinator.heartbeat()

        let after = try #require(coordinator.cursors)
        #expect(after.syncEpoch != before.syncEpoch)
        #expect(after.lastFullSnapshotRevision == after.highestObservedServerRevision)
        #expect(coordinator.store.freshness == .fresh)
        #expect(coordinator.store.snapshotsByID[itemID] != nil)
        #expect(cache.load()?.cursors?.syncEpoch == after.syncEpoch)
    }

    @Test func partialRefetchAdvancesOnlyTheObservedCursor() async throws {
        // Test 11, client half: a server-side write refetched
        // item-by-item must not mark the whole cache current; the
        // heartbeat then finds the gap and only the bootstrap closes it.
        let control = try ConvergenceHarness.control()
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t11")
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 11")
        let coordinator = ConvergenceHarness.coordinator(for: device)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        try await control.seedItem(id: itemID, version: 2)
        let model = DecisionModel(store: coordinator.store, itemID: itemID)
        await model.validate()

        let partial = try #require(coordinator.cursors)
        #expect(partial.lastFullSnapshotRevision == before.lastFullSnapshotRevision)
        #expect(partial.highestObservedServerRevision > partial.lastFullSnapshotRevision)

        await coordinator.heartbeat()

        let converged = try #require(coordinator.cursors)
        #expect(converged.lastFullSnapshotRevision == converged.highestObservedServerRevision)
        #expect(converged.lastFullSnapshotRevision > before.lastFullSnapshotRevision)
        #expect(coordinator.store.freshness == .fresh)
    }

    // MARK: - Two-device convergence (tests 1 and 2)

    @Test func resolveOnOneDeviceConvergesTheOther() async throws {
        // Test 1, client half: device A resolves; device B's heartbeat
        // finds the gap and its bootstrap converges on the same state.
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t1")
        let a = try await ConvergenceHarness.pairDevice(displayName: "Convergence 1A")
        let b = try await ConvergenceHarness.pairDevice(displayName: "Convergence 1B")
        let deviceA = ConvergenceHarness.coordinator(for: a)
        let deviceB = ConvergenceHarness.coordinator(for: b)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let model = DecisionModel(store: deviceA.store, itemID: itemID)
        await model.validate()
        await model.submit(.stop)
        #expect(deviceA.store.snapshotsByID[itemID]?.item.status == .resolved)
        #expect(deviceB.store.snapshotsByID[itemID]?.item.status == .open)

        await deviceB.heartbeat()

        let converged = try #require(deviceB.store.snapshotsByID[itemID])
        #expect(converged == deviceA.store.snapshotsByID[itemID])
        let cursorsB = try #require(deviceB.cursors)
        #expect(cursorsB.lastFullSnapshotRevision == cursorsB.highestObservedServerRevision)
        #expect(
            cursorsB.highestObservedServerRevision
                == deviceA.cursors?.highestObservedServerRevision)
    }

    @Test func staleSecondDeviceSubmissionRendersTheReplacement() async throws {
        // Test 2, client half: device B validated while the item was
        // open, device A then resolved it, and B's submission against
        // the superseded version is rejected with the replacement item
        // rendered — never applied, never an error dead-end.
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t2")
        let a = try await ConvergenceHarness.pairDevice(displayName: "Convergence 2A")
        let b = try await ConvergenceHarness.pairDevice(displayName: "Convergence 2B")
        let deviceA = ConvergenceHarness.coordinator(for: a)
        let deviceB = ConvergenceHarness.coordinator(for: b)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let modelB = DecisionModel(store: deviceB.store, itemID: itemID)
        await modelB.validate()
        #expect(modelB.actionsEnabled)

        let modelA = DecisionModel(store: deviceA.store, itemID: itemID)
        await modelA.validate()
        await modelA.submit(.stop)

        // dismiss is a concluding action this item type offers; the
        // point is the version binding, not which decision B picked.
        await modelB.submit(.dismiss)

        #expect(modelB.phase == .superseded)
        let replacement = try #require(deviceB.store.snapshotsByID[itemID])
        #expect(replacement.item.status == .resolved)
        #expect(replacement == deviceA.store.snapshotsByID[itemID])
    }

    // MARK: - Pairing (tests 13 and 14)

    @Test func consumedAndUnknownCodesRejectUndifferentiated() async throws {
        // Test 13, client face: a consumed code cannot pair a second
        // device, and the UI can say no more than the daemon did — a
        // consumed code and a never-minted one read identically. The
        // expired-code half is deliberately not driven here: the daemon
        // collapses expired and consumed into one undifferentiated
        // rejection (this very anti-probing contract), and expiry is
        // pinned by signet's own clock-injected tests.
        let apiURL = try #require(ConvergenceEnvironment.apiURL)
        let control = try ConvergenceHarness.control()
        let code = try await control.mintPairingCode()

        let winner = PairingModel(
            client: APIClientFactory.live(serverURL: apiURL),
            credentials: InMemoryCredentialStore())
        winner.pairingCode = code
        winner.displayName = "Convergence 13"
        let credential = await winner.pair()
        #expect(credential != nil)
        #expect(credential?.token.hasPrefix("fsd1.") == true)

        var failures: Set<String> = []
        for probe in [code, "ZZZZZZZZ"] {
            let loser = PairingModel(
                client: APIClientFactory.live(serverURL: apiURL),
                credentials: InMemoryCredentialStore())
            loser.pairingCode = probe
            loser.displayName = "Convergence 13 probe"
            #expect(await loser.pair() == nil)
            guard case .failed(let message) = loser.phase else {
                Issue.record("expected a rejection for \(probe), got \(loser.phase)")
                continue
            }
            failures.insert(message)
        }
        #expect(failures.count == 1)
    }

    @Test func simultaneousPairingAttemptsYieldOneDevice() async throws {
        // Test 14, client half: two concurrent exchanges of one code
        // produce exactly one device; either racer may win, and the
        // winner's token authorizes.
        let apiURL = try #require(ConvergenceEnvironment.apiURL)
        let control = try ConvergenceHarness.control()
        let code = try await control.mintPairingCode()
        let client = APIClientFactory.live(serverURL: apiURL)

        async let first = client.pairDevice(
            body: .json(.init(pairing_code: code, display_name: "Racer 1")))
        async let second = client.pairDevice(
            body: .json(.init(pairing_code: code, display_name: "Racer 2")))
        let outcomes = try await [first, second]

        var tokens: [String] = []
        for outcome in outcomes {
            if case .created(let created) = outcome {
                tokens.append(try created.body.json.device_token)
            }
        }
        #expect(tokens.count == 1)

        let token = try #require(tokens.first)
        let authorized = APIClientFactory.live(serverURL: apiURL) { token }
        _ = try await authorized.getSyncRevision().ok
    }

    // MARK: - Revocation honesty (tests 15 and 16)

    @Test func revokedDeviceCannotSubmitAPreparedCommand() async throws {
        // Test 15, client half: revocation lands between preparing the
        // decision and submitting it. The fresh submission dies at the
        // credential gate before any acceptance, so nothing renders
        // applied, nothing stays pending, and the device state
        // surfaces. Absence of a side effect is asserted through the
        // surviving device B, the real analogue of the mock's snapshot
        // hook.
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t15")
        let a = try await ConvergenceHarness.pairDevice(displayName: "Convergence 15A")
        let b = try await ConvergenceHarness.pairDevice(displayName: "Convergence 15B")
        let storeA = InboxStore(client: a.client, device: a.identity)
        await storeA.refresh()
        let model = DecisionModel(store: storeA, itemID: itemID)
        await model.validate()
        #expect(model.actionsEnabled)
        let before = try await snapshot(of: itemID, via: b)

        _ = try await a.client.revokeDevice(path: .init(device_id: a.deviceID)).ok

        await model.submit(.stop)

        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
        #expect(model.submissionError != nil)
        #expect(storeA.pendingCommandsByItemID[itemID] == nil)
        #expect(storeA.freshness == .unauthenticated)
        #expect(!model.actionsEnabled)
        #expect(try await snapshot(of: itemID, via: b) == before)
    }

    @Test func revokedRetryOfAnUncommittedCommandStaysAmbiguousNotFalselyRejected() async throws {
        // Test 16, client half, reject branch: the original attempt
        // never reached the daemon, revocation lands while the command
        // is unresolved, and the verbatim retry gets the credential
        // gate's 401 — which proves nothing about commitment, so the
        // slot stays held rather than settling as "not recorded".
        //
        // §5.14 lets a daemon serve a revoked retry its recorded result
        // instead; the real daemon's authorizer rejects every revoked
        // request before the service's idempotent replay can run, so
        // the 401 branch is the only one observable over the wire (the
        // replay branch is pinned by signet's service-level Go tests,
        // and the client's rendering of it by the mock suite). The
        // client is honest under either.
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t16")
        let a = try await ConvergenceHarness.pairDevice(displayName: "Convergence 16A")
        let b = try await ConvergenceHarness.pairDevice(displayName: "Convergence 16B")
        let storeA = InboxStore(client: a.client, device: a.identity)
        await storeA.refresh()
        let model = DecisionModel(store: storeA, itemID: itemID)
        await model.validate()
        let before = try await snapshot(of: itemID, via: b)

        a.transport.fail(operations: ["submitCommand"])
        await model.submit(.stop)
        #expect(storeA.pendingCommandsByItemID[itemID]?.state == .unresolved)
        a.transport.restore()

        _ = try await a.client.revokeDevice(path: .init(device_id: a.deviceID)).ok

        #expect(model.canRetryLostResponse)
        await model.retryLostResponse()

        #expect(storeA.pendingCommandsByItemID[itemID]?.state == .unresolved)
        #expect(model.appliedRecord == nil)
        #expect(storeA.freshness == .unauthenticated)
        #expect(!model.actionsEnabled)
        #expect(try await snapshot(of: itemID, via: b) == before)
    }

    // MARK: - Attachment render (issue #128, acceptance 3)

    @Test func anUploadedImageRendersFromTheRealDaemon() async throws {
        // #128 acceptance 3: an image uploaded to the real daemon reads
        // back through getAttachment and decodes to the inline .image
        // phase, where before the route existed every attachment settled
        // as the unavailable placeholder (the failure Codex raised on
        // #126). Mirrors the mock
        // AttachmentLoaderTests.anImageDigestDecodesToTheImagePhase, here
        // against a live freeside-signet-dev instead of MockServer.
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 128")
        let digest = try await ConvergenceHarness.uploadAttachment(
            AttentionFixtures.fixtureImagePNG, on: device)
        let loader = AttachmentLoader(client: device.client)

        await loader.load(digest)

        guard case .image = loader.phase(for: digest) else {
            Issue.record("expected .image, got \(String(describing: loader.phase(for: digest)))")
            return
        }
    }

    @Test func anUnstoredDigestRendersTheUnavailablePlaceholder() async throws {
        // The negative half: a well-formed but unstored digest gets the
        // daemon's authoritative 404, which the loader maps to
        // .unavailable (the card's placeholder branch) rather than a hang
        // or a crash.
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 128N")
        let unstored = "sha256:" + String(repeating: "11", count: 32)
        let loader = AttachmentLoader(client: device.client)

        await loader.load(unstored)

        #expect(loader.phase(for: unstored) == .unavailable)
    }

    private func snapshot(
        of itemID: String, via device: LiveDevice
    ) async throws -> Components.Schemas.AttentionItemSnapshot {
        try await device.client.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
    }
}
