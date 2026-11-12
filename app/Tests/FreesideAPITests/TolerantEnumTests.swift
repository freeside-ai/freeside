import Foundation
import FreesideAPI
import Testing

/// Every generated enum that lists `unknown` decodes a value a newer daemon
/// adds as `unknown` (api/README.md, Response Tolerance), and still refuses a
/// value of another JSON type. TolerantEnums.all is generated from the client,
/// so a new response enum joins this check without a test edit.
@Suite struct TolerantEnumTests {
    @Test func everyListedEnumDecodesAnUndeclaredStringAsUnknown() throws {
        #expect(TolerantEnums.all.count >= 100)
        for type in TolerantEnums.all {
            try expectTolerant(type)
        }
    }

    private func expectTolerant<T: UnknownTolerantEnum>(_ type: T.Type) throws {
        let decoder = JSONDecoder()
        let undeclared = try decoder.decode(T.self, from: Data(#""added_by_a_newer_daemon""#.utf8))
        #expect(undeclared.rawValue == T.unknown.rawValue, "\(T.self)")
        let explicit = try decoder.decode(T.self, from: Data(#""unknown""#.utf8))
        #expect(explicit.rawValue == T.unknown.rawValue, "\(T.self)")
        // A declared member other than unknown still decodes as itself.
        let declared = try #require(T.allCases.first { $0.rawValue != T.unknown.rawValue }, "\(T.self)")
        let encoded = try JSONEncoder().encode(declared.rawValue)
        #expect(try decoder.decode(T.self, from: encoded).rawValue == declared.rawValue, "\(T.self)")
        #expect(throws: DecodingError.self, "\(T.self)") {
            try decoder.decode(T.self, from: Data("42".utf8))
        }
        #expect(throws: DecodingError.self, "\(T.self)") {
            try decoder.decode(T.self, from: Data("null".utf8))
        }
    }
}
