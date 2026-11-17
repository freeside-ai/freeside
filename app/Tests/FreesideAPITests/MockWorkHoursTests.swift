import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// The mock's `GET /work-hours` (#3241): it serves fixture days through the
/// generated client, bounds each day in the requested zone as the daemon
/// does, and refuses the requests the daemon refuses with the same
/// messages.
@Suite struct MockWorkHoursTests {
    private static let project = WorkStatusFixtures.project

    private func server() async -> MockServer {
        let server = MockServer()
        await server.setWorkHours(project: Self.project, days: WorkStatusFixtures.workHoursDays())
        return server
    }

    @Test func servesFixtureDaysAndZeroDays() async throws {
        let client = APIClientFactory.mock(server: await server())
        let hours = try await client.getWorkHours(
            query: .init(from: "2026-08-09", days: 4, time_zone: "UTC", project_id: Self.project)
        ).ok.body.json
        #expect(hours.days.map(\.date) == ["2026-08-09", "2026-08-10", "2026-08-11", "2026-08-12"])
        #expect(hours.project_id == Self.project && hours.time_zone == "UTC")
        #expect(hours.days[0].elapsed_seconds == 0 && hours.days[0].tasks.isEmpty)
        #expect(hours.days[1].elapsed_seconds == 10_800)
        #expect(hours.days[1].tasks.map(\.task_id) == ["task-implement", "task-review"])
        #expect(hours.totals.elapsed_seconds == 14_400 && hours.totals.agent_working_seconds == 10_800)
        #expect(hours.totals.held_by_reason.map(\.reason) == [.usage_pool_exhausted, .base_advanced])
        #expect(hours.totals.task_count == 2)
    }

    /// A day runs from the first instant of its date in the requested zone:
    /// 23 hours across New York's spring change, and from the change itself
    /// where Santiago's skips midnight.
    @Test func boundsEachDayInTheRequestedZone() async throws {
        let client = APIClientFactory.mock(server: await server())
        let newYork = try await client.getWorkHours(
            query: .init(from: "2026-03-08", days: 1, time_zone: "America/New_York")
        ).ok.body.json
        let day = try #require(newYork.days.first)
        #expect(day.ends_at.timeIntervalSince(day.starts_at) == 23 * 3_600)
        #expect(day.starts_at == ISO8601DateFormatter().date(from: "2026-03-08T05:00:00Z"))

        let santiago = try await client.getWorkHours(
            query: .init(from: "2026-09-05", days: 2, time_zone: "America/Santiago")
        ).ok.body.json
        #expect(
            santiago.days.map(\.starts_at)
                == ["2026-09-05T04:00:00Z", "2026-09-06T04:00:00Z"].map {
                    ISO8601DateFormatter().date(from: $0)
                })
    }

    /// Names the daemon's zone database holds, which Foundation's list of
    /// known identifiers partly omits.
    @Test(arguments: ["Asia/Kolkata", "Etc/UTC", "US/Eastern", "EST5EDT", "Etc/GMT+5", "UTC", "GMT"])
    func acceptsWhatTheDaemonAccepts(zone: String) async throws {
        let hours = try await APIClientFactory.mock(server: await server()).getWorkHours(
            query: .init(from: "2026-08-10", days: 1, time_zone: zone)
        ).ok.body.json
        #expect(hours.time_zone == zone)
    }

    @Test func limitsToAProjectAndRefusesAnUnknownOne() async throws {
        let server = await server()
        await server.setWorkHours(
            project: WorkStatusFixtures.otherProject, days: Array(WorkStatusFixtures.workHoursDays().prefix(1)))
        let client = APIClientFactory.mock(server: server)
        let every = try await client.getWorkHours(query: .init(from: "2026-08-10", days: 2, time_zone: "UTC"))
            .ok.body.json
        #expect(every.project_id == nil && every.totals.elapsed_seconds == 2 * 10_800 + 3_600)
        let other = try await client.getWorkHours(
            query: .init(from: "2026-08-10", days: 2, time_zone: "UTC", project_id: WorkStatusFixtures.otherProject)
        ).ok.body.json
        #expect(other.totals.elapsed_seconds == 10_800)
        let missing = try await client.getWorkHours(
            query: .init(from: "2026-08-10", days: 2, time_zone: "UTC", project_id: "no-such-project"))
        #expect(throws: Never.self) { try missing.notFound }
    }

    /// The daemon's 400s, by their messages (`TestHTTPWorkHours`).
    @Test(arguments: [
        ("days=2&time_zone=UTC", "from is required"),
        ("from=2026-08-12&from=2026-08-13&days=2&time_zone=UTC", "from must be given once"),
        ("from=2026-8-12&days=2&time_zone=UTC", "from must be a calendar date, YYYY-MM-DD"),
        ("from=2026-02-30&days=2&time_zone=UTC", "from must be a calendar date, YYYY-MM-DD"),
        ("from=2026-08-12&time_zone=UTC", "days is required"),
        ("from=0000-12-31&days=1&time_zone=UTC", "from must be a calendar date, YYYY-MM-DD"),
        ("from=9999-12-01&days=31&time_zone=UTC", "the period must end by 9999-12-30"),
        ("from=2026-08-12&days=2&time_zone=GMT%2B0500", "time_zone \"GMT+0500\" is not a known IANA time zone"),
        ("from=2026-08-12&days=two&time_zone=UTC", "days must be an integer from 1 to 92"),
        ("from=2026-08-12&days=0&time_zone=UTC", "days must be an integer from 1 to 92"),
        ("from=2026-08-12&days=93&time_zone=UTC", "days must be an integer from 1 to 92"),
        ("from=2026-08-12&days=2", "time_zone is required"),
        ("from=2026-08-12&days=2&time_zone=", "time_zone must not be empty"),
        ("from=2026-08-12&days=2&time_zone=Local", "time_zone \"Local\" is not a known IANA time zone"),
        ("from=2026-08-12&days=2&time_zone=Mars/Olympus", "time_zone \"Mars/Olympus\" is not a known IANA time zone"),
        ("from=2026-08-12&days=2&time_zone=UTC&project_id=", "project_id must not be empty"),
        ("from=2026-08-12&days=2&time_zone=UTC&project_id=a&project_id=b", "project_id must be given once"),
    ])
    func refusesWhatTheDaemonRefuses(query: String, message: String) async throws {
        let request = HTTPRequest(method: .get, scheme: nil, authority: nil, path: "/work-hours?" + query)
        let (response, body) = try await MockServerTransport(server: await server()).send(
            request, body: nil, baseURL: try #require(URL(string: "https://freeside.invalid")),
            operationID: "getWorkHours")
        #expect(response.status == .badRequest)
        let data = try await Data(collecting: try #require(body), upTo: 1 << 16)
        let error = try JSONDecoder().decode(Components.Schemas._Error.self, from: data)
        #expect(error.message == message)
    }
}
