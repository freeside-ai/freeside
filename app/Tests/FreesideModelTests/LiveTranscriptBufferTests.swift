import FreesideAPI
import OpenAPIRuntime
import Testing

@testable import FreesideModel

@Suite struct LiveTranscriptBufferTests {
    private static func live(
        _ records: String, start: Int64, end: Int64? = nil, omitted: Bool = false, unreadable: Int = 0
    ) -> Components.Schemas.LiveTranscript {
        .init(
            start: start, end: end ?? start + Int64(records.utf8.count), records: .init(Array(records.utf8)),
            omitted_before: omitted, unreadable_records: unreadable)
    }

    /// Answers that continue from the held end append, and the next read
    /// starts where the last one ended, past any records the daemon
    /// withheld and only counted.
    @Test func appendsContiguousAnswers() {
        var buffer = LiveTranscriptBuffer()
        buffer.append(Self.live("{\"a\":1}\n", start: 0))
        buffer.append(Self.live("{\"b\":2}\n", start: 8, end: 40, unreadable: 1))
        #expect(String(decoding: buffer.bytes, as: UTF8.self) == "{\"a\":1}\n{\"b\":2}\n")
        #expect(buffer.end == 40)
        #expect(buffer.unreadableRecords == 1)
        #expect(!buffer.omittedBefore)
    }

    /// An answer that skips ahead replaces what is held and says earlier
    /// records are missing, so the records shown stay contiguous.
    @Test func anAnswerThatSkipsAheadReplacesTheHeldRecords() {
        var buffer = LiveTranscriptBuffer()
        buffer.append(Self.live("{\"a\":1}\n", start: 0))
        buffer.append(Self.live("{\"z\":9}\n", start: 900, omitted: true))
        #expect(String(decoding: buffer.bytes, as: UTF8.self) == "{\"z\":9}\n")
        #expect(buffer.omittedBefore)
        #expect(buffer.end == 908)
    }

    /// Past the bound, whole records leave from the front.
    @Test func keepsABoundedTailOfWholeRecords() {
        var buffer = LiveTranscriptBuffer()
        let record = "{\"r\":\"" + String(repeating: "y", count: 1 << 16) + "\"}\n"
        var end: Int64 = 0
        for _ in 0..<(LiveTranscriptBuffer.byteLimit / record.utf8.count + 4) {
            buffer.append(Self.live(record, start: end))
            end = buffer.end
        }
        #expect(buffer.bytes.count <= LiveTranscriptBuffer.byteLimit)
        #expect(buffer.bytes.count % record.utf8.count == 0)
        #expect(buffer.omittedBefore)
    }

    @Test func dropReleasesEverything() {
        var buffer = LiveTranscriptBuffer()
        buffer.append(Self.live("{\"a\":1}\n", start: 0, omitted: false, unreadable: 2))
        buffer.drop()
        #expect(buffer == LiveTranscriptBuffer())
    }

    /// Every answer that isn't live drops the held records and the offset
    /// before the sheet shows anything else.
    @Test(arguments: [
        (Components.Schemas.InvocationTranscriptAvailability.available, LiveTranscriptBuffer.Next.keep),
        (.withheld, .withheld),
        (.unavailable, .notKept),
        (.unknown, .notKept),
    ])
    func anAnswerThatIsNotLiveDropsTheHeldRecords(
        availability: Components.Schemas.InvocationTranscriptAvailability, next: LiveTranscriptBuffer.Next
    ) {
        var buffer = LiveTranscriptBuffer()
        #expect(buffer.take(Self.answer(.live, live: Self.live("{\"a\":1}\n", start: 0, unreadable: 1))) == .showLive)
        #expect(!buffer.bytes.isEmpty)
        #expect(buffer.take(Self.answer(availability)) == next)
        #expect(buffer == LiveTranscriptBuffer())
    }

    private static func answer(
        _ availability: Components.Schemas.InvocationTranscriptAvailability,
        live: Components.Schemas.LiveTranscript? = nil
    ) -> Components.Schemas.InvocationTranscript {
        .init(
            run_id: "run-1", invocation_id: "inv-1", availability: availability, transcript: nil,
            live: live.map { .init(value1: $0) }, prompt: nil)
    }
}
