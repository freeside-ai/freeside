import Foundation

/// The workflow policy layers the mock daemon holds, and the projection the
/// daemon's bootstrap renders from them (signet renderWorkflowPolicy): each
/// pane key resolves the project's value over the general value over the
/// built-in default.
public struct WorkflowPolicyLayers: Sendable, Hashable {
    /// One layer of one key: general when projectID is nil.
    public struct Layer: Sendable, Hashable {
        public var projectID: String?
        public var key: Components.Schemas.WorkflowPolicyKey

        public init(projectID: String?, key: Components.Schemas.WorkflowPolicyKey) {
            self.projectID = projectID
            self.key = key
        }
    }

    /// The value a layer holds and the revision that set it.
    public struct Entry: Sendable, Hashable {
        public var value: String
        public var setBy: String
        public var setAt: Date

        public init(value: String, setBy: String, setAt: Date) {
            self.value = value
            self.setBy = setBy
            self.setAt = setAt
        }
    }

    /// The newest workflow policy revision, 0 when none was ever set.
    public var revision: Int
    public var entries: [Layer: Entry]

    public init(revision: Int = 0, entries: [Layer: Entry] = [:]) {
        self.revision = revision
        self.entries = entries
    }

    /// The snapshot for the general section and each served project.
    public func snapshot(projects: [String], asOfRevision: Int64) -> Components.Schemas.WorkflowPolicySnapshot {
        .init(
            as_of_revision: asOfRevision, revision: revision,
            general: WorkflowPolicyFixtures.paneKeys.map { setting($0, projectID: nil) },
            projects: projects.map { project in
                .init(
                    project_id: project,
                    settings: WorkflowPolicyFixtures.paneKeys.map { setting($0, projectID: project) })
            })
    }

    private func setting(
        _ key: Components.Schemas.WorkflowPolicyKey, projectID: String?
    ) -> Components.Schemas.WorkflowPolicySetting {
        if let projectID, let entry = entries[.init(projectID: projectID, key: key)] {
            return .init(key: key, value: entry.value, origin: .project, set_by: entry.setBy, set_at: entry.setAt)
        }
        if let entry = entries[.init(projectID: nil, key: key)] {
            return .init(key: key, value: entry.value, origin: .general, set_by: entry.setBy, set_at: entry.setAt)
        }
        return .init(
            key: key, value: WorkflowPolicyFixtures.defaultValue(key), origin: ._default, set_by: nil, set_at: nil)
    }
}

public enum WorkflowPolicyFixtures {
    /// The project the mock daemon serves.
    public static let projectID = "freeside"

    /// The pane's keys in pane order: the gates, then the caps.
    public static let paneKeys: [Components.Schemas.WorkflowPolicyKey] = [
        .gates_period_follow_up_filing, .gates_period_issue_comment, .gates_period_source_issue_closure,
        .follow_up_filing_period_max_per_day, .follow_up_filing_period_max_per_run,
        .follow_up_filing_period_max_depth,
    ]

    /// The built-in default (domain.WorkflowPolicyKey.Default).
    public static func defaultValue(_ key: Components.Schemas.WorkflowPolicyKey) -> String {
        switch key {
        case .gates_period_follow_up_filing, .gates_period_issue_comment, .gates_period_source_issue_closure:
            "false"
        case .follow_up_filing_period_max_per_day: "10"
        case .follow_up_filing_period_max_per_run: "5"
        case .follow_up_filing_period_max_depth: "2"
        case .unknown: ""
        }
    }

    /// The mock's layers: the source-issue closure gate on for every
    /// project, and a lower daily cap for the served project, so the pane
    /// shows each origin.
    public static func defaultLayers() -> WorkflowPolicyLayers {
        .init(
            revision: 2,
            entries: [
                .init(projectID: nil, key: .gates_period_source_issue_closure): .init(
                    value: "true", setBy: "device:\(DeviceFixtures.otherDeviceID)", setAt: date(1_300)),
                .init(projectID: projectID, key: .follow_up_filing_period_max_per_day): .init(
                    value: "3", setBy: "device:\(DeviceFixtures.otherDeviceID)", setAt: date(1_400)),
            ])
    }

    private static func date(_ offset: TimeInterval) -> Date {
        Date(timeIntervalSince1970: 1_786_502_400 + offset)
    }
}
