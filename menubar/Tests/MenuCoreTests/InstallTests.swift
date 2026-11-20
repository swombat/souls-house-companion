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
                                 launchAgent: dir.appendingPathComponent("home/LaunchAgents/house.souls.companion.plist").path,
                                 state: dir.appendingPathComponent("home/.local/share/souls-house-companion/state.json").path)
        try FileManager.default.createDirectory(atPath: (paths.bundled as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
    }

    /// A fake companion. `status` is the JSON it prints for `status --json`
    /// (nil: it fails); `fail` lists subcommands that exit 1. `install`
    /// writes the companion's real LaunchAgent, or with `partialInstall` a
    /// truncated one and exits 1, like a write that failed half way.
    func fake(at path: String, version: String, status: String? = nil, fail: [String] = [],
              partialInstall: Bool = false) throws {
        let plist = paths.launchAgent
        try validPlist().write(toFile: dir.appendingPathComponent("good.plist").path, atomically: true, encoding: .utf8)
        let install = partialInstall
            ? "printf '<?xml version=\"1.0\"?><plist><dict><key>Label</key>' > '\(plist)'; exit 1"
            : "cp '\(dir.appendingPathComponent("good.plist").path)' '\(plist)'"
        let body = """
            #!/bin/sh
            echo "\(version) $*" >> '\(log)'
            for f in \(fail.joined(separator: " ")); do [ "$1" = "$f" ] && exit 1; done
            case "$1" in
              version) echo '\(version)' ;;
              status) \(status.map { "echo '\($0)'" } ?? "exit 1") ;;
              install) mkdir -p "$(dirname '\(plist)')" && \(install) ;;
              pause|resume) : ;;
            esac
            """
        try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
        try body.write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
    }

    func validPlist() -> String {
        """
        <?xml version="1.0" encoding="UTF-8"?>
        <plist version="1.0"><dict>
        <key>Label</key><string>house.souls.companion</string>
        <key>ProgramArguments</key><array><string>\(paths.installed)</string><string>serve</string></array>
        </dict></plist>
        """
    }

    func put(_ text: String, at path: String) throws {
        try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent,
                                                withIntermediateDirectories: true)
        try text.write(toFile: path, atomically: true, encoding: .utf8)
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
        try put(validPlist(), at: paths.launchAgent)
        XCTAssertEqual(ensure(), .upToDate)
        XCTAssertFalse(calls().contains { $0.contains("pause") || $0.contains("install") })
    }

    func testANewerInstalledCompanionIsLeftExactlyAsItIs() throws {
        try fake(at: paths.bundled, version: "v0.4.1")
        try fake(at: paths.installed, version: "v0.5.0", status: #"{"enrolled":true,"paused":false}"#)
        let before = try Data(contentsOf: URL(fileURLWithPath: paths.installed))
        XCTAssertEqual(ensure(), .existingInstall(version: "v0.5.0"))
        XCTAssertEqual(try Data(contentsOf: URL(fileURLWithPath: paths.installed)), before)
        XCTAssertEqual(calls().filter { $0.hasPrefix("v0.5.0") }, ["v0.5.0 version"], "only asked its version")
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.launchAgent))
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.installed + ".previous"))
    }

    func testAnUnversionedInstalledCompanionIsLeftAlone() throws {
        try fake(at: paths.bundled, version: "v0.4.1")
        try fake(at: paths.installed, version: "development")
        XCTAssertEqual(ensure(), .existingInstall(version: "development"))
        XCTAssertEqual(installedVersion(), "development")
    }

    /// Daniel's Mac, 20 Nov: v0.4.0 installed and paired, v0.4.1 opened.
    func testAnOlderReleaseIsUpdatedKeepingItsPairing() throws {
        try fake(at: paths.bundled, version: "v0.4.1")
        try fake(at: paths.installed, version: "v0.4.0", status: #"{"enrolled":true,"paused":false}"#)
        try put(validPlist(), at: paths.launchAgent)
        try put(#"{"device_id":"d1"}"#, at: paths.state)
        let plistBefore = try Data(contentsOf: URL(fileURLWithPath: paths.launchAgent))
        let legacy = dir.appendingPathComponent("home/LaunchAgents/house.souls.menu.plist").path
        XCTAssertFalse(earlierSetup(paths, legacyMenuAgent: legacy) { runCLI($0, $1, timeout: 5) })
        XCTAssertEqual(ensure(), .updated(from: "v0.4.0", to: "v0.4.1"))
        XCTAssertEqual(installedVersion(), "v0.4.1")
        XCTAssertEqual(versionOf(paths.installed + ".previous") { runCLI($0, $1, timeout: 5) }, "v0.4.0")
        XCTAssertEqual(try String(contentsOfFile: paths.state, encoding: .utf8), #"{"device_id":"d1"}"#)
        XCTAssertEqual(try Data(contentsOf: URL(fileURLWithPath: paths.launchAgent)), plistBefore)
        XCTAssertFalse(calls().contains { $0.contains("install") || $0.contains("pause") })
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.installed + ".staging"))
        XCTAssertEqual(ensure(), .upToDate, "second launch")
    }

    func testABrokenUpdatePutsTheEarlierCompanionBack() throws {
        try fake(at: paths.bundled, version: "v0.4.1")
        try fake(at: paths.installed, version: "v0.4.0")
        // The bundled copy answers from inside the app but not once installed:
        // simulate by making `version` fail when run from the installed path.
        let body = try String(contentsOfFile: paths.bundled, encoding: .utf8)
            .replacingOccurrences(of: "case \"$1\" in",
                                  with: "[ \"$0\" = '\(paths.installed)' ] && exit 1\ncase \"$1\" in")
        try body.write(toFile: paths.bundled, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: paths.bundled)
        XCTAssertEqual(ensure(), .failed("The updated companion didn't answer; the earlier one was put back"))
        XCTAssertEqual(installedVersion(), "v0.4.0")
    }

    func testReleaseOrdering() {
        XCTAssertTrue(isUpgrade(from: "v0.4.0", to: "v0.4.1"))
        XCTAssertTrue(isUpgrade(from: "v0.9.9", to: "v0.10.0"))
        XCTAssertFalse(isUpgrade(from: "v0.4.1", to: "v0.4.1"))
        XCTAssertFalse(isUpgrade(from: "v0.5.0", to: "v0.4.1"))
        XCTAssertFalse(isUpgrade(from: "development", to: "v0.4.1"))
        XCTAssertFalse(isUpgrade(from: "v0.4.0", to: "dev-abc1234"))
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
        try fake(at: paths.installed, version: "v1", fail: ["version"])
        XCTAssertTrue(earlierSetup(paths, legacyMenuAgent: legacy, run: run), "companion that doesn't answer")
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

    // An earlier setup can outlive its binary: its login job, its enrollment.
    // Then nothing is copied and `install` is never run.
    func assertLeftoversRefused(file: StaticString = #filePath, line: UInt = #line) {
        let run: (String, [String]) -> CLIResult = { runCLI($0, $1, timeout: 5) }
        let legacy = dir.appendingPathComponent("home/LaunchAgents/house.souls.menu.plist").path
        XCTAssertTrue(earlierSetup(paths, legacyMenuAgent: legacy, run: run), file: file, line: line)
        XCTAssertEqual(ensure(), .leftovers, file: file, line: line)
        XCTAssertFalse(FileManager.default.fileExists(atPath: paths.installed), "no binary written", file: file, line: line)
        XCTAssertFalse(calls().contains { $0.contains("install") }, "install never run", file: file, line: line)
    }

    func testOrphanLaunchAgentRefusesInstall() throws {
        try fake(at: paths.bundled, version: "v2")
        try put(validPlist(), at: paths.launchAgent)
        assertLeftoversRefused()
    }

    func testOrphanEnrollmentRefusesInstall() throws {
        try fake(at: paths.bundled, version: "v2")
        try put("{}", at: paths.state)
        assertLeftoversRefused()
    }

    func testUncheckableEnrollmentRefusesInstall() throws {
        try fake(at: paths.bundled, version: "v2")
        let stateDir = (paths.state as NSString).deletingLastPathComponent
        try put("{}", at: paths.state)
        try FileManager.default.setAttributes([.posixPermissions: 0o000], ofItemAtPath: stateDir)
        defer { try? FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: stateDir) }
        XCTAssertNil(present(paths.state), "unreadable directory: can't tell")
        assertLeftoversRefused()
    }

    func testFailedLaunchAgentWriteIsAFailure() throws {
        try fake(at: paths.bundled, version: "v2", partialInstall: true)
        XCTAssertEqual(ensure(), .failed("The companion is installed but its login item couldn't be written"))
        XCTAssertTrue(FileManager.default.fileExists(atPath: paths.launchAgent), "the partial plist was left")
        // Retrying doesn't take the partial file for a working one.
        XCTAssertEqual(ensure(), .failed("The companion is installed but its login item couldn't be written"))
    }

    func testInvalidExistingLaunchAgentIsNotUpToDate() throws {
        try fake(at: paths.bundled, version: "v2")
        try fake(at: paths.installed, version: "v2")
        try put("<plist><dict></dict></plist>", at: paths.launchAgent)
        XCTAssertEqual(ensure(), .failed("The companion is installed but its login item couldn't be written"))
    }
}
