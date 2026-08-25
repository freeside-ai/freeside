import FreesideAPI
import SwiftUI

/// One item's self-contained decision card: header, reason, evidence,
/// labeled agent claims, the bindings the decision will commit against,
/// and exactly the item's requested actions. Actions stay disabled until
/// the model's revalidation of current state succeeds.
struct DecisionDetailView: View {
    private enum ProposalEditor: String, Identifiable {
        case revision
        case snooze
        var id: String { rawValue }
    }

    @State private var model: DecisionModel
    @State private var proposalEditor: ProposalEditor?
    @State private var detailsExpanded: Bool
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute] = [:]
    private let attachments: AttachmentLoader

    @MainActor
    init(store: InboxStore, itemID: String, detailsExpanded: Bool = false) {
        _model = State(initialValue: DecisionModel(store: store, itemID: itemID))
        _detailsExpanded = State(initialValue: detailsExpanded)
        attachments = store.attachments
    }

    var body: some View {
        Group {
            if let snapshot = model.snapshot {
                ScrollView {
                    card(snapshot.item)
                        .padding(14)
                        .freesideCard()
                        .padding()
                        .frame(maxWidth: 560, alignment: .leading)
                }
            } else {
                ContentUnavailableView(
                    "Item unavailable",
                    systemImage: "questionmark.circle",
                    description: Text("This attention item is not in the inbox.")
                )
            }
        }
        // Re-validate on open and whenever the cache is evicted for a new
        // sync epoch (the id carries the store's cache generation), so a
        // card left open across a restore recertifies the re-bootstrapped
        // snapshot instead of sitting on a stale validation (issue #162).
        .task(id: model.revalidationID) { await model.validate() }
        .sheet(item: $proposalEditor) { editor in
            switch editor {
            case .revision:
                if let facts = model.proposalFacts {
                    RunProposalRevisionSheet(facts: facts) { revision in
                        Task { await model.submitRunProposalRevision(revision) }
                    }
                }
            case .snooze:
                RunProposalSnoozeSheet { until in
                    Task { await model.snooze(until: until) }
                }
            }
        }
        .navigationTitle(model.snapshot.map { AttentionDisplay.title($0.item) } ?? "Decision")
    }

    @ViewBuilder
    private func card(_ item: Components.Schemas.AttentionItem) -> some View {
        VStack(alignment: .leading, spacing: 16) {
            header(item)
            banner
            Text(item.reason)
                .font(FreesideFont.body)
                .foregroundStyle(Color.ink)

            if let adjudication = item.finding_adjudication?.value1 {
                findingAdjudication(adjudication)
                actions(item)
            } else {
                actions(item)
            }

            // Daemon-derived commit-plan notice (plan §5.6): the reserved
            // plan channel was consumed without structuring the import, and
            // that must never be silent.
            if let notice = item.commit_plan_notice?.value1 {
                LabeledContent("Commit plan", value: AttentionDisplay.label(notice))
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
            }

            if let facts = model.proposalFacts {
                cardSection("Authenticated proposal") {
                    LabeledContent("Intent", value: facts.intent.rawValue)
                    LabeledContent("Expected cost", value: "\(facts.expected_cost_units) units")
                    LabeledContent("Components", value: "\(facts.scope.component_count)")
                    LabeledContent("Declared paths", value: "\(facts.scope.declared_path_count)")
                    LabeledContent(
                        "Control plane", value: facts.scope.touches_control_plane ? "Yes" : "No")
                    if let prior = facts.supersedes?.value1 {
                        Divider()
                        Text("Revision context")
                            .font(FreesideFont.sans(.caption, weight: .semibold))
                        proposalRevisionRows(prior)
                    }
                }
            }

            if !item.evidence_snapshot.isEmpty {
                cardSection("Evidence") {
                    ForEach(item.evidence_snapshot, id: \.id) { artifact in
                        AttachmentRow(
                            label: artifact._type.rawValue, digest: artifact.digest,
                            attachments: attachments)
                    }
                }
            }

            if !item.agent_claims.isEmpty {
                // Dashed: a claim must read as a claim (plan §9).
                cardSection("Agent claims (unverified)", dashed: true) {
                    // Keyed by position: the daemon permits two claims on
                    // the same artifact under different labels, so no
                    // claim field is unique on its own and an id-keyed
                    // ForEach could drop a row the user must review.
                    ForEach(Array(item.agent_claims.enumerated()), id: \.offset) { _, claim in
                        AttachmentRow(
                            label: claim.label, digest: claim.digest,
                            attachments: attachments, text: claim.text)
                    }
                }
            }

            DisclosureGroup("Details", isExpanded: $detailsExpanded) {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(detailRows(item).enumerated()), id: \.offset) { _, row in
                        LabeledContent(row.label) {
                            Text(row.value)
                                .multilineTextAlignment(.trailing)
                        }
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkDim)
                    }
                }
                .padding(.top, 6)
            }
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .textSelection(.enabled)
        }
    }

    @ViewBuilder
    private func findingAdjudication(
        _ binding: Components.Schemas.FindingAdjudicationBinding
    ) -> some View {
        ForEach(binding.proposals, id: \.finding_id) { proposal in
            cardSection(
                proposal.producer == .model
                    ? "Model proposal (unverified)" : "Daemon recommendation",
                dashed: proposal.producer == .model
            ) {
                LabeledContent("Finding", value: proposal.finding_id)
                LabeledContent("Recommended route", value: AttentionDisplay.label(proposal.route))
                LabeledContent(
                    "Goal relationship", value: AttentionDisplay.label(proposal.goal_relationship))
                LabeledContent(
                    "Work-unit compatibility",
                    value: AttentionDisplay.label(proposal.compatibility?.value1))
                if let confidence = proposal.confidence?.value1 {
                    LabeledContent("Proposal confidence", value: AttentionDisplay.label(confidence))
                }
                Text(proposal.rationale)
                    .fixedSize(horizontal: false, vertical: true)
            }

            cardSection("Daemon facts") {
                LabeledContent("Binding digest", value: binding.adjudication_digest)
                LabeledContent("Run", value: binding.run_id)
                LabeledContent("Round", value: "\(binding.round)")
            }

            if !proposal.assumptions.isEmpty {
                findingList("Assumptions", values: proposal.assumptions)
            }
            if !proposal.cited_rules.isEmpty {
                findingList("Cited repository rules", values: proposal.cited_rules)
            }
            if !proposal.offered_alternatives.isEmpty {
                cardSection("Viable alternatives") {
                    ForEach(proposal.offered_alternatives, id: \.route) { alternative in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(AttentionDisplay.label(alternative.route))
                                .font(FreesideFont.sans(.callout, weight: .semibold))
                            Text(alternative.consequence)
                        }
                    }
                    Picker(
                        "Selected route",
                        selection: alternativeSelection(for: proposal)
                    ) {
                        Text("Keep recommendation")
                            .tag(Optional<Components.Schemas.AdjudicationRoute>.none)
                        ForEach(proposal.offered_alternatives, id: \.route) { alternative in
                            Text(AttentionDisplay.label(alternative.route))
                                .tag(Optional(alternative.route))
                        }
                    }
                    .pickerStyle(.menu)
                }
            }
            if !proposal.open_questions.isEmpty {
                findingList("Gating questions", values: proposal.open_questions)
            }
        }
    }

    private func findingList(_ title: String, values: [String]) -> some View {
        cardSection(title) {
            ForEach(Array(values.enumerated()), id: \.offset) { _, value in
                Label(value, systemImage: "circle.fill")
                    .labelStyle(FindingListLabelStyle())
            }
        }
    }

    private func alternativeSelection(
        for proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> Binding<Components.Schemas.AdjudicationRoute?> {
        Binding(
            get: { alternativeSelections[proposal.finding_id] },
            set: { route in
                if let route {
                    alternativeSelections[proposal.finding_id] = route
                } else {
                    alternativeSelections.removeValue(forKey: proposal.finding_id)
                }
            }
        )
    }

    private func selectedAlternatives(
        _ binding: Components.Schemas.FindingAdjudicationBinding
    ) -> [Components.Schemas.AlternativeChoice] {
        Self.selectedAlternatives(binding, selections: alternativeSelections)
    }

    static func selectedAlternatives(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        selections: [String: Components.Schemas.AdjudicationRoute]
    ) -> [Components.Schemas.AlternativeChoice] {
        binding.proposals.compactMap { proposal in
            guard let route = selections[proposal.finding_id] else { return nil }
            return .init(finding_id: proposal.finding_id, route: route)
        }
    }

    private func actionInputReady(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) -> Bool {
        guard action == .choose_alternative_route else { return true }
        guard let binding = item.finding_adjudication?.value1 else { return false }
        return !selectedAlternatives(binding).isEmpty
    }

    private func detailRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [AttentionDisplay.BindingRow] {
        AttentionDisplay.detailBindingRows(
            item,
            priorProposalDigest: model.proposalFacts?.supersedes?.value1.proposal_digest,
            proposalDigest: model.proposalFacts?.proposal_digest
        )
    }

    @ViewBuilder
    private func proposalRevisionRows(
        _ prior: Components.Schemas.RunProposalRevisionFacts
    ) -> some View {
        LabeledContent("Prior intent", value: prior.intent.rawValue)
        LabeledContent("Prior cost", value: "\(prior.expected_cost_units) units")
        LabeledContent(
            "Prior scope",
            value: "\(prior.scope.component_count) components, \(prior.scope.declared_path_count) paths")
        LabeledContent(
            "Prior control plane", value: prior.scope.touches_control_plane ? "Yes" : "No")
    }

    private func header(_ item: Components.Schemas.AttentionItem) -> some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 4) {
                Text(AttentionDisplay.title(item))
                    .font(FreesideFont.title)
                    .foregroundStyle(Color.ink)
                creationTimestamp(item.created_at)
            }
            Spacer()
            PriorityBadge(priority: item.priority)
            if let posture = item.posture?.value1 {
                HealthPostureBadge(posture: posture)
            }
            StatusBadge(status: item.status)
        }
    }

    @ViewBuilder
    private func creationTimestamp(_ createdAt: Date?) -> some View {
        Group {
            if let createdAt {
                Text(
                    "Created \(createdAt, format: .dateTime.month(.abbreviated).day().year().hour().minute())"
                )
            } else {
                Text("Created: not recorded")
            }
        }
        .font(FreesideFont.callout)
        .foregroundStyle(Color.inkDim)
    }

    @ViewBuilder
    private var banner: some View {
        if model.phase == .superseded {
            bannerLabel(
                "This item changed before your decision applied. Nothing was committed; re-review the replacement below.",
                systemImage: "arrow.triangle.2.circlepath",
                tint: .accentText, wash: .accentWash
            )
        } else {
            // An applied record persists even when the item stays open
            // (a non-resolving action such as acknowledge or open_pr).
            if let record = model.appliedRecord {
                // Success is quiet: a plain tick on a neutral wash, never
                // green and never the accent.
                bannerLabel(
                    "Decision applied: \(AttentionDisplay.label(record.action))",
                    systemImage: "checkmark",
                    tint: .inkDim, wash: .neutralWash
                )
            }
            // The retry affordance leads: when a preserved command may
            // hold a recorded result, resending it is the actionable
            // step, whatever else failed.
            if model.canRetryLostResponse {
                VStack(alignment: .leading, spacing: 8) {
                    bannerLabel(
                        "The response was lost; the decision may already be recorded.",
                        systemImage: "arrow.clockwise",
                        tint: .accentText, wash: .accentWash
                    )
                    Button("Retry") {
                        Task { await model.retryLostResponse() }
                    }
                    .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                }
            } else if case .failed(let message) = model.validation {
                bannerLabel(
                    "Couldn't validate current state: \(message)",
                    systemImage: "exclamationmark",
                    tint: .waxText, wash: .waxWash
                )
            } else if let message = model.submissionError {
                bannerLabel(
                    "Submission failed: \(message)",
                    systemImage: "exclamationmark",
                    tint: .waxText, wash: .waxWash
                )
            }
        }
    }

    private func cardSection(
        _ title: String, dashed: Bool = false, @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: title)
            content()
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.ground))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(Color.rule, style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : []))
        )
    }

    /// One labeled attachment row. Plan §9's presentation layers supersede
    /// the earlier digest-leading choice: content stays in the evidence layer,
    /// while its binding digest appears once in the card's collapsed details.
    /// A text claim renders its daemon-verified inline content directly;
    /// otherwise fetched image bytes render inline and a failed fetch gets a
    /// placeholder. Attachment bytes remain memory-only.
    private struct AttachmentRow: View {
        let label: String
        let digest: String
        let attachments: AttachmentLoader
        var text: Components.Schemas.ClaimText? = nil

        var body: some View {
            VStack(alignment: .leading, spacing: 6) {
                Text(label)
                    .font(FreesideFont.sans(.callout, weight: .semibold))
                if let text {
                    // No accessibility override: VoiceOver must hear the
                    // content, and the visible label already names the claim.
                    claimText(text)
                        .font(FreesideFont.callout)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                } else {
                    fetchedAttachment
                        .task(id: digest) { await attachments.load(digest) }
                }
            }
        }

        private func claimText(_ text: Components.Schemas.ClaimText) -> Text {
            switch text.media_type {
            case .text_sol_plain:
                return Text(text.content)
            case .text_sol_markdown:
                // Inline-only interpretation keeps a summary a compact
                // paragraph; unparsable markdown falls back to the raw
                // content rather than dropping the claim's body.
                let attributed = try? AttributedString(
                    markdown: text.content,
                    options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))
                return Text(attributed ?? AttributedString(text.content))
            }
        }

        @ViewBuilder
        private var fetchedAttachment: some View {
            switch attachments.phase(for: digest) {
            case .image(let image):
                platformImage(image)
                    .resizable()
                    .scaledToFit()
                    .frame(maxWidth: 320, alignment: .leading)
                    .clipShape(RoundedRectangle(cornerRadius: 6))
                    .accessibilityLabel("\(label) attachment image")
            case .unavailable:
                Label("Attachment unavailable", systemImage: "photo.badge.exclamationmark")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            case .loading, .notImage, nil:
                EmptyView()
            }
        }

        private func platformImage(_ image: PlatformImage) -> Image {
            #if canImport(UIKit)
                Image(uiImage: image)
            #elseif canImport(AppKit)
                Image(nsImage: image)
            #endif
        }
    }

    /// A card banner: tinted wash, glyph and message in the state color.
    private func bannerLabel(_ text: String, systemImage: String, tint: Color, wash: Color) -> some View {
        Label {
            Text(text)
        } icon: {
            Image(systemName: systemImage)
                .font(.system(size: 10, weight: .semibold))
        }
        .font(FreesideFont.callout)
        .textSelection(.enabled)
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(wash, in: RoundedRectangle(cornerRadius: 8))
        .foregroundStyle(tint)
    }

    @ViewBuilder
    private func actions(_ item: Components.Schemas.AttentionItem) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            if model.validation == .pending {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small).tint(.waterText)
                    Text("Validating current state…")
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkDim)
                }
            }
            // Keyed by position: the daemon boundary does not enforce
            // uniqueness in requested_decision, and duplicate ForEach
            // identities can drop or cross-wire buttons.
            ForEach(Array(model.offeredActions.enumerated()), id: \.offset) { offset, action in
                Button {
                    switch action {
                    case .start_with_changes:
                        proposalEditor = .revision
                    case .snooze:
                        proposalEditor = .snooze
                    case .choose_alternative_route:
                        guard let binding = item.finding_adjudication?.value1 else { return }
                        Task { await model.submitFindingAlternatives(selectedAlternatives(binding)) }
                    default:
                        Task { await model.submit(action) }
                    }
                } label: {
                    HStack {
                        Text(AttentionDisplay.label(action))
                        if model.phase == .submitting(action) {
                            ProgressView().controlSize(.small)
                        }
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(FreesideActionButtonStyle(tone: Self.tone(for: action, leading: offset == 0)))
                .disabled(
                    !model.actionsEnabled || !model.isSubmittable(action)
                        || !actionInputReady(action, item: item))
            }
            if item._type == .blocked {
                Text("A blocked item is informational; it resolves when the external wait clears.")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            } else if model.offeredActions.contains(where: { !model.isSubmittable($0) }) {
                Text("Actions carrying discussion or parameters arrive with later units.")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            }
        }
    }

    /// The leading offered action is the primary one; stops and declines
    /// are destructive; everything else is neutral.
    private static func tone(
        for action: Components.Schemas.Action, leading: Bool
    ) -> FreesideActionButtonStyle.Tone {
        switch action {
        case .decline, .stop, .stop_unattended:
            return .destructive
        default:
            return leading ? .primary : .neutral
        }
    }
}

private struct FindingListLabelStyle: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            configuration.icon
                .font(.system(size: 4))
                .foregroundStyle(Color.inkDim)
            configuration.title
        }
    }
}

private struct RunProposalRevisionSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var expectedCost: Int
    @State private var componentCount: Int
    @State private var declaredPathCount: Int
    @State private var touchesControlPlane: Bool
    private let originalFacts: Components.Schemas.RunProposalFactsSnapshot
    let submit: (Components.Schemas.RunProposalRevisionInput) -> Void

    init(
        facts: Components.Schemas.RunProposalFactsSnapshot,
        submit: @escaping (Components.Schemas.RunProposalRevisionInput) -> Void
    ) {
        _expectedCost = State(initialValue: facts.expected_cost_units)
        _componentCount = State(initialValue: facts.scope.component_count)
        _declaredPathCount = State(initialValue: facts.scope.declared_path_count)
        _touchesControlPlane = State(initialValue: facts.scope.touches_control_plane)
        originalFacts = facts
        self.submit = submit
    }

    private var changesProposal: Bool {
        expectedCost != originalFacts.expected_cost_units
            || componentCount != originalFacts.scope.component_count
            || declaredPathCount != originalFacts.scope.declared_path_count
            || touchesControlPlane != originalFacts.scope.touches_control_plane
    }

    var body: some View {
        NavigationStack {
            Form {
                LabeledContent("Intent", value: "Implement subject")
                Stepper("Expected cost: \(expectedCost) units", value: $expectedCost, in: 1...1_000_000)
                Stepper("Components: \(componentCount)", value: $componentCount, in: 1...32)
                Stepper("Declared paths: \(declaredPathCount)", value: $declaredPathCount, in: 1...4096)
                Toggle("Touches control plane", isOn: $touchesControlPlane)
            }
            .navigationTitle("Start with changes")
            .tint(.accentText)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Submit") {
                        submit(
                            .init(
                                intent: .implement_subject,
                                expected_cost_units: expectedCost,
                                scope: .init(
                                    component_count: componentCount,
                                    declared_path_count: declaredPathCount,
                                    touches_control_plane: touchesControlPlane)))
                        dismiss()
                    }
                    .disabled(!changesProposal)
                }
            }
        }
        .frame(minWidth: 380, minHeight: 280)
    }
}

private struct RunProposalSnoozeSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var until = Date().addingTimeInterval(60 * 60)
    let submit: (Date) -> Void

    var body: some View {
        NavigationStack {
            Form {
                DatePicker(
                    "Snooze until", selection: $until, in: Date()..., displayedComponents: [.date, .hourAndMinute])
            }
            .navigationTitle("Snooze proposal")
            .tint(.accentText)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Snooze") {
                        submit(until)
                        dismiss()
                    }
                }
            }
        }
        .frame(minWidth: 380, minHeight: 220)
    }
}

struct HealthPostureBadge: View {
    let posture: Components.Schemas.HealthPosture

    var body: some View {
        StateChip(label: AttentionDisplay.label(posture), color: color)
    }

    private var color: Color {
        switch posture {
        case .blocking: return .waxText
        case .advisory: return .inkDim
        }
    }
}
