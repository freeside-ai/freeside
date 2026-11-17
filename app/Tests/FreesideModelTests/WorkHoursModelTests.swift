import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// The Over Time tab's fetch (#3242) against the mock's `GET /work-hours`:
/// the request's period, zone, and filter, as the answer echoes them; the
/// load states; Earlier and Later; and an answer dropped once the period
/// or filter moved on.
@Suite @MainActor struct WorkHoursModelTests {
    private static let project = WorkStatusFixtures.project
    private static let utc = TimeZone.gmt

    private func server() async -> MockServer {
        let server = MockServer()
        await server.setWorkHours(project: Self.project, days: WorkStatusFixtures.workHoursWeekDays())
        return server
    }

    /// The model reads today as the mock's clock does.
    private func model(_ server: MockServer) -> WorkHoursModel {
        WorkHoursModel(
            coordinator: SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore()),
            now: { RunFixtures.screenshotInstant })
    }

    private func hours(_ model: WorkHoursModel) throws -> Components.Schemas.WorkHours {
        guard case .loaded(let hours) = model.loadState else {
            Issue.record("not loaded: \(model.loadState)")
            throw CancellationError()
        }
        return hours
    }

    @Test func readsTheWeekEndingTodayUnderTheFilter() async throws {
        let model = model(await server())
        #expect(model.loadState == .loading)
        await model.load(timeZone: Self.utc, projectID: Self.project)
        let hours = try hours(model)
        #expect(
            hours.days.map(\.date)
                == WorkHoursPeriod(endingOn: try #require(CalendarDate("2026-08-12"))).dates.map(\.text))
        #expect(hours.time_zone == Self.utc.identifier && hours.project_id == Self.project)
        #expect(hours.totals.elapsed_seconds > 0)
        guard case .loaded(let chart) = model.display else {
            Issue.record("no chart: \(model.display)")
            return
        }
        #expect(chart.days.count == WorkHoursPeriod.length)
    }

    /// The capture instant is still 2026-08-11 in Los Angeles, so the
    /// period ends there; with no filter, every project's runs are read.
    @Test func namesTheReadersZoneAndNoProject() async throws {
        let model = model(await server())
        let losAngeles = try #require(TimeZone(identifier: "America/Los_Angeles"))
        #expect(model.period(in: losAngeles).last.text == "2026-08-11")
        await model.load(timeZone: losAngeles, projectID: nil)
        let hours = try hours(model)
        #expect(hours.time_zone == "America/Los_Angeles" && hours.project_id == nil)
        #expect(hours.days.first?.date == "2026-08-05" && hours.days.last?.date == "2026-08-11")
    }

    @Test func earlierAndLaterStepAWeekAndLaterStopsAtToday() async throws {
        let model = model(await server())
        #expect(!model.canGoLater)
        model.later()
        #expect(model.periodsBack == 0)
        model.earlier()
        #expect(model.canGoLater && model.period(in: Self.utc).last.text == "2026-08-05")
        await model.load(timeZone: Self.utc, projectID: Self.project)
        #expect(try hours(model).days.map(\.date).first == "2026-07-30")
        model.later()
        #expect(!model.canGoLater && model.period(in: Self.utc).last.text == "2026-08-12")
    }

    @Test func aFailedFetchFailsAndRetryRecovers() async throws {
        let server = await server()
        let model = model(server)
        await server.setBeforeRespond { operationID in
            if operationID == "getWorkHours" { throw MockServer.ForcedStatus(500) }
        }
        await model.load(timeZone: Self.utc, projectID: Self.project)
        #expect(model.loadState == .failed(WorkHoursModel.loadFailureMessage))
        #expect(model.display == .failed(WorkHoursModel.loadFailureMessage))

        await server.setBeforeRespond(nil)
        await model.load(timeZone: Self.utc, projectID: Self.project)
        _ = try hours(model)
    }

    /// The daemon's 404 for a project it doesn't have is the same failure.
    @Test func anUnknownProjectFails() async throws {
        let model = model(await server())
        await model.load(timeZone: Self.utc, projectID: "no-such-project")
        #expect(model.loadState == .failed(WorkHoursModel.loadFailureMessage))
    }

    /// A reload of the same period keeps the answer on screen; a load of
    /// another period shows loading until it answers.
    @Test func aReloadKeepsTheAnswerAndANewPeriodLoads() async throws {
        let server = await server()
        let model = model(server)
        await model.load(timeZone: Self.utc, projectID: Self.project)
        let first = try hours(model)
        let hold = FirstCallHold()
        await server.setBeforeRespond { if $0 == "getWorkHours" { await hold.pass() } }

        let reload = Task { await model.load(timeZone: Self.utc, projectID: Self.project) }
        await hold.reached.wait()
        #expect(try hours(model) == first)
        await hold.release.open()
        await reload.value

        let nextHold = FirstCallHold()
        await server.setBeforeRespond { if $0 == "getWorkHours" { await nextHold.pass() } }
        model.earlier()
        let next = Task { await model.load(timeZone: Self.utc, projectID: Self.project) }
        await nextHold.reached.wait()
        #expect(model.loadState == .loading)
        await nextHold.release.open()
        await next.value
        #expect(try hours(model).days.first?.date == "2026-07-30")
    }

    @Test func dropsAnAnswerThatArrivesAfterThePeriodChanged() async throws {
        let server = await server()
        let model = model(server)
        let hold = FirstCallHold()
        await server.setBeforeRespond { if $0 == "getWorkHours" { await hold.pass() } }

        let stale = Task { await model.load(timeZone: Self.utc, projectID: Self.project) }
        await hold.reached.wait()
        model.earlier()
        await model.load(timeZone: Self.utc, projectID: Self.project)
        #expect(try hours(model).days.first?.date == "2026-07-30")

        await hold.release.open()
        await stale.value
        #expect(try hours(model).days.first?.date == "2026-07-30")
    }

    @Test func dropsAnAnswerThatArrivesAfterTheFilterChanged() async throws {
        let server = await server()
        let model = model(server)
        let hold = FirstCallHold()
        await server.setBeforeRespond { if $0 == "getWorkHours" { await hold.pass() } }

        let stale = Task { await model.load(timeZone: Self.utc, projectID: Self.project) }
        await hold.reached.wait()
        await model.load(timeZone: Self.utc, projectID: nil)
        #expect(try hours(model).project_id == nil)

        await hold.release.open()
        await stale.value
        #expect(try hours(model).project_id == nil)
    }
}

/// Holds the first call that passes until `release` opens, and lets every
/// later call through.
private actor FirstCallHold {
    let reached = AsyncGate()
    let release = AsyncGate()
    private var held = false

    func pass() async {
        guard !held else { return }
        held = true
        await reached.open()
        await release.wait()
    }
}
