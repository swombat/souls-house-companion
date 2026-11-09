// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "SoulsHouseMenu",
    platforms: [.macOS(.v13)],
    targets: [
        .target(name: "MenuCore", path: "Sources/MenuCore"),
        .executableTarget(name: "SoulsHouseMenu", dependencies: ["MenuCore"], path: "Sources/SoulsHouseMenu"),
        .testTarget(name: "MenuCoreTests", dependencies: ["MenuCore"], path: "Tests/MenuCoreTests"),
    ]
)
