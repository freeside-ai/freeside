import Foundation
import FreesideTestSupport
import Testing

@testable import FreesideModel

@Suite struct ReadinessDirectoryCredentialStoreTests {
    private let runURL = URL(string: "http://127.0.0.1:52811")!
    private let credential = DeviceCredential(
        deviceID: "device-7", token: testDeviceToken(for: "device-7"), ntfySubscription: .mock)!

    private func directory() throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }

    @Test func aSavedCredentialLoadsBackFromAPrivateFile() throws {
        let directory = try directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = ReadinessDirectoryCredentialStore(directory: directory, deploymentURL: runURL)

        #expect(try store.load() == nil)
        try store.save(credential)
        #expect(try store.load() == credential)
        let mode = try FileManager.default.attributesOfItem(atPath: store.fileURL.path)[.posixPermissions]
        #expect((mode as? NSNumber)?.intValue == 0o600)
        // The rename left no temporary file behind.
        let entries = try FileManager.default.contentsOfDirectory(atPath: directory.path)
        #expect(entries == [store.fileURL.lastPathComponent])

        try store.delete()
        #expect(try store.load() == nil)
        try store.delete()
    }

    @Test func anotherDeploymentsCredentialIsNeverLoaded() throws {
        // The same root restarted on another port is another deployment: its
        // token must not reach a daemon that did not mint it.
        let directory = try directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        try ReadinessDirectoryCredentialStore(directory: directory, deploymentURL: runURL).save(credential)

        let other = ReadinessDirectoryCredentialStore(
            directory: directory, deploymentURL: URL(string: "http://127.0.0.1:52812")!)
        #expect(try other.load() == nil)
    }

    @Test(arguments: ["", "{}", #"{"formatVersion":2}"#])
    func aMalformedFileFailsLoud(raw: String) throws {
        let directory = try directory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = ReadinessDirectoryCredentialStore(directory: directory, deploymentURL: runURL)
        try Data(raw.utf8).write(to: store.fileURL)

        #expect(throws: ReadinessDirectoryCredentialStore.StoreError.malformed) { try store.load() }
    }
}
