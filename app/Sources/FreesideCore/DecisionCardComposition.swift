import FreesideAPI
import SwiftUI

enum DecisionCardModule: String, CaseIterable {
    case facts
    case specRevision
    case specification
    case factBlock
    case findingFacts
    case recommendation
    case checklist
    case stageRail
    case comparison
    case yieldChart
    case summary
    case claims
    case evidence
    case details
}

// Keep in sync with export.SummaryEvidenceLabel in
// daemon/internal/export/evidence_source.go.
enum AgentClaimLabels {
    static let summary = "freeside.summary"
    static let addressals = "Addressals"
    static let specification = "Specification"

    static func isApprovalMaterial(_ label: String) -> Bool {
        label == addressals || label == specification
    }
}

struct DecisionCardComposition: Equatable {
    let modules: [DecisionCardModule]
    let actionInsertionIndex: Int
    let reviewingActionInsertionIndex: Int?

    /// A claim module leads when it renders above the action region: that is
    /// the whole meaning of prominence here, so it is read from
    /// `actionInsertionIndex` rather than from another module's position.
    func claimsAreProminent(at moduleIndex: Int) -> Bool {
        guard let claimsIndex = modules.firstIndex(of: .claims) else { return false }
        return moduleIndex == claimsIndex && claimsIndex < actionInsertionIndex
    }

    func claims(
        from claims: [Components.Schemas.AgentClaim],
        at moduleIndex: Int,
        prominentClaimIndex: Int?
    ) -> [Components.Schemas.AgentClaim] {
        let claims = claims.filter {
            !($0.label == AgentClaimLabels.summary && $0.text != nil)
                && !AgentClaimLabels.isApprovalMaterial($0.label)
        }
        let claimModuleIndices = modules.indices.filter { modules[$0] == .claims }
        guard claimModuleIndices.count > 1 else { return claims }
        let leads = moduleIndex == claimModuleIndices.first
        guard let prominentClaimIndex, claims.indices.contains(prominentClaimIndex) else {
            // Without a caller-chosen prominent claim, the claim the operator
            // can read leads and an attachment stays supporting context
            // (plan §9). This is the split the macOS action region already
            // applies, where text claims sit above the actions and attachment
            // claims move to the inspector.
            return claims.filter { ($0.text != nil) == leads }
        }
        return claims.enumerated().compactMap { index, claim in
            leads == (index == prominentClaimIndex) ? claim : nil
        }
    }

    func summaries(
        from claims: [Components.Schemas.AgentClaim]
    ) -> [Components.Schemas.AgentClaim] {
        claims.filter { $0.label == AgentClaimLabels.summary && $0.text != nil }
    }

    static let sharedModuleSet = DecisionCardModule.allCases

    /// Every composition places `.facts` ahead of `actionInsertionIndex`: the
    /// daemon's typed facts inform the decision, so they can never render
    /// below the actions. Where each type puts them among its own modules is
    /// that type's judgement, not a shared rule, so a card leads with the
    /// module its §9 row leads with (the readiness checklist, the stage rail,
    /// the disputed positions) and keeps the identifier-shaped facts last.
    static func forType(_ type: Components.Schemas.AttentionType) -> Self {
        switch type {
        case .ready_for_final_review:
            // The verdict leads, then the review's shape; the diff's base and
            // head are audit coordinates, so they sit last before the actions.
            return .init(
                modules: [
                    .recommendation, .checklist, .factBlock, .yieldChart, .facts, .summary,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 5,
                reviewingActionInsertionIndex: 8)
        case .execution_failure:
            return .init(
                modules: [
                    .recommendation, .stageRail, .facts, .claims, .factBlock, .summary, .claims,
                    .evidence, .details,
                ],
                actionInsertionIndex: 4,
                reviewingActionInsertionIndex: nil)
        case .review_dispute:
            return .init(
                modules: [
                    .comparison, .factBlock, .facts, .summary, .claims, .evidence, .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .review_diminishing_returns:
            return .init(
                modules: [
                    .recommendation, .yieldChart, .facts, .factBlock, .summary, .claims,
                    .evidence, .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .finding_adjudication:
            // Section 9's finding_adjudication row leads with two things: the
            // recommended route as a labeled proposal, and the finding's
            // daemon-authenticated facts in their own register (#984). Both
            // live in .findingFacts, so it joins .recommendation ahead of
            // actionInsertionIndex; the remaining assumptions/cited-rules/
            // alternatives/gating-questions content stays in .factBlock,
            // which the §9 "Below" column covers, after the action region.
            return .init(
                modules: [
                    .recommendation, .findingFacts, .facts, .factBlock, .summary, .claims,
                    .evidence, .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .agent_question:
            // Section 9: the question leads as a labeled agent claim, and it
            // is answerable without the transcript, so the claim module runs
            // ahead of the action region instead of sitting in the lower
            // sections with the supporting context. The second claim module
            // carries that supporting context, which stays below the actions.
            return .init(
                modules: [
                    .recommendation, .claims, .facts, .factBlock, .summary, .claims, .evidence,
                    .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .spec_approval:
            return .init(
                modules: [
                    .recommendation, .specRevision, .facts, .specification, .factBlock, .summary,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .review_contradiction, .review_configuration,
            .publish_blocked, .run_proposal:
            return .init(
                modules: [
                    .recommendation, .facts, .factBlock, .summary, .claims, .evidence, .details,
                ],
                actionInsertionIndex: 2,
                reviewingActionInsertionIndex: nil)
        case .system_health, .blocked:
            return .init(
                modules: [.recommendation, .facts, .factBlock, .claims, .evidence, .details],
                actionInsertionIndex: 2,
                reviewingActionInsertionIndex: nil)
        }
    }
}

struct DecisionGraphicPresentations: Equatable {
    var stageRail: DecisionStageRailPresentation?
    var comparison: DecisionComparisonPresentation?
    var changeSummary: DecisionChangeSummaryPresentation?
    var attemptTimings: DecisionFactPresentation?
    var diminishingYield: DecisionYieldPresentation?
    var prominentClaimIndex: Int?

    init(
        stageRail: DecisionStageRailPresentation? = nil,
        comparison: DecisionComparisonPresentation? = nil,
        changeSummary: DecisionChangeSummaryPresentation? = nil,
        attemptTimings: DecisionFactPresentation? = nil,
        diminishingYield: DecisionYieldPresentation? = nil,
        prominentClaimIndex: Int? = nil
    ) {
        self.stageRail = stageRail
        self.comparison = comparison
        self.changeSummary = changeSummary
        self.attemptTimings = attemptTimings
        self.diminishingYield = diminishingYield
        self.prominentClaimIndex = prominentClaimIndex
    }
}

struct DecisionChecklistPresentation: Equatable {
    enum Result: Equatable {
        case passed
        case failed
        case informational

        var marker: String {
            switch self {
            case .passed: "✓"
            case .failed: "!"
            case .informational: "•"
            }
        }

        var accessibilityState: String {
            switch self {
            case .passed: "passed"
            case .failed: "needs attention"
            case .informational: "informational"
            }
        }
    }

    struct Row: Equatable, Identifiable {
        let label: String
        let value: String
        let result: Result

        var id: String { label }
    }

    let rows: [Row]
    let summary: String
    let accessibilitySummary: String

    init?(_ item: Components.Schemas.AttentionItem) {
        var rows: [Row] = []
        let detail = item.readiness_detail?.value1
        let invalidation = item.readiness_invalidation?.value1
        let freshness = item.base_freshness?.value1
        // Staleness is a daemon fact on either axis: the superseding
        // invalidation, or the base-advance watch observing a moved base. Both
        // demote the verdict and its bound coordinates; the client compares
        // nothing itself and derives no reason from the verdict class.
        let stale = invalidation != nil || freshness?.advanced == true
        if invalidation != nil {
            rows.append(
                .init(label: "Verification verdict", value: "Invalidated", result: .failed))
        } else if let readiness = item.readiness?.value1 {
            let verdict: String
            switch readiness._class {
            case .ready_clean: verdict = "Clean"
            case .ready_degraded: verdict = "Degraded"
            }
            rows.append(
                .init(
                    label: "Verification verdict",
                    value: stale ? "\(verdict), stale" : verdict,
                    result: stale || readiness._class == .ready_degraded ? .failed : .passed))
        } else if item._type == .ready_for_final_review {
            rows.append(
                .init(label: "Verification verdict", value: "Unavailable", result: .failed))
        }
        if let detail {
            rows.append(
                .init(
                    label: "Bound to",
                    value:
                        "\(AttentionDisplay.shortRevision(detail.candidate_head)) on "
                        + "\(detail.base.base_ref)@\(AttentionDisplay.shortRevision(detail.base.base_sha))",
                    result: stale ? .failed : .passed))
        }
        if let invalidation {
            rows.append(
                .init(
                    label: AttentionDisplay.label(invalidation.reason),
                    value:
                        "bound \(AttentionDisplay.shortRevision(invalidation.bound)), "
                        + "observed \(AttentionDisplay.shortRevision(invalidation.observed))",
                    result: .failed))
        }
        for requirement in detail?.requirements ?? [] {
            rows.append(Self.requirementRow(requirement))
        }
        if let notice = item.commit_plan_notice?.value1 {
            rows.append(
                .init(
                    label: "Commit plan",
                    value: AttentionDisplay.label(notice),
                    result: .informational))
        }
        if let freshness {
            rows.append(
                .init(
                    label: "Base freshness",
                    value: freshness.advanced
                        ? "Advanced past \(AttentionDisplay.shortRevision(freshness.admitted_base_sha)), "
                            + "now \(AttentionDisplay.shortRevision(freshness.observed_base_sha))"
                        : "Current",
                    result: freshness.advanced ? .failed : .passed))
        }
        if let history = item.yield_history?.value1 {
            let unresolved = history.rounds.reduce(into: 0) { count, round in
                count += round.findings_ingested - round.fixed - round.declined - round.deferred
            }
            if unresolved > 0 {
                rows.append(
                    .init(
                        label: "Terminal review",
                        value: unresolved == 1
                            ? "1 finding unresolved" : "\(unresolved) findings unresolved",
                        result: .failed))
            } else {
                switch history.terminal_outcome {
                case .clean:
                    rows.append(.init(label: "Terminal review", value: "Clean", result: .passed))
                case .findings:
                    rows.append(
                        .init(
                            label: "Terminal review",
                            value: "Findings dispositioned",
                            result: .passed))
                }
            }
        }
        guard !rows.isEmpty else { return nil }
        self.rows = rows
        let checks = rows.filter { $0.result != .informational }
        let failures = checks.filter { $0.result == .failed }.count
        let informationalCount = rows.count - checks.count
        let checkSummary =
            checks.isEmpty
            ? nil
            : failures == 0
                ? "all \(checks.count) checks passed"
                : "\(failures) of \(checks.count) checks need attention"
        let informationalSummary =
            informationalCount == 0
            ? nil
            : informationalCount == 1
                ? "1 informational note"
                : "\(informationalCount) informational notes"
        summary =
            "Readiness checklist: "
            + [checkSummary, informationalSummary].compactMap { $0 }.joined(separator: "; ") + "."
        accessibilitySummary =
            summary + " "
            + rows.map { row in
                "\(row.label): \(row.value), \(row.result.accessibilityState)"
            }.joined(separator: "; ") + "."
    }
}

extension DecisionChecklistPresentation {
    /// One evaluated requirement as a checklist row. The label is the daemon's
    /// requirement key; the value is its typed state, and a waived failure
    /// names the waiver's identity, the dimension it covers, and its granting
    /// authority (plan §6) while keeping the failure marker, so a degraded
    /// card says why it is degraded without opening the technical details.
    fileprivate static func requirementRow(
        _ requirement: Components.Schemas.ReadinessRequirement
    ) -> Row {
        let advisory = requirement.kind == .optional
        let label = advisory ? "\(requirement.requirement_key) (optional)" : requirement.requirement_key
        let state = AttentionDisplay.label(requirement.state)
        switch requirement.state {
        case .passed:
            return .init(label: label, value: state, result: .passed)
        case .not_applicable:
            return .init(label: label, value: state, result: .informational)
        case .failed, .not_run:
            if let waiver = requirement.waiver?.value1 {
                return .init(
                    label: label,
                    value:
                        "\(state), waived for \(waiver.dimension) by "
                        + AttentionDisplay.label(waiver.authority).lowercased()
                        + ", waiver \(waiver.id)",
                    result: .failed)
            }
            return .init(
                label: label, value: advisory ? "\(state) (advisory)" : state, result: .failed)
        }
    }
}

struct DecisionYieldPresentation: Equatable {
    struct Round: Equatable, Identifiable {
        let number: Int
        let newFindings: Int
        let recurringFindings: Int

        var id: Int { number }
        var total: Int { newFindings + recurringFindings }
        var text: String {
            "Round \(number): \(newFindings) new, \(recurringFindings) recurring"
        }
    }

    let rounds: [Round]
    let summary: String

    init(rounds: [Round]) {
        self.rounds = rounds
        summary = "Review yield: " + rounds.map(\.text).joined(separator: "; ") + "."
    }

    init?(_ item: Components.Schemas.AttentionItem) {
        guard let history = item.yield_history?.value1 else { return nil }
        self.init(
            rounds: history.rounds.map {
                .init(
                    number: $0.round,
                    newFindings: $0.new_findings,
                    recurringFindings: $0.recurring_findings)
            })
    }
}

struct DecisionStageRailPresentation: Equatable {
    enum State: Equatable {
        case completed
        case current
        case failed
        case pending

        var accessibilityLabel: String {
            switch self {
            case .completed: "completed"
            case .current: "current"
            case .failed: "failed"
            case .pending: "pending"
            }
        }
    }

    struct Entry: Equatable, Identifiable {
        let id: String
        let title: String
        let detail: String?
        let context: String?
        let timestamp: String?
        let state: State

        init(
            id: String,
            title: String,
            detail: String? = nil,
            context: String? = nil,
            timestamp: String? = nil,
            state: State
        ) {
            self.id = id
            self.title = title
            self.detail = detail
            self.context = context
            self.timestamp = timestamp
            self.state = state
        }

        var accessibilityLabel: String {
            [title, state.accessibilityLabel, detail, context, timestamp]
                .compactMap { $0 }
                .joined(separator: ", ")
        }
    }

    let entries: [Entry]
    let summary: String

    static func failure(stages: [String], failedStageIndex: Int) -> Self? {
        guard stages.indices.contains(failedStageIndex) else { return nil }
        let entries = stages.enumerated().map { index, stage in
            Entry(
                id: "\(index)-\(stage)",
                title: stage,
                state: index < failedStageIndex ? .completed : index == failedStageIndex ? .failed : .pending)
        }
        return .init(
            entries: entries,
            summary:
                "\(stages[failedStageIndex]) failed, stage \(failedStageIndex + 1) of \(stages.count).")
    }

    static func timeline(entries: [Entry]) -> Self {
        let summary =
            entries.isEmpty
            ? "No stage, round, or decision history recorded."
            : "Stage history: "
                + entries.map { entry in
                    [entry.title, entry.detail, entry.context, entry.timestamp]
                        .compactMap { $0 }
                        .joined(separator: ", ")
                }.joined(separator: "; ") + "."
        return .init(entries: entries, summary: summary)
    }
}

struct DecisionComparisonPresentation: Equatable {
    struct Position: Equatable, Identifiable {
        let title: String
        let text: String

        var id: String { title }
    }

    let positions: [Position]
    let verifiableFacts: [DecisionFactPresentation.Fact]
    let summary: String

    init(
        positions: [Position],
        verifiableFacts: [DecisionFactPresentation.Fact]
    ) {
        self.positions = positions
        self.verifiableFacts = verifiableFacts
        let positionSummary = positions.map { "\($0.title): \($0.text)" }.joined(separator: "; ")
        summary = "Disputed positions: \(positionSummary)."
    }
}

struct DecisionChangeSummaryPresentation: Equatable {
    let text: String
    let summary: String

    init(text: String) {
        self.text = text
        summary = "Change summary: \(text)"
    }
}

struct DecisionFactPresentation: Equatable {
    struct Fact: Equatable, Identifiable {
        let label: String
        let value: String

        var id: String { label }
    }

    let title: String
    let facts: [Fact]
}

private struct DecisionModuleContainer<Content: View>: View {
    let title: String
    var dashed = false
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: title)
            content
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.ground))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(
                    Color.rule,
                    style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : [])))
    }
}

struct DecisionChecklistModuleView: View {
    let presentation: DecisionChecklistPresentation

    var body: some View {
        DecisionModuleContainer(title: "Readiness checklist") {
            ForEach(presentation.rows) { row in
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(row.result.marker)
                        .font(FreesideFont.sans(.callout, weight: .bold))
                        .foregroundStyle(
                            row.result == .failed
                                ? Color.waxText
                                : row.result == .informational ? Color.inkDim : Color.ink)
                    Text(row.label)
                    Spacer(minLength: 8)
                    Text(row.value)
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(row.result == .failed ? Color.waxText : Color.inkDim)
                }
            }
            Text(presentation.summary)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.accessibilitySummary))
    }
}

struct DecisionYieldChartModuleView: View {
    let presentation: DecisionYieldPresentation
    var showsBars = true

    var body: some View {
        DecisionModuleContainer(title: "Review yield") {
            ForEach(presentation.rounds) { round in
                VStack(alignment: .leading, spacing: 4) {
                    Text(round.text)
                        .font(FreesideFont.monoCaption)
                    if showsBars {
                        GeometryReader { geometry in
                            let total = max(
                                presentation.rounds.map(\.total).max() ?? 1,
                                1)
                            HStack(spacing: 0) {
                                Rectangle()
                                    .fill(Color.accentBorder)
                                    .frame(
                                        width: geometry.size.width
                                            * CGFloat(round.newFindings) / CGFloat(total))
                                Rectangle()
                                    .fill(Color.waxText)
                                    .frame(
                                        width: geometry.size.width
                                            * CGFloat(round.recurringFindings) / CGFloat(total))
                            }
                        }
                        .frame(height: 8)
                        .clipShape(Capsule())
                    }
                }
            }
            Text(presentation.summary)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.summary))
    }
}

struct DecisionComparisonModuleView: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    let presentation: DecisionComparisonPresentation

    var body: some View {
        DecisionModuleContainer(title: "Positions") {
            let layout =
                dynamicTypeSize >= .accessibility1
                ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
                : AnyLayout(HStackLayout(alignment: .top, spacing: 8))
            layout {
                ForEach(presentation.positions) { position in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(position.title)
                            .font(FreesideFont.sans(.callout, weight: .semibold))
                        Text(position.text)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: 6).fill(Color.neutralWash))
                }
            }
            Text(presentation.summary)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.summary))
    }
}

struct StageRail: View {
    enum AccessibilityStyle {
        case summary
        case entries
    }

    @ScaledMetric(relativeTo: .body) private var connectorLength: CGFloat = 44
    let title: String?
    let presentation: DecisionStageRailPresentation
    let axis: Axis
    var showsSummaryText = true
    var accessibilityStyle: AccessibilityStyle = .summary

    @ViewBuilder
    var body: some View {
        switch accessibilityStyle {
        case .summary:
            rail
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(presentation.summary))
        case .entries:
            rail
        }
    }

    private var rail: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let title {
                Text(title)
                    .font(FreesideFont.title)
            }
            if axis == .vertical {
                verticalRail
            } else {
                horizontalRail
            }
            if showsSummaryText || presentation.entries.isEmpty {
                Text(presentation.summary)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
        }
    }

    private var verticalRail: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(presentation.entries.enumerated()), id: \.element.id) { index, entry in
                HStack(alignment: .top, spacing: 12) {
                    VStack(spacing: 0) {
                        marker(entry.state)
                        if index < presentation.entries.count - 1 {
                            Rectangle()
                                .fill(Color.milestoneConnector)
                                .frame(width: 2, height: connectorLength)
                                .accessibilityHidden(true)
                        }
                    }
                    entryLabel(entry)
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(entry.accessibilityLabel))
            }
        }
    }

    private var horizontalRail: some View {
        HStack(alignment: .top, spacing: 0) {
            ForEach(Array(presentation.entries.enumerated()), id: \.element.id) { index, entry in
                VStack(spacing: 6) {
                    HStack(spacing: 0) {
                        Rectangle()
                            .fill(index == 0 ? Color.clear : Color.milestoneConnector)
                            .frame(height: 2)
                        marker(entry.state)
                        Rectangle()
                            .fill(
                                index == presentation.entries.count - 1
                                    ? Color.clear : Color.milestoneConnector
                            )
                            .frame(height: 2)
                    }
                    entryLabel(entry)
                        .multilineTextAlignment(.center)
                }
                .frame(maxWidth: .infinity)
            }
        }
    }

    private func marker(_ state: DecisionStageRailPresentation.State) -> some View {
        Circle()
            .fill(markerColor(state))
            .frame(width: 10, height: 10)
            .accessibilityHidden(true)
    }

    private func markerColor(_ state: DecisionStageRailPresentation.State) -> Color {
        switch state {
        case .completed: return .milestonePrior
        case .current: return .accentBorder
        case .failed: return .waxText
        case .pending: return .milestoneConnector
        }
    }

    private func entryLabel(_ entry: DecisionStageRailPresentation.Entry) -> some View {
        VStack(alignment: axis == .vertical ? .leading : .center, spacing: 4) {
            Text(entry.title)
                .font(FreesideFont.sans(.headline, weight: .semibold))
                .foregroundStyle(entry.state == .failed ? Color.waxText : Color.ink)
            if let detail = entry.detail {
                Text(detail)
                    .font(FreesideFont.sans(.subheadline, weight: .medium))
            }
            if let context = entry.context {
                Text(context)
                    .font(FreesideFont.subheadline)
                    .foregroundStyle(Color.inkDim)
            }
            if let timestamp = entry.timestamp {
                Text(timestamp)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .frame(maxWidth: axis == .horizontal ? .infinity : nil, alignment: .leading)
    }
}
