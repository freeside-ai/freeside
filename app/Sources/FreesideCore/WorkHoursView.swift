import FreesideAPI
import SwiftUI

/// The work status view's Over Time tab (#3242): how a period's hours
/// divided by class, one bar per day in the run time strips' classes,
/// colours, and order, each drawn to the scale of the period's largest
/// day. A day is a button that shows its classes, held reasons, and tasks
/// below the bars. It reads and links; it never acts on a task.
struct WorkHoursView: View {
    let display: WorkHoursModel.Display
    let period: WorkHoursPeriod
    let canGoLater: Bool
    let onEarlier: () -> Void
    let onLater: () -> Void
    let onRetry: () -> Void
    /// Opens the page of a task the client holds.
    let onOpenTask: (String) -> Void
    @State private var selectedDate: CalendarDate?
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    /// The legend's swatch grows with its text.
    @ScaledMetric(relativeTo: .callout) private var swatch: CGFloat = 9

    private static let barHeight: CGFloat = 12

    /// `selectedDate` selects that day, so a screenshot can show its
    /// detail.
    init(
        display: WorkHoursModel.Display, period: WorkHoursPeriod, canGoLater: Bool,
        onEarlier: @escaping () -> Void = {}, onLater: @escaping () -> Void = {},
        onRetry: @escaping () -> Void = {}, onOpenTask: @escaping (String) -> Void = { _ in },
        selectedDate: CalendarDate? = nil
    ) {
        self.display = display
        self.period = period
        self.canGoLater = canGoLater
        self.onEarlier = onEarlier
        self.onLater = onLater
        self.onRetry = onRetry
        self.onOpenTask = onOpenTask
        _selectedDate = State(initialValue: selectedDate)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            periodControls
            switch display {
            case .loading:
                Text("Loading hours…")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            case .failed(let message):
                Notice(
                    tone: .wax, keyword: "Failed", sentence: message,
                    action: .init(label: "Retry", accessibilityLabel: "Retry loading the hours", handler: onRetry))
            case .loaded(let chart):
                loaded(chart)
            }
        }
    }

    private var now: Date { pinnedNow ?? Date() }

    // MARK: Period

    private var periodControls: some View {
        ViewThatFits(in: .horizontal) {
            HStack(alignment: .center, spacing: 8) {
                rangeText
                Spacer(minLength: 8)
                stepButtons
            }
            VStack(alignment: .leading, spacing: 8) {
                rangeText.fixedSize(horizontal: false, vertical: true)
                stepButtons
            }
        }
    }

    private var rangeText: some View {
        Text("\(dayName(period.first, weekday: false)) to \(dayName(period.last, weekday: false))")
            .font(FreesideFont.rowTitle)
            .foregroundStyle(Color.ink)
    }

    private var stepButtons: some View {
        HStack(spacing: 8) {
            Button("Earlier", action: onEarlier)
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary, expands: false))
                .accessibilityLabel("Earlier seven days")
            Button("Later", action: onLater)
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary, expands: false))
                .disabled(!canGoLater)
                .accessibilityLabel("Later seven days")
        }
    }

    // MARK: Loaded

    @ViewBuilder
    private func loaded(_ chart: WorkHoursChart) -> some View {
        if !chart.isEmpty {
            totals(chart)
        }
        if chart.skippedRunCount > 0 {
            Notice(tone: .accent, keyword: "Incomplete", sentence: Self.incompleteSentence(chart.skippedRunCount))
                .accessibilityElement(children: .combine)
        }
        if chart.startsBeforeHoldHistory {
            Notice(
                tone: .neutral, keyword: "Holds",
                sentence:
                    "Freeside began recording holds on \(FreesideFormat.shortTime(chart.holdHistorySince, now: now, locale: locale, timeZone: timeZone)). Held time before then counts as other."
            )
            .accessibilityElement(children: .combine)
        }
        if chart.isEmpty {
            Text("No run had time on these days.")
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        } else {
            VStack(alignment: .leading, spacing: 4) {
                ForEach(chart.days) { dayRow($0, scale: chart.scale) }
            }
            WrappingHStack(horizontalSpacing: 14, verticalSpacing: 4) {
                ForEach(chart.classes, id: \.kind) { legendEntry($0) }
            }
            // The totals heading says the same in one sentence.
            .accessibilityHidden(true)
            if let day = chart.days.first(where: { $0.date == selectedDate }) {
                detail(day)
            }
        }
    }

    /// The period's elapsed time and task count, read as one heading
    /// sentence with each class's total, as the strips' heading is.
    private func totals(_ chart: WorkHoursChart) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            KeywordLabel(text: "Elapsed")
            Text(RunTimeStripView.duration(chart.elapsed, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
            Text(Self.taskCount(chart.taskCount))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Self.summary(chart, locale: locale))
        .accessibilityAddTraits(.isHeader)
    }

    private func dayRow(_ day: WorkHoursChart.Day, scale: TimeInterval) -> some View {
        let isSelected = day.date == selectedDate
        let marks = Self.marks(day, locale: locale, timeZone: timeZone)
        return Button {
            selectedDate = day.date
        } label: {
            VStack(alignment: .leading, spacing: 6) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(dayName(day.date))
                        .font(FreesideFont.rowTitle)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                    Spacer(minLength: 8)
                    Text(RunTimeStripView.duration(day.elapsed, locale: locale))
                        .font(FreesideFont.trailingSummary)
                        .foregroundStyle(Color.ink)
                }
                bar(day, scale: scale)
                if !marks.isEmpty {
                    Text(marks.joined(separator: " · "))
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .modifier(SidebarRowSurface(isSelected: isSelected))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .freesideFocusRing(cornerRadius: 4)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Self.dayLabel(day, marks: marks, now: now, locale: locale, timeZone: timeZone))
        .accessibilityHint("Shows this day's tasks")
        .accessibilityAddTraits(isSelected ? [.isButton, .isSelected] : .isButton)
    }

    /// The day's classes stacked in the strips' order on a faint track,
    /// at the period's scale. A clear hairline opens each later class, as
    /// in the strip, unless the class is too thin to spare it.
    private func bar(_ day: WorkHoursChart.Day, scale: TimeInterval) -> some View {
        GeometryReader { proxy in
            HStack(spacing: 0) {
                let drawn = day.classes.filter { $0.seconds > 0 }
                ForEach(Array(drawn.enumerated()), id: \.element.kind) { index, entry in
                    let width = scale > 0 ? proxy.size.width * CGFloat(entry.seconds / scale) : 0
                    Rectangle()
                        .fill(RunTimeStripView.color(entry.kind))
                        .padding(.leading, index > 0 && width > 3 ? 1 : 0)
                        .frame(width: width)
                }
                Spacer(minLength: 0)
            }
            // A zero day draws the track alone, at the bar's height.
            .frame(width: proxy.size.width, height: proxy.size.height, alignment: .leading)
            .background(Color.rule)
            .mask { RoundedRectangle(cornerRadius: 3) }
        }
        .frame(height: Self.barHeight)
        .accessibilityHidden(true)
    }

    private func legendEntry(_ entry: WorkHoursChart.ClassTime) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            RoundedRectangle(cornerRadius: 2)
                .fill(RunTimeStripView.color(entry.kind))
                .frame(width: swatch, height: swatch)
            Text(RunTimeStripView.label(entry.kind))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
            Text(RunTimeStripView.duration(entry.seconds, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
    }

    // MARK: Selected Day

    /// The selected day's marks, its classes with their held reasons, then
    /// its tasks, largest first. A task the client holds opens its page;
    /// any other shows its id and opens nothing.
    private func detail(_ day: WorkHoursChart.Day) -> some View {
        let marks = Self.marks(day, locale: locale, timeZone: timeZone)
        return VStack(alignment: .leading, spacing: 10) {
            Text(dayName(day.date))
                .font(FreesideFont.rowTitle)
                .foregroundStyle(Color.ink)
                .accessibilityAddTraits(.isHeader)
            if !marks.isEmpty {
                Text(marks.joined(separator: " · "))
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
            VStack(alignment: .leading, spacing: 4) {
                ForEach(day.reportedClasses, id: \.kind) { entry in
                    figure(
                        RunTimeStripView.label(entry.kind), seconds: entry.seconds,
                        swatch: RunTimeStripView.color(entry.kind))
                    if entry.kind == .held {
                        ForEach(day.held, id: \.reason) { held in
                            figure(RunDisplay.label(held.reason), seconds: held.seconds, swatch: nil)
                                .padding(.leading, swatch + 6)
                        }
                    }
                }
            }
            if day.tasks.isEmpty {
                Text("No task had time this day.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            } else {
                VStack(alignment: .leading, spacing: 6) {
                    KeywordLabel(text: "Tasks")
                        .accessibilityAddTraits(.isHeader)
                    ForEach(day.tasks) { taskRow($0) }
                }
            }
        }
    }

    private func figure(_ label: String, seconds: TimeInterval, swatch color: Color?) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            if let color {
                RoundedRectangle(cornerRadius: 2)
                    .fill(color)
                    .frame(width: swatch, height: swatch)
            }
            Text(label)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            Text(RunTimeStripView.duration(seconds, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(label), \(RunTimeStripView.duration(seconds, locale: locale, width: .wide))")
    }

    @ViewBuilder
    private func taskRow(_ task: WorkHoursChart.TaskTime) -> some View {
        let duration = RunTimeStripView.duration(task.seconds, locale: locale)
        let spoken = RunTimeStripView.duration(task.seconds, locale: locale, width: .wide)
        if let name = task.name {
            Button {
                onOpenTask(task.id)
            } label: {
                taskLine(Text(name).font(FreesideFont.rowTitle), duration: duration)
                    .modifier(SidebarRowSurface(isSelected: false))
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("\(name), \(spoken)")
            .accessibilityHint("Opens the task")
            .accessibilityAddTraits(.isButton)
        } else {
            // No row surface: the card and its hover would offer a press
            // that opens nothing. The inset keeps the id under the names.
            taskLine(Text(task.id).font(FreesideFont.monoValue), duration: duration)
                .padding(.horizontal, SidebarRowSurface.padding.width)
                .padding(.vertical, SidebarRowSurface.padding.height)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("Task \(task.id), \(spoken)")
        }
    }

    private func taskLine(_ title: Text, duration: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            title
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            Text(duration)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
    }

    // MARK: Words

    /// A date as the period names it, `Wed, Aug 12`, with the year when it
    /// isn't the reader's current year. Formatted in GMT at the date's
    /// noon, so the reader's zone can't move it to another date.
    private func dayName(_ date: CalendarDate, weekday: Bool = true) -> String {
        Self.dayName(date, weekday: weekday, now: now, locale: locale, timeZone: timeZone)
    }

    static func dayName(
        _ date: CalendarDate, weekday: Bool, now: Date, locale: Locale, timeZone: TimeZone
    ) -> String {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        var style = Date.FormatStyle(locale: locale, calendar: calendar, timeZone: .gmt).month(.abbreviated).day()
        if weekday { style = style.weekday(.abbreviated) }
        if date.year != CalendarDate(containing: now, in: timeZone).year { style = style.year() }
        return date.noonUTC.formatted(style)
    }

    /// Today so far, and where hold history doesn't cover the day.
    static func marks(_ day: WorkHoursChart.Day, locale: Locale, timeZone: TimeZone) -> [String] {
        var marks: [String] = []
        if day.isPartial { marks.append("So far today") }
        switch day.holdHistory {
        case .recorded: break
        case .none: marks.append("No hold history")
        case .began(let since):
            let style = Date.FormatStyle(locale: locale, timeZone: timeZone).hour().minute()
            marks.append("Hold history from \(since.formatted(style))")
        }
        return marks
    }

    /// The day, its elapsed time, each class it reports, and its marks:
    /// the day's accessible label.
    static func dayLabel(
        _ day: WorkHoursChart.Day, marks: [String], now: Date, locale: Locale, timeZone: TimeZone
    ) -> String {
        let classes = day.reportedClasses.map { entry in
            "\(RunTimeStripView.label(entry.kind).lowercased()) \(RunTimeStripView.duration(entry.seconds, locale: locale, width: .wide))"
        }
        let elapsed = RunTimeStripView.duration(day.elapsed, locale: locale, width: .wide)
        return
            "\(dayName(day.date, weekday: true, now: now, locale: locale, timeZone: timeZone)), elapsed \(elapsed): \(classes.joined(separator: ", "))."
            + marks.map { " \($0)." }.joined()
    }

    /// The period's totals in one sentence for assistive technology: the
    /// elapsed time and task count, then each class's total.
    static func summary(_ chart: WorkHoursChart, locale: Locale) -> String {
        let classes = chart.classes.map { entry in
            "\(RunTimeStripView.label(entry.kind).lowercased()) \(RunTimeStripView.duration(entry.seconds, locale: locale, width: .wide))"
        }
        return
            "Elapsed \(RunTimeStripView.duration(chart.elapsed, locale: locale, width: .wide)) across \(taskCount(chart.taskCount)): "
            + classes.joined(separator: ", ") + "."
    }

    static func taskCount(_ count: Int) -> String {
        count == 1 ? "1 task" : "\(count) tasks"
    }

    static func incompleteSentence(_ skipped: Int) -> String {
        skipped == 1
            ? "1 run was left out because its record failed the daemon's check, so these figures are incomplete."
            : "\(skipped) runs were left out because their records failed the daemon's check, so these figures are incomplete."
    }
}
