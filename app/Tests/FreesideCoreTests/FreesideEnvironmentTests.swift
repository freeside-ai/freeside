import Foundation
import Testing

@testable import FreesideCore

@Suite struct FreesideEnvironmentTests {
    @Test func eachSourceWinsInPrecedenceOrder() throws {
        #expect(
            try FreesideEnvironment.resolve(
                environmentVariable: "dev", infoPlistValue: "prod", isDebugBuild: true) == .dev)
        #expect(
            try FreesideEnvironment.resolve(
                environmentVariable: nil, infoPlistValue: "dev", isDebugBuild: true) == .dev)
        #expect(
            try FreesideEnvironment.resolve(
                environmentVariable: nil, infoPlistValue: nil, isDebugBuild: true) == .ephemeral)
        #expect(
            try FreesideEnvironment.resolve(
                environmentVariable: nil, infoPlistValue: nil, isDebugBuild: false) == .prod)
        // The Xcode production scheme: a debug build attached to prod.
        #expect(
            try FreesideEnvironment.resolve(
                environmentVariable: "prod", infoPlistValue: nil, isDebugBuild: true) == .prod)
    }

    @Test func anUnknownValueFailsInsteadOfFallingThrough() {
        #expect(
            throws: FreesideEnvironment.ResolutionError(source: "FREESIDE_ENV", value: "Prod")
        ) {
            try FreesideEnvironment.resolve(
                environmentVariable: "Prod", infoPlistValue: "prod", isDebugBuild: false)
        }
        #expect(
            throws: FreesideEnvironment.ResolutionError(
                source: "Info.plist FreesideEnvironment", value: "")
        ) {
            try FreesideEnvironment.resolve(
                environmentVariable: nil, infoPlistValue: "", isDebugBuild: false)
        }
    }

    /// The plan's "Derived identifiers" table; `install-mac-app.sh` writes the
    /// same strings, and `scripts/test-install-mac-app.sh` pins its side.
    @Test func supervisedTiersDeriveThePlanIdentifiers() throws {
        let home = try #require(
            FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first)

        #expect(FreesideEnvironment.prod.bundleIdentifier == "ai.freeside.app.macos")
        #expect(FreesideEnvironment.prod.launchdLabel == "ai.freeside.daemon")
        #expect(FreesideEnvironment.prod.launchAgentPlistName == "ai.freeside.daemon.plist")
        #expect(FreesideEnvironment.prod.displayName == "Freeside")
        #expect(FreesideEnvironment.prod.supervisedAPIURL?.absoluteString == "http://127.0.0.1:7331")
        #expect(
            FreesideEnvironment.prod.daemonStateDirectory()
                == home.appendingPathComponent("Freeside/daemon", isDirectory: true))
        #expect(FreesideEnvironment.prod.badgeTitle == nil)

        #expect(FreesideEnvironment.dev.bundleIdentifier == "ai.freeside.app.macos.dev")
        #expect(FreesideEnvironment.dev.launchdLabel == "ai.freeside.daemon.dev")
        #expect(FreesideEnvironment.dev.launchAgentPlistName == "ai.freeside.daemon.dev.plist")
        #expect(FreesideEnvironment.dev.displayName == "Freeside Dev")
        #expect(FreesideEnvironment.dev.supervisedAPIURL?.absoluteString == "http://127.0.0.1:7332")
        #expect(
            FreesideEnvironment.dev.daemonStateDirectory()
                == home.appendingPathComponent("Freeside Dev/daemon", isDirectory: true))
        #expect(FreesideEnvironment.dev.badgeTitle == "Dev")
    }

    @Test func ephemeralDerivesNothing() {
        let ephemeral = FreesideEnvironment.ephemeral
        #expect(!ephemeral.isSupervised)
        #expect(ephemeral.bundleIdentifier == nil)
        #expect(ephemeral.launchdLabel == nil)
        #expect(ephemeral.launchAgentPlistName == nil)
        #expect(ephemeral.displayName == nil)
        #expect(ephemeral.supervisedAPIURL == nil)
        #expect(ephemeral.stateRoot() == nil)
        #expect(ephemeral.daemonStateDirectory() == nil)
        #expect(ephemeral.badgeTitle == "Ephemeral")
    }
}
