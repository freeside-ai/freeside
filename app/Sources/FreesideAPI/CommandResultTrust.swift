import Foundation

/// Validates that an externally returned command result is the exact durable
/// record for the command the client submitted. Generated decoding enforces
/// field types, but not the OpenAPI revision minimum or request correlation.
public enum CommandResultTrust {
    public static func accepts(
        _ result: Components.Schemas.CommandResult,
        for command: Components.Schemas.ClientCommand
    ) -> Bool {
        let payload = command.payload
        guard result.revision >= 1,
            let expectedMessage = try? recordedMessage(payload)
        else { return false }
        let record = result.record
        return record.command_id == command.command_id
            && record.device_id == command.device_id
            && record.item_id == payload.item_id
            && record.item_version == payload.item_version
            && record.pr_head_sha == payload.pr_head_sha
            && record.artifact_digests == Array(Set(payload.artifact_digests)).sorted()
            && record.action == payload.action
            && record.message == expectedMessage
            && record.attachments == (payload.attachments ?? [])
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
        case .start_with_changes:
            guard let revision = payload.run_proposal_revision?.value1 else {
                return payload.message ?? ""
            }
            let touchesControlPlane = revision.scope.touches_control_plane ? "true" : "false"
            return "{\"intent\":\"\(revision.intent.rawValue)\","
                + "\"expected_cost_units\":\(revision.expected_cost_units),"
                + "\"scope\":{\"component_count\":\(revision.scope.component_count),"
                + "\"declared_path_count\":\(revision.scope.declared_path_count),"
                + "\"touches_control_plane\":\(touchesControlPlane)}}"
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
