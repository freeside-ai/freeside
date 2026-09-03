import Foundation
import FreesideAPI
import Observation

#if canImport(AppKit)
    import AppKit
#elseif canImport(UIKit)
    import UIKit
#endif

/// One attention item's decision surface: revalidates the item's current
/// state on open, exposes exactly the actions the item requests, and
/// submits a ClientCommand bound to the rendered snapshot's versions and
/// digests. Consequential actions stay disabled until the current state
/// validates, and "applied" renders only from a received CommandResult.
/// A submission whose outcome is unknown lives in the store's per-item
/// pending-command ledger: it survives this model's recreation, blocks
/// every new command for the item, and resolves only through a verbatim
/// resend (plan §5.14 sync test 4).
@MainActor
@Observable
public final class DecisionModel {
    public enum ValidationState: Equatable {
        case pending
        case validated
        case failed(String)
    }

    public enum SubmissionPhase: Equatable {
        case idle
        case submitting(Components.Schemas.Action)
        case applied
        /// A stale or closed-item submission was rejected; the rendered
        /// snapshot is the replacement item the daemon returned.
        case superseded
    }

    public let itemID: String
    public private(set) var validation: ValidationState = .pending
    public private(set) var phase: SubmissionPhase = .idle
    public private(set) var appliedRecord: Components.Schemas.CommandRecord?
    public private(set) var submissionError: String?
    public private(set) var proposalFacts: Components.Schemas.RunProposalFactsSnapshot?

    private let store: InboxStore
    private let openURL: (URL) async -> Bool
    private let onConclusion: @MainActor (DecisionConclusion) -> Void
    /// Overlapping validations resolve by recency: only the newest call
    /// may write the outcome, so a stale late failure cannot clobber a
    /// newer success (or vice versa).
    private var validationGeneration = 0
    /// Advances only after this model durably claims the item's pending-command
    /// slot. Message composers use it to distinguish a submission that started
    /// from one rejected locally before any command could leave the device.
    private var submissionClaimGeneration = 0
    /// A recorded command emits at most one receipt, after canonical state
    /// proves that its item left the active queue. Revalidation and replay
    /// share this gate so recovery cannot duplicate the receipt.
    private var concludedCommandID: String?
    /// The store's cache generation at the moment this card last
    /// certified current state. An epoch eviction bumps that generation
    /// (`InboxStore.discardSnapshots`), so a validation that predates the
    /// eviction cannot certify the rows a later bootstrap repopulates —
    /// `actionsEnabled` fails closed until a fresh validation (#162).
    private var validatedCacheGeneration = 0
    /// The daemon-derived action surface for this device and item (plan §8),
    /// fetched when the card opens. It drives the action ranking's available
    /// set and is referenced by the submitted command and the action-bearing
    /// telemetry events. Nil until fetched, or when the fetch failed (an older
    /// build, or a device with no registered contract): the local filter and a
    /// digest-free command then stand.
    public private(set) var actionSurface: Components.Schemas.DecisionActionSurface?
    /// The store cache generation the action surface was fetched under. An
    /// epoch eviction (a daemon restore) bumps it, so a surface derived
    /// against a now-dead epoch is refetched rather than reused (issue #162).
    private var actionSurfaceCacheGeneration = 0
    /// Advances on every action-surface fetch. Only the newest fetch may
    /// install its result, so an overlapping or cancelled fetch whose response
    /// lands out of order cannot install a surface the stale check would then
    /// trust (issue #162).
    private var actionSurfaceRequestGeneration = 0
    private var cardOpenedEmitted = false
    private var actionEventsEmittedCommandID: String?
    private var drillDownEmitted = false
    private var detailsRevealEmitted = false
    private var notDecidableEmitted = false

    public init(store: InboxStore, itemID: String) {
        self.store = store
        self.itemID = itemID
        openURL = Self.openExternalURL
        onConclusion = { _ in }
    }

    init(
        store: InboxStore,
        itemID: String,
        onConclusion: @escaping @MainActor (DecisionConclusion) -> Void
    ) {
        self.store = store
        self.itemID = itemID
        openURL = Self.openExternalURL
        self.onConclusion = onConclusion
    }

    init(
        store: InboxStore,
        itemID: String,
        openURL: @escaping (URL) async -> Bool,
        onConclusion: @escaping @MainActor (DecisionConclusion) -> Void = { _ in }
    ) {
        self.store = store
        self.itemID = itemID
        self.openURL = openURL
        self.onConclusion = onConclusion
    }

    private static func openExternalURL(_ url: URL) async -> Bool {
        #if canImport(AppKit)
            return NSWorkspace.shared.open(url)
        #elseif canImport(UIKit)
            guard UIApplication.shared.canOpenURL(url) else { return false }
            return await withCheckedContinuation { continuation in
                UIApplication.shared.open(url, options: [:]) { opened in
                    continuation.resume(returning: opened)
                }
            }
        #endif
    }

    static func pullRequestURL(for reference: Components.Schemas.PRReference) -> URL? {
        let parts = reference.repo.split(separator: "/", omittingEmptySubsequences: false)
        guard parts.count == 2, reference.number > 0,
            parts.allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." }),
            var url = URL(string: "https://github.com")
        else { return nil }
        for part in parts {
            url.appendPathComponent(String(part))
        }
        url.appendPathComponent("pull")
        url.appendPathComponent(String(reference.number))
        return url
    }

    /// Re-keys the view's validation task on cache generation and the exact
    /// item and conversation tuples it renders. A selected card therefore
    /// revalidates after either an epoch eviction or an ordinary same-epoch
    /// advance, including a heartbeat that repairs an auxiliary post-command
    /// conversation read.
    public var revalidationID: String {
        let epochKey = "\(itemID)#\(store.cacheGeneration)"
        guard let snapshot else { return epochKey }
        let snapshotKey =
            "\(epochKey)#\(snapshot.as_of_revision)#\(snapshot.entity_version)#\(snapshot.item.item_version)"
        guard let conversation = store.conversation(for: snapshot.item) else {
            return snapshotKey
        }
        return "\(snapshotKey)#\(conversation.as_of_revision)#\(conversation.entity_version)"
    }

    public var snapshot: Components.Schemas.AttentionItemSnapshot? {
        store.snapshotsByID[itemID]
    }

    public var conversation: Components.Schemas.ConversationSnapshot? {
        snapshot.flatMap { store.conversation(for: $0.item) }
    }

    public var revisedSpecification: Components.Schemas.AttentionItemSnapshot? {
        guard let snapshot, snapshot.item._type == .spec_approval,
            snapshot.item.status == .superseded,
            let runID = Self.runID(of: snapshot.item.subject)
        else { return nil }
        return store.snapshotsByID.values
            .filter {
                $0.item.id != itemID && $0.item._type == .spec_approval
                    && $0.item.status == .open
                    && Self.runID(of: $0.item.subject) == runID
            }
            .max { $0.item.item_version < $1.item.item_version }
    }

    private static func runID(of subject: Components.Schemas.Subject) -> String? {
        switch subject {
        case .run(let run), .proposal_batch(let run): return run.run_id
        case .project, .system: return nil
        }
    }

    /// The item's in-flight command with an unknown outcome, owned by the
    /// store so navigation cannot forget it.
    public var pendingCommand: Components.Schemas.ClientCommand? {
        store.pendingCommandsByItemID[itemID]?.command
    }

    /// Exactly the item's requested decision set (plan §4; approve is not
    /// universal). The card renders these and nothing else.
    public var offeredActions: [Components.Schemas.Action] {
        snapshot?.item.requested_decision ?? []
    }

    /// Whether this unit can submit the action for this item: pending
    /// actions' accepted effects (conversations, parameters, proposal
    /// revisions) belong to later units, and signet policy pins blocked
    /// read-only (#97) — since #96 it offers the empty set, so the
    /// blocked guard is a backstop for a stray offered action. The
    /// boundary rejects both, so the card offers them disabled instead
    /// of as buttons that can only fail.
    public func isSubmittable(_ action: Components.Schemas.Action) -> Bool {
        guard snapshot?.item._type != .blocked else { return false }
        if action == .discuss, conversation?.conversation.status == .awaiting_agent {
            return false
        }
        return ActionOutcome.of(action) != .pending
    }

    /// Consequential actions are enabled only when the current state has
    /// validated, the item is still open, no submission is in flight, and
    /// no earlier command's outcome is unknown: an in-flight command can
    /// still commit after any refetch, so a pending ledger entry blocks
    /// every new command until it settles.
    public var actionsEnabled: Bool {
        guard validation == .validated, let snapshot else { return false }
        // A validation certifies one sync epoch's snapshot. If the cache
        // was evicted for a new epoch since (its generation advanced),
        // the rendered row was repopulated by a bootstrap this card never
        // validated, so it must not enable actions until it revalidates
        // (plan §5.14 cache eviction on epoch change; issue #162).
        guard store.cacheGeneration == validatedCacheGeneration else { return false }
        guard snapshot.item.status == .open else { return false }
        if snapshot.item._type == .run_proposal {
            guard let proposalFacts, proposalFactsMatch(snapshot, proposalFacts) else { return false }
        }
        guard !store.isNavigationReserved(itemID: itemID) else { return false }
        guard pendingCommand == nil else { return false }
        // A definitive negative sync signal overrides a point-in-time
        // validation (plan §5.14): while the daemon is unreachable or
        // the credential is rejected, the cached view is read-only
        // however recently this card validated. Unvalidated carries no
        // signal either way; the per-item validation above decides.
        switch store.freshness {
        case .unreachable, .syncFailing, .unauthenticated: return false
        case .unvalidated, .fresh: break
        }
        switch phase {
        case .idle, .superseded: return true
        case .submitting, .applied: return false
        }
    }

    /// Certifies current state as validated and stamps the cache
    /// generation it certified against, so a later epoch eviction (which
    /// bumps that generation) invalidates it even after a bootstrap
    /// repopulates the row (issue #162). Every certify site routes
    /// through here so none can leave the stamp behind.
    private func markValidated() {
        validation = .validated
        validatedCacheGeneration = store.cacheGeneration
    }

    /// The shared message when a certify site cannot render current
    /// state: a cached higher `entity_version` from a dead pre-restore
    /// epoch shadows the reset authoritative snapshot (issue #162). The
    /// heartbeat's epoch eviction and the card's revalidation clear it.
    private static let shadowedByStaleCache =
        "current state is behind a cached snapshot; awaiting resync"

    /// The message when a daemon restore lands mid-submit: a committed
    /// result may have been rolled back, so it is settled as ambiguous
    /// (retry preserved) rather than shown as applied (issue #162).
    private static let restoredBeforeConfirmed =
        "the daemon restored before this result was confirmed"

    /// The message when the pending-command ledger could not be durably
    /// recorded, so the command was not sent: sending an unpersisted
    /// command_id risks losing the lost-response retry across a relaunch
    /// (issue #163). Failing closed keeps the item decidable once the
    /// device can persist again.
    private static let ledgerPersistFailed =
        "the decision could not be saved on this device and was not submitted"

    /// Refetches the item's canonical state and swaps it into the store,
    /// so the card can never expose an action against a state it hasn't
    /// seen (plan §5.14 sync test 9: no stale action on a resolved item).
    /// The item's current decision surface digest, from the fetched action
    /// surface or the cached item. Every comprehension event carries it.
    private var itemDecisionSurfaceDigest: String? {
        actionSurface?.item_decision_surface_digest
            ?? store.snapshotsByID[itemID]?.item.decision_surface.digest
    }

    /// Records card_opened once, the moment the decision card appears, using
    /// the item's cached decision-surface digest. Emitted before validation and
    /// the action-surface fetch (`refreshActionSurface`) so the reported
    /// open-to-decision duration includes their latency and a fast
    /// resolve-and-leave still records the open (plan §8, §9; issue #924).
    public func emitCardOpened() {
        guard !cardOpenedEmitted,
            let digest = store.snapshotsByID[itemID]?.item.decision_surface.digest
        else { return }
        cardOpenedEmitted = true
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: itemID, itemDecisionSurfaceDigest: digest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        Task { await store.drainComprehensionEvents() }
    }

    /// Fetches the device's action surface for this item (best-effort). Runs
    /// after card_opened so the open event never waits on the network. Called
    /// when the decision card appears and whenever the cache is evicted.
    public func refreshActionSurface() async {
        // Refetch when the cached surface no longer matches the item's current
        // decision-surface digest, or when a sync-epoch eviction (a daemon
        // restore) invalidated the epoch it was derived under. A card left open
        // across either change (`revalidationID` reruns this) would otherwise
        // filter actions through the stale surface and submit its superseded
        // digest, which the daemon rejects as stale (plan §8; issue #162).
        let currentItemDigest = store.snapshotsByID[itemID]?.item.decision_surface.digest
        let surfaceIsStale =
            actionSurface.map {
                $0.item_decision_surface_digest != currentItemDigest
                    || actionSurfaceCacheGeneration != store.cacheGeneration
            } ?? true
        guard surfaceIsStale else { return }
        // Capture the request generation and the digest/epoch this fetch is
        // for. The response is installed only if this is still the newest fetch
        // and the item's decision surface and cache epoch have not changed
        // since: an overlapping or cancelled fetch whose response lands out of
        // order must not install a surface the stale check would then trust and
        // stamp onto later commands (which the daemon rejects as stale).
        actionSurfaceRequestGeneration += 1
        let generation = actionSurfaceRequestGeneration
        let expectedItemDigest = currentItemDigest
        let expectedCacheGeneration = store.cacheGeneration
        // Drop the stale surface before awaiting its replacement: once
        // validation enables the card, a submit that races this refetch must
        // not carry the superseded digest. With no surface the card falls back
        // to the local filter and a digest-free command until the refetch lands.
        actionSurface = nil
        let fetched = try? await store.client.getActionSurface(
            path: .init(item_id: itemID)
        ).ok.body.json
        guard generation == actionSurfaceRequestGeneration,
            store.cacheGeneration == expectedCacheGeneration,
            store.snapshotsByID[itemID]?.item.decision_surface.digest == expectedItemDigest
        else { return }
        actionSurface = fetched
        actionSurfaceCacheGeneration = store.cacheGeneration
    }

    /// Emits action_taken, and recommendation_override when the chosen action
    /// differs from the item's recommendation, once per accepted command. The
    /// caller passes the surface the command was built against, not the model's
    /// current `actionSurface`: a shared sync can replace or clear the latter
    /// during the submit await, and the daemon rejects an action event whose
    /// digest does not match the command's stamped evidence (plan §8). Emits
    /// only when a surface backed the command.
    private func emitDecisionEvents(
        for record: Components.Schemas.CommandRecord,
        surface: Components.Schemas.DecisionActionSurface?
    ) async {
        guard let surface,
            actionEventsEmittedCommandID != record.command_id
        else { return }
        actionEventsEmittedCommandID = record.command_id
        store.enqueueComprehensionEvent(
            kind: .action_taken, itemID: itemID,
            itemDecisionSurfaceDigest: surface.item_decision_surface_digest,
            decisionActionSurfaceDigest: surface.digest, commandID: record.command_id)
        let recommended = store.snapshotsByID[itemID]?.item.recommendation?.value1.action
        if let recommended, recommended != record.action {
            store.enqueueComprehensionEvent(
                kind: .recommendation_override, itemID: itemID,
                itemDecisionSurfaceDigest: surface.item_decision_surface_digest,
                decisionActionSurfaceDigest: surface.digest, commandID: record.command_id)
        }
        await store.drainComprehensionEvents()
    }

    /// Emits drill_down_opened once when the card's evidence drill-down displays.
    public func emitDrillDownOpened() {
        emitViewEvent(&drillDownEmitted, kind: .drill_down_opened)
    }

    /// Emits details_opened_before_acting once when technical details reveal
    /// while no command is in flight.
    public func emitDetailsOpenedBeforeActing() {
        guard phase == .idle else { return }
        emitViewEvent(&detailsRevealEmitted, kind: .details_opened_before_acting)
    }

    /// Emits not_decidable_here_shown once when the card offers this device no
    /// action for the item.
    public func emitNotDecidableHereShown() {
        emitViewEvent(&notDecidableEmitted, kind: .not_decidable_here_shown)
    }

    private func emitViewEvent(
        _ emitted: inout Bool, kind: Components.Schemas.ComprehensionEventKind
    ) {
        guard !emitted, let digest = itemDecisionSurfaceDigest else { return }
        emitted = true
        store.enqueueComprehensionEvent(
            kind: kind, itemID: itemID, itemDecisionSurfaceDigest: digest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        Task { await store.drainComprehensionEvents() }
    }

    public func validate() async {
        validationGeneration += 1
        let generation = validationGeneration
        validation = .pending
        var reconciledSnoozeCommandID: String?
        if let entry = store.pendingCommandsByItemID[itemID],
            entry.command.payload.action == .snooze,
            entry.state == .unresolved
        {
            reconciledSnoozeCommandID = entry.command.command_id
            store.setPendingCommandState(
                itemID: itemID,
                commandID: entry.command.command_id,
                state: .inFlight)
        }
        defer {
            if let reconciledSnoozeCommandID,
                let entry = store.pendingCommandsByItemID[itemID],
                entry.command.command_id == reconciledSnoozeCommandID,
                entry.state == .inFlight
            {
                // Every non-definitive exit gives the durable command back
                // to Retry. This includes cancellation, a superseding
                // validation, epoch churn, and the bounded loop exhausting
                // its reads.
                store.setPendingCommandState(
                    itemID: itemID,
                    commandID: reconciledSnoozeCommandID,
                    state: .unresolved)
                if appliedRecord?.command_id == reconciledSnoozeCommandID {
                    appliedRecord = nil
                }
                phase = .idle
                if submissionError == nil {
                    submissionError =
                        "the snooze was recorded but current state could not be confirmed"
                }
            }
        }
        do {
            // Certify only a snapshot that is actually current. Two
            // hazards, both closed by a bounded re-fetch (#162):
            //   - apply refuses a snapshot a cached higher entity_version
            //     outranks. Within an epoch the daemon is monotonic, so
            //     that is an out-of-order read the daemon's next response
            //     supersedes; a restore's reset instead stays below the
            //     dead pre-restore row and never certifies.
            //   - an epoch eviction can land during the fetch's await (all
            //     @MainActor, so heartbeat() runs while this is suspended).
            //     The response is then from a possibly dead epoch, so the
            //     generation captured before the fetch no longer matches;
            //     drop it and re-fetch against the current epoch rather
            //     than applying or certifying it.
            for _ in 0..<2 {
                let generationBefore = store.cacheGeneration
                if let commandID = reconciledSnoozeCommandID,
                    let command = pendingCommand,
                    command.command_id == commandID
                {
                    // Replay precedes the certifying read. A resend may be
                    // the attempt that first commits the snooze, so a
                    // snapshot fetched before it cannot prove the result.
                    guard await confirmPendingSnooze(command, since: generationBefore)
                    else { return }
                    guard generation == validationGeneration else { return }
                    guard store.cacheGeneration == generationBefore else { continue }
                }
                let output = try await store.client.getAttentionItem(
                    path: .init(item_id: itemID))
                guard generation == validationGeneration else { return }
                guard store.cacheGeneration == generationBefore else { continue }
                if case .notFound = output, appliedRecord?.action == .snooze {
                    if let commandID = reconciledSnoozeCommandID {
                        store.clearPendingCommand(itemID: itemID, commandID: commandID)
                    } else if appliedRecord?.command_id != concludedCommandID {
                        validation = .failed(
                            "the snooze could not be reconfirmed against current daemon state")
                        return
                    }
                    store.removeSnapshot(
                        itemID: itemID,
                        atLeastEntityVersion: snapshot?.entity_version ?? 0)
                    proposalFacts = nil
                    markValidated()
                    emitConclusionIfVerified(resultingStatus: nil)
                    return
                }
                let current = try output.ok.body.json
                if let commandID = reconciledSnoozeCommandID {
                    // A visible state fetched after the replay proves the
                    // snooze is no longer active. Settle its durable slot,
                    // but clear the record so it cannot claim a later
                    // reopen or another operator's resolution.
                    store.clearPendingCommand(itemID: itemID, commandID: commandID)
                    if appliedRecord?.command_id == commandID {
                        appliedRecord = nil
                    }
                }
                if store.apply(current) {
                    var confirmed = current
                    if let conversationID = current.item.conversation_id {
                        guard
                            let pair = try await fetchStableConversationPair(
                                item: current,
                                conversationID: conversationID,
                                since: generationBefore)
                        else { continue }
                        guard generation == validationGeneration,
                            store.cacheGeneration == generationBefore,
                            store.apply(pair.item),
                            store.apply(pair.conversation)
                        else { continue }
                        confirmed = pair.item
                    }
                    if appliedRecord?.action == .snooze {
                        // An active snooze is authoritative absence (404).
                        // Any visible proposal, whether reopened or already
                        // decided after the client missed that interval,
                        // proves this local snooze no longer owns its state.
                        appliedRecord = nil
                    }
                    if current.item._type == .run_proposal {
                        let facts = try await store.client.getRunProposalFacts(
                            path: .init(item_id: itemID)
                        ).ok.body.json
                        guard generation == validationGeneration,
                            store.cacheGeneration == generationBefore,
                            proposalFactsMatch(confirmed, facts)
                        else { continue }
                        proposalFacts = facts
                    } else {
                        proposalFacts = nil
                    }
                    markValidated()
                    // Phase converges with canonical state: applied sticks
                    // only while the item is closed. A record-only decision
                    // whose post-commit refetch failed earlier must not
                    // strand a still-open item once a later revalidation
                    // succeeds.
                    if phase == .applied, snapshot?.item.status == .open {
                        phase = .idle
                    }
                    emitConclusionIfVerified(resultingStatus: confirmed.item.status)
                    return
                }
                // Refused within the epoch: the loop re-fetches to converge.
            }
            validation = .failed(Self.shadowedByStaleCache)
        } catch {
            guard generation == validationGeneration else { return }
            // SwiftUI cancels this view-owned task when a resolved item
            // leaves the open inbox. URLSessionTransport wraps that
            // cancellation in ClientError, so recognize the task flag as
            // well as a bare CancellationError. Cancellation is lifecycle,
            // not evidence that the daemon failed; keep validation pending
            // (and actions disabled) until a later task certifies state.
            if error is CancellationError || Task.isCancelled { return }
            proposalFacts = nil
            validation = .failed(String(describing: error))
        }
    }

    private func proposalFactsMatch(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        _ facts: Components.Schemas.RunProposalFactsSnapshot
    ) -> Bool {
        facts.as_of_revision == snapshot.as_of_revision
            && facts.entity_version == snapshot.entity_version
            && facts.item_version == snapshot.item.item_version
            && snapshot.item.artifact_digests == [facts.proposal_digest]
    }

    public func submitRunProposalRevision(
        _ revision: Components.Schemas.RunProposalRevisionInput
    ) async {
        await submit(.start_with_changes, revision: revision)
    }

    public func snooze(until: Date) async {
        await submit(.snooze, snoozeUntil: until)
    }

    @discardableResult
    public func submitDiscuss(message: String) async -> Bool {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            submissionError = "enter a message before sending"
            return false
        }
        let generationBefore = submissionClaimGeneration
        await submit(.discuss, message: trimmed)
        return submissionClaimGeneration != generationBefore
    }

    @discardableResult
    public func submitRequestChanges(message: String) async -> Bool {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            submissionError = "describe the requested changes before sending"
            return false
        }
        guard trimmed.lengthOfBytes(using: .utf8) <= 8192 else {
            submissionError = "requested changes must be 8 KiB or less"
            return false
        }
        let generationBefore = submissionClaimGeneration
        await submit(.request_changes, message: trimmed)
        return submissionClaimGeneration != generationBefore
    }

    @discardableResult
    /// Submits an answer. `answerRoute` is required exactly for answer_and_retry
    /// on an implementation-stage question (AgentQuestionPresentation.answerRoute);
    /// the daemon refuses it anywhere else.
    public func submitAnswer(
        _ action: Components.Schemas.Action, message: String,
        answerRoute: Components.Schemas.AnswerRoute? = nil
    ) async -> Bool {
        guard action == .answer_and_retry || action == .answer_without_retry else { return false }
        return await submitMessageAction(
            action, message: message, emptyError: "enter an answer before sending",
            answerRoute: answerRoute)
    }

    @discardableResult
    public func submitReturnToAgent(message: String) async -> Bool {
        await submitMessageAction(
            .return_to_agent, message: message,
            emptyError: "describe what the agent should change before returning the work")
    }

    private func submitMessageAction(
        _ action: Components.Schemas.Action, message: String, emptyError: String,
        answerRoute: Components.Schemas.AnswerRoute? = nil
    ) async -> Bool {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            submissionError = emptyError
            return false
        }
        guard trimmed.lengthOfBytes(using: .utf8) <= 8192 else {
            submissionError = "the message must be 8 KiB or less"
            return false
        }
        let generationBefore = submissionClaimGeneration
        await submit(action, message: trimmed, answerRoute: answerRoute)
        return submissionClaimGeneration != generationBefore
    }

    public func submit(_ action: Components.Schemas.Action) async {
        await submit(
            action, revision: nil, snoozeUntil: nil, alternativeChoices: nil,
            reviewedSnapshot: nil)
    }

    public func submitCapabilityRetry(
        manifestDigest: String,
        reviewedSnapshot: Components.Schemas.AttentionItemSnapshot
    ) async {
        await submit(
            .retry_with_capabilities, capabilityManifestDigest: manifestDigest,
            revision: nil, snoozeUntil: nil, alternativeChoices: nil,
            reviewedSnapshot: reviewedSnapshot)
    }

    func submitConfirmed(
        _ action: Components.Schemas.Action,
        reviewedSnapshot: Components.Schemas.AttentionItemSnapshot
    ) async {
        await submit(
            action, revision: nil, snoozeUntil: nil, alternativeChoices: nil,
            reviewedSnapshot: reviewedSnapshot)
    }

    public func submitFindingAlternatives(
        _ choices: [Components.Schemas.AlternativeChoice]
    ) async {
        await submit(
            .choose_alternative_route,
            revision: nil,
            snoozeUntil: nil,
            alternativeChoices: choices
        )
    }

    private func submit(
        _ action: Components.Schemas.Action,
        capabilityManifestDigest: String? = nil,
        revision: Components.Schemas.RunProposalRevisionInput? = nil,
        snoozeUntil: Date? = nil,
        alternativeChoices: [Components.Schemas.AlternativeChoice]? = nil,
        message: String? = nil,
        answerRoute: Components.Schemas.AnswerRoute? = nil,
        reviewedSnapshot: Components.Schemas.AttentionItemSnapshot? = nil
    ) async {
        guard actionsEnabled, isSubmittable(action), let snapshot else { return }
        if let reviewedSnapshot {
            guard Self.hasSameDecisionBindings(reviewedSnapshot, snapshot) else { return }
        }
        guard (action == .start_with_changes) == (revision != nil),
            (action == .retry_with_capabilities) == (capabilityManifestDigest != nil),
            (action == .snooze) == (snoozeUntil != nil),
            (action == .choose_alternative_route) == (alternativeChoices != nil),
            ([
                .discuss, .request_changes, .answer_and_retry, .answer_without_retry,
                .return_to_agent,
            ] as Set).contains(action) == (message != nil)
        else { return }
        let urlToOpen: URL?
        if action == .open_pr {
            guard let reference = snapshot.item.pr_reference?.value1,
                let url = Self.pullRequestURL(for: reference)
            else {
                submissionError = "the pull request link is unavailable"
                return
            }
            urlToOpen = url
        } else {
            urlToOpen = nil
        }
        // Snapshot the surface the command is built against. A shared sync can
        // replace or clear `actionSurface` while submitCommand is suspended, so
        // the action telemetry must emit from this immutable value, not the
        // model's current surface, to match the stamped evidence (plan §8).
        let submittedSurface = actionSurface
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString,
            device_id: store.device.deviceID,
            expected_entity_version: snapshot.entity_version,
            // The authoritative bindings for a decision command are the
            // payload's item_version, pr_head_sha, and artifact_digests;
            // the named-bindings map stays empty here per the contract.
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: itemID,
                action: action,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests,
                message: message,
                capability_manifest_digest: capabilityManifestDigest.map {
                    .init(value1: $0)
                },
                answer_route: answerRoute.map { .init(value1: $0) },
                run_proposal_revision: revision.map {
                    .init(value1: $0)
                },
                snooze_until: snoozeUntil,
                alternative_choices: alternativeChoices,
                decision_action_surface_digest: submittedSurface?.digest
            )
        )
        if let urlToOpen {
            // Opening suspends on iOS. Coordinate every model through a
            // shared, process-local reservation, but do not make the command
            // replayable until UIKit confirms navigation: a crash or failed
            // open must not later record engagement that never happened.
            guard store.reserveNavigation(itemID: itemID) else { return }
            let opened = await openURL(urlToOpen)
            store.releaseNavigation(itemID: itemID)
            guard opened else {
                submissionError = "the pull request could not be opened"
                return
            }
        }
        // The command claims the item's in-flight slot and durably records
        // itself before the first byte leaves: a card recreated mid-flight
        // sees the pending entry and cannot mint a second command, and a
        // relaunch after a lost response still has the command_id to replay
        // (#163). Only a definitive outcome below releases the slot.
        switch store.registerPendingCommand(command) {
        case .registered:
            submissionClaimGeneration += 1
        case .slotOccupied:
            // Another command already holds the item; nothing to send.
            return
        case .notPersisted:
            // The ledger write failed: sending now would risk losing the
            // reusable command_id on relaunch, so fail closed and surface
            // it rather than treat it as disposable-cache loss (#163).
            submissionError = Self.ledgerPersistFailed
            return
        }
        submissionError = nil
        // A new submission supersedes the previously displayed record; a
        // stale one would also mask the lost-response retry affordance.
        appliedRecord = nil
        phase = .submitting(action)
        // If an epoch eviction lands while the command is in flight, the
        // conflict replacement below is from a possibly dead epoch; the
        // generation captured here gates certifying it (#162).
        let generationBefore = store.cacheGeneration
        do {
            let output = try await store.client.submitCommand(body: .json(command))
            switch output {
            case .ok(let ok):
                guard store.cacheGeneration == generationBefore else {
                    // Eviction during submitCommand: the 200 itself is from
                    // a possibly rolled-back pre-restore epoch. Keep the
                    // ledger slot (its retry affordance is what
                    // discardSnapshots preserves) and settle as ambiguous
                    // instead of clearing it as applied (#162).
                    await settleAmbiguousOutcome(
                        command, message: Self.restoredBeforeConfirmed)
                    return
                }
                let result = try ok.body.json
                guard Self.commandResultIsValid(result, for: command) else {
                    // A syntactically decoded response is still untrusted:
                    // only the submitted command's exact durable record and
                    // a positive server cursor may settle its ledger slot.
                    await settleAmbiguousOutcome(
                        command, message: "the daemon returned an invalid command result")
                    return
                }
                // Emit the §8 action telemetry from the validated result before
                // the read-your-write refetch. A transient GET failure, the
                // snooze-404 branch, or an eviction during the refetch all
                // settle this accepted command without reaching the post-refetch
                // path, so emitting here keeps action_taken and
                // recommendation_override from being dropped. The events are
                // idempotent by id, and the daemon re-validates each against the
                // recorded command, so an emit for a command a later restore
                // rolls back is harmlessly poison-dropped.
                await emitDecisionEvents(for: result.record, surface: submittedSurface)
                // Read-your-write BEFORE settling. Not every action resolves
                // its item (plan §4: viewing a PR is navigation, acknowledge
                // means seen, never resolved), so read-your-write is a
                // canonical refetch, never a local resolve — and settling
                // (record + slot release) only after it confirms the
                // generation means a restore that lands during the refetch
                // is handled as ambiguous, never shown as a false "applied"
                // with the retry slot already dropped (#162).
                let generationBeforeRefetch = store.cacheGeneration
                let refetched: Components.Schemas.AttentionItemSnapshot
                do {
                    let output = try await store.client.getAttentionItem(
                        path: .init(item_id: itemID))
                    if action == .snooze, case .notFound = output {
                        guard store.cacheGeneration == generationBeforeRefetch else {
                            await settleAmbiguousOutcome(
                                command, message: Self.restoredBeforeConfirmed)
                            return
                        }
                        // Absence alone is ambiguous across a daemon
                        // restore: the restored frontier may predate both
                        // this command and its proposal. Verbatim replay is
                        // the durable proof that the 404 means an active
                        // snooze rather than a rolled-back write.
                        guard
                            await confirmPendingSnooze(
                                command, since: generationBeforeRefetch)
                        else { return }
                        store.clearPendingCommand(
                            itemID: itemID, commandID: command.command_id)
                        store.removeSnapshot(
                            itemID: itemID, atLeastEntityVersion: snapshot.entity_version)
                        proposalFacts = nil
                        phase = .applied
                        emitConclusionIfVerified(resultingStatus: nil)
                        return
                    }
                    refetched = try output.ok.body.json
                } catch {
                    guard store.cacheGeneration == generationBeforeRefetch else {
                        // Evicted during a failed refetch: the commit may be
                        // rolled back, so keep the slot and settle ambiguous.
                        await settleAmbiguousOutcome(
                            command, message: Self.restoredBeforeConfirmed)
                        return
                    }
                    // The command committed but current state is unknown;
                    // settle the record and fail closed until revalidation.
                    store.revisionObserver?(result.revision)
                    if action == .snooze {
                        // A snooze is not settled until authoritative
                        // absence and a verbatim replay agree. Keep its
                        // durable command available across this unknown
                        // refetch so a later validation cannot mistake a
                        // post-restore 404 for successful invisibility.
                        appliedRecord = nil
                        store.setPendingCommandState(
                            itemID: itemID,
                            commandID: command.command_id,
                            state: .unresolved)
                        phase = .idle
                        submissionError =
                            "the snooze was recorded but current state could not be confirmed"
                    } else {
                        appliedRecord = result.record
                        store.clearPendingCommand(
                            itemID: itemID, commandID: command.command_id)
                        phase = .applied
                    }
                    validation = .failed(String(describing: error))
                    return
                }
                guard store.cacheGeneration == generationBeforeRefetch else {
                    // Evicted during the refetch: the committed result may
                    // be rolled back by the restore, so keep the slot and
                    // settle ambiguous rather than clearing it and showing a
                    // false applied (#162).
                    await settleAmbiguousOutcome(
                        command, message: Self.restoredBeforeConfirmed)
                    return
                }
                store.revisionObserver?(result.revision)
                appliedRecord = result.record
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                guard store.apply(refetched) else {
                    // A higher rendered version refuses the refetch within
                    // the epoch; revalidate to converge on it (#162).
                    phase = .applied
                    await validate()
                    return
                }
                if ActionOutcome.of(action) == .discusses {
                    guard let conversationID = refetched.item.conversation_id else {
                        phase = .idle
                        await validate()
                        return
                    }
                    do {
                        guard
                            let pair = try await fetchStableConversationPair(
                                item: refetched,
                                conversationID: conversationID,
                                since: generationBeforeRefetch)
                        else {
                            phase = .idle
                            await validate()
                            return
                        }
                        guard store.cacheGeneration == generationBeforeRefetch,
                            store.apply(pair.item),
                            store.apply(pair.conversation)
                        else {
                            phase = .idle
                            await validate()
                            return
                        }
                    } catch {
                        phase = .idle
                        await validate()
                        return
                    }
                }
                phase = refetched.item.status == .open ? .idle : .applied
                emitConclusionIfVerified(resultingStatus: refetched.item.status)
            case .conflict(let conflict):
                // Staleness and closure share this shape (the recorded #65
                // decision): the replacement is the canonical state, and
                // its status gates whether deciding again is possible.
                let rejection = try conflict.body.json
                // The 409 proves this command never committed, so release
                // the slot regardless of whether the replacement rendered.
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                guard store.cacheGeneration == generationBefore,
                    store.apply(rejection.replacement_item)
                else {
                    // Either an epoch eviction landed mid-submit (the
                    // replacement may be dead-epoch) or a higher rendered
                    // version refuses it (#162). Revalidate against the
                    // current epoch rather than certifying it.
                    phase = .idle
                    await validate()
                    return
                }
                if let conversationID = rejection.replacement_item.item.conversation_id {
                    do {
                        guard
                            let pair = try await fetchStableConversationPair(
                                item: rejection.replacement_item,
                                conversationID: conversationID,
                                since: generationBefore)
                        else {
                            phase = .idle
                            await validate()
                            return
                        }
                        guard store.cacheGeneration == generationBefore,
                            store.apply(pair.item),
                            store.apply(pair.conversation)
                        else {
                            phase = .idle
                            await validate()
                            return
                        }
                        let discussionIsAwaiting: Bool
                        if case .discusses = ActionOutcome.of(action) {
                            discussionIsAwaiting =
                                pair.conversation.conversation.status == .awaiting_agent
                        } else {
                            discussionIsAwaiting = false
                        }
                        if discussionIsAwaiting {
                            phase = .idle
                            submissionError = "the agent is still replying to the last message"
                        } else {
                            phase = .superseded
                        }
                        markValidated()
                    } catch {
                        phase = .idle
                        submissionError =
                            "the item changed, but the discussion thread could not be refreshed"
                        validation = .failed(String(describing: error))
                    }
                } else {
                    phase = .superseded
                    markValidated()
                }
            case .undocumented(let statusCode, _):
                if statusCode == 401 {
                    // The credential gate rejected this first request
                    // before any acceptance, so the fresh command was
                    // definitively not recorded (test 15); what failed is
                    // the device's credential, so it surfaces as device
                    // state, not a card error to retry through.
                    store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                    phase = .idle
                    store.freshness = .unauthenticated
                    submissionError =
                        "the daemon no longer accepts this device's credential; the decision was not submitted"
                } else if (400..<500).contains(statusCode) {
                    // An authoritative daemon rejection (misuse, unknown
                    // item): the command was definitively not recorded.
                    store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                    phase = .idle
                    submissionError = "the daemon rejected the command (status \(statusCode))"
                    await validate()
                } else {
                    // A transient server failure (5xx) proves nothing: the
                    // command may have committed with the response path
                    // failing, so its ledger slot stays claimed.
                    await settleAmbiguousOutcome(
                        command, message: "the daemon answered \(statusCode)")
                }
            }
        } catch {
            await settleAmbiguousOutcome(command, message: String(describing: error))
        }
    }

    /// Reads the bound thread between two views of its item. Matching item
    /// resource versions prove the conversation was observed while those
    /// decision bindings were current; an agent completion advances both in
    /// one transaction, so a changed confirming item rejects the torn pair.
    private func fetchStableConversationPair(
        item: Components.Schemas.AttentionItemSnapshot,
        conversationID: String,
        since generationBefore: Int
    ) async throws -> (
        item: Components.Schemas.AttentionItemSnapshot,
        conversation: Components.Schemas.ConversationSnapshot
    )? {
        let conversation = try await store.client.getConversation(
            path: .init(conversation_id: conversationID)
        ).ok.body.json
        guard store.cacheGeneration == generationBefore else { return nil }
        let frontier = try await store.client.getSyncRevision().ok.body.json
        let confirmed = try await store.client.getAttentionItem(
            path: .init(item_id: itemID)
        ).ok.body.json
        if let activeCursors = store.syncCursorsProvider?(),
            activeCursors.syncEpoch != frontier.sync_epoch
        {
            return nil
        }
        store.revisionObserver?(frontier.revision)
        guard store.cacheGeneration == generationBefore,
            Self.hasSameResourceState(item, confirmed)
        else { return nil }
        // The first item may legitimately become stale while the thread is
        // in flight. A post-object server frontier accepts that race while
        // bounding the returned thread before it can reach cache; an active
        // coordinator also rejects a response from a different sync epoch.
        try ConversationContractValidation.validate(
            conversation,
            expectedID: conversationID,
            maximumRevision: frontier.revision)
        return (confirmed, conversation)
    }

    private static func hasSameResourceState(
        _ before: Components.Schemas.AttentionItemSnapshot,
        _ after: Components.Schemas.AttentionItemSnapshot
    ) -> Bool {
        before.entity_version == after.entity_version
            && before.item.status == after.item.status
            && before.item.conversation_id == after.item.conversation_id
            && hasSameDecisionBindings(before, after)
    }

    private static func hasSameDecisionBindings(
        _ reviewed: Components.Schemas.AttentionItemSnapshot,
        _ current: Components.Schemas.AttentionItemSnapshot
    ) -> Bool {
        reviewed.entity_version == current.entity_version
            && reviewed.item.id == current.item.id
            && reviewed.item.item_version == current.item.item_version
            && reviewed.item.pr_head_sha == current.item.pr_head_sha
            && reviewed.item.artifact_digests == current.item.artifact_digests
    }

    /// Re-gates the external result before its record or cursor can become
    /// trusted local state. Generated decoding enforces field types, not the
    /// OpenAPI revision minimum or correlation with the submitted command.
    static func commandResultIsValid(
        _ result: Components.Schemas.CommandResult,
        for command: Components.Schemas.ClientCommand
    ) -> Bool {
        CommandResultTrust.accepts(result, for: command)
    }

    /// Replays a still-durable snooze before either absence or later
    /// visibility may settle it. The replay is the only proof that a 404 did
    /// not come from a restored frontier that predates the command.
    private func confirmPendingSnooze(
        _ command: Components.Schemas.ClientCommand,
        since generationBefore: Int
    ) async -> Bool {
        switch await replayLostResponse(command, since: generationBefore) {
        case .recovered:
            return true
        case .conflicted(let applied, _):
            appliedRecord = nil
            if applied {
                phase = .superseded
                markValidated()
                submissionError = nil
            } else {
                phase = .idle
                validation = .failed(Self.shadowedByStaleCache)
            }
            return false
        case .rejected:
            appliedRecord = nil
            phase = .idle
            submissionError = "the decision was not recorded"
            validation = .failed("the snooze is absent from current daemon state")
            return false
        case .lost:
            appliedRecord = nil
            store.setPendingCommandState(
                itemID: itemID, commandID: command.command_id, state: .unresolved)
            phase = .idle
            submissionError =
                "the response was lost again; the decision may still be recorded"
            validation = .failed("the snooze could not be reconfirmed")
            return false
        case .displaced:
            phase = .idle
            validation = .failed("the snooze changed while it was being reconfirmed")
            return false
        }
    }

    /// Emits only after a recorded command and canonical evidence agree that
    /// the item left the active queue. A visible `.open` item is record-only
    /// (or a proposal whose snooze has already expired), so it never advances.
    private func emitConclusionIfVerified(
        resultingStatus: Components.Schemas.ItemStatus?
    ) {
        guard let record = appliedRecord,
            record.command_id != concludedCommandID
        else { return }
        switch (ActionOutcome.of(record.action), resultingStatus) {
        case (.snoozesProposal, nil):
            break
        case (.records, _), (.discusses, _), (.pending, _), (_, .some(.open)), (_, nil):
            return
        case (_, .some):
            break
        }
        concludedCommandID = record.command_id
        onConclusion(
            DecisionConclusion(
                itemID: itemID,
                actionLabel: AttentionDisplay.label(record.action),
                resultingStatus: resultingStatus,
                at: .now))
    }

    /// A submit failure that proves nothing about commitment (transport
    /// loss, a 5xx): the command's ledger slot, claimed before the send,
    /// stays held, so nothing renders as applied, no new command can be
    /// minted for the item, and the outcome survives navigation.
    /// Revalidation refetches canonical state, and one immediate resend
    /// attempts to settle; if that is ambiguous too, the ledger holds
    /// and the card offers Retry.
    private func settleAmbiguousOutcome(
        _ command: Components.Schemas.ClientCommand, message: String
    ) async {
        // The first attempt has now definitively failed ambiguously: the
        // slot moves to unresolved, which is what offers the retry.
        store.setPendingCommandState(
            itemID: itemID, commandID: command.command_id, state: .unresolved)
        phase = .idle
        submissionError = message
        if command.payload.action == .snooze {
            // Snooze validation performs the one immediate replay before
            // the canonical read that can settle it. The replay may be the
            // attempt that first commits the command.
            await validate()
            return
        }
        await validate()
        let generationBefore = store.cacheGeneration
        switch await replayLostResponse(command, since: generationBefore) {
        case .recovered:
            // Settled: converge the snapshot and phase on canonical state.
            await validate()
        case .rejected:
            await validate()
        case .conflicted(let applied, let awaitingAgent):
            guard applied else {
                // A higher rendered version refused the replacement (#162):
                // revalidate to converge on the newer read (same epoch) or
                // fail closed (restore), rather than certifying it.
                await validate()
                break
            }
            // Settled by a 409: the applied replacement is canonical and
            // presents exactly as a live conflict would.
            let discussionIsAwaiting =
                awaitingAgent && ActionOutcome.of(command.payload.action) == .discusses
            phase = discussionIsAwaiting ? .idle : .superseded
            markValidated()
            submissionError =
                discussionIsAwaiting ? "the agent is still replying to the last message" : nil
        case .lost, .displaced:
            break
        }
    }

    /// True when a preserved command may hold a recorded result: the
    /// pending ledger holds this item's command in the unresolved state
    /// (an in-flight first attempt may still succeed, so it offers no
    /// retry) and no local record settles that same command. An older
    /// record from a different decision (another card instance's earlier
    /// action) must not hide the newer pending command's affordance.
    /// Resending the identical command is always safe: it replays,
    /// applies at most once, or is rejected authoritatively.
    public var canRetryLostResponse: Bool {
        guard let entry = store.pendingCommandsByItemID[itemID],
            entry.state == .unresolved
        else { return false }
        if case .submitting = phase { return false }
        if let record = appliedRecord, record.command_id == entry.command.command_id {
            return false
        }
        return true
    }

    public func retryLostResponse() async {
        guard canRetryLostResponse, let pending = pendingCommand else { return }
        submissionError = nil
        phase = .submitting(pending.payload.action)
        if pending.payload.action == .snooze {
            // validate() claims the unresolved slot and performs replay
            // before the canonical read that can settle it.
            await validate()
            return
        }
        // The resend is itself in flight; other instances must not offer
        // a concurrent retry while it runs.
        store.setPendingCommandState(
            itemID: itemID, commandID: pending.command_id, state: .inFlight)
        let generationBefore = store.cacheGeneration
        switch await replayLostResponse(pending, since: generationBefore) {
        case .recovered:
            // The stale or unknown snapshot converges on canonical state;
            // validate() also converges the phase, so a recovered
            // record-only action leaves the item open and decidable.
            await validate()
        case .conflicted(let applied, let awaitingAgent):
            guard applied else {
                // A higher rendered version refused the replacement (#162):
                // revalidate to converge on the newer read (same epoch) or
                // fail closed (restore), rather than certifying it.
                phase = .idle
                await validate()
                break
            }
            let discussionIsAwaiting =
                awaitingAgent && ActionOutcome.of(pending.payload.action) == .discusses
            phase = discussionIsAwaiting ? .idle : .superseded
            markValidated()
            submissionError =
                discussionIsAwaiting ? "the agent is still replying to the last message" : nil
        case .rejected:
            phase = .idle
            submissionError = "the decision was not recorded"
            await validate()
        case .lost:
            // Ambiguous again: back to unresolved so the retry stays
            // offered everywhere. Refresh canonical state as well: an
            // undecodable answered error can throw before the generated
            // output reaches the typed conflict case, and must never leave
            // a stale card looking superseded.
            store.setPendingCommandState(
                itemID: itemID, commandID: pending.command_id, state: .unresolved)
            phase = .idle
            submissionError = "the response was lost again; the decision may still be recorded"
            await validate()
        case .displaced:
            // Another flow settled the slot while this retry was in
            // flight; converge on canonical state instead of latching
            // the submitting spinner.
            phase = .idle
            await validate()
        }
    }

    private enum ReplayOutcome {
        case recovered
        /// The resend hit a 409: the command never committed and the item
        /// advanced elsewhere; the applied replacement is canonical and
        /// deserves the same superseded presentation as a live conflict.
        /// `applied` is false when a dead pre-restore row shadowed the
        /// replacement, so the caller must not certify it (#162).
        case conflicted(applied: Bool, awaitingAgent: Bool)
        /// The daemon answered authoritatively without a recorded result:
        /// the original command never committed, so nothing is recoverable.
        case rejected
        /// The resend itself failed in transport; still ambiguous.
        case lost
        /// The pending slot moved to a newer command while this replay
        /// was in flight: the completion is stale and must not write
        /// model state that belongs to the newer submission.
        case displaced
    }

    @discardableResult
    private func replayLostResponse(
        _ command: Components.Schemas.ClientCommand, since generationBefore: Int
    ) async -> ReplayOutcome {
        do {
            let output = try await store.client.submitCommand(body: .json(command))
            // A completion is stale once the slot moved to a newer
            // command: canonical store data may still apply below, but
            // no model state belonging to the newer submission is
            // written, and only the slot's own command may clear it.
            let ownsSlot = pendingCommand?.command_id == command.command_id
            switch output {
            case .ok(let ok):
                guard ownsSlot else { return .displaced }
                guard store.cacheGeneration == generationBefore else {
                    // The 200 resumed after an epoch eviction: a pre-restore
                    // commit is ambiguous post-restore, so keep the slot
                    // unresolved (retry stays offered) rather than clearing
                    // it as recovered (#162).
                    return .lost
                }
                let result = try ok.body.json
                guard Self.commandResultIsValid(result, for: command) else {
                    return .lost
                }
                store.revisionObserver?(result.revision)
                appliedRecord = result.record
                submissionError = nil
                phase = .applied
                // The original submit lost its response before it could emit, so
                // an in-session replay is the first confirmation of this accepted
                // command: emit its §8 action telemetry here too (idempotent by
                // id). Emit only from a surface whose digest still matches the
                // replayed command's stamped evidence; a cross-relaunch replay
                // runs on a recreated model whose action surface is gone (or a
                // sync may have replaced it), so a non-matching surface makes
                // the surface-referenced events an accepted best-effort loss
                // rather than an emit the daemon rejects as unbacked.
                let replaySurface =
                    actionSurface?.digest == command.payload.decision_action_surface_digest
                    ? actionSurface : nil
                await emitDecisionEvents(for: result.record, surface: replaySurface)
                if command.payload.action != .snooze {
                    store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                }
                return .recovered
            case .conflict(let conflict):
                // A recorded command replays as 200 before any state
                // check, so an authoritative non-replay answer proves
                // the command never committed; the replacement it
                // carries is canonical state either way.
                var isCurrent = false
                var awaitingAgent = false
                if let rejection = try? conflict.body.json {
                    // An epoch eviction during the replay makes the
                    // replacement possibly dead-epoch: drop it rather than
                    // apply it, so the caller revalidates (#162).
                    isCurrent =
                        store.cacheGeneration == generationBefore
                        && store.apply(rejection.replacement_item)
                    if isCurrent,
                        let conversationID = rejection.replacement_item.item.conversation_id
                    {
                        do {
                            if let pair = try await fetchStableConversationPair(
                                item: rejection.replacement_item,
                                conversationID: conversationID,
                                since: generationBefore)
                            {
                                isCurrent =
                                    store.apply(pair.item) && store.apply(pair.conversation)
                                awaitingAgent =
                                    isCurrent
                                    && pair.conversation.conversation.status == .awaiting_agent
                            } else {
                                isCurrent = false
                            }
                        } catch {
                            isCurrent = false
                        }
                    }
                }
                guard pendingCommand?.command_id == command.command_id else {
                    return .displaced
                }
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                return .conflicted(applied: isCurrent, awaitingAgent: awaitingAgent)
            case .undocumented(let statusCode, _):
                if statusCode == 401 {
                    // The resend died at the credential gate, which
                    // proves nothing about the original attempt's
                    // commitment: a revoked device's retry may be served
                    // its recorded result or rejected (test 16, the
                    // daemon's choice), so the slot stays held and the
                    // revoked state surfaces instead of a false "not
                    // recorded".
                    store.freshness = .unauthenticated
                    return ownsSlot ? .lost : .displaced
                }
                if (400..<500).contains(statusCode) {
                    guard ownsSlot else { return .displaced }
                    store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                    return .rejected
                }
                // A 5xx on the resend proves nothing; still ambiguous.
                return ownsSlot ? .lost : .displaced
            }
        } catch {
            let ownsSlot = pendingCommand?.command_id == command.command_id
            return ownsSlot ? .lost : .displaced
        }
    }
}
