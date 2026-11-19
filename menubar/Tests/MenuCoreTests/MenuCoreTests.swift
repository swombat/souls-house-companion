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

    // A stand-in companion for the run-commands switch: it records every call
    // and answers status from its own state, the way the real CLI does.
    final class FakeCompanion {
        var paused: Bool?
        var accepted = ""
        let contract = "darwin-process-group-preview-v1"
        var failPause = false, failChange = false, failResume = false, failStatusAfterChange = false
        var calls: [[String]] = []
        private var changed = false

        init(paused: Bool?, accepted: String = "") { self.paused = paused; self.accepted = accepted }

        func result(_ ok: Bool, _ json: [String: Any]? = nil) -> CLIResult {
            let data = json.flatMap { try? JSONSerialization.data(withJSONObject: $0) } ?? Data()
            return CLIResult(outcome: .exited(ok ? 0 : 1), output: data, truncated: false)
        }

        func run(_ args: [String]) -> CLIResult {
            calls.append(args)
            switch args.first {
            case "status":
                if changed && failStatusAfterChange { return result(false) }
                var st: [String: Any] = ["execute_lifecycle": contract, "accepted_execute_lifecycle": accepted]
                if let paused { st["paused"] = paused }
                return result(true, st)
            case "pause":
                if failPause { return result(false) }
                paused = true; return result(true)
            case "resume":
                if failResume { return result(false) }
                paused = false; return result(true)
            case "execute-lifecycle":
                changed = true
                if failChange { return result(false) }
                accepted = args.contains("--revoke") ? "" : args[2]
                return result(true)
            default:
                return result(false)
            }
        }
    }

    func testAllowPausesAcceptsTheReportedContractAndResumes() {
        let f = FakeCompanion(paused: false)
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: f.run), .done)
        XCTAssertEqual(f.calls.map { $0.first! }, ["status", "pause", "execute-lifecycle", "status", "resume"])
        XCTAssertEqual(f.calls[2], ["execute-lifecycle", "--accept-contract", f.contract])
        XCTAssertEqual(f.accepted, f.contract)
        XCTAssertEqual(f.paused, false)
    }

    func testAnExistingPauseIsKept() {
        let f = FakeCompanion(paused: true)
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: f.run), .done)
        XCTAssertFalse(f.calls.contains(["resume"]))
        XCTAssertFalse(f.calls.contains(["pause"]))
        XCTAssertEqual(f.paused, true)
        // Unknown pause state is treated as paused, never resumed.
        let g = FakeCompanion(paused: nil)
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: g.run), .done)
        XCTAssertFalse(g.calls.contains(["resume"]))
    }

    func testRevokeThatCannotBeConfirmedDoesNotResume() {
        let f = FakeCompanion(paused: false, accepted: "darwin-process-group-preview-v1")
        f.failChange = true
        XCTAssertEqual(setExecuteLifecycle(allow: false, run: f.run), .revokeUnconfirmedPaused)
        XCTAssertFalse(f.calls.contains(["resume"]))
        XCTAssertEqual(f.paused, true)
        // An unreadable status after the revoke is not a confirmation either.
        let g = FakeCompanion(paused: false, accepted: "darwin-process-group-preview-v1")
        g.failStatusAfterChange = true
        XCTAssertEqual(setExecuteLifecycle(allow: false, run: g.run), .revokeUnconfirmedPaused)
        XCTAssertFalse(g.calls.contains(["resume"]))
    }

    func testRevokeConfirmedResumes() {
        let f = FakeCompanion(paused: false, accepted: "darwin-process-group-preview-v1")
        XCTAssertEqual(setExecuteLifecycle(allow: false, run: f.run), .done)
        XCTAssertEqual(f.calls[2], ["execute-lifecycle", "--revoke"])
        XCTAssertEqual(f.accepted, "")
        XCTAssertEqual(f.paused, false)
    }

    func testResumeFailureIsReportedWithTheChange() {
        let f = FakeCompanion(paused: false)
        f.failResume = true
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: f.run), .changedButPaused)
        XCTAssertEqual(lifecycleMessage(.changedButPaused, allow: true), "Commands allowed, but the companion is paused")
        let g = FakeCompanion(paused: false)
        g.failResume = true; g.failChange = true
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: g.run), .notChangedAndPaused)
    }

    func testNothingChangesWhenPauseFailsOrStatusIsUnreadable() {
        let f = FakeCompanion(paused: false)
        f.failPause = true
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: f.run), .couldNotPause)
        XCTAssertFalse(f.calls.contains { $0.first == "execute-lifecycle" })
        let unreadable = setExecuteLifecycle(allow: true) { _ in CLIResult(outcome: .exited(1), output: Data(), truncated: false) }
        XCTAssertEqual(unreadable, .couldNotRead)
    }

    func testFailedAcceptResumesIntoTheOldState() {
        let f = FakeCompanion(paused: false)
        f.failChange = true
        XCTAssertEqual(setExecuteLifecycle(allow: true, run: f.run), .notChanged)
        XCTAssertEqual(f.paused, false)
        XCTAssertEqual(f.accepted, "")
    }

    func testSnapshotReadsTheOptInOnlyWhenStatusSaysSo() {
        let clock = DateFormatter()
        let failed = CLIResult(outcome: .exited(1), output: Data(), truncated: false)
        func status(_ json: String) -> CLIResult { CLIResult(outcome: .exited(0), output: Data(json.utf8), truncated: false) }
        XCTAssertEqual(makeSnapshot(status: status(#"{"execute_lifecycle":"c","accepted_execute_lifecycle":"c"}"#), desktop: failed, clock: clock).executeAccepted, true)
        XCTAssertEqual(makeSnapshot(status: status(#"{"execute_lifecycle":"c","accepted_execute_lifecycle":""}"#), desktop: failed, clock: clock).executeAccepted, false)
        XCTAssertEqual(makeSnapshot(status: status(#"{"execute_lifecycle":"c2","accepted_execute_lifecycle":"c1"}"#), desktop: failed, clock: clock).executeAccepted, false)
        XCTAssertNil(makeSnapshot(status: status(#"{"paused":false}"#), desktop: failed, clock: clock).executeAccepted)
        XCTAssertNil(makeSnapshot(status: failed, desktop: failed, clock: clock).executeAccepted)
    }
}
