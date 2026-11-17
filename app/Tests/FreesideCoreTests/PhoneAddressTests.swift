import Foundation
import Testing

@testable import FreesideCore

@Suite struct PhoneAddressTests {
    private let loopback = URL(string: "http://127.0.0.1:7331")!

    @Test func aTailscaleAPIURLIsUsedAsItIs() {
        let tailnet = URL(string: "http://100.101.102.103:7331")!

        #expect(PhoneAddress.resolve(apiURL: tailnet, localAddresses: []) == .tailnet(tailnet))
    }

    @Test func loopbackBecomesThisMacsTailscaleIPv4AtTheSamePort() {
        let resolved = PhoneAddress.resolve(
            apiURL: URL(string: "http://127.0.0.1:7332")!,
            localAddresses: ["127.0.0.1", "192.168.1.20", "fd7a:115c:a1e0::1", "100.101.102.103"])

        #expect(resolved == .forwarded(URL(string: "http://100.101.102.103:7332")!))
    }

    @Test func anIPv6OnlyTailnetIsThisMacOnly() {
        let resolved = PhoneAddress.resolve(
            apiURL: loopback, localAddresses: ["127.0.0.1", "fd7a:115c:a1e0::1"])

        #expect(resolved == .thisMacOnly(loopback))
    }

    @Test func noTailscaleAddressIsThisMacOnly() {
        let resolved = PhoneAddress.resolve(
            apiURL: loopback, localAddresses: ["127.0.0.1", "::1", "192.168.1.20", "100.63.0.1"])

        #expect(resolved == .thisMacOnly(loopback))
    }
}
