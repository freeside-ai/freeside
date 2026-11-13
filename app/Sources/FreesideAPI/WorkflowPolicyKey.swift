import Foundation

extension Components.Schemas.WorkflowPolicyKey {
    /// Whether the key takes value (domain.WorkflowPolicyKey.ValidateValue):
    /// a gate "true" or "false", a cap a canonical positive decimal. The
    /// mock daemon refuses any other, and the app sends no other.
    public func accepts(_ value: String) -> Bool {
        switch self {
        case .gates_period_follow_up_filing, .gates_period_issue_comment, .gates_period_source_issue_closure:
            return value == "true" || value == "false"
        case .follow_up_filing_period_max_per_day, .follow_up_filing_period_max_per_run,
            .follow_up_filing_period_max_depth:
            guard let number = Int(value), number > 0 else { return false }
            return String(number) == value
        case .unknown:
            return false
        }
    }
}
