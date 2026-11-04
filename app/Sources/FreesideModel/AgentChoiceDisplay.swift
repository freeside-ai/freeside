import FreesideAPI

/// The New Task form's agent choices (plan §5.4, task lines): which lines a
/// writer and reviewer choice send, what each catalog agent reads as, and
/// what picks the agent for a role left unchosen. Pure: the sheet passes the
/// synced catalog and routing policy in.
///
/// A role the operator leaves unchosen gets no line, so the daemon picks its
/// agent at first admission. Today that is the lineup's agent; once the
/// daemon routes by policy (#2393) it may be any agent the routing policy
/// orders for the role.
public enum AgentChoiceDisplay {
    public typealias Role = Components.Schemas.TaskLineRole

    /// The roles one writer choice sets together.
    public static let writerRoles: [Role] = [.specifier, .implementer, .remediator]

    /// The lines a submission carries: a writer sets the three writer roles, a
    /// reviewer sets the reviewer role. With neither it returns nil, never an
    /// empty list, which the daemon rejects.
    public static func taskLines(writer: String?, reviewer: String?) -> [Components.Schemas.TaskLineInput]? {
        typealias Line = Components.Schemas.TaskLineInput
        let writerLines = writer.map { agent in writerRoles.map { Line(role: $0, agent: agent) } } ?? []
        let reviewerLines = reviewer.map { [Line(role: .reviewer, agent: $0)] } ?? []
        let lines = writerLines + reviewerLines
        return lines.isEmpty ? nil : lines
    }

    /// The chosen agents the catalog doesn't carry, writer first. A newer
    /// catalog can drop an agent the operator already chose; the form keeps
    /// the choice and blocks Submit instead of swapping it silently. With no
    /// catalog, no chosen agent is carried.
    public static func missingAgents(
        writer: String?, reviewer: String?, catalog: Components.Schemas.AgentCatalog?
    ) -> [String] {
        let known = Set(catalog?.agents.map(\.name) ?? [])
        var missing: [String] = []
        for agent in [writer, reviewer].compactMap({ $0 }) where !known.contains(agent) && !missing.contains(agent) {
            missing.append(agent)
        }
        return missing
    }

    /// One catalog agent's facts as a choice lists them: model, effort, and
    /// lineage group.
    public static func facts(_ agent: Components.Schemas.AgentCatalogEntry) -> String {
        [agent.model, RunDisplay.label(agent.effort), agent.lineage_group ?? "No lineage group"]
            .joined(separator: " · ")
    }

    /// The default entry's label: what picks the agent when the operator
    /// chooses none for `roles`. The lineup names its agent when it binds one
    /// agent to every role; "or routing policy" follows when the policy lists
    /// one of the roles.
    public static func defaultLabel(
        roles: [Role], policy: Components.Schemas.RoutingPolicy?, catalog: Components.Schemas.AgentCatalog?
    ) -> String {
        let lineupAgents = unique(roles.compactMap { role in catalog?.lineup.first { $0.role == role }?.agent })
        let lineup = lineupAgents.count == 1 ? "lineup \(lineupAgents[0])" : "lineup"
        let policyListsARole = roles.contains { role in policy?.roles.contains { $0.role == role } == true }
        return "Default · \(lineup)" + (policyListsARole ? " or routing policy" : "")
    }

    private static func unique(_ names: [String]) -> [String] {
        var seen = Set<String>()
        return names.filter { seen.insert($0).inserted }
    }

    /// A saved request's lines as one line of text: the writer once when the
    /// three writer lines name one agent, otherwise each role on its own.
    /// Nil when the request carries no lines.
    public static func summary(_ lines: [Components.Schemas.TaskLineInput]?) -> String? {
        guard let lines, !lines.isEmpty else { return nil }
        let byRole = Dictionary(lines.map { ($0.role, $0.agent) }, uniquingKeysWith: { first, _ in first })
        var parts: [String] = []
        let writerAgents = writerRoles.compactMap { byRole[$0] }
        if writerAgents.count == writerRoles.count, Set(writerAgents).count == 1 {
            parts.append("Writer \(writerAgents[0])")
        } else {
            parts += writerRoles.compactMap { role in byRole[role].map { "\(roleTitle(role)) \($0)" } }
        }
        if let reviewer = byRole[.reviewer] { parts.append("Reviewer \(reviewer)") }
        return parts.joined(separator: " · ")
    }

    private static func roleTitle(_ role: Role) -> String {
        switch role {
        case .specifier: "Specifier"
        case .implementer: "Implementer"
        case .remediator: "Remediator"
        case .reviewer: "Reviewer"
        }
    }
}
