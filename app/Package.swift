// swift-tools-version: 6.1

import PackageDescription

var products: [Product] = [
    .library(name: "FreesideAPI", targets: ["FreesideAPI"]),
]

var targets: [Target] = [
    .target(
        name: "FreesideAPI",
        dependencies: [
            .product(name: "HTTPTypes", package: "swift-http-types"),
            .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
            .product(name: "OpenAPIURLSession", package: "swift-openapi-urlsession"),
        ],
        plugins: [
            .plugin(name: "OpenAPIGenerator", package: "swift-openapi-generator"),
        ]
    ),
    .testTarget(
        name: "FreesideAPITests",
        dependencies: ["FreesideAPI"]
    ),
]

#if !os(Linux)
products.append(.library(name: "FreesideCore", targets: ["FreesideCore"]))
targets.append(
    .target(
        name: "FreesideCore",
        dependencies: ["FreesideAPI"]
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
    ],
    targets: targets
)
