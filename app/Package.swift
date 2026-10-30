// swift-tools-version: 6.1

import PackageDescription

var products: [Product] = [
    .library(name: "FreesideAPI", targets: ["FreesideAPI"])
]

var targets: [Target] = [
    .target(
        name: "FreesideAPI",
        dependencies: [
            // swift-crypto (CryptoKit's portable twin) backs the text-claim
            // digest check on Linux, where FreesideAPI's tests run in CI.
            .product(name: "Crypto", package: "swift-crypto"),
            .product(name: "HTTPTypes", package: "swift-http-types"),
            .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
            .product(name: "OpenAPIURLSession", package: "swift-openapi-urlsession"),
        ],
        resources: [.copy("openapi.yaml"), .copy("openapi-generator-config.yaml")]
    ),
    .testTarget(
        name: "FreesideAPITests",
        dependencies: ["FreesideAPI"]
    ),
    // The Linux-clean half of the app: the session, stores, sync, models,
    // and the presentation data the views read. No Apple UI framework, so
    // it builds and tests on Linux beside FreesideAPI; FreesideCore keeps
    // the views and the platform glue and re-exports this module.
    .target(
        name: "FreesideModel",
        dependencies: [
            "FreesideAPI",
            // Directly, not only transitively via FreesideAPI: the sync
            // coordinator inspects OpenAPIRuntime's ClientError to tell a
            // decode failure (the daemon answered) from a transport
            // outage (it did not).
            .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
            // The model code's digests use the portable Crypto API, so
            // the models need no Apple-only framework.
            .product(name: "Crypto", package: "swift-crypto"),
        ]
    ),
    // The mocks and helpers both app test targets share. A regular target
    // so FreesideModelTests and FreesideCoreTests can each depend on it.
    .target(
        name: "FreesideTestSupport",
        dependencies: [
            "FreesideModel", "FreesideAPI",
            .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
            .product(name: "HTTPTypes", package: "swift-http-types"),
            .product(name: "Crypto", package: "swift-crypto"),
        ],
        path: "Tests/FreesideTestSupport"
    ),
    .testTarget(
        name: "FreesideModelTests",
        dependencies: ["FreesideModel", "FreesideAPI", "FreesideTestSupport"]
    ),
]

#if !os(Linux)
    products.append(.library(name: "FreesideCore", targets: ["FreesideCore"]))
    targets.append(
        .target(
            name: "FreesideCore",
            dependencies: ["FreesideModel", "FreesideAPI"],
            // The §15 faces (see Sources/FreesideCore/Fonts/README.md),
            // registered at runtime by FreesideFont so neither app target
            // needs an Info.plist font list.
            resources: [.copy("Fonts")]
        )
    )
    targets.append(
        .testTarget(
            name: "FreesideCoreTests",
            dependencies: [
                "FreesideCore", "FreesideModel", "FreesideAPI", "FreesideTestSupport",
                .product(name: "Crypto", package: "swift-crypto"),
            ],
            // Processed, not copied: `.copy` keeps the folder, and the
            // two build systems then disagree on whether it is the
            // bundle's own resource directory. Processing places the
            // digest files at the resource root under both.
            resources: [.process("Resources")]
        )
    )
    // The §5.14 real-daemon convergence pass (issue #72): every suite is
    // env-gated on a running freeside-signet-dev harness and skips
    // otherwise, so plain `swift test` never needs a daemon. Run through
    // scripts/run-convergence.sh at the repo root.
    targets.append(
        .testTarget(
            name: "FreesideConvergenceTests",
            dependencies: [
                "FreesideCore", "FreesideAPI",
                .product(name: "Crypto", package: "swift-crypto"),
            ]
        )
    )
#endif

let package = Package(
    name: "FreesideApp",
    platforms: [
        .macOS(.v14),
        .iOS(.v17),
    ],
    products: products,
    dependencies: [
        // Retained for the manual command plugin used by generate-api-client.sh.
        .package(
            url: "https://github.com/apple/swift-openapi-generator",
            exact: "1.13.0"
        ),
        .package(
            url: "https://github.com/apple/swift-openapi-runtime",
            exact: "1.12.0"
        ),
        .package(
            url: "https://github.com/apple/swift-openapi-urlsession",
            exact: "1.3.0"
        ),
        .package(
            url: "https://github.com/apple/swift-http-types",
            exact: "1.6.0"
        ),
        .package(
            url: "https://github.com/apple/swift-crypto",
            exact: "4.5.1"
        ),
    ],
    targets: targets
)
