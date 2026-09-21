import Foundation

/// Validates that an externally returned command result is the exact durable
/// record for the command the client submitted. Generated decoding enforces
/// field types, but not the OpenAPI revision minimum or request correlation.
public enum CommandResultTrust {
    public static func accepts(
        _ result: Components.Schemas.CommandResult,
        for command: Components.Schemas.ClientCommand
    ) -> Bool {
        guard result.revision >= 1 else { return false }
        switch command.payload {
        case .decision(let payload):
            guard case .decision(let record) = result.record,
                let expectedMessage = try? recordedMessage(payload)
            else { return false }
            return record.command_id == command.command_id
                && record.device_id == command.device_id
                && record.item_id == payload.item_id
                && record.item_version == payload.item_version
                && record.pr_head_sha == payload.pr_head_sha
                && record.artifact_digests == Array(Set(payload.artifact_digests)).sorted()
                && record.action == payload.action
                && record.message == expectedMessage
                && record.attachments == (payload.attachments ?? [])
        case .stop_task(let payload):
            guard case .stop_task(let record) = result.record else { return false }
            return record.command_id == command.command_id && record.device_id == command.device_id
                && record.task_id == payload.task_id && record.project_id == payload.project_id
                && record.expected_sync_epoch == payload.expected_sync_epoch
                && record.expected_entity_version == command.expected_entity_version
                && record.cancellation.target.task_id == payload.task_id
                && record.cancellation.target.project_id == payload.project_id
                && record.cancellation.fence_revision <= result.revision
                && record.expected_entity_version < result.revision
                && record.cancellation.sync_epoch == payload.expected_sync_epoch
                && MockContractValidation.cancellationBreach(record.cancellation) == nil
        case .submit_task(let payload):
            // A submit_task result names the created-or-fetched task; its source
            // digest must be the sha256 of exactly the submitted source, and its
            // task and specification-run identities must be present.
            guard case .submit_task(let record) = result.record else { return false }
            return record.command_id == command.command_id
                && record.device_id == command.device_id
                && record.project_id == payload.project_id
                && record.source_digest.value1 == MockContractValidation.sha256Digest(of: payload.source)
                && !record.task_id.isEmpty
                && !record.specification_run_id.isEmpty
        }
    }

    /// The daemon's durable command-message normalization. Keeping the mock
    /// server and the client-side trust gate on this one implementation makes
    /// typed action replay and result correlation use the same byte form.
    static func recordedMessage(
        _ payload: Components.Schemas.DecisionPayload
    ) throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        switch payload.action {
        case .retry_with_capabilities:
            return payload.capability_manifest_digest?.value1 ?? ""
        case .start_with_changes:
            guard let revision = payload.task_proposal_revision?.value1 else {
                return payload.message ?? ""
            }
            let touchesControlPlane = revision.scope.touches_control_plane ? "true" : "false"
            return "{\"intent\":\"\(revision.intent.rawValue)\","
                + "\"expected_cost_units\":\(revision.expected_cost_units),"
                + "\"scope\":{\"component_count\":\(revision.scope.component_count),"
                + "\"declared_path_count\":\(revision.scope.declared_path_count),"
                + "\"touches_control_plane\":\(touchesControlPlane)}}"
        case .approve_with_changes:
            // The daemon stores the flat canonical {resolves} delta the store's
            // revise path decodes, never the kind-keyed wire arm.
            guard let revision = payload.effect_proposal_revision?.value1 else {
                return payload.message ?? ""
            }
            let resolves = revision.source_issue_closure.resolves
            return "{\"resolves\":\(resolves ? "true" : "false")}"
        case .snooze:
            guard let until = payload.snooze_until else { return payload.message ?? "" }
            return try RFC3339DateTranscoder().encode(until)
        case .choose_alternative_route:
            guard let choices = payload.alternative_choices else {
                return payload.message ?? ""
            }
            return String(
                decoding: try encoder.encode(choices.sorted { $0.finding_id < $1.finding_id }),
                as: UTF8.self
            )
        default:
            return payload.message ?? ""
        }
    }
}
