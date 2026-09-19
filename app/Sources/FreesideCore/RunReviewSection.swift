import FreesideAPI
import SwiftUI

struct RunReviewSection: View {
    let coordinator: SyncCoordinator
    let runID: String
    let facts: Components.Schemas.RunReviewFacts?
    var hasTimeline = true
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow

    static func sourceLabel(_ kind: String) -> String {
        switch kind {
        case "freeside_invoked": "Daemon facts · Freeside-invoked review"
        case "external", "github", "external_github": "External review · Source: \(kind)"
        default: "Unknown review source: \(kind)"
        }
    }

    static func missingCompletionMessage(_ state: Components.Schemas.ReviewProgressState) -> String {
        switch state {
        case .pending, .running: "Not completed"
        case .completed, .failed: "Completion time unavailable"
        }
    }

    static func availabilityMessage(
        hasTimeline: Bool, state: SyncCoordinator.TimelineLoadState?, freshness: InboxStore.Freshness
    ) -> String? {
        if !hasTimeline {
            return state == .loading ? "Loading review…" : "Review details unavailable"
        }
        if state == .loading { return "Showing saved review details while refreshing…" }
        if state == .unavailable { return "Review refresh failed. Showing saved details." }
        if state != .loaded || freshness != .fresh { return "Saved review details. Freshness unconfirmed." }
        return nil
    }

    private struct Selection: Identifiable {
        let round: Components.Schemas.RunReviewRound
        var id: String { round.invocation_id }
    }

    @State private var selection: Selection?
    @State private var retry = 0
    @State private var expandedFacts: Set<String>
    @State private var showsPriorRounds: Bool

    /// Where the section's folds live when its host persists them (per task
    /// id, with the rest of the task timeline's folds). Without it they are
    /// view-local, which is what a screenshot and a host with no task want.
    struct Folds {
        /// A round's facts, by the round's invocation id.
        let facts: (String) -> Binding<Bool>
        let priorRounds: Binding<Bool>
    }
    private let folds: Folds?

    init(
        coordinator: SyncCoordinator, runID: String, facts: Components.Schemas.RunReviewFacts?,
        hasTimeline: Bool = true, startsExpanded: Bool = false, folds: Folds? = nil
    ) {
        self.coordinator = coordinator
        self.runID = runID
        self.facts = facts
        self.hasTimeline = hasTimeline
        self.folds = folds
        // A screenshot captures the open state by starting there; live use
        // starts every fold collapsed.
        _expandedFacts = State(
            initialValue: startsExpanded ? Set(facts?.rounds.map(\.invocation_id) ?? []) : [])
        _showsPriorRounds = State(initialValue: startsExpanded)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: "Review")
            let state = coordinator.timelineLoadStates[runID]
            let availability = Self.availabilityMessage(
                hasTimeline: hasTimeline, state: state, freshness: coordinator.store.freshness)
            if let availability {
                Text(availability).font(FreesideFont.callout).foregroundStyle(Color.inkDim)
            }
            let rounds = RunHistoryPresentation.rounds(facts)
            if let current = rounds.first {
                roundsBlock(current: current, prior: Array(rounds.dropFirst()))
            } else if hasTimeline && state == .loaded && availability == nil {
                Text("No review requested yet")
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
            if state != .loading && availability != nil {
                Button("Retry review details") { retry += 1 }
                    .font(FreesideFont.callout)
            }
        }
        .foregroundStyle(Color.ink)
        .sheet(item: $selection) { selection in
            ReviewEvidenceView(coordinator: coordinator, runID: runID, round: selection.round)
        }
        .task(id: retry) {
            if retry > 0 { await coordinator.refreshTimeline(for: runID) }
        }
    }

    /// The rounds as one nested ground block: the newest round's verdict and
    /// facts, then every earlier round folded to a single hollow-marker line.
    private func roundsBlock(
        current: Components.Schemas.RunReviewRound, prior: [Components.Schemas.RunReviewRound]
    ) -> some View {
        let attention = ReviewRoundPresentation.hasOpenAdjudication(
            runID: runID, in: coordinator.store.orderedSnapshots)
        return VStack(alignment: .leading, spacing: 10) {
            roundRows(current, isCurrent: true, attention: attention)
            if !prior.isEmpty {
                let showsPriorRounds = folds?.priorRounds ?? $showsPriorRounds
                Button {
                    showsPriorRounds.wrappedValue.toggle()
                } label: {
                    markedRow(isCurrent: false, time: prior.first.flatMap(ReviewRoundPresentation.time)) {
                        Text(ReviewRoundPresentation.priorSummary(prior))
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.inkDim)
                            .fixedSize(horizontal: false, vertical: true)
                            .multilineTextAlignment(.leading)
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityValue(showsPriorRounds.wrappedValue ? "Expanded" : "Collapsed")
                if showsPriorRounds.wrappedValue {
                    ForEach(prior, id: \.invocation_id) { round in
                        roundRows(round, isCurrent: false, attention: false)
                    }
                }
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.ground))
    }

    @ViewBuilder
    private func roundRows(
        _ round: Components.Schemas.RunReviewRound, isCurrent: Bool, attention: Bool
    ) -> some View {
        markedRow(isCurrent: isCurrent, time: ReviewRoundPresentation.time(round)) {
            WrappingHStack(horizontalSpacing: 8, verticalSpacing: 4) {
                Text("Round \(round.round)")
                    .font(FreesideFont.sans(.callout, weight: isCurrent ? .semibold : .regular))
                    .foregroundStyle(isCurrent ? Color.ink : Color.inkDim)
                StateChip(
                    label: ReviewRoundPresentation.verdict(round),
                    cut: ReviewRoundPresentation.cut(round, isCurrent: isCurrent, hasOpenAdjudication: attention))
            }
        }
        let expanded =
            folds?.facts(round.invocation_id)
            ?? Binding(
                get: { expandedFacts.contains(round.invocation_id) },
                set: { open in
                    if open {
                        expandedFacts.insert(round.invocation_id)
                    } else {
                        expandedFacts.remove(round.invocation_id)
                    }
                })
        // The link shares the disclosure's row while the closed label and
        // the link fit on one line, and drops beneath it when they do not.
        ViewThatFits(in: .horizontal) {
            HStack(alignment: .top, spacing: 12) {
                roundFacts(round, isExpanded: expanded)
                // This row is chosen only when the link fits whole, so it
                // keeps that width and the disclosure takes the rest.
                evidence(round).layoutPriority(1)
            }
            VStack(alignment: .leading, spacing: 8) {
                roundFacts(round, isExpanded: expanded)
                evidence(round)
            }
        }
        .padding(.leading, ChronologyMarker.diameter + 8)
    }

    /// A marker, the row's content, and its time trailing; the time drops
    /// under the content when the line does not fit.
    private func markedRow<Content: View>(
        isCurrent: Bool, time: Date?, @ViewBuilder content: () -> Content
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            ChronologyMarker(isCurrent: isCurrent)
                .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 1 }
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    content()
                    Spacer(minLength: 8)
                    timeText(time)
                }
                VStack(alignment: .leading, spacing: 4) {
                    content()
                    timeText(time)
                }
            }
        }
    }

    @ViewBuilder
    private func timeText(_ time: Date?) -> some View {
        if let time {
            Text(formattedTime(time))
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
                .exactInstant(time)
        }
    }

    private func roundFacts(
        _ round: Components.Schemas.RunReviewRound, isExpanded: Binding<Bool>
    ) -> some View {
        KeywordDisclosure(
            keyword: "Round facts", summary: ReviewRoundPresentation.factsSummary(round), isExpanded: isExpanded
        ) {
            VStack(alignment: .leading, spacing: 6) {
                Text(RunDisplay.reviewIdentity(round))
                    .font(FreesideFont.callout)
                Text(round.outcome.map { "Outcome: \($0.value1.rawValue.capitalized)" } ?? "Outcome unavailable")
                    .font(FreesideFont.callout)
                Text(round.findings_count.map { "\($0) findings" } ?? "Findings count unavailable")
                    .font(FreesideFont.callout)
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
                if let requested = round.requested_at {
                    Text("Requested: \(formattedTime(requested))")
                        .font(FreesideFont.caption)
                        .exactInstant(requested)
                } else {
                    Text("Request time unavailable").font(FreesideFont.caption)
                }
                if let completed = round.completed_at {
                    Text("Completed: \(formattedTime(completed))")
                        .font(FreesideFont.caption)
                        .exactInstant(completed)
                } else {
                    Text(Self.missingCompletionMessage(round.state)).font(FreesideFont.caption)
                }
                Text(Self.sourceLabel(round.source.kind))
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
                if let status = round.source.status {
                    Text("Source status: \(status)").font(FreesideFont.caption)
                }
            }
            .padding(.top, 6)
            // The open facts must not decide whether the link shares the
            // row: only the closed label's width does, so expanding never
            // moves the link.
            .frame(idealWidth: 0, maxWidth: .infinity, alignment: .leading)
        }
    }

    @ViewBuilder
    private func evidence(_ round: Components.Schemas.RunReviewRound) -> some View {
        switch round.evidence.availability {
        case .available:
            // No fixed width: beside the disclosure it is only chosen when
            // it fits, and beneath it the label must wrap at large text
            // sizes rather than widen the block past its card.
            Button {
                selection = Selection(round: round)
            } label: {
                FreesideLink(title: "Inspect reviewer output")
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .buttonStyle(.plain)
        case .unavailable:
            evidenceNote("Evidence unavailable")
        case .unknown:
            evidenceNote("Evidence unknown")
        }
    }

    private func evidenceNote(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    private func formattedTime(_ date: Date) -> String {
        FreesideFormat.shortTime(date, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
    }
}

/// The review round's derived strings and chip cut, kept apart from the view
/// so the summaries are testable. Every value is already in `RunReviewRound`
/// or the synced Inbox; nothing here asks the daemon for more.
enum ReviewRoundPresentation {
    /// The verdict chip: the round's state until it completes, then its
    /// outcome. Findings carry the open count when the daemon sent
    /// dispositions, else the findings count, else the word alone.
    static func verdict(_ round: Components.Schemas.RunReviewRound) -> String {
        switch round.state {
        case .pending: return "Pending"
        case .running: return "Running"
        case .failed: return "Failed"
        case .completed:
            switch round.outcome?.value1 {
            case .clean?: return "Clean"
            case .findings?:
                if let open = round.dispositions?.value1.open { return "Findings · \(open) open" }
                return round.findings_count.map { "Findings · \($0)" } ?? "Findings"
            case nil: return "Completed"
            }
        }
    }

    /// Faint for every round but the newest; attention only while the newest
    /// round's findings have an open adjudication item for this run.
    static func cut(
        _ round: Components.Schemas.RunReviewRound, isCurrent: Bool, hasOpenAdjudication: Bool
    ) -> StateChip.Cut {
        guard isCurrent else { return .faint }
        return hasOpenAdjudication && round.outcome?.value1 == .findings ? .attention : .ink
    }

    static func hasOpenAdjudication(
        runID: String, in items: [Components.Schemas.AttentionItemSnapshot]
    ) -> Bool {
        items.contains { snapshot in
            guard snapshot.item._type == .finding_adjudication, snapshot.item.status == .open,
                case .run(let subject) = snapshot.item.subject
            else { return false }
            return subject.run_id == runID
        }
    }

    /// `Head <8> · Base <8>`: the one binding line. The full digests stay in
    /// Technical details.
    static func bindingLine(head: String, base: String) -> String {
        "Head \(head.prefix(8)) · Base \(base.prefix(8))"
    }

    static func sourceSummary(_ kind: String) -> String {
        switch kind {
        case "freeside_invoked": "Freeside-invoked"
        case "external", "github", "external_github": "External · \(kind)"
        default: "Unknown source · \(kind)"
        }
    }

    static func factsSummary(_ round: Components.Schemas.RunReviewRound) -> String {
        "\(bindingLine(head: round.head_sha, base: round.base_sha)) · \(sourceSummary(round.source.kind))"
    }

    /// The instant a round's row carries: when it completed, else when it
    /// was requested.
    static func time(_ round: Components.Schemas.RunReviewRound) -> Date? {
        round.completed_at ?? round.requested_at
    }

    /// The folded line for every round after the newest, newest first:
    /// `Rounds 2 and 1 · clean at their bound heads · historical`.
    static func priorSummary(_ rounds: [Components.Schemas.RunReviewRound]) -> String {
        // The app's copy is English, so the list is too, whatever the device
        // locale: "Rounds 3, 2, and 1", never "Rounds 3, 2 und 1".
        let numbers = rounds.map { String($0.round) }
        let list = numbers.formatted(.list(type: .and).locale(Locale(identifier: "en_US")))
        let name = (numbers.count == 1 ? "Round " : "Rounds ") + list
        let verdicts: String
        if rounds.allSatisfy({ $0.state == .completed && $0.outcome?.value1 == .clean }) {
            verdicts = rounds.count == 1 ? "clean at its bound head" : "clean at their bound heads"
        } else {
            // (counted form, lone form): "1 with findings, 1 clean" across
            // several rounds, but "Round 1 · findings" for one.
            let groups: [(String, String, (Components.Schemas.RunReviewRound) -> Bool)] = [
                ("with findings", "findings", { $0.state == .completed && $0.outcome?.value1 == .findings }),
                ("clean", "clean", { $0.state == .completed && $0.outcome?.value1 == .clean }),
                ("failed", "failed", { $0.state == .failed }),
                ("not completed", "not completed", { $0.state == .pending || $0.state == .running }),
                ("outcome unavailable", "outcome unavailable", { $0.state == .completed && $0.outcome == nil }),
            ]
            verdicts = groups.compactMap { counted, lone, matches in
                let count = rounds.count(where: matches)
                if count == 0 { return nil }
                return rounds.count == 1 ? lone : "\(count) \(counted)"
            }.joined(separator: ", ")
        }
        return "\(name) · \(verdicts) · historical"
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
            Text("Agent claims · Round \(round.round) · Head \(round.head_sha.prefix(8))")
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
                Text("Reviewed head \(round.head_sha.prefix(8)) · Base \(round.base_sha.prefix(8))")
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
