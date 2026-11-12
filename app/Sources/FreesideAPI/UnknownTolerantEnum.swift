/// A wire enum that decodes a value it doesn't declare as `unknown`.
///
/// Every string enum a response reaches lists `unknown`
/// (api/README.md, Response Tolerance), so an app built from an older
/// contract keeps decoding when a newer daemon adds a member. The member
/// alone doesn't do that: the generated enum's synthesized decoding throws on
/// an undeclared value. Conforming a generated enum to this protocol replaces
/// that decoding, and the conformance doesn't compile unless the enum has an
/// `unknown` case.
///
/// The conformances are generated (TolerantEnums.swift, written by
/// app/scripts/write-tolerant-enums.sh), so a new response enum can't be left
/// strict by hand. `unknown` never grants anything: wherever a value enables a
/// control, marks something verified, or hides a warning, `unknown` takes the
/// most cautious branch, and the app never sends it back.
public protocol UnknownTolerantEnum: RawRepresentable, Decodable, CaseIterable, Sendable where RawValue == String {
    static var unknown: Self { get }
}

extension UnknownTolerantEnum {
    /// Decodes a declared value as itself and any other string as `unknown`.
    /// A value that isn't a string still fails, so a changed wire type is
    /// never mistaken for a new member.
    public init(from decoder: any Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = Self(rawValue: raw) ?? .unknown
    }
}
