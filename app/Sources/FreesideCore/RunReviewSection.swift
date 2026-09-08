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
    @State private var presentation: ReviewEvidencePresentation?
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
                if evidence.availability == .available, let presentation {
                    ScrollView {
                        ReviewEvidenceContent(round: round, evidence: evidence, presentation: presentation)
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
                let loaded = try await coordinator.reviewEvidence(for: runID, round: round)
                if loaded.availability == .available {
                    let parsing = Task.detached(priority: .userInitiated) {
                        ReviewEvidencePresentation(
                            events: Array(loaded.events?.data ?? []),
                            result: loaded.result.map { Array($0.data) }, exitStatus: loaded.exit_status)
                    }
                    presentation = await parsing.value
                    try Task.checkCancellation()
                }
                evidence = loaded
            } catch is CancellationError {
                return
            } catch {
                failed = true
            }
        }
    }
}

struct ReviewEvidenceContent: View {
    let round: Components.Schemas.RunReviewRound
    let evidence: Components.Schemas.ReviewEvidence
    let presentation: ReviewEvidencePresentation
    @State private var showsRaw = false

    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            VStack(alignment: .leading, spacing: 6) {
                heading("Daemon facts")
                Text("\(RunDisplay.label(round.state)) · \(RunDisplay.reviewIdentity(round))")
                if let outcome = round.outcome?.value1 {
                    Text("Outcome: \(outcome.rawValue.capitalized)")
                }
                if let count = round.findings_count { Text("\(count) findings recorded") }
                Text("Reviewed head \(round.head_sha.prefix(12)) · Base \(round.base_sha.prefix(12))")
                    .font(FreesideFont.monoCaption)
                if let exit = presentation.exitStatus { Text("Collected process exit status: \(exit)") }
            }
            Divider()
            VStack(alignment: .leading, spacing: 10) {
                heading("Reviewer’s reported conclusion · Agent claim")
                switch presentation.conclusion {
                case .noFindings:
                    Text("The reviewer reported no findings.")
                        .font(FreesideFont.sans(.headline, weight: .semibold))
                case .findings(let findings):
                    Text("The reviewer reported \(findings.count) findings.")
                    ForEach(Array(findings.enumerated()), id: \.offset) { _, finding in
                        VStack(alignment: .leading, spacing: 6) {
                            Text("\(finding.severity) · \(finding.location.label)")
                                .font(FreesideFont.monoCaption)
                            Text(finding.explanation)
                        }
                        .padding(12)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                    }
                case .absent, .unreadable:
                    Text("No readable reviewer result.")
                    Text(
                        presentation.conclusion == .absent
                            ? "The retained result is absent or empty."
                            : "The retained result does not match the supported findings format."
                    ).foregroundStyle(Color.inkDim)
                }
            }
            VStack(alignment: .leading, spacing: 8) {
                heading("Reviewer’s final message · Agent claim")
                if let explanation = presentation.explanation {
                    Text(explanation)
                } else {
                    Text("No explanation was retained.").foregroundStyle(Color.inkDim)
                }
            }
            VStack(alignment: .leading, spacing: 8) {
                heading("Recorded reviewer activity")
                Text("Agent messages: \(presentation.messageCount) · Commands: \(presentation.commandCount)")
                Text(presentation.terminalState ?? "No terminal turn state recorded")
                Text("This transcript shows recorded activity, not independent verification of the review.")
                    .font(FreesideFont.caption).foregroundStyle(Color.inkDim)
                LazyVStack(alignment: .leading, spacing: 12) {
                    ForEach(presentation.entries.filter { !$0.isDiagnostic }) { entry in
                        entryView(entry)
                    }
                }
            }
            if presentation.diagnosticCount > 0 {
                VStack(alignment: .leading, spacing: 8) {
                    heading("Diagnostics and unsupported output")
                    Text("\(presentation.diagnosticCount) entries · Separate from reviewer activity")
                        .font(FreesideFont.caption).foregroundStyle(Color.inkDim)
                    LazyVStack(alignment: .leading, spacing: 12) {
                        ForEach(presentation.entries.filter(\.isDiagnostic)) { entry in
                            entryView(entry)
                        }
                    }
                }
            }
            DisclosureGroup("Raw retained output", isExpanded: $showsRaw) {
                if showsRaw {
                    VStack(alignment: .leading, spacing: 12) {
                        heading("Events · Raw retained bytes")
                        ReviewOutputText(bytes: Array(evidence.events?.data ?? []))
                        heading("Result · Raw retained bytes")
                        ReviewOutputText(bytes: Array(evidence.result?.data ?? []))
                    }
                    .font(FreesideFont.monoCaption)
                }
            }
        }
        .font(FreesideFont.callout)
        .foregroundStyle(Color.ink)
        .textSelection(.enabled)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func heading(_ title: String) -> some View {
        Text(title).font(FreesideFont.sans(.headline, weight: .semibold))
    }

    private func entryView(_ entry: ReviewEvidencePresentation.Entry) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(entry.kind.rawValue).font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            if let command = entry.command {
                Text(command).font(FreesideFont.monoCaption)
                if let exit = entry.exitCode {
                    Text("Exit code: \(exit)").font(FreesideFont.caption)
                } else {
                    Text("Exit code not recorded").font(FreesideFont.caption)
                }
                if let status = entry.status { Text("Status: \(status)").font(FreesideFont.caption) }
            }
            if entry.invalidUTF8 {
                Text("Invalid UTF-8 is shown with replacement characters. Retained bytes are unchanged.")
                    .font(FreesideFont.caption).foregroundStyle(Color.inkDim)
            }
            Text(entry.text)
                .font(entry.kind == .command || entry.isDiagnostic ? FreesideFont.monoCaption : FreesideFont.callout)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.rule.opacity(0.2), in: RoundedRectangle(cornerRadius: 8))
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
