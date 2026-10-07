import CryptoKit
import Foundation
import Observation

/// Convenience data only: decoded text can populate a draft, never a command.
/// The submission ledger remains the sole source of retry identity.
@MainActor @Observable
public final class TaskPromptHistory {
    struct Entry: Codable, Equatable {
        var taskIDs: [String]
        let projectID: String
        let source: String
    }

    struct Record: Codable {
        let version: Int
        let deploymentID: String
        let deviceID: String
        let entries: [Entry]
    }

    private(set) var entries: [Entry] = []
    public var saveWarning: String?
    private let deploymentID: String
    private let deviceID: String
    private let fileURL: URL?
    private let write: (Data, URL) throws -> Void

    init(
        deploymentID: String, deviceID: String, directory: URL? = nil,
        write: @escaping (Data, URL) throws -> Void = { data, url in
            try FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try data.write(to: url, options: .atomic)
        }
    ) {
        self.deploymentID = deploymentID
        self.deviceID = deviceID
        self.write = write
        let deviceKey = SHA256.hash(data: Data(deviceID.utf8)).map { String(format: "%02x", $0) }.joined()
        fileURL = directory?.appendingPathComponent("task-prompt-history-\(deviceKey).json")
        guard let fileURL, let data = try? Data(contentsOf: fileURL),
            let record = try? JSONDecoder().decode(Record.self, from: data),
            record.version == 1, record.deploymentID == deploymentID, record.deviceID == deviceID,
            record.entries.count <= 50,
            record.entries.allSatisfy({
                !$0.projectID.isEmpty && !$0.source.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                    && !$0.taskIDs.isEmpty && $0.taskIDs.allSatisfy { !$0.isEmpty }
            })
        else { return }
        entries = record.entries
    }

    func prompts(for projectID: String) -> [String] {
        entries.reversed().filter { $0.projectID == projectID }.map(\.source)
    }

    /// Called only with a result accepted by CommandResultTrust. Task IDs
    /// suppress replay even when identical adjacent prompts were coalesced.
    func record(projectID: String, source: String, taskID: String) {
        guard !projectID.isEmpty, !source.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
            !taskID.isEmpty, !entries.contains(where: { $0.taskIDs.contains(taskID) })
        else { return }
        if let index = entries.lastIndex(where: { $0.projectID == projectID }), entries[index].source == source {
            var entry = entries.remove(at: index)
            entry.taskIDs.append(taskID)
            entries.append(entry)
        } else {
            entries.append(Entry(taskIDs: [taskID], projectID: projectID, source: source))
        }
        entries = Array(entries.suffix(50))
        guard let fileURL else { return }
        do {
            let data = try JSONEncoder().encode(
                Record(version: 1, deploymentID: deploymentID, deviceID: deviceID, entries: entries))
            try write(data, fileURL)
            saveWarning = nil
        } catch {
            saveWarning = "The task was accepted, but its prompt history couldn't be saved."
        }
    }
}

/// A browsing session snapshots its entries, preserving the exact draft
/// until Down returns to it or a project switch ends the session.
@MainActor @Observable
final class TaskPromptBrowseState {
    private var prompts: [String] = []
    private var index: Int?
    private var draft = ""

    var isBrowsing: Bool { index != nil }

    func up(from source: String, history: [String]) -> String? {
        if let index {
            self.index = min(index + 1, prompts.count - 1)
        } else {
            guard !history.isEmpty else { return nil }
            draft = source
            prompts = history
            index = 0
        }
        return index.map { prompts[$0] }
    }

    func down() -> String? {
        guard let index else { return nil }
        if index == 0 { return end() }
        self.index = index - 1
        return prompts[index - 1]
    }

    func end() -> String? {
        guard isBrowsing else { return nil }
        let restored = draft
        edited()
        return restored
    }

    func edited() {
        index = nil
        prompts = []
        draft = ""
    }
}
