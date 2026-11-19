import FreesideAPI

extension Operations.submitCommand.Output {
    /// The status of a refusal, a declared 400, 401, 403, 404, 413, or
    /// 500 or an undocumented status, or nil for an acceptance or a stale 409.
    var refusalStatus: Int? {
        switch self {
        case .badRequest: 400
        case .unauthorized: 401
        case .forbidden: 403
        case .notFound: 404
        case .contentTooLarge: 413
        case .internalServerError: 500
        case .undocumented(let status, _): status
        case .ok, .conflict: nil
        }
    }
}
