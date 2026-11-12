import FreesideAPI
import Testing

@testable import FreesideModel

/// #1266: a 400 whose reason says the daemon's build serves no command of
/// this kind reads as the update prompt; any other 400, including a reason
/// this app doesn't know, reads as the caller's own message.
@Suite struct CommandRefusalTests {
    private func refusal(
        _ reason: Components.Schemas.CommandRequestRejectionReason?
    ) -> Operations.submitCommand.Output.BadRequest {
        .init(body: .json(.init(message: "refused", reason: reason)))
    }

    @Test func onlyAnUnservedKindReadsAsTheUpdatePrompt() {
        #expect(CommandRefusal.message(refusal(.unknown_command_kind), otherwise: "x") == CommandRefusal.unservedKind)
        #expect(CommandRefusal.message(refusal(nil), otherwise: "x") == "x")
        #expect(CommandRefusal.message(refusal(.unknown), otherwise: "x") == "x")
    }
}
