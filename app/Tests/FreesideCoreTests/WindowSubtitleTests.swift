import Foundation
import Testing

@testable import FreesideCore

@Suite struct WindowSubtitleTests {
    private func subtitle(
        _ environment: FreesideEnvironment, url: String? = nil, readiness: String? = nil
    ) -> String? {
        WindowSubtitle.text(
            environment: environment, serverURL: url.flatMap { URL(string: $0) },
            readinessDirectory: readiness.map { URL(fileURLWithPath: $0, isDirectory: true) })
    }

    @Test func prodShowsNothingOnItsOwnDaemon() {
        #expect(subtitle(.prod) == nil)
        #expect(subtitle(.prod, url: "http://127.0.0.1:7331") == nil)
    }

    @Test func prodOnAnyOtherPortIsARealWorkRun() {
        #expect(subtitle(.prod, url: "http://127.0.0.1:49152") == "Real-work run on :49152")
        // The known limit: nothing distinguishes the dev daemon from a run.
        #expect(subtitle(.prod, url: "http://127.0.0.1:7332") == "Real-work run on :7332")
    }

    @Test func aURLWithNoPortShowsNothing() {
        #expect(subtitle(.prod, url: "https://daemon.example") == nil)
    }

    @Test func devShowsNothing() {
        #expect(subtitle(.dev) == nil)
        #expect(subtitle(.dev, url: "http://127.0.0.1:7332") == nil)
        #expect(subtitle(.dev, url: "http://127.0.0.1:49152") == nil)
    }

    @Test func ephemeralBeforeConnectingSaysEphemeral() {
        #expect(subtitle(.ephemeral) == "Ephemeral")
    }

    @Test func ephemeralWithOnlyAURLShowsItsPort() {
        #expect(subtitle(.ephemeral, url: "http://127.0.0.1:49152") == ":49152")
    }

    @Test func ephemeralFromDevInstanceNamesTheTierAndTheWorktree() {
        #expect(
            subtitle(
                .ephemeral, url: "http://127.0.0.1:49152",
                readiness: "/Users/op/src/freeside-wt/.dev-instance.Ab12Cd/daemon")
                == "Ephemeral · freeside-wt")
        #expect(
            subtitle(.ephemeral, readiness: "/Users/op/src/freeside-wt/.dev-instance.Ab12Cd/daemon")
                == "Ephemeral · freeside-wt")
    }

    // The primary checkout's directory shares the product's name, so the
    // tier prefix is what keeps it from reading as the app's own title.
    @Test func ephemeralFromThePrimaryCheckoutStillNamesTheTier() {
        #expect(
            subtitle(.ephemeral, readiness: "/Users/op/src/freeside/.dev-instance.Ab12Cd/daemon")
                == "Ephemeral · freeside")
    }

    @Test func ephemeralWithAnotherReadinessShapeFallsThrough() {
        #expect(subtitle(.ephemeral, url: "http://127.0.0.1:49152", readiness: "/tmp/run-1") == ":49152")
        #expect(subtitle(.ephemeral, readiness: "/tmp/run-1") == "Ephemeral")
        #expect(subtitle(.ephemeral, readiness: "/.dev-instance.Ab12Cd/daemon") == "Ephemeral")
    }
}
