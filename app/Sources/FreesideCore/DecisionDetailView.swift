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
    private let attachments: AttachmentLoader

    @MainActor
    init(store: InboxStore, itemID: String) {
        _model = State(initialValue: DecisionModel(store: store, itemID: itemID))
        attachments = store.attachments
    }

    var body: some View {
        Group {
            if let snapshot = model.snapshot {
                ScrollView {
                    card(snapshot.item)
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
        .navigationTitle(model.snapshot.map { AttentionDisplay.title($0.item._type) } ?? "Decision")
    }

    @ViewBuilder
    private func card(_ item: Components.Schemas.AttentionItem) -> some View {
        VStack(alignment: .leading, spacing: 16) {
            header(item)
            banner
            Text(item.reason)
                .font(.body)

            // Daemon-derived commit-plan notice (plan §5.6): the reserved
            // plan channel was consumed without structuring the import, and
            // that must never be silent.
            if let notice = item.commit_plan_notice?.value1 {
                LabeledContent("Commit plan", value: AttentionDisplay.label(notice))
                    .font(.callout)
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
                            .font(.caption.weight(.semibold))
                        proposalRevisionRows(prior)
                        Text(prior.proposal_digest)
                            .font(.caption.monospaced())
                            .lineLimit(1)
                            .truncationMode(.middle)
                        Image(systemName: "arrow.down")
                            .foregroundStyle(.secondary)
                        Text(facts.proposal_digest)
                            .font(.caption.monospaced())
                            .lineLimit(1)
                            .truncationMode(.middle)
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
                cardSection("Agent claims (unverified)") {
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

            cardSection("Decision binds to") {
                LabeledContent("Item version", value: "\(item.item_version)")
                if !item.pr_head_sha.isEmpty {
                    LabeledContent("PR head", value: item.pr_head_sha)
                }
                ForEach(item.artifact_digests, id: \.self) { digest in
                    Text(digest)
                        .font(.caption.monospaced())
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .foregroundStyle(.secondary)
                }
                let recoveryRows =
                    AttentionDisplay.reviewRecoveryBindingRows(item)
                    + AttentionDisplay.reviewConfigurationRecoveryRows(item)
                    + AttentionDisplay.codexReenrollmentRecoveryRows(item)
                if !recoveryRows.isEmpty {
                    Divider()
                    ForEach(Array(recoveryRows.enumerated()), id: \.offset) { _, row in
                        LabeledContent(row.label) {
                            Text(row.value)
                                .font(.caption.monospaced())
                                .multilineTextAlignment(.trailing)
                                .textSelection(.enabled)
                        }
                    }
                }
            }

            actions(item)
        }
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
        HStack(alignment: .firstTextBaseline) {
            Text(AttentionDisplay.title(item._type))
                .font(.title2.weight(.semibold))
            Spacer()
            PriorityBadge(priority: item.priority)
            if let posture = item.posture?.value1 {
                HealthPostureBadge(posture: posture)
            }
            StatusBadge(status: item.status)
        }
    }

    @ViewBuilder
    private var banner: some View {
        if model.phase == .superseded {
            bannerLabel(
                "This item changed before your decision applied. Nothing was committed; re-review the replacement below.",
                systemImage: "arrow.triangle.2.circlepath",
                tint: .orange
            )
        } else {
            // An applied record persists even when the item stays open
            // (a non-resolving action such as acknowledge or open_pr).
            if let record = model.appliedRecord {
                bannerLabel(
                    "Decision applied: \(AttentionDisplay.label(record.action))",
                    systemImage: "checkmark.circle.fill",
                    tint: .green
                )
            }
            // The retry affordance leads: when a preserved command may
            // hold a recorded result, resending it is the actionable
            // step, whatever else failed.
            if model.canRetryLostResponse {
                VStack(alignment: .leading, spacing: 8) {
                    bannerLabel(
                        "The response was lost; the decision may already be recorded.",
                        systemImage: "arrow.clockwise.circle.fill",
                        tint: .orange
                    )
                    Button("Retry") {
                        Task { await model.retryLostResponse() }
                    }
                    .buttonStyle(.bordered)
                }
            } else if case .failed(let message) = model.validation {
                bannerLabel(
                    "Couldn't validate current state: \(message)",
                    systemImage: "exclamationmark.triangle.fill",
                    tint: .red
                )
            } else if let message = model.submissionError {
                bannerLabel(
                    "Submission failed: \(message)",
                    systemImage: "exclamationmark.triangle.fill",
                    tint: .red
                )
            }
        }
    }

    private func cardSection(
        _ title: String, @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title)
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
                .textCase(.uppercase)
            content()
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
    }

    /// One labeled attachment row: always the digest (the decision
    /// stays visibly bound to it, whatever the bytes do), plus the
    /// rendering underneath. A text claim renders its inline,
    /// digest-bound content directly (plan §9's summary carrier; the
    /// daemon already re-verified digest == sha256(content), so no fetch
    /// runs). Otherwise the fetched bytes render — the image inline when
    /// they decode (plan §4), a placeholder when the fetch fails or the
    /// digest is missing, and nothing extra for a non-image attachment,
    /// which keeps its plain digest row.
    private struct AttachmentRow: View {
        let label: String
        let digest: String
        let attachments: AttachmentLoader
        var text: Components.Schemas.ClaimText? = nil

        var body: some View {
            VStack(alignment: .leading, spacing: 6) {
                LabeledContent(label) {
                    Text(digest)
                        .font(.caption.monospaced())
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                if let text {
                    // No accessibility override: the content is the text a
                    // VoiceOver user must hear, and the claim's label is
                    // already announced by the labeled digest row above.
                    claimText(text)
                        .font(.callout)
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
                    .font(.caption)
                    .foregroundStyle(.secondary)
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

    private func bannerLabel(_ text: String, systemImage: String, tint: Color) -> some View {
        Label(text, systemImage: systemImage)
            .font(.callout)
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(tint.opacity(0.12), in: RoundedRectangle(cornerRadius: 8))
            .foregroundStyle(tint)
    }

    @ViewBuilder
    private func actions(_ item: Components.Schemas.AttentionItem) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            if model.validation == .pending {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text("Validating current state…")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            // Keyed by position: the daemon boundary does not enforce
            // uniqueness in requested_decision, and duplicate ForEach
            // identities can drop or cross-wire buttons.
            ForEach(Array(model.offeredActions.enumerated()), id: \.offset) { _, action in
                Button {
                    switch action {
                    case .start_with_changes:
                        proposalEditor = .revision
                    case .snooze:
                        proposalEditor = .snooze
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
                .disabled(!model.actionsEnabled || !model.isSubmittable(action))
            }
            if item._type == .blocked {
                Text("A blocked item is informational; it resolves when the external wait clears.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if model.offeredActions.contains(where: { !model.isSubmittable($0) }) {
                Text("Actions carrying discussion or parameters arrive with later units.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .buttonStyle(.bordered)
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
        Text(AttentionDisplay.label(posture))
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(color.opacity(0.15), in: Capsule())
            .foregroundStyle(color)
    }

    private var color: Color {
        switch posture {
        case .blocking: return .red
        case .advisory: return .secondary
        }
    }
}
