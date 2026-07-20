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

    @Test func restoreEvictsTheCachedCardDespiteVersionRollback() async throws {
        // #165 + #162 convergence gate: a real checkpoint/restore rolls the
        // item version back below what the client cached and rotates the
        // epoch in one operation. The epoch change must evict the higher-
        // versioned card and bootstrap to the restored lower version, never
        // letting the dead pre-restore row shadow the reset fetch (#162), and
        // a decision validated afterward binds to the restored state.
        let control = try ConvergenceHarness.control()
        let itemID = try await ConvergenceHarness.seedUniqueItem(label: "t165")  // version 1
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 165")
        let cache = InMemoryCacheStore()
        let coordinator = ConvergenceHarness.coordinator(for: device, cache: cache)

        // Checkpoint the version-1 world, bootstrap the client onto it, then
        // advance the client past the checkpoint so it caches version 2.
        let checkpoint = try await control.checkpoint()
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)
        #expect(coordinator.store.snapshotsByID[itemID]?.item.item_version == 1)

        try await control.seedItem(id: itemID, version: 2)
        await coordinator.heartbeat()
        #expect(coordinator.store.snapshotsByID[itemID]?.item.item_version == 2)

        // Restore: the version rolls back to 1 and the epoch rotates in one call.
        try await control.restore(checkpoint: checkpoint)
        await coordinator.heartbeat()

        // The epoch changed, so the client discarded the cached version 2 and
        // bootstrapped fresh onto the restored version 1.
        let after = try #require(coordinator.cursors)
        #expect(after.syncEpoch != before.syncEpoch)
        #expect(coordinator.store.freshness == .fresh)
        #expect(after.lastFullSnapshotRevision == after.highestObservedServerRevision)
        #expect(coordinator.store.snapshotsByID[itemID]?.item.item_version == 1)
        #expect(cache.load()?.cursors?.syncEpoch == after.syncEpoch)

        // A decision validated after the restore certifies against the
        // restored card, not the evicted replacement (#162 generation guard).
        let model = DecisionModel(store: coordinator.store, itemID: itemID)
        await model.validate()
        #expect(model.actionsEnabled)
        #expect(model.snapshot?.item.item_version == 1)
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
        // The real daemon stamps the decision instant with the conclusion
        // (#171); this is the wire-shape proof the mock mirrors.
        #expect(deviceA.store.snapshotsByID[itemID]?.item.decided_at != nil)
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
        #expect(replacement.item.decided_at != nil)
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

    // MARK: - Text-claim carrier (issue #217)

    @Test func aTextClaimRoundTripsThroughTheRealDaemon() async throws {
        // #217: the daemon constructs a markdown text claim (digest bound
        // to the content by domain.ClaimText.ComputeDigest and re-validated
        // on every decode) and the generated client must deliver the inline
        // carrier intact through a real bootstrap — content, media type,
        // and the claim digest's membership in the item's binding set.
        let control = try ConvergenceHarness.control()
        let itemID = ConvergenceHarness.uniqueItemID("t217")
        let summary = "Work on **\(itemID)** is ready; one decision is open."
        try await control.seedItem(id: itemID, version: 1, textClaim: summary)
        let device = try await ConvergenceHarness.pairDevice(displayName: "Convergence 217")
        let coordinator = ConvergenceHarness.coordinator(for: device, cache: InMemoryCacheStore())

        await coordinator.bootstrap()

        let item = try #require(coordinator.store.snapshotsByID[itemID]?.item)
        let claim = try #require(item.agent_claims.first { $0.text != nil })
        let text = try #require(claim.text)
        #expect(text.media_type == .text_sol_markdown)
        #expect(text.content == summary)
        #expect(item.artifact_digests.contains(claim.digest))
    }

    // MARK: - Policy matrix parity (issue #204)

    /// Cross-language proof that the Swift fixture matrix
    /// (`AttentionFixtures.phase1ActionSets`) and the daemon's authoritative
    /// per-type action policy agree, for every (type, action) cell. It drives
    /// the real policy over the control seed boundary — the wire-level port of
    /// signet's `policy_test.go` — so it fails when *either* side's matrix
    /// changes alone: the daemon's accept/reject verdict must equal the
    /// fixture's classification for every action.
    @Test(arguments: AttentionFixtures.phase1Types)
    func daemonPolicyMatchesTheFixtureMatrix(
        _ type: Components.Schemas.AttentionType
    ) async throws {
        let control = try ConvergenceHarness.control()
        let allowed = try #require(AttentionFixtures.phase1ActionSets[type])
        let allowedSet = Set(allowed)

        // The whole allowed set is accepted at the policy boundary (blocked: the
        // empty set). A fixture that offered an action the daemon disallows would
        // 400 here — one drift signal.
        let whole = try await control.seedItemOutcome(
            id: ConvergenceHarness.uniqueItemID("pol-\(type.rawValue)-all"),
            type: type, actions: allowed)
        #expect(
            whole.statusCode == 200,
            "allowed set for \(type.rawValue) rejected: \(whole.message ?? "")")

        // Each action's individual verdict must equal the fixture's
        // classification: allowed → accepted; otherwise → the typed
        // ErrActionNotAllowedForType 400. Removing an action from either side
        // alone flips exactly these cells.
        for action in AttentionFixtures.phase1Actions {
            let outcome = try await control.seedItemOutcome(
                id: ConvergenceHarness.uniqueItemID("pol-\(type.rawValue)-\(action.rawValue)"),
                type: type, actions: [action])
            if allowedSet.contains(action) {
                #expect(
                    outcome.statusCode == 200,
                    "\(action.rawValue) should be allowed for \(type.rawValue): \(outcome.message ?? "")")
            } else {
                #expect(
                    outcome.statusCode == 400,
                    "\(action.rawValue) should be rejected for \(type.rawValue)")
                #expect(
                    outcome.message?.contains("is not allowed for") == true,
                    "want not-allowed 400 for \(action.rawValue): \(outcome.message ?? "")")
            }
        }

        // Every actionable type must offer at least one decision; the empty set
        // is the client-visible ErrNoActions 400. blocked is the read-only
        // exception, proven accepted by the whole-set seed above.
        if type != .blocked {
            let empty = try await control.seedItemOutcome(
                id: ConvergenceHarness.uniqueItemID("pol-\(type.rawValue)-empty"),
                type: type, actions: [])
            #expect(empty.statusCode == 400)
            #expect(
                empty.message?.contains("offers no requested decision") == true,
                "expected ErrNoActions for \(type.rawValue), got: \(empty.message ?? "")")
        }
    }

    /// The invalid/unknown arm: an unrecognized attention type or action string
    /// is rejected with a client-visible 400 without either enum's validation
    /// being weakened — the values are sent as raw strings the typed Swift enums
    /// cannot hold, and the daemon's domain gate rejects them.
    @Test func daemonRejectsUnknownTypeAndAction() async throws {
        let control = try ConvergenceHarness.control()

        let unknownType = try await control.seedItemOutcome(
            id: ConvergenceHarness.uniqueItemID("pol-unknown-type"),
            type: "not_a_real_type", actions: ["stop"])
        #expect(unknownType.statusCode == 400)
        #expect(
            unknownType.message?.contains("unknown attention type") == true,
            "got: \(unknownType.message ?? "")")

        let unknownAction = try await control.seedItemOutcome(
            id: ConvergenceHarness.uniqueItemID("pol-unknown-action"),
            type: Components.Schemas.AttentionType.spec_approval.rawValue,
            actions: ["not_a_real_action"])
        #expect(unknownAction.statusCode == 400)
        #expect(
            unknownAction.message?.contains("invalid action") == true,
            "got: \(unknownAction.message ?? "")")
    }

    private func snapshot(
        of itemID: String, via device: LiveDevice
    ) async throws -> Components.Schemas.AttentionItemSnapshot {
        try await device.client.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
    }
}
