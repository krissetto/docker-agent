// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "DockerAgentApple",
    platforms: [.macOS("26.0"), .iOS("26.0")],
    products: [
        .library(name: "AgentKit", targets: ["AgentKit"]),
        .executable(name: "DockerAgentApple", targets: ["DockerAgentApple"]),
    ],
    targets: [
        .target(name: "AgentKit"),
        .executableTarget(name: "DockerAgentApple", dependencies: ["AgentKit"], path: "Sources/DockerAgentApp"),
        .testTarget(name: "AgentKitTests", dependencies: ["AgentKit", "DockerAgentApple"], resources: [.copy("Fixtures")]),
    ]
)
