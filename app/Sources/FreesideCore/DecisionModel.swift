import FreesideAPI
import Foundation
import Observation

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

    private let store: InboxStore
    /// Overlapping validations resolve by recency: only the newest call
    /// may write the outcome, so a stale late failure cannot clobber a
    /// newer success (or vice versa).
    private var validationGeneration = 0

    public init(store: InboxStore, itemID: String) {
        self.store = store
        self.itemID = itemID
    }

    public var snapshot: Components.Schemas.AttentionItemSnapshot? {
        store.snapshotsByID[itemID]
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
        return ActionOutcome.of(action) != .pending
    }

    /// Consequential actions are enabled only when the current state has
    /// validated, the item is still open, no submission is in flight, and
    /// no earlier command's outcome is unknown: an in-flight command can
    /// still commit after any refetch, so a pending ledger entry blocks
    /// every new command until it settles.
    public var actionsEnabled: Bool {
        guard validation == .validated, let snapshot else { return false }
        guard snapshot.item.status == .open else { return false }
        guard pendingCommand == nil else { return false }
        // A definitive negative sync signal overrides a point-in-time
        // validation (plan §5.14): while the daemon is unreachable or
        // the credential is rejected, the cached view is read-only
        // however recently this card validated. Unvalidated carries no
        // signal either way; the per-item validation above decides.
        switch store.freshness {
        case .unreachable, .unauthenticated: return false
        case .unvalidated, .fresh: break
        }
        switch phase {
        case .idle, .superseded: return true
        case .submitting, .applied: return false
        }
    }

    /// Refetches the item's canonical state and swaps it into the store,
    /// so the card can never expose an action against a state it hasn't
    /// seen (plan §5.14 sync test 9: no stale action on a resolved item).
    public func validate() async {
        validationGeneration += 1
        let generation = validationGeneration
        validation = .pending
        do {
            let output = try await store.client.getAttentionItem(
                path: .init(item_id: itemID))
            let current = try output.ok.body.json
            // Canonical data always applies (the store is version-
            // monotonic); the outcome writes below belong to the newest
            // call only.
            store.apply(current)
            guard generation == validationGeneration else { return }
            validation = .validated
            // Phase converges with canonical state: applied sticks only
            // while the item is closed. A record-only decision whose
            // post-commit refetch failed earlier must not strand a
            // still-open item once a later revalidation succeeds.
            if phase == .applied, snapshot?.item.status == .open {
                phase = .idle
            }
        } catch {
            guard generation == validationGeneration else { return }
            validation = .failed(String(describing: error))
        }
    }

    public func submit(_ action: Components.Schemas.Action) async {
        guard actionsEnabled, isSubmittable(action), let snapshot else { return }
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
                artifact_digests: snapshot.item.artifact_digests
            )
        )
        // The command claims the item's in-flight slot before the first
        // byte leaves: a card recreated mid-flight sees the pending
        // entry and cannot mint a second command; only a definitive
        // outcome below releases the slot.
        guard store.registerPendingCommand(command) else { return }
        submissionError = nil
        // A new submission supersedes the previously displayed record; a
        // stale one would also mask the lost-response retry affordance.
        appliedRecord = nil
        phase = .submitting(action)
        do {
            let output = try await store.client.submitCommand(body: .json(command))
            switch output {
            case .ok(let ok):
                let result = try ok.body.json
                appliedRecord = result.record
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                // The result carries only the record and revision, and not
                // every action resolves its item (plan §4: open_pr is
                // navigation, acknowledge means seen, never resolved), so
                // read-your-write is a canonical refetch, never a local
                // resolve. An item the daemon left open stays decidable.
                do {
                    let refetched = try await store.client.getAttentionItem(
                        path: .init(item_id: itemID)
                    ).ok.body.json
                    store.apply(refetched)
                    phase = refetched.item.status == .open ? .idle : .applied
                } catch {
                    // The command committed but current state is unknown;
                    // fail closed until a revalidation succeeds.
                    phase = .applied
                    validation = .failed(String(describing: error))
                }
            case .conflict(let conflict):
                // Staleness and closure share this shape (the recorded #65
                // decision): the replacement is the canonical state, and
                // its status gates whether deciding again is possible.
                let rejection = try conflict.body.json
                store.apply(rejection.replacement_item)
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                phase = .superseded
                validation = .validated
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
        await validate()
        switch await replayLostResponse(command) {
        case .recovered, .rejected:
            // Settled: converge the snapshot and phase on canonical state.
            await validate()
        case .conflicted:
            // Settled by a 409: the applied replacement is canonical and
            // presents exactly as a live conflict would.
            phase = .superseded
            validation = .validated
            submissionError = nil
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
        // The resend is itself in flight; other instances must not offer
        // a concurrent retry while it runs.
        store.setPendingCommandState(
            itemID: itemID, commandID: pending.command_id, state: .inFlight)
        switch await replayLostResponse(pending) {
        case .recovered:
            // The stale or unknown snapshot converges on canonical state;
            // validate() also converges the phase, so a recovered
            // record-only action leaves the item open and decidable.
            await validate()
        case .conflicted:
            phase = .superseded
            validation = .validated
        case .rejected:
            phase = .idle
            submissionError = "the decision was not recorded"
            await validate()
        case .lost:
            // Ambiguous again: back to unresolved so the retry stays
            // offered everywhere.
            store.setPendingCommandState(
                itemID: itemID, commandID: pending.command_id, state: .unresolved)
            phase = .idle
            submissionError = "the response was lost again; the decision may still be recorded"
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
        case conflicted
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
        _ command: Components.Schemas.ClientCommand
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
                let result = try ok.body.json
                appliedRecord = result.record
                submissionError = nil
                phase = .applied
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                return .recovered
            case .conflict(let conflict):
                // A recorded command replays as 200 before any state
                // check, so an authoritative non-replay answer proves
                // the command never committed; the replacement it
                // carries is canonical state either way.
                if let rejection = try? conflict.body.json {
                    store.apply(rejection.replacement_item)
                }
                guard ownsSlot else { return .displaced }
                store.clearPendingCommand(itemID: itemID, commandID: command.command_id)
                return .conflicted
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
