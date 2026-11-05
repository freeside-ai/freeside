import FreesideAPI

/// The New Task form's agent choices (plan §5.4, task lines): which lines a
/// per-role choice sends, what each catalog agent reads as, and what picks
/// the agent for a role left unchosen. Pure: the sheet passes the synced
/// catalog and routing policy in.
///
/// A role the operator leaves unchosen gets no line, so the daemon picks its
/// agent at first admission. Today that is the lineup's agent; once the
/// daemon routes by policy (#2393) it may be any agent the routing policy
/// orders for the role.
public enum AgentChoiceDisplay {
    public typealias Role = Components.Schemas.TaskLineRole

    /// An alternate-agent offer as a picker names it: the agent, its model,
    /// and the usage pool it draws on, so the operator sees what changes.
    public static func offerLabel(_ offer: Components.Schemas.AlternateAgentOffer) -> String {
        "\(offer.agent) · \(offer.model) · \(offer.pool)"
    }

    /// The roles in the order the form shows them and a submission sends
    /// their lines. The generated `TaskLineRole` declares the same order;
    /// naming it here keeps the line order explicit.
    public static let roleOrder: [Role] = [.specifier, .implementer, .remediator, .reviewer]

    /// The roles that write the change, which the summary reads as one
    /// writer when they name one agent.
    public static let writerRoles: [Role] = [.specifier, .implementer, .remediator]

    /// The lines a submission carries: one per chosen role, in `roleOrder`.
    /// With no choice it returns nil, never an empty list, which the daemon
    /// rejects.
    public static func taskLines(choices: [Role: String]) -> [Components.Schemas.TaskLineInput]? {
        let lines = roleOrder.compactMap { role in
            choices[role].map { Components.Schemas.TaskLineInput(role: role, agent: $0) }
        }
        return lines.isEmpty ? nil : lines
    }

    /// The chosen agents the catalog doesn't carry, in role order, each once.
    /// A newer catalog can drop an agent the operator already chose; the form
    /// keeps the choice and blocks Submit instead of swapping it silently.
    /// With no catalog, no chosen agent is carried.
    public static func missingAgents(
        choices: [Role: String], catalog: Components.Schemas.AgentCatalog?
    ) -> [String] {
        let known = Set(catalog?.agents.map(\.name) ?? [])
        return unique(roleOrder.compactMap { choices[$0] }.filter { !known.contains($0) })
    }

    /// One catalog agent's facts as a choice lists them: model, effort, and
    /// lineage group.
    public static func facts(_ agent: Components.Schemas.AgentCatalogEntry) -> String {
        [agent.model, RunDisplay.label(agent.effort), agent.lineage_group ?? "No lineage group"]
            .joined(separator: " · ")
    }

    /// The default entry's label: what picks the agent when the operator
    /// chooses none for `role`. The lineup names its agent for the role;
    /// "or routing policy" follows when the policy lists the role.
    public static func defaultLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Components.Schemas.AgentCatalog?
    ) -> String {
        let lineup = catalog?.lineup.first { $0.role == role }.map { "lineup \($0.agent)" } ?? "lineup"
        let policyListsTheRole = policy?.roles.contains { $0.role == role } == true
        return "Default · \(lineup)" + (policyListsTheRole ? " or routing policy" : "")
    }

    private static func unique(_ names: [String]) -> [String] {
        var seen = Set<String>()
        return names.filter { seen.insert($0).inserted }
    }

    /// Lines as one sentence of verbs, as the Agents chip and a saved
    /// request read them: `claude-opus writes · codex reviews` when the three
    /// writer lines name one agent, otherwise each writer agent with its
    /// roles' verbs (`claude-opus specifies · codex implements and
    /// remediates`). Nil when there are no lines.
    public static func summary(_ lines: [Components.Schemas.TaskLineInput]?) -> String? {
        guard let lines, !lines.isEmpty else { return nil }
        let byRole = Dictionary(lines.map { ($0.role, $0.agent) }, uniquingKeysWith: { first, _ in first })
        var parts: [String] = []
        let writerAgents = writerRoles.compactMap { byRole[$0] }
        if writerAgents.count == writerRoles.count, Set(writerAgents).count == 1 {
            parts.append("\(writerAgents[0]) writes")
        } else {
            for agent in unique(writerAgents) {
                let verbs = writerRoles.filter { byRole[$0] == agent }.map(verb)
                parts.append("\(agent) \(verbs.joined(separator: " and "))")
            }
        }
        if let reviewer = byRole[.reviewer] { parts.append("\(reviewer) \(verb(.reviewer))") }
        return parts.joined(separator: " · ")
    }

    /// A role's name as the form shows it, such as the role chip's keyword.
    public static func title(for role: Role) -> String {
        switch role {
        case .specifier: "Specifier"
        case .implementer: "Implementer"
        case .remediator: "Remediator"
        case .reviewer: "Reviewer"
        }
    }

    private static func verb(_ role: Role) -> String {
        switch role {
        case .specifier: "specifies"
        case .implementer: "implements"
        case .remediator: "remediates"
        case .reviewer: "reviews"
        }
    }
}
