// swift-tools-version: 6.3

import PackageDescription

let package = Package(
    name: "TellyouwhatHTTPContracts",
    platforms: [
        .iOS(.v18),
        .macOS(.v15),
    ],
    products: [
        .library(name: "HealthAPI", targets: ["HealthAPI"]),
        .library(name: "JournalAPI", targets: ["JournalAPI"]),
    ],
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
            url: "https://github.com/apple/swift-http-types",
            exact: "1.6.0"
        ),
    ],
    targets: [
        .target(
            name: "HealthAPI",
            dependencies: [
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ],
            path: "HealthAPI",
            exclude: ["app.openapi.yaml"],
            plugins: [
                .plugin(name: "OpenAPIGenerator", package: "swift-openapi-generator"),
            ]
        ),
        .target(
            name: "JournalAPI",
            dependencies: [
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ],
            path: "JournalAPI",
            exclude: ["app.openapi.yaml"],
            plugins: [
                .plugin(name: "OpenAPIGenerator", package: "swift-openapi-generator"),
            ]
        ),
        .testTarget(
            name: "HealthAPITests",
            dependencies: ["HealthAPI"]
        ),
        .testTarget(
            name: "JournalAPITests",
            dependencies: ["JournalAPI"]
        ),
    ]
)
