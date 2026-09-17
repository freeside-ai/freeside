import FreesideAPI

// Test-only convenience accessors for the discriminated command unions, so the
// existing decision-command tests read and mutate the decision arm directly.
// The getter traps on a non-decision value, which never occurs in these tests;
// the setter re-wraps, so a test can tweak a field after constructing.
extension Components.Schemas.ClientCommand.payloadPayload {
    var asDecision: Components.Schemas.DecisionPayload {
        get {
            guard case .decision(let payload) = self else {
                preconditionFailure("expected a decision payload")
            }
            return payload
        }
        set { self = .decision(newValue) }
    }
}

extension Components.Schemas.CommandResult.recordPayload {
    var asDecision: Components.Schemas.CommandRecord {
        get {
            guard case .decision(let record) = self else {
                preconditionFailure("expected a decision record")
            }
            return record
        }
        set { self = .decision(newValue) }
    }
}

extension Components.Schemas.CommandRejection {
    var asDecision: Components.Schemas.StaleVersionRejection {
        guard case .StaleVersionRejection(let rejection) = self else {
            preconditionFailure("expected a decision rejection")
        }
        return rejection
    }
}
