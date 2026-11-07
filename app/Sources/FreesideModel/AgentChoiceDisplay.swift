import FreesideAPI

/// The New Task form's agent choices (plan §5.4, task lines): which lines a
/// per-role choice sends, what each catalog agent reads as, and what picks
/// the agent for a role left unchosen. Pure: the sheet passes the synced
/// catalog and routing policy in.
///
/// The form lists the tree's agents by name, but a choice is the agent's
/// selection (enrollment, model, effort): that is what a line records, so a
/// later tree edit never changes what a chosen line runs.
///
/// A role the operator leaves unchosen gets no line, so the daemon picks its
/// agent at first admission. Today that is the lineup's agent; once the
/// daemon routes by policy (#2393) it may be any agent the routing policy
/// orders for the role.
public enum AgentChoiceDisplay {
    public typealias Role = Components.Schemas.TaskLineRole
    public typealias Selection = Components.Schemas.TaskLineSelection

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
    public static func taskLines(choices: [Role: Selection]) -> [Components.Schemas.TaskLineInput]? {
        let lines = roleOrder.compactMap { role in
            choices[role].map { Components.Schemas.TaskLineInput(role: role, selection: $0) }
        }
        return lines.isEmpty ? nil : lines
    }

    /// The chosen selections the catalog no longer offers, as labels in role
    /// order, each once. A choice is offered while a tree agent carries it and
    /// its enrollment is still listed. A newer catalog can drop either; the
    /// form keeps the choice and blocks Submit instead of swapping it
    /// silently. With no catalog, no choice is offered.
    public static func missingSelections(
        choices: [Role: Selection], catalog: Components.Schemas.AgentCatalog?
    ) -> [String] {
        unique(
            roleOrder.compactMap { choices[$0] }.filter { !offers(catalog, $0) }.map(selectionLabel))
    }

    /// Whether the catalog offers a selection: a tree agent carries it and
    /// the daemon still lists its enrollment.
    public static func offers(_ catalog: Components.Schemas.AgentCatalog?, _ selection: Selection) -> Bool {
        guard let catalog else { return false }
        return catalog.agents.contains { $0.selection == selection }
            && catalog.enrollments.contains { $0.enrollment_id == selection.enrollment_id }
    }

    /// The agents a role's picker lists: each one the catalog offers, and
    /// only the first in tree order of agents that share a selection. The
    /// tree writes one definition under several names when two roles pick
    /// one model and effort, and a picker tagging rows by selection would
    /// show every twin selected; `choiceLabel` reads that first name too.
    public static func pickable(_ catalog: Components.Schemas.AgentCatalog?) -> [Components.Schemas.AgentCatalogEntry] {
        guard let catalog else { return [] }
        var seen: Set<Selection> = []
        return catalog.agents.filter { offers(catalog, $0.selection) && seen.insert($0.selection).inserted }
    }

    /// A selection as one label: `enrollment · model · effort`.
    public static func selectionLabel(_ selection: Selection) -> String {
        "\(selection.enrollment_id) · \(selection.model_id) · \(selection.effort)"
    }

    /// A choice as the form names it: the tree agent that carries the
    /// selection, or the selection's own label when no agent does.
    public static func choiceLabel(_ selection: Selection, catalog: Components.Schemas.AgentCatalog?) -> String {
        catalog?.agents.first { $0.selection == selection }?.name ?? selectionLabel(selection)
    }

    /// One catalog agent's facts as a choice lists them: model, effort, and
    /// lineage group.
    public static func facts(_ agent: Components.Schemas.AgentCatalogEntry) -> String {
        [agent.model, RunDisplay.label(agent.effort), agent.lineage_group ?? "No lineage group"]
            .joined(separator: " · ")
    }

    /// The default entry's label: what picks the agent when the operator
    /// chooses none for `role`. The lineup names its agent for the role and
    /// the selection it runs; "or routing policy" follows when the policy
    /// lists the role.
    public static func defaultLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Components.Schemas.AgentCatalog?
    ) -> String {
        let lineup =
            catalog?.lineup.first { $0.role == role }.map { "lineup \($0.agent) (\(selectionLabel($0.selection)))" }
            ?? "lineup"
        let policyListsTheRole = policy?.roles.contains { $0.role == role } == true
        return "Default · \(lineup)" + (policyListsTheRole ? " or routing policy" : "")
    }

    private static func unique(_ names: [String]) -> [String] {
        var seen = Set<String>()
        return names.filter { seen.insert($0).inserted }
    }

    /// Lines as one sentence of verbs, as the Agents chip and a saved
    /// request read them: `claude-opus writes · codex reviews` when the three
    /// writer lines carry one selection, otherwise each writer choice with
    /// its roles' verbs (`claude-opus specifies · codex implements and
    /// remediates`). Each choice reads as its choiceLabel, so a selection no
    /// tree agent carries reads as `enrollment · model · effort`. Nil when
    /// there are no lines.
    public static func summary(
        _ lines: [Components.Schemas.TaskLineInput]?, catalog: Components.Schemas.AgentCatalog? = nil
    ) -> String? {
        guard let lines, !lines.isEmpty else { return nil }
        let byRole = Dictionary(
            lines.map { ($0.role, choiceLabel($0.selection, catalog: catalog)) },
            uniquingKeysWith: { first, _ in first })
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
