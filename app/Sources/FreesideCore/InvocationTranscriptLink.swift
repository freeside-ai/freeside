import FreesideAPI
import FreesideModel
import SwiftUI

/// Rule 13 (#1131): an ended writer invocation's `Open Transcript` link on
/// the run page, with a dim mono `unverified` beside it. A tap reads the
/// invocation's transcript reference and prompt, fetches the bytes through
/// the shared attachment loader, so the card's size and availability gates
/// apply, and opens the transcript sheet. A read with no transcript, one
/// over the device's size cap, and a fetch that fails each say so under the
/// link and open nothing.
struct InvocationTranscriptLink: View {
    let coordinator: SyncCoordinator
    let runID: String
    let invocationID: String
    /// The stage and attempt, such as `Implementation · Round 1`.
    let title: String
    let outcome: AgentTranscriptSheet.Outcome
    let workingTime: TimeInterval?
    /// False in screenshots: the link draws without its action.
    var rendersInteractiveControls = true

    private struct Opened: Identifiable {
        let id = UUID()
        let digest: String
        let mediaType: String
        let bytes: [UInt8]
        let prompt: String?
    }

    private enum Notice {
        case notKept
        case tooLarge
        case notLoaded

        var text: String {
            switch self {
            case .notKept: "No transcript was kept for this invocation"
            case .tooLarge: "The transcript is too large to open on this device"
            case .notLoaded: "Couldn't load the transcript. Try again."
            }
        }
    }

    @State private var opened: Opened?
    @State private var isOpening = false
    @State private var notice: Notice?

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                if rendersInteractiveControls {
                    Button("Open Transcript") { open() }
                        .buttonStyle(.plain)
                        .disabled(isOpening)
                } else {
                    Text("Open Transcript")
                }
                Text("unverified")
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
            }
            .font(FreesideFont.noticeAction)
            .foregroundStyle(Color.accentText)
            if let notice {
                Text(notice.text)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .sheet(item: $opened) { opened in
            AgentTranscriptSheet(
                digest: opened.digest, bytes: opened.bytes, mediaType: opened.mediaType, title: title,
                outcome: outcome, workingTime: workingTime, prompt: opened.prompt)
        }
    }

    private func open() {
        isOpening = true
        notice = nil
        Task {
            defer { isOpening = false }
            let read: Components.Schemas.InvocationTranscript
            do {
                read = try await coordinator.invocationTranscript(runID: runID, invocationID: invocationID)
            } catch is CancellationError {
                return
            } catch SyncCoordinator.InvocationTranscriptReadError.unavailable {
                notice = .notKept
                return
            } catch {
                notice = .notLoaded
                return
            }
            guard read.availability == .available, let claim = read.transcript?.value1 else {
                notice = .notKept
                return
            }
            let attachments = coordinator.store.attachments
            await attachments.load(claim.digest, reference: AttachmentReference(claim.metadata))
            if case .tooLarge = attachments.phase(for: claim.digest) {
                notice = .tooLarge
                return
            }
            guard let bytes = await attachments.nonImageBytes(for: claim.digest) else {
                notice = .notLoaded
                return
            }
            opened = Opened(
                digest: claim.digest, mediaType: claim.metadata.media_type.rawValue, bytes: Array(bytes),
                prompt: read.prompt)
        }
    }
}
