import FreesideAPI
import SwiftUI

struct RunReviewSection: View {
    let coordinator: SyncCoordinator
    let runID: String
    let facts: Components.Schemas.RunReviewFacts?

    private struct Selection: Identifiable {
        let round: Components.Schemas.RunReviewRound
        var id: String { round.invocation_id }
    }

    @State private var selection: Selection?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Review").font(FreesideFont.title)
            if let facts, !facts.rounds.isEmpty {
                ForEach(facts.rounds, id: \.invocation_id) { round in
                    roundCard(round)
                }
            } else {
                Text("No review requested yet")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .foregroundStyle(Color.ink)
        .sheet(item: $selection) { selection in
            ReviewEvidenceView(coordinator: coordinator, runID: runID, round: selection.round)
        }
    }

    private func roundCard(_ round: Components.Schemas.RunReviewRound) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Round \(round.round)").font(FreesideFont.sans(.headline, weight: .semibold))
                Spacer()
                Text(RunDisplay.label(round.state))
                    .font(FreesideFont.caption)
                    .padding(.horizontal, 9).padding(.vertical, 4)
                    .background(Color.rule.opacity(0.4), in: Capsule())
            }
            Text("Head \(round.head_sha.prefix(12))")
                .font(FreesideFont.monoCaption)
                .textSelection(.enabled)
            Text(RunDisplay.reviewIdentity(round))
                .font(FreesideFont.callout)
            if let outcome = round.outcome?.value1, let count = round.findings_count {
                Text("\(outcome.rawValue.capitalized) · \(count) findings")
                    .font(FreesideFont.callout)
            }
            if let counts = round.dispositions?.value1 {
                Text(
                    "\(counts.fixed) fixed · \(counts.declined) declined · \(counts.deferred) deferred · \(counts.open) open"
                )
                .font(FreesideFont.caption)
            }
            if let failure = round.failure?.value1 {
                Text("\(failure._class.capitalized): \(failure.reason)")
                    .font(FreesideFont.callout)
                    .textSelection(.enabled)
            }
            if round.retry_pending {
                Text("Retry pending").font(FreesideFont.callout)
            }
            if round.requested_at == nil {
                Text("Request time unavailable").font(FreesideFont.caption)
            }
            Text(
                round.source.kind == "freeside_invoked"
                    ? "Daemon facts · Freeside-invoked review" : "Source: \(round.source.kind)"
            )
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            if let status = round.source.status {
                Text("Source status: \(status)").font(FreesideFont.caption)
            }
            switch round.evidence.availability {
            case .available:
                Button("Inspect reviewer output") { selection = Selection(round: round) }
                    .font(FreesideFont.callout)
            case .unavailable:
                Text("Evidence unavailable").font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            case .unknown:
                Text("Evidence unknown").font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
    }
}

private struct ReviewEvidenceView: View {
    let coordinator: SyncCoordinator
    let runID: String
    let round: Components.Schemas.RunReviewRound
    @Environment(\.dismiss) private var dismiss
    @State private var evidence: Components.Schemas.ReviewEvidence?
    @State private var failed = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Text("Reviewer output").font(FreesideFont.title)
                Spacer()
                Button("Done") { dismiss() }
            }
            Text("Agent claims · Round \(round.round) · Head \(round.head_sha.prefix(12))")
                .font(FreesideFont.caption)
            Text("Private, sensitive output. Not publishable verifier evidence.")
                .font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            if let evidence {
                if evidence.availability == .available {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 12) {
                            ReviewOutputText(bytes: Array(evidence.events?.data ?? []))
                            ReviewOutputText(bytes: Array(evidence.result?.data ?? []))
                        }
                        .font(FreesideFont.monoCaption)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                } else {
                    Text(evidence.availability == .unknown ? "Evidence unknown" : "Evidence unavailable")
                }
            } else if failed {
                Text("Evidence could not be authenticated or loaded.")
            } else {
                ProgressView("Loading reviewer output…")
            }
        }
        .padding(24)
        .frame(minWidth: 280, minHeight: 280)
        .task {
            do {
                evidence = try await coordinator.reviewEvidence(for: runID, round: round)
            } catch is CancellationError {
                return
            } catch {
                failed = true
            }
        }
    }
}

struct ReviewOutputText: View {
    let bytes: [UInt8]

    var hasReplacementCharacters: Bool { String(bytes: bytes, encoding: .utf8) == nil }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if hasReplacementCharacters {
                Text("Invalid UTF-8 is shown with replacement characters. Retained bytes are unchanged.")
                    .font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            }
            Text(String(decoding: bytes, as: UTF8.self))
        }
    }
}
