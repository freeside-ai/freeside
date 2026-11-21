import FreesideAPI
import FreesideModel
import SwiftUI

/// A running writer's transcript (#1926): it reads the daemon's live answer
/// about every three seconds while open, from the offset it already holds,
/// appends the new records, and re-parses them off the main actor. The
/// records are unverified, never stored, and inert, as the finished sheet's
/// are.
///
/// Any answer that isn't live drops the held records before the sheet shows
/// anything else. Once the writer's transcript is kept, the sheet becomes
/// the finished or failed sheet over the kept bytes.
struct RunningTranscriptSheet: View {
    let coordinator: SyncCoordinator
    let runID: String
    let invocationID: String
    let title: String
    let startedAt: Date?
    /// The ended outcome once the run page has observed it; nil while it
    /// hasn't, when the kept stream's own last record decides.
    let endedOutcome: AgentTranscriptSheet.Outcome?
    let workingTime: TimeInterval?

    static let pollInterval: Duration = .seconds(3)
    /// How many answers with no transcript an ended writer gets before the
    /// sheet says none was kept: the run page can see the writer end a
    /// little before the daemon records its kept transcript.
    static let endedMissLimit = 10

    /// The kept transcript, once the writer has ended.
    private struct Kept: Equatable {
        let digest: String
        let mediaType: String
        let bytes: [UInt8]
        let prompt: String?
        let outcome: AgentTranscriptSheet.Outcome
    }

    @State private var buffer = LiveTranscriptBuffer()
    @State private var running = AgentTranscriptSheet.Running()
    @State private var prompt: String?
    @State private var kept: Kept?
    /// Set when the kept transcript can't open here, or an ended writer
    /// kept none, so polling stops.
    @State private var stopped = false
    @State private var endedMisses = 0
    /// Folds held for this sheet only: the running view's fold keys are
    /// positions in a stream that grows and trims, so none is saved.
    @State private var folds = AgentTranscriptFoldPreferences(defaults: nil)

    var body: some View {
        if let kept {
            AgentTranscriptSheet(
                digest: kept.digest, bytes: kept.bytes, mediaType: kept.mediaType, title: title,
                outcome: kept.outcome, workingTime: workingTime, prompt: kept.prompt)
        } else {
            AgentTranscriptSheet(
                digest: invocationID, bytes: [], mediaType: "application/jsonl", title: title, outcome: .running,
                prompt: prompt, running: running, folds: folds
            )
            // Restarts when the run page sees the writer end, so the loop
            // reads the ended outcome it now has.
            .task(id: endedOutcome) { await follow() }
        }
    }

    /// Reads until the transcript is kept or the sheet closes.
    private func follow() async {
        running.startedAt = startedAt
        while !Task.isCancelled, kept == nil, !stopped {
            await readOnce()
            guard kept == nil, !stopped else { return }
            try? await Task.sleep(for: Self.pollInterval)
        }
    }

    private func readOnce() async {
        let answer: Components.Schemas.InvocationTranscript
        do {
            answer = try await coordinator.invocationTranscript(
                runID: runID, invocationID: invocationID, after: buffer.end)
        } catch SyncCoordinator.InvocationTranscriptReadError.offsetRefused {
            // The daemon no longer serves from this offset, as after a
            // restarted handoff: start again from 0.
            drop(notice: nil)
            return
        } catch SyncCoordinator.InvocationTranscriptReadError.unavailable {
            missed()
            return
        } catch {
            // A failed or cancelled read keeps what is shown; the next poll
            // tries again.
            return
        }
        prompt = answer.prompt
        switch buffer.take(answer) {
        case .showLive:
            let bytes = buffer.bytes
            let parsed = await Task.detached(priority: .userInitiated) {
                AgentTranscriptPresentation(bytes: bytes, streamEnded: false)
            }.value
            guard !Task.isCancelled else { return }
            running.presentation = parsed
            running.omittedBefore = buffer.omittedBefore
            running.unreadableRecords = buffer.unreadableRecords
            running.notice = nil
        case .keep:
            drop(notice: nil)
            await keep(answer)
        case .withheld:
            drop(notice: "Freeside stopped showing this transcript")
        case .notKept:
            missed()
        }
    }

    /// An answer with no transcript. While the writer runs, the daemon may
    /// not be reading it yet; once it has ended, the kept transcript may not
    /// be recorded yet. The sheet says none was kept only after an ended
    /// writer's misses run out.
    private func missed() {
        guard endedOutcome != nil else {
            drop(notice: "Waiting for the agent’s transcript")
            return
        }
        endedMisses += 1
        guard endedMisses >= Self.endedMissLimit else {
            drop(notice: "Waiting for the agent’s transcript")
            return
        }
        drop(notice: "No transcript was kept for this invocation")
        stopped = true
    }

    /// Drops every held record before anything else is shown.
    private func drop(notice: String?) {
        buffer.drop()
        running.presentation = nil
        running.omittedBefore = false
        running.unreadableRecords = 0
        running.notice = notice
    }

    /// Fetches the kept transcript through the shared loader, so the
    /// card's size and availability gates apply, as the ended link does.
    private func keep(_ answer: Components.Schemas.InvocationTranscript) async {
        guard let claim = answer.transcript?.value1 else { return }
        let attachments = coordinator.store.attachments
        await attachments.load(claim.digest, reference: AttachmentReference(claim.metadata))
        if case .tooLarge = attachments.phase(for: claim.digest) {
            running.notice = "The transcript is too large to open on this device"
            stopped = true
            return
        }
        guard let bytes = await attachments.nonImageBytes(for: claim.digest) else {
            // The next poll tries the load again.
            running.notice = "Couldn't load the transcript"
            return
        }
        let held = Array(bytes)
        let outcome: AgentTranscriptSheet.Outcome
        if let endedOutcome {
            outcome = endedOutcome
        } else {
            let finished = await Task.detached(priority: .userInitiated) {
                AgentTranscriptPresentation(bytes: held, streamEnded: true).result?.isError == false
            }.value
            outcome = finished ? .finished : .failed
        }
        guard !Task.isCancelled else { return }
        kept = Kept(
            digest: claim.digest, mediaType: claim.metadata.media_type.rawValue, bytes: held,
            prompt: answer.prompt, outcome: outcome)
    }
}
