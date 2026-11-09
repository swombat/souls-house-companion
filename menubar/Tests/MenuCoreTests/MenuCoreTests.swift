import XCTest
@testable import MenuCore

final class MenuCoreTests: XCTestCase {
    var dir: URL!

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    func script(_ name: String, _ body: String) throws -> String {
        let url = dir.appendingPathComponent(name)
        try ("#!/bin/sh\n" + body).write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
        return url.path
    }

    func testSilentChildTimesOutOnTime() throws {
        let start = Date()
        let r = runCLI(try script("silent", "sleep 5"), [], timeout: 0.2)
        XCTAssertEqual(r.outcome, .timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(start), 1.5)
    }

    func testTermResistantChildIsKilled() throws {
        let start = Date()
        let r = runCLI(try script("stubborn", "trap '' TERM; sleep 5"), [], timeout: 0.2, grace: 0.3)
        XCTAssertEqual(r.outcome, .timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(start), 2)
    }

    func testInheritedStdoutDoesNotHoldTheCall() throws {
        let start = Date()
        let r = runCLI(try script("orphan", "(sleep 5; echo late) & echo now"), [], timeout: 3, grace: 0.3)
        XCTAssertEqual(r.outcome, .exited(0))
        XCTAssertLessThan(Date().timeIntervalSince(start), 1.5)
    }

    func testOutputIsCappedOnEveryPath() throws {
        let r = runCLI(try script("big", "head -c 2097152 /dev/zero"), [], timeout: 5, cap: 1 << 20)
        XCTAssertEqual(r.output.count, 1 << 20)
        XCTAssertTrue(r.truncated)
        XCTAssertFalse(r.succeeded)
    }

    func testUnreadableStatusIsUnknownNotOff() throws {
        let clock = DateFormatter()
        let failed = CLIResult(outcome: .exited(1), output: Data(), truncated: false)
        let ok = CLIResult(outcome: .exited(0), output: Data(#"{"paused":false,"accessibility":{"trusted":true}}"#.utf8), truncated: false)
        XCTAssertEqual(makeSnapshot(status: ok, desktop: failed, clock: clock).desktop, .unknown)
        let garbage = CLIResult(outcome: .exited(0), output: Data("{".utf8), truncated: false)
        XCTAssertEqual(makeSnapshot(status: ok, desktop: garbage, clock: clock).desktop, .unknown)
        let off = CLIResult(outcome: .exited(0), output: Data(#"{"on":false,"reason":"switch_off"}"#.utf8), truncated: false)
        let s = makeSnapshot(status: failed, desktop: off, clock: clock)
        XCTAssertEqual(s.desktop, .off)
        XCTAssertNil(s.trusted)  // a failed status read keeps no stale trust
        XCTAssertNil(s.paused)
        XCTAssertFalse(s.companionReachable)
    }

    func testChangesRunInIntentOrder() throws {
        let state = dir.appendingPathComponent("switch").path
        let cli = try script("cli", "case \"$1\" in on) sleep 0.5; echo on > \"\(state)\";; off) echo off > \"\(state)\";; esac")
        let c = Coordinator()
        let done = expectation(description: "both")
        done.expectedFulfillmentCount = 2
        c.change({ runCLI(cli, ["on"], timeout: 5).succeeded }) { _ in done.fulfill() }
        c.change({ runCLI(cli, ["off"], timeout: 5).succeeded }) { _ in done.fulfill() }
        wait(for: [done], timeout: 5)
        XCTAssertEqual(try String(contentsOfFile: state, encoding: .utf8), "off\n")
    }

    func testStaleObservationIsDropped() {
        let c = Coordinator()
        let before = c.beginObservation()
        let done = expectation(description: "change")
        c.change({ true }) { _ in done.fulfill() }
        wait(for: [done], timeout: 2)
        XCTAssertFalse(c.isCurrent(before))
        XCTAssertTrue(c.isCurrent(c.beginObservation()))
    }
}
