import Crypto
import Foundation
import FreesideAPI
import Observation

/// What the New Task composer holds before it submits (#2935).
public struct NewTaskDraft: Equatable, Sendable {
    public var source = ""
    /// The project the operator chose; the composer falls back to the first
    /// known project when this is nil or no longer known.
    public var projectID: String?
    public var choices: [AgentChoiceDisplay.Role: AgentChoiceDisplay.Selection] = [:]
    public var agentsOpen = false
    public var attachments = TaskAttachmentDraft()

    public init() {}

    /// Nothing the operator would miss. A project alone isn't a draft, so
    /// opening and closing an untouched composer leaves none behind.
    public var isEmpty: Bool {
        source.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && attachments.isEmpty && choices.isEmpty
    }

}

/// The device's one New Task draft, kept for the app session and, without
/// its attachments, across a relaunch. Device-local convenience data: it
/// never reaches the daemon, and the composer re-checks a restored project
/// and agent choice against what sync currently holds.
///
/// Each change writes the file at once. The record is a few kilobytes, and
/// writing on every edit leaves nothing pending for a quit to lose.
@MainActor @Observable
public final class NewTaskDraftStore {
    struct Record: Codable, Equatable {
        let version: Int
        let deploymentID: String
        let deviceID: String
        let source: String
        let projectID: String?
        /// Keyed by the role's raw value, so a role a later build drops is
        /// skipped on load instead of failing the whole record.
        let choices: [String: AgentChoiceDisplay.Selection]
        let agentsOpen: Bool
        /// Attachment bytes stay out of the file; this says some were left out.
        let hadAttachments: Bool
    }

    public var draft: NewTaskDraft {
        didSet {
            if !draft.attachments.isEmpty { attachmentsDroppedAtRelaunch = false }
            save()
        }
    }
    /// True when the draft restored at launch had attachments the file
    /// couldn't carry. Adding an attachment or clearing the draft resets it.
    public private(set) var attachmentsDroppedAtRelaunch = false
    public var saveWarning: String?
    private let deploymentID: String
    private let deviceID: String
    private let fileURL: URL?
    private let write: (Data, URL) throws -> Void
    private let remove: (URL) throws -> Void
    /// What the file holds, so an edit the record doesn't carry (a
    /// keystroke's no-op set, an attachment) writes nothing.
    @ObservationIgnored private var saved: Record?

    /// Without a directory the draft lives in memory only, as mock sessions
    /// keep it.
    init(
        deploymentID: String, deviceID: String, directory: URL? = nil,
        write: @escaping (Data, URL) throws -> Void = { data, url in
            try FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try data.write(to: url, options: .atomic)
        },
        remove: @escaping (URL) throws -> Void = { url in
            do {
                try FileManager.default.removeItem(at: url)
            } catch CocoaError.fileNoSuchFile {}
        }
    ) {
        self.deploymentID = deploymentID
        self.deviceID = deviceID
        self.write = write
        self.remove = remove
        let deviceKey = SHA256.hash(data: Data(deviceID.utf8)).map { String(format: "%02x", $0) }.joined()
        let fileURL = directory?.appendingPathComponent("new-task-draft-\(deviceKey).json")
        self.fileURL = fileURL
        // One assignment: a second would run `didSet` and rewrite the file.
        let (restored, record) = Self.load(fileURL, deploymentID: deploymentID, deviceID: deviceID)
        draft = restored
        attachmentsDroppedAtRelaunch = (record?.hadAttachments ?? false) && !restored.isEmpty
        saved = record
    }

    /// The file's draft when this deployment and device wrote it, and its
    /// record; otherwise no draft.
    private static func load(
        _ fileURL: URL?, deploymentID: String, deviceID: String
    ) -> (NewTaskDraft, Record?) {
        guard let fileURL, let data = try? Data(contentsOf: fileURL),
            let record = try? JSONDecoder().decode(Record.self, from: data),
            record.version == 1, record.deploymentID == deploymentID, record.deviceID == deviceID
        else { return (NewTaskDraft(), nil) }
        var restored = NewTaskDraft()
        restored.source = record.source
        restored.projectID = record.projectID
        restored.choices = record.choices.reduce(into: [:]) { choices, entry in
            if let role = AgentChoiceDisplay.Role(rawValue: entry.key) { choices[role] = entry.value }
        }
        restored.agentsOpen = record.agentsOpen
        return (restored, record)
    }

    /// Empties the draft and removes the file: a submit that settled, or
    /// the operator's Discard. Removing, not writing an empty record, still
    /// works on a full disk, so a sent draft can't come back at relaunch.
    public func clear() {
        attachmentsDroppedAtRelaunch = false
        // With nothing saved, the empty draft's didSet writes nothing.
        saved = nil
        draft = NewTaskDraft()
        guard let fileURL else { return }
        do {
            try remove(fileURL)
            saveWarning = nil
        } catch {
            saveWarning = Self.saveFailure
        }
    }

    /// Puts back a draft a submit took that came back unsent, unless the
    /// operator has started another meanwhile.
    public func restore(_ unsent: NewTaskDraft) {
        guard draft.isEmpty else { return }
        draft = unsent
    }

    static let saveFailure = "The draft couldn't be saved on this device. It's kept until the app quits."

    private func save() {
        guard let fileURL else { return }
        // An empty draft after clear() leaves no file to write.
        guard !(draft.isEmpty && saved == nil) else { return }
        let record = Record(
            version: 1, deploymentID: deploymentID, deviceID: deviceID, source: draft.source,
            projectID: draft.projectID,
            choices: draft.choices.reduce(into: [:]) { $0[$1.key.rawValue] = $1.value },
            agentsOpen: draft.agentsOpen,
            hadAttachments: !draft.attachments.isEmpty || attachmentsDroppedAtRelaunch)
        guard record != saved else { return }
        do {
            try write(JSONEncoder().encode(record), fileURL)
            saved = record
            saveWarning = nil
        } catch {
            saveWarning = Self.saveFailure
        }
    }
}
