import Foundation
import FreesideAPI
import FreesideCore
import Testing

@MainActor
private func makeCoordinator(
    server: MockServer, cache: CacheStore = InMemoryCacheStore()
) -> SyncCoordinator {
    SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
}

/// The client half of plan §5.14's cursor and freshness semantics,
/// against the mock daemon.
@Suite @MainActor struct SyncCoordinatorTests {
    @Test func bootstrapSetsBothCursorsAndPersistsTheCache() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        #expect(coordinator.store.freshness == .unvalidated)

        await coordinator.bootstrap()

        let cursors = try #require(coordinator.cursors)
        #expect(cursors.lastFullSnapshotRevision == cursors.highestObservedServerRevision)
        #expect(coordinator.store.rows.count == AttentionFixtures.phase1Types.count)
        #expect(coordinator.store.freshness == .fresh)
        let persisted = try #require(cache.load())
        #expect(persisted.cursors == cursors)
        #expect(persisted.attentionItems.count == coordinator.store.rows.count)
    }

    @Test func partialRefetchAdvancesOnlyTheObservedCursor() async throws {
        // Test 11, client half: a concurrent write refetched item-by-item
        // must not mark the whole cache current; the heartbeat then finds
        // the gap and only the bootstrap closes it.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        await server.advance(itemID: "item-spec_approval")
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
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

    @Test func epochChangeDiscardsTheCacheAndBootstraps() async throws {
        // Test 8, client half: a restored daemon issues a new epoch; the
        // client discards its cache and cursors — even though they sit
        // ahead of the restored revision — and bootstraps fresh.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        await server.advance(itemID: "item-spec_approval")
        await coordinator.heartbeat()
        let before = try #require(coordinator.cursors)

        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()

        let after = try #require(coordinator.cursors)
        #expect(after.syncEpoch != before.syncEpoch)
        // The dead epoch's cursors are gone, not compared: the new pair
        // adopts the restored revision even though it runs behind.
        #expect(after.lastFullSnapshotRevision < before.lastFullSnapshotRevision)
        #expect(after.highestObservedServerRevision == after.lastFullSnapshotRevision)
        #expect(coordinator.store.freshness == .fresh)
        #expect(cache.load()?.cursors?.syncEpoch == after.syncEpoch)
    }

    @Test func aDeadEpochIsEvictedEvenWhenTheRebootstrapFails() async throws {
        // §5.14 cache eviction on epoch change: the rows are dead the
        // moment the heartbeat reports a new epoch, so an outage during
        // the re-bootstrap must not keep rendering (or persisting)
        // pre-restore state.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        #expect(!coordinator.store.rows.isEmpty)

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.cursors == nil)
        #expect(cache.load() == nil)
        #expect(coordinator.store.freshness == .unreachable)

        await server.setBeforeRespond(nil)
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .fresh)
        #expect(!coordinator.store.rows.isEmpty)
    }

    @Test func launchingFromTheCacheRendersRowsWithoutClaimingFreshness() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        // A new session over the same cache: rows render before any
        // network call, but nothing claims they are current.
        let second = makeCoordinator(server: server, cache: cache)
        #expect(second.store.rows.count == first.store.rows.count)
        #expect(second.cursors == first.cursors)
        #expect(second.store.freshness == .unvalidated)

        await second.heartbeat()
        #expect(second.store.freshness == .fresh)
    }

    @Test func unreachableDaemonDegradesToTheBannerAndRecovers() async throws {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let rows = coordinator.store.rows

        await server.setBeforeRespond { _ in throw MockOutage() }
        await coordinator.heartbeat()

        // The cached view survives; only the freshness claim changes.
        #expect(coordinator.store.freshness == .unreachable)
        #expect(coordinator.store.rows == rows)

        await server.setBeforeRespond(nil)
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func rejectedCredentialSurfacesAsUnauthenticated() async throws {
        // An enforcing server with no credential: every sync read is
        // 401, which is a distinct honest state (revoked or unpaired),
        // never a generic outage.
        let coordinator = makeCoordinator(server: MockServer(authMode: .enforcing))
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func aStaleBootstrapResponseNeverWinsOverANewerRound() async throws {
        // Refute-first finding: a bootstrap response held open across a
        // restore must not land late and win the cache back for the
        // dead epoch. Only the newest sync round may adopt.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setAfterRespond { operationID in
            if operationID == "getSyncBootstrap" {
                await reached.open()
                await release.wait()
            }
        }
        // The stale round: its epoch-1 snapshot is computed, its
        // response held open.
        let stale = Task { await coordinator.bootstrap() }
        await reached.wait()
        await server.setAfterRespond(nil)

        // The restore lands and a newer round adopts the new epoch.
        await server.rotateEpoch()
        await coordinator.bootstrap()
        let adopted = try #require(coordinator.cursors)

        await release.open()
        await stale.value

        #expect(coordinator.cursors == adopted)
        #expect(cache.load()?.cursors == adopted)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func resolveOnOneDeviceConvergesTheOther() async throws {
        // Test 1, client half: device A resolves; device B's heartbeat
        // finds the gap and its bootstrap converges on the same state.
        let server = MockServer()
        let deviceA = makeCoordinator(server: server)
        let deviceB = makeCoordinator(server: server)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let model = DecisionModel(store: deviceA.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(deviceA.store.snapshotsByID["item-spec_approval"]?.item.status == .resolved)
        #expect(deviceB.store.snapshotsByID["item-spec_approval"]?.item.status == .open)

        await deviceB.heartbeat()

        let converged = try #require(deviceB.store.snapshotsByID["item-spec_approval"])
        #expect(converged == deviceA.store.snapshotsByID["item-spec_approval"])
        // B is fully current again; A's own full-snapshot cursor lags by
        // design until its next heartbeat, its partial read having
        // advanced only the observed cursor.
        let cursorsB = try #require(deviceB.cursors)
        #expect(cursorsB.lastFullSnapshotRevision == cursorsB.highestObservedServerRevision)
        #expect(
            cursorsB.highestObservedServerRevision
                == deviceA.cursors?.highestObservedServerRevision)
    }

    @Test func anUnresolvedCommandSurvivesRelaunch() async throws {
        // #115, §5.14 test 4 across a restart: a command whose response
        // was lost keeps its retry affordance through a relaunch, and
        // the restored slot still blocks a second command for the item.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: first.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        let entry = try #require(first.store.pendingCommandsByItemID["item-spec_approval"])
        #expect(entry.state == .unresolved)

        let second = makeCoordinator(server: server, cache: cache)
        let restored = try #require(
            second.store.pendingCommandsByItemID["item-spec_approval"])
        #expect(restored.command == entry.command)
        #expect(restored.state == .unresolved)
        #expect(
            !second.store.registerPendingCommand(
                makeCommand(itemID: "item-spec_approval", commandID: "cmd-duplicate")))
    }

    @Test func anInFlightEntryRestoresAsUnresolved() async throws {
        // A command persisted mid-flight has failed ambiguously by the
        // time a relaunch reads it: no task awaits its response, so only
        // the unresolved state (the retry affordance) is honest.
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: MockServer(), cache: cache)
        #expect(first.store.registerPendingCommand(makeCommand(itemID: "item-x")))
        #expect(cache.load()?.pendingCommands?["item-x"]?.state == .inFlight)

        let second = makeCoordinator(server: MockServer(), cache: cache)
        #expect(second.store.pendingCommandsByItemID["item-x"]?.state == .unresolved)
    }

    @Test func restoreDropsEntriesTheReGateRejects() async throws {
        // Decoded ledger fields are re-gated, never trusted (Codex P2 on
        // #125): another device's command must not occupy this device's
        // slots — after a re-pair its verbatim resend would die at the
        // daemon's device gate and clear a possibly committed outcome —
        // and a key naming a different item than its command must not
        // block that item. Only the consistent same-device entry lands.
        let cache = InMemoryCacheStore()
        cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-mine": .init(
                        command: makeCommand(itemID: "item-mine"), state: .unresolved),
                    "item-foreign": .init(
                        command: makeCommand(
                            itemID: "item-foreign", commandID: "cmd-foreign",
                            deviceID: "device-old"),
                        state: .unresolved),
                    "item-mismatched": .init(
                        command: makeCommand(
                            itemID: "item-other", commandID: "cmd-mismatched"),
                        state: .unresolved),
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)

        #expect(coordinator.store.pendingCommandsByItemID.count == 1)
        #expect(coordinator.store.pendingCommandsByItemID["item-mine"] != nil)
    }

    @Test func aRestoredRetryReplaysTheRecordedResult() async throws {
        // #115 acceptance 2, recorded-result branch: the command
        // committed, its response was lost, the app restarted. The
        // restored verbatim resend is served the recorded result — no
        // second side effect — and the slot clears.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: first.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        let lost = try #require(first.store.pendingCommandsByItemID["item-spec_approval"])
        await server.setAfterRespond(nil)

        let second = makeCoordinator(server: server, cache: cache)
        await second.bootstrap()
        let restored = DecisionModel(store: second.store, itemID: "item-spec_approval")
        await restored.validate()
        #expect(restored.canRetryLostResponse)

        await restored.retryLostResponse()

        #expect(restored.appliedRecord?.command_id == lost.command.command_id)
        #expect(second.store.pendingCommandsByItemID["item-spec_approval"] == nil)
        #expect(
            second.store.snapshotsByID["item-spec_approval"]?.item.status == .resolved)
    }

    @Test func aRestoredRetryAcceptsAuthoritativeRejection() async throws {
        // #115 acceptance 2, rejection branch: a restored command the
        // daemon never recorded, for an item it does not know, draws an
        // authoritative rejection on resend and the slot clears — the
        // decision was definitively not recorded, nothing to recover.
        let cache = InMemoryCacheStore()
        cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-ghost": .init(
                        command: makeCommand(itemID: "item-ghost"), state: .unresolved)
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        let model = DecisionModel(store: coordinator.store, itemID: "item-ghost")
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()

        #expect(coordinator.store.pendingCommandsByItemID["item-ghost"] == nil)
        #expect(model.submissionError != nil)
    }

    @Test func aHeartbeatEpochDiscardPreservesTheLedger() async throws {
        // #115 acceptance 4 on the eager path: the heartbeat's epoch
        // mismatch evicts rows and cursors immediately, but commitment
        // is epoch-independent — the ledger survives the eviction, the
        // persisted file, and a relaunch inside the outage window.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(coordinator.store.pendingCommandsByItemID["item-spec_approval"] != nil)
        await server.setAfterRespond(nil)

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.cursors == nil)
        let persisted = try #require(cache.load())
        #expect(persisted.cursors == nil)
        #expect(persisted.attentionItems.isEmpty)
        #expect(persisted.pendingCommands?["item-spec_approval"] != nil)

        let second = makeCoordinator(server: server, cache: cache)
        #expect(
            second.store.pendingCommandsByItemID["item-spec_approval"]?.state
                == .unresolved)
    }

    @Test func aBootstrapEpochDiscardPreservesTheLedger() async throws {
        // #115 acceptance 4 on the backstop path: an epoch change first
        // observed by a direct bootstrap discards and re-adopts in one
        // motion; the re-persisted cache carries the new cursors and the
        // surviving ledger together.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        await server.setAfterRespond(nil)

        await server.rotateEpoch()
        await coordinator.bootstrap()

        let persisted = try #require(cache.load())
        #expect(persisted.cursors?.syncEpoch == coordinator.cursors?.syncEpoch)
        #expect(persisted.pendingCommands?["item-spec_approval"] != nil)
        #expect(
            coordinator.store.pendingCommandsByItemID["item-spec_approval"] != nil)
    }

    @Test func clearingTheLastLedgerEntryAfterDiscardRemovesTheFile() async throws {
        // Once the surviving ledger settles with no cursors to keep, the
        // file goes too: keeping one would undo the epoch eviction.
        let cache = InMemoryCacheStore()
        let command = makeCommand(itemID: "item-x")
        cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-x": .init(command: command, state: .unresolved)
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        #expect(coordinator.store.pendingCommandsByItemID["item-x"] != nil)

        coordinator.store.clearPendingCommand(
            itemID: "item-x", commandID: command.command_id)

        #expect(cache.load() == nil)
    }

    @Test func staleSecondDeviceSubmissionRendersTheReplacement() async throws {
        // Test 2, client half: device B validated while the item was
        // open, device A then resolved it, and B's submission against
        // the superseded version is rejected with the replacement item
        // rendered — never applied, never an error dead-end.
        let server = MockServer()
        let deviceA = makeCoordinator(server: server)
        let deviceB = makeCoordinator(server: server)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let modelB = DecisionModel(store: deviceB.store, itemID: "item-spec_approval")
        await modelB.validate()
        #expect(modelB.actionsEnabled)

        let modelA = DecisionModel(store: deviceA.store, itemID: "item-spec_approval")
        await modelA.validate()
        await modelA.submit(.approve)

        // stop is a concluding action this unit can submit; the point is
        // the version binding, not which decision B picked.
        await modelB.submit(.stop)

        #expect(modelB.phase == .superseded)
        let replacement = try #require(deviceB.store.snapshotsByID["item-spec_approval"])
        #expect(replacement.item.status == .resolved)
        #expect(replacement == deviceA.store.snapshotsByID["item-spec_approval"])
    }

    @Test func anEpochEvictionInvalidatesAPriorValidation() async {
        // A card validated before a daemon restore must not stay enabled
        // once the heartbeat evicts the dead epoch and re-bootstraps the
        // rows: the pre-restore validation certified an epoch this card
        // never revalidated, so actions fail closed until it does (#162;
        // plan §5.14 cache eviction on epoch change).
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        // The daemon restores under a new epoch; the heartbeat discards
        // the cache and re-bootstraps the same open item.
        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()

        #expect(coordinator.store.cacheGeneration > 0)
        #expect(model.snapshot?.item.status == .open)  // the row is back...
        #expect(!model.actionsEnabled)                 // ...but not certified

        // A fresh validation against the new epoch re-enables actions.
        await model.validate()
        #expect(model.actionsEnabled)
    }

    @Test func aValidationEvictedMidFetchRefetchesInsteadOfCertifying() async {
        // Everything is @MainActor, so a heartbeat eviction can land while
        // validate()'s getAttentionItem is suspended at its await. The
        // response is then from a possibly dead epoch, so validate must
        // drop it and re-fetch against the current epoch rather than
        // applying and certifying it under the new generation (#162).
        //
        // The in-process mock computes each response at delivery time, so
        // it cannot hand back a genuinely stale-epoch body; what is
        // asserted here is that the eviction is detected and forces the
        // re-fetch (a second getAttentionItem), which is the mechanism that
        // protects against the stale-epoch response a real daemon can send.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")

        let reached = AsyncGate()
        let release = AsyncGate()
        let firstGet = OneShot()
        let getCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                await getCalls.increment()
                if await firstGet.fire() {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let validation = Task { await model.validate() }
        await reached.wait()

        // Evict the cache for a new epoch while the fetch is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await validation.value

        // The guard fired: a second fetch happened rather than certifying
        // the in-flight response, and the model certified current state.
        #expect(await getCalls.count == 2)
        #expect(model.validation == .validated)
    }

    @Test func aPostCommitRefetchEvictedMidFetchRefetchesInsteadOfCertifying() async {
        // The read-your-write refetch after a successful submit is another
        // @MainActor await an eviction can land inside; like validate() it
        // must not apply/certify a response that resumed under a changed
        // cache generation (#162). Same mock caveat as the validate case:
        // the assertion is that the eviction forces a re-validate rather
        // than certifying the in-flight refetch.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()  // getAttentionItem #1
        #expect(model.actionsEnabled)

        // Installed after the initial validate(), so the first fetch this
        // hook sees is the post-commit read-your-write refetch; hold it.
        let reached = AsyncGate()
        let release = AsyncGate()
        let getCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                if await getCalls.incrementAndGet() == 1 {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Evict the cache for a new epoch while the refetch is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // The guard fired: the .ok path recovered against the current
        // epoch (at least one further getAttentionItem) instead of
        // certifying the single in-flight refetch. (The exact count depends
        // on the recovery path — settleAmbiguousOutcome re-validates and
        // replays — so this pins the property, not a brittle count.)
        #expect(await getCalls.count >= 2)
    }

    @Test func aSuccessResultEvictedMidFlightIsTreatedAsAmbiguousNotCleared() async {
        // A 200 that resumes after a mid-flight eviction is from a possibly
        // rolled-back pre-restore epoch. Clearing the ledger then would drop
        // the retry state discardSnapshots() preserves, so the .ok arm must
        // treat it as ambiguous (keep the slot, replay) rather than applied
        // (#162). Observable via the replay: a second submitCommand runs.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        let reached = AsyncGate()
        let release = AsyncGate()
        let firstSubmit = OneShot()
        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                await submitCalls.increment()
                if await firstSubmit.fire() {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Evict the cache for a new epoch while the command is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // Treated as ambiguous (settleAmbiguousOutcome replays), not cleared
        // as applied: a second submitCommand ran.
        #expect(await submitCalls.count == 2)
    }

    @Test func aRefetchEvictedMidFlightIsSettledAmbiguousNotFalselyApplied() async {
        // The 200 was valid (no eviction during submitCommand), but a
        // restore lands during the read-your-write refetch. Because the
        // record and slot are settled only after the refetch confirms the
        // generation, this is handled as ambiguous (keep the slot, replay)
        // rather than left showing a false "applied" with the retry slot
        // dropped (#162). Observable via the replay: a second submitCommand.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        // Installed after the initial validate(), so the first fetch this
        // hook sees is the post-commit refetch; hold it.
        let reached = AsyncGate()
        let release = AsyncGate()
        let getCalls = Counter()
        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { await submitCalls.increment() }
            if operationID == "getAttentionItem", await getCalls.incrementAndGet() == 1 {
                await reached.open()
                await release.wait()
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Restore during the read-your-write refetch.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // Settled ambiguous (a replay ran), not the refetch-eviction branch
        // just revalidating and leaving the optimistic record in place.
        #expect(await submitCalls.count == 2)
    }
}

private struct MockOutage: Error {}
