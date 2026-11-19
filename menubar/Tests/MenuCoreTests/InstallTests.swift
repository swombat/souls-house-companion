import XCTest
@testable import MenuCore

/// ensureCompanion against fake companions: shell scripts that print a
/// version, answer status, and log every pause/resume/install they get.
final class InstallTests: XCTestCase {
    var dir: URL!
    var log: String { dir.appendingPathComponent("calls.log").path }
    var paths: CompanionInstall!

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        paths = CompanionInstall(bundled: dir.appendingPathComponent("app/souls-house-companion").path,
                                 installed: dir.appendingPathComponent("home/.local/bin/souls-house-companion").path,
                                 launchAgent: dir.appendingPathComponent("home/LaunchAgents/house.souls.companion.plist").path)
        try FileManager.default.createDirectory(atPath: (paths.bundled as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
    }

    /// A fake companion. `status` is the JSON it prints for `status --json`
    /// (nil: it fails); `fail` lists subcommands that exit 1.
    func fake(at path: String, version: String, status: String? = nil, fail: [String] = []) throws {
        let plist = paths.launchAgent
        let body = """
            #!/bin/sh
            echo "\(version) $*" >> '\(log)'
            for f in \(fail.joined(separator: " ")); do [ "$1" = "$f" ] && exit 1; done
            case "$1" in
              version) echo '\(version)' ;;
              status) \(status.map { "echo '\($0)'" } ?? "exit 1") ;;
              install) mkdir -p "$(dirname '\(plist)')" && : > '\(plist)' ;;
              pause|resume) : ;;
            esac
            """
        try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
        try body.write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
    }

    func calls() -> [String] {
        ((try? String(contentsOfFile: log, encoding: .utf8)) ?? "").split(separator: "\n").map(String.init)
    }

    func ensure() -> InstallOutcome { ensureCompanion(paths) { runCLI($0, $1, timeout: 5) } }

    func installedVersion() -> String? {
        versionOf(paths.installed) { runCLI($0, $1, timeout: 5) }
    }

    func testFirstInstallCopiesAndConfiguresWithoutStarting() throws {
        try fake(at: paths.bundled, version: "v2")
        XCTAssertTrue(paths.needsFirstInstall)
        XCTAssertEqual(ensure(), .installed(version: "v2"))
        XCTAssertEqual(installedVersion(), "v2")
        XCTAssertTrue(FileManager.default.fileExists(atPath: paths.launchAgent))
        XCTAssertFalse(calls().contains { $0.hasSuffix("resume") }, "first install must not start the service")
        XCTAssertFalse(paths.needsFirstInstall)
    }

    func testSameVersionIsLeftAlone() throws {
        try fake(at: paths.bundled, version: "v2")
        try fake(at: paths.installed, version: "v2", status: #"{"enrolled":true,"paused":false}"#)
        try FileManager.default.createDirectory(atPath: (paths.launchAgent as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: paths.launchAgent, contents: Data())
        XCTAssertEqual(ensure(), .upToDate)
        XCTAssertFalse(calls().contains { $0.contains("pause") || $0.contains("install") })
    }

    func testADifferentInstalledCompanionIsLeftExactlyAsItIs() throws {
        try fake(at: paths.bundled, version: "v2")
        try fake(at: paths.installed, version: "v1", status: #"{"enrolled":true,"paused":false}"#)
        let before = try Data(contentsOf: URL(fileURLWithPath: paths.installed))
        XCTAssertEqual(ensure(), .existingInstall(version: "v1"))
        XCTAssertEqual(try Data(contentsOf: URL(fileURLWithPath: paths.installed)), before)
        XCTAssertEqual(calls().filter { $0.hasPrefix("v1") }, ["v1 version"], "only asked its version")
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.launchAgent))
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.installed + ".previous"))
    }

    func testAnInstalledCompanionThatDoesntAnswerIsLeftAlone() throws {
        try fake(at: paths.bundled, version: "v2")
        try fake(at: paths.installed, version: "v1", fail: ["version"])
        XCTAssertEqual(ensure(), .existingInstall(version: nil))
        XCTAssertFalse(calls().contains { $0.hasPrefix("v2 install") || $0.contains("pause") })
    }

    func testEarlierSetupIsRecognised() throws {
        let legacy = dir.appendingPathComponent("home/LaunchAgents/house.souls.menu.plist").path
        let run: (String, [String]) -> CLIResult = { runCLI($0, $1, timeout: 5) }
        try fake(at: paths.bundled, version: "v2")
        XCTAssertFalse(earlierSetup(paths, legacyMenuAgent: legacy, run: run), "fresh Mac")
        XCTAssertEqual(ensure(), .installed(version: "v2"))
        XCTAssertFalse(earlierSetup(paths, legacyMenuAgent: legacy, run: run), "set up by this release")
        FileManager.default.createFile(atPath: legacy, contents: Data())
        XCTAssertTrue(earlierSetup(paths, legacyMenuAgent: legacy, run: run), "old menu agent")
        try FileManager.default.removeItem(atPath: legacy)
        try FileManager.default.removeItem(atPath: paths.installed)
        try fake(at: paths.installed, version: "v1")
        XCTAssertTrue(earlierSetup(paths, legacyMenuAgent: legacy, run: run), "other companion")
    }

    func testSymlinkedInstallIsRefused() throws {
        try fake(at: paths.bundled, version: "v2")
        let real = dir.appendingPathComponent("elsewhere").path
        try fake(at: real, version: "v1")
        try FileManager.default.createDirectory(atPath: (paths.installed as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(atPath: paths.installed, withDestinationPath: real)
        guard case .failed = ensure() else { return XCTFail("expected refusal") }
        XCTAssertEqual(versionOf(real) { runCLI($0, $1, timeout: 5) }, "v1")
    }

    func testBrokenBundledCompanionInstallsNothing() throws {
        try fake(at: paths.bundled, version: "v2", fail: ["version"])
        XCTAssertEqual(ensure(), .failed("The companion inside this app didn't start"))
        XCTAssertTrue(paths.needsFirstInstall)
    }
}
