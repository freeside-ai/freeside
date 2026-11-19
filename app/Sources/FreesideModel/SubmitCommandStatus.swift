import FreesideAPI

extension Operations.submitCommand.Output {
    /// The HTTP status this answer arrived with. The command models read the
    /// bodies of 200, 400, and 409 and classify every other answer by its
    /// status alone, so a status the schema declares with a plain `Error`
    /// body (403, 404, 413, 500) takes the same arm an undocumented answer
    /// with that code took before the schema declared it.
    var statusCode: Int {
        switch self {
        case .ok: 200
        case .badRequest: 400
        case .forbidden: 403
        case .notFound: 404
        case .conflict: 409
        case .contentTooLarge: 413
        case .internalServerError: 500
        case .undocumented(let statusCode, _): statusCode
        }
    }
}
