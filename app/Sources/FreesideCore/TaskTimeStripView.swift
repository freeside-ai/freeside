import FreesideAPI
import SwiftUI

/// Where a task's time went across its runs, to scale (#3234): one bar
/// from the first run's start to the last run's end, each run's part cut
/// as the run page's strip cuts it, and the time between runs drawn as a
/// thin line. Each run's part is a button to its run history, and the
/// Elapsed line ends in a menu of every run of the task (#3373).
struct TaskTimeStripView: View {
    /// One run the trailing menu lists.
    struct RunEntry: Identifiable, Equatable {
        let id: String
        /// As the page's run cards name it.
        let title: String
    }

    let strip: TaskTimeStrip.Strip
    /// Every run of the task in the timeline's order: the trailing menu
    /// lists them all, and the bar's parts take their titles from them.
    let runs: [RunEntry]
    /// What choosing a run in the trailing menu does.
    let onChooseRun: (String) -> Void
    /// Opens a run's history. Nil (a screenshot) leaves each run's part a
    /// button that does nothing.
    var onOpenRun: ((String) -> Void)? = nil
    /// False draws the trailing menu's label alone, for a renderer that
    /// can't draw a menu.
    var rendersInteractiveControls = true
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    /// The legend's swatch grows with its text.
    @ScaledMetric(relativeTo: .callout) private var swatch: CGFloat = 9

    private static let barHeight: CGFloat = 12
    /// Taller than the bar, so a thin part's button still takes a press
    /// above and below it.
    private static let hitPadding: CGFloat = 8
    /// The line drawn across time no run spans: thinner than any run's
    /// part, so it reads as a gap by its shape in every contrast setting,
    /// not only by its shade.
    private static let betweenRunsHeight: CGFloat = 2

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            // The runs control trails the heading on its line, and takes a
            // line of its own under it where the column can't hold both.
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    heading
                    Spacer(minLength: 8)
                    runsControl
                }
                VStack(alignment: .leading, spacing: 4) {
                    heading
                    runsControl
                        .frame(maxWidth: .infinity, alignment: .trailing)
                }
            }
            bar
            WrappingHStack(horizontalSpacing: 14, verticalSpacing: 4) {
                ForEach(Self.legendKinds(strip, locale: locale), id: \.self) { kind in
                    legendEntry(
                        swatch: RoundedRectangle(cornerRadius: 2).fill(RunTimeStripView.color(kind))
                            .frame(width: swatch, height: swatch),
                        label: RunTimeStripView.label(kind), total: strip.total(kind))
                }
                if Self.showsBetweenRuns(strip, locale: locale) {
                    legendEntry(
                        swatch: Capsule().fill(Color.ruleStrong)
                            .frame(width: swatch, height: Self.betweenRunsHeight)
                            .frame(height: swatch),
                        label: Self.betweenRunsLabel, total: strip.betweenRuns)
                }
            }
            .accessibilityHidden(true)
        }
    }

    private var heading: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            KeywordLabel(text: "Elapsed")
            Text(RunTimeStripView.duration(strip.elapsed, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
            Text(Self.runCount(strip.runCount))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
        // The legend says the same in words; the heading reads it once.
        // The runs control stays outside it, its own element.
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Self.summary(strip, locale: locale))
        .accessibilityAddTraits(.isHeader)
    }

    /// The task's runs: a menu of them, or with one run a button to it.
    /// Either opens what the outline row's `Runs` popup opens.
    private var runsControl: some View {
        runsControlContent
            // The label's padding widens the press and the focus ring, as
            // the task's actions menu's does, but lays out as the text
            // alone, so the text stays flush right and the line keeps its
            // height.
            .padding(.horizontal, -Self.runsLabelInset.width)
            .padding(.vertical, -Self.runsLabelInset.height)
    }

    private static let runsLabelInset = CGSize(width: 8, height: 5)

    @ViewBuilder private var runsControlContent: some View {
        if !rendersInteractiveControls {
            if !runs.isEmpty { runsLabel }
        } else if let only = runs.first, runs.count == 1 {
            Button {
                onChooseRun(only.id)
            } label: {
                runsLabel
            }
            .buttonStyle(.plain)
            .runsControlChrome()
            .accessibilityLabel("Runs, 1")
            .accessibilityHint("Opens the run.")
        } else if !runs.isEmpty {
            Menu {
                ForEach(runs) { entry in
                    Button(entry.title) { onChooseRun(entry.id) }
                }
            } label: {
                runsLabel
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .menuIndicator(.hidden)
            .runsControlChrome()
            .accessibilityLabel("Runs, \(runs.count)")
            .accessibilityHint("Lists the task's runs. Choosing one opens it.")
        }
    }

    /// Dim at the label step, semibold, with the menu mark the task's
    /// actions menu draws when it is a menu. One run's button has none, as
    /// the outline row's chip with one entry has none.
    private var runsLabel: some View {
        Text(runs.count > 1 ? "Runs \(runs.count) \u{25BE}" : "Runs \(runs.count)")
            .font(FreesideFont.textMenu)
            .foregroundStyle(Color.inkDim)
            .padding(.horizontal, Self.runsLabelInset.width)
            .padding(.vertical, Self.runsLabelInset.height)
            .contentShape(Rectangle())
    }

    /// The kinds the legend names: each whose total shows as more than
    /// `0m`, judged on the text the legend draws, so a kind under half a
    /// minute is left out too. With none, `Other` stays, so the legend is
    /// never empty. The spoken summary still names every kind.
    static func legendKinds(_ strip: TaskTimeStrip.Strip, locale: Locale) -> [RunTimeStrip.Kind] {
        let kinds = RunTimeStrip.Kind.allCases.filter { showsTime(strip.total($0), locale: locale) }
        return kinds.isEmpty ? [.other] : kinds
    }

    /// Whether the legend names `Between runs`, by the same rule: one run
    /// has no time between runs, so it names none.
    static func showsBetweenRuns(_ strip: TaskTimeStrip.Strip, locale: Locale) -> Bool {
        showsTime(strip.betweenRuns, locale: locale)
    }

    private static func showsTime(_ total: TimeInterval, locale: Locale) -> Bool {
        RunTimeStripView.duration(total, locale: locale) != RunTimeStripView.duration(0, locale: locale)
    }

    /// One part of the bar as the reader meets it: its spoken label, and
    /// the run it opens, nil for time between runs.
    struct Item {
        let part: TaskTimeStrip.Part
        let label: String
        let opensRunID: String?
    }

    /// The bar's parts in order, each labelled with its run's title (or
    /// `Between runs`), its length, and its span.
    static func items(
        _ strip: TaskTimeStrip.Strip, titles: [String: String], now: Date, locale: Locale, timeZone: TimeZone
    ) -> [Item] {
        strip.parts.map { part in
            let name: String
            let opens: String?
            switch part {
            case .run(let run):
                name = titles[run.runID] ?? "Run \(ShortIdentifier.short(run.runID))"
                opens = run.runID
            case .betweenRuns:
                name = betweenRunsLabel
                opens = nil
            }
            let from = FreesideFormat.shortTime(part.start, now: now, locale: locale, timeZone: timeZone)
            let to = FreesideFormat.shortTime(part.end, now: now, locale: locale, timeZone: timeZone)
            let length = RunTimeStripView.duration(part.duration, locale: locale, width: .wide)
            return Item(part: part, label: "\(name), \(length), \(from) to \(to)", opensRunID: opens)
        }
    }

    private var bar: some View {
        // The first title wins where the timeline lists a run twice.
        let titles = Dictionary(runs.map { ($0.id, $0.title) }, uniquingKeysWith: { first, _ in first })
        let items = Self.items(
            strip, titles: titles, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
        return GeometryReader { proxy in
            HStack(spacing: 0) {
                ForEach(Array(items.enumerated()), id: \.offset) { index, item in
                    let width = proxy.size.width * CGFloat(item.part.duration / strip.elapsed)
                    // A clear gap opens each later part, wider than the
                    // hairline between a run's own segments, so runs read
                    // apart; a part too thin to spare it keeps its fill.
                    let gap: CGFloat = index > 0 && width > 6 ? 3 : 0
                    if case .run(let run) = item.part, let runID = item.opensRunID {
                        Button {
                            onOpenRun?(runID)
                        } label: {
                            segments(run, width: width, gap: gap)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel(item.label)
                        .accessibilityHint("Opens its run history.")
                        .help(item.label)
                    } else {
                        Capsule()
                            .fill(Color.ruleStrong)
                            .frame(height: Self.betweenRunsHeight)
                            .padding(.leading, gap)
                            .frame(width: width, height: Self.barHeight + 2 * Self.hitPadding)
                            .accessibilityElement()
                            .accessibilityLabel(item.label)
                            .help(item.label)
                    }
                }
            }
            .mask {
                RoundedRectangle(cornerRadius: 3).frame(height: Self.barHeight)
            }
        }
        .frame(height: Self.barHeight + 2 * Self.hitPadding)
    }

    /// One run's part: its segments in the run strip's colours.
    private func segments(_ run: TaskTimeStrip.RunPart, width: CGFloat, gap: CGFloat) -> some View {
        let fill = max(width - gap, 0)
        return HStack(spacing: 0) {
            ForEach(Array(run.segments.enumerated()), id: \.offset) { index, segment in
                let segmentWidth = fill * CGFloat(segment.duration / run.duration)
                Rectangle()
                    .fill(RunTimeStripView.color(segment.kind))
                    .padding(.leading, index > 0 && segmentWidth > 3 ? 1 : 0)
                    .frame(width: segmentWidth, height: Self.barHeight)
            }
        }
        .padding(.leading, gap)
        .frame(width: width, alignment: .leading)
        .padding(.vertical, Self.hitPadding)
        .contentShape(Rectangle())
    }

    private func legendEntry(swatch: some View, label: String, total: TimeInterval) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            swatch
            Text(label)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
            Text(RunTimeStripView.duration(total, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
    }

    static let betweenRunsLabel = "Between runs"

    static func runCount(_ count: Int) -> String {
        count == 1 ? "across 1 run" : "across \(count) runs"
    }

    /// The strip in one sentence for assistive technology: the elapsed
    /// time and run count, then each kind's total, between runs only when
    /// there is more than one run.
    static func summary(_ strip: TaskTimeStrip.Strip, locale: Locale) -> String {
        let kinds = RunTimeStrip.Kind.allCases.map { kind in
            (RunTimeStripView.label(kind), strip.total(kind))
        }
        let between = strip.runCount > 1 ? [(betweenRunsLabel, strip.betweenRuns)] : []
        let totals = (kinds + between).map { label, total in
            "\(label.lowercased()) \(RunTimeStripView.duration(total, locale: locale, width: .wide))"
        }
        return "Elapsed \(RunTimeStripView.duration(strip.elapsed, locale: locale, width: .wide)) "
            + "\(runCount(strip.runCount)): " + totals.joined(separator: ", ") + "."
    }
}

extension View {
    /// The runs control's chrome, shared by its menu and its one-run button.
    fileprivate func runsControlChrome() -> some View {
        fixedSize()
            .freesideFocusRing(cornerRadius: 4)
            // The window's selection default would make the label a
            // selectable text view that answers for the control's spoken
            // label: the construct the ready card's accessibility recursion
            // ran through (#2993). A control's label is not selectable
            // (app/README.md, Copyable Text).
            .textSelection(.disabled)
    }
}
