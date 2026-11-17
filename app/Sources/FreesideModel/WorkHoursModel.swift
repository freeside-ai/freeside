import Foundation
import FreesideAPI
import Observation

/// The work status view's Over Time tab (#3242): one seven-day period of
/// `GET /work-hours`, read in the reader's zone under the list's project
/// filter, and Earlier and Later to step it.
///
/// The read is computed on request and moves no server revision, so the
/// model owns it and never caches it. It loads when the tab shows, when the
/// period or filter changes, and on Retry; the sync heartbeat doesn't
/// reload it.
@MainActor
@Observable
package final class WorkHoursModel {
    package enum LoadState: Equatable {
        case loading
        case loaded(Components.Schemas.WorkHours)
        case failed(String)
    }

    /// What the tab draws: the load state with the loaded answer projected.
    package enum Display: Equatable {
        case loading
        case loaded(WorkHoursChart)
        case failed(String)
    }

    /// What one load asked for; a loaded answer is kept on screen while a
    /// reload asks for the same.
    private struct Request: Equatable {
        let period: WorkHoursPeriod
        let timeZone: String
        let projectID: String?
    }

    package private(set) var loadState: LoadState = .loading
    /// How many periods before the one ending today the tab shows; zero
    /// is the current period.
    package private(set) var periodsBack = 0

    static let loadFailureMessage = "Couldn't load the hours. Check the connection to the daemon and try again."

    private let coordinator: SyncCoordinator
    private let now: @MainActor () -> Date
    private var loadGeneration = 0
    private var loadedRequest: Request?

    /// `now` decides which date is today; tests pin it.
    package init(coordinator: SyncCoordinator, now: @escaping @MainActor () -> Date = { Date() }) {
        self.coordinator = coordinator
        self.now = now
    }

    /// Later is off once the period ends today.
    package var canGoLater: Bool { periodsBack > 0 }

    package func period(in timeZone: TimeZone) -> WorkHoursPeriod {
        WorkHoursPeriod(endingOn: CalendarDate(containing: now(), in: timeZone), periodsBack: periodsBack)
    }

    package func earlier() { periodsBack += 1 }

    package func later() { periodsBack = max(0, periodsBack - 1) }

    /// The load state, with a loaded answer's tasks named from the tasks
    /// the client holds now, so a task that syncs later gains its name.
    package var display: Display {
        switch loadState {
        case .loading: .loading
        case .failed(let message): .failed(message)
        case .loaded(let hours):
            WorkHoursChart(hours, tasks: coordinator.tasks).map(Display.loaded)
                ?? .failed(Self.loadFailureMessage)
        }
    }

    /// Reads the period in `timeZone`, every project's runs when
    /// `projectID` is nil. An answer that arrives after a later load began
    /// is dropped. Any answer but 200, including a 400 for a zone the
    /// daemon doesn't know, is one failure.
    package func load(timeZone: TimeZone, projectID: String?) async {
        let request = Request(period: period(in: timeZone), timeZone: timeZone.identifier, projectID: projectID)
        loadGeneration += 1
        let generation = loadGeneration
        // `loadedRequest` is set only while an answer is loaded.
        if loadedRequest != request {
            loadState = .loading
            loadedRequest = nil
        }
        do {
            let output = try await coordinator.store.client.getWorkHours(
                query: .init(
                    from: request.period.first.text, days: WorkHoursPeriod.length, time_zone: request.timeZone,
                    project_id: projectID))
            guard generation == loadGeneration else { return }
            guard case .ok(let ok) = output else { return fail() }
            let hours = try ok.body.json
            guard WorkHoursChart(hours, tasks: []) != nil else { return fail() }
            loadState = .loaded(hours)
            loadedRequest = request
        } catch {
            guard generation == loadGeneration, !(error is CancellationError), !Task.isCancelled else { return }
            fail()
        }
    }

    private func fail() {
        loadState = .failed(Self.loadFailureMessage)
        loadedRequest = nil
    }
}
