import FreesideAPI
import SwiftUI

/// One item's self-contained decision card: header, reason, evidence,
/// labeled agent claims, the bindings the decision will commit against,
/// and exactly the item's requested actions. Actions stay disabled until
/// the model's revalidation of current state succeeds.
struct DecisionDetailView: View {
    @State private var model: DecisionModel
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
        .task(id: model.itemID) { await model.validate() }
        .navigationTitle(model.snapshot.map { AttentionDisplay.title($0.item._type) } ?? "Decision")
    }

    @ViewBuilder
    private func card(_ item: Components.Schemas.AttentionItem) -> some View {
        VStack(alignment: .leading, spacing: 16) {
            header(item)
            banner
            Text(item.reason)
                .font(.body)

            if !item.evidence_snapshot.isEmpty {
                cardSection("Evidence") {
                    ForEach(item.evidence_snapshot, id: \.id) { artifact in
                        AttachmentRow(
                            label: artifact._type, digest: artifact.digest,
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
                            attachments: attachments)
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
            }

            actions(item)
        }
    }

    private func header(_ item: Components.Schemas.AttentionItem) -> some View {
        HStack(alignment: .firstTextBaseline) {
            Text(AttentionDisplay.title(item._type))
                .font(.title2.weight(.semibold))
            Spacer()
            PriorityBadge(priority: item.priority)
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
    /// fetched rendering underneath — the image inline when the bytes
    /// decode (plan §4), a placeholder when the fetch fails or the
    /// digest is missing, and nothing extra for a non-image attachment,
    /// which keeps its plain digest row.
    private struct AttachmentRow: View {
        let label: String
        let digest: String
        let attachments: AttachmentLoader

        var body: some View {
            VStack(alignment: .leading, spacing: 6) {
                LabeledContent(label) {
                    Text(digest)
                        .font(.caption.monospaced())
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
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
            .task(id: digest) { await attachments.load(digest) }
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
                    Task { await model.submit(action) }
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
