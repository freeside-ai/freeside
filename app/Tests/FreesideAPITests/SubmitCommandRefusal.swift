import FreesideAPI

extension Operations.submitCommand.Output {
    /// The status of a refusal, the documented 400 or an undocumented status,
    /// or nil for an acceptance or a stale 409.
    var refusalStatus: Int? {
        switch self {
        case .badRequest: 400
        case .undocumented(let status, _): status
        case .ok, .conflict: nil
        }
    }
}
