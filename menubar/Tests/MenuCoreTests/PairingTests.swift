import XCTest
@testable import MenuCore

/// Disk listing, the pair command line, and the pair command's JSON lines,
/// against fake companions (shell scripts).
final class PairingTests: XCTestCase {
    var dir: URL!

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    func script(_ body: String) throws -> String {
        let path = dir.appendingPathComponent("companion").path
        try ("#!/bin/sh\n" + body).write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
        return path
    }

    // MARK: disks

    let volumesJSON = """
        [{"mount_id":1,"mountpoint":"/","key":"","denied":"sealed system volume"},
         {"mount_id":2,"mountpoint":"/Volumes/Backup","key":"K-backup"},
         {"mount_id":3,"mountpoint":"/System/Volumes/Data","key":"K-data"},
         {"mount_id":4,"mountpoint":"/Volumes/Odd","key":"K-odd","denied":"unexpected group"},
         {"mount_id":5,"mountpoint":"/Volumes/Photos Drive","key":"K-photos"}]
        """
    /// Every listed disk is an ordinary writable drive unless a test says otherwise.
    let plainDrives: (String) -> VolumeTraits? = { _ in VolumeTraits(readOnly: false, browsable: true) }

    func testGrantableVolumesKeepOnlyKeyedUndeniedAndPutMainDiskFirst() {
        let v = grantableVolumes(from: Data(volumesJSON.utf8), traits: plainDrives)
        XCTAssertEqual(v?.map(\.key), ["K-data", "K-backup", "K-photos"])
        XCTAssertEqual(v?.map(\.label), ["Main disk (your files)", "Backup", "Photos Drive"])
        XCTAssertEqual(v?.map(\.isMain), [true, false, false])
    }

    /// Daniel's Mac on 19 Nov listed Hardware, Preboot, Update, VM, iSCPreboot,
    /// xarts and the installer's own disk image beside the main disk.
    func testSystemVolumesAndTheInstallerImageAreNotOffered() {
        let json = """
            [{"mountpoint":"/System/Volumes/Data","key":"K-data"},
             {"mountpoint":"/System/Volumes/Hardware","key":"K-hw"},
             {"mountpoint":"/System/Volumes/Preboot","key":"K-pre"},
             {"mountpoint":"/System/Volumes/Update","key":"K-up"},
             {"mountpoint":"/System/Volumes/VM","key":"K-vm"},
             {"mountpoint":"/System/Volumes/iSCPreboot","key":"K-isc"},
             {"mountpoint":"/System/Volumes/xarts","key":"K-x"},
             {"mountpoint":"/private/var/folders/xy/T/AppTranslocation/1","key":"K-tr"},
             {"mountpoint":"/Volumes/souls.house","key":"K-dmg"},
             {"mountpoint":"/Volumes/Hidden","key":"K-hid"},
             {"mountpoint":"/Volumes/Gone","key":"K-gone"},
             {"mountpoint":"/Volumes/Backup","key":"K-backup"}]
            """
        let traits: (String) -> VolumeTraits? = { m in
            switch m {
            case "/Volumes/souls.house": return VolumeTraits(readOnly: true, browsable: true)
            case "/Volumes/Hidden": return VolumeTraits(readOnly: false, browsable: false)
            case "/Volumes/Gone": return nil
            default: return VolumeTraits(readOnly: false, browsable: true)
            }
        }
        XCTAssertEqual(grantableVolumes(from: Data(json.utf8), traits: traits)?.map(\.key), ["K-data", "K-backup"])
    }

    func testRootMountpointIsAlsoTheMainDisk() {
        let v = grantableVolumes(from: Data(#"[{"mountpoint":"/","key":"K-root"}]"#.utf8), traits: { _ in nil })
        XCTAssertEqual(v?.first?.label, "Main disk (your files)")
        XCTAssertEqual(v?.first?.isMain, true)
    }

    func testDuplicateKeysAppearOnce() {
        let v = grantableVolumes(from: Data(#"[{"mountpoint":"/Volumes/A","key":"K"},{"mountpoint":"/Volumes/B","key":"K"}]"#.utf8),
                                 traits: plainDrives)
        XCTAssertEqual(v?.map(\.label), ["A"])
    }

    func testEmptyListIsAnEmptyListAndGarbageIsNil() {
        XCTAssertEqual(grantableVolumes(from: Data("[]".utf8)), [])
        XCTAssertNil(grantableVolumes(from: Data("not json".utf8)))
        XCTAssertNil(grantableVolumes(from: Data(#"{"a":1}"#.utf8)))
    }

    func testListVolumesRunsTheCompanion() throws {
        let path = try script("[ \"$1\" = volumes ] && cat <<'J'\n\(volumesJSON)\nJ\n")
        let v = listVolumes(companion: path, traits: plainDrives) { runCLI($0, $1, timeout: 5) }
        XCTAssertEqual(v?.count, 3)
    }

    func testListVolumesIsNilWhenTheCompanionFails() throws {
        let path = try script("exit 1\n")
        XCTAssertNil(listVolumes(companion: path) { runCLI($0, $1, timeout: 5) })
    }

    // MARK: the command line

    func testDeviceName() {
        XCTAssertEqual(pairDeviceName("Paulina\u{2019}s MacBook Air"), "Paulina\u{2019}s MacBook Air")
        XCTAssertEqual(pairDeviceName(nil), "This Mac")
        XCTAssertEqual(pairDeviceName("  \n "), "This Mac")
        XCTAssertEqual(pairDeviceName("A\u{7}B\n"), "AB")
        let long = pairDeviceName(String(repeating: "\u{00E9}", count: 80))
        XCTAssertEqual(long.utf8.count, 100)
    }

    func testPairArguments() {
        XCTAssertEqual(pairArguments(url: "https://souls.house", name: "Paulina's MacBook Air",
                                     choices: [PairChoice(key: "K1", write: false), PairChoice(key: "K2", write: true)]),
                       ["pair", "--url", "https://souls.house", "--name", "Paulina's MacBook Air",
                        "--volume", "K1", "--volume", "K2=write"])
    }

    // MARK: JSON lines

    func testParsePairEvent() {
        XCTAssertEqual(parsePairEvent(#"{"event":"code","code":"ABCD-EFGH","expires_at":"2026-11-19T22:30:00Z"}"#),
                       .code("ABCD-EFGH", expiresAt: ISO8601DateFormatter().date(from: "2026-11-19T22:30:00Z")))
        XCTAssertEqual(parsePairEvent(#"{"event":"code","code":"ABCD-EFGH"}"#), .code("ABCD-EFGH", expiresAt: nil))
        XCTAssertEqual(parsePairEvent(#"{"event":"paired","device_id":"d1"}"#), .paired)
        XCTAssertEqual(parsePairEvent(#"{"event":"expired"}"#), .expired)
        XCTAssertEqual(parsePairEvent(#"{"event":"cancelled"}"#), .cancelled)
        XCTAssertEqual(parsePairEvent(#"{"event":"uncertain"}"#), .uncertain)
        XCTAssertEqual(parsePairEvent(#"{"event":"paired","device_id":"d1","paused":true}"#), .pairedPaused)
        XCTAssertNil(parsePairEvent(#"{"event":"code"}"#))
        XCTAssertNil(parsePairEvent(#"{"event":"surprise"}"#))
        XCTAssertNil(parsePairEvent("not json"))
        XCTAssertNil(parsePairEvent(""))
    }

    func testLineSplitterHandlesPartialLinesAndTheEnd() {
        var s = LineSplitter()
        XCTAssertEqual(s.add(Data("{\"a\":".utf8)), [])
        XCTAssertEqual(s.add(Data("1}\n{\"b\":2}\n{\"c\"".utf8)), ["{\"a\":1}", "{\"b\":2}"])
        XCTAssertEqual(s.finish(), ["{\"c\""])
        XCTAssertEqual(s.finish(), [])
    }

    // MARK: running pair

    /// Runs a fake pair and returns every state it reported, waiting for the final one.
    func run(_ body: String, cancelWhen: ((PairState) -> Bool)? = nil, timeout: TimeInterval = 10) throws -> [PairState] {
        let run = PairingRun(path: try script(body), args: ["pair"])
        let lock = NSLock()
        var states: [PairState] = []
        let done = XCTestExpectation(description: "final state")
        run.start { state in
            lock.lock(); states.append(state); lock.unlock()
            if let cancelWhen, cancelWhen(state) { run.cancel() }
            if state.isFinal { done.fulfill() }
        }
        wait(for: [done], timeout: timeout)
        lock.lock(); defer { lock.unlock() }
        return states
    }

    func testPairedAfterCode() throws {
        let states = try run("""
            echo '{"event":"code","code":"ABCD-EFGH","expires_at":"2026-11-19T22:30:00Z"}'
            sleep 0.2
            echo '{"event":"paired","device_id":"d1"}'
            """)
        XCTAssertEqual(states.count, 2)
        guard case .code("ABCD-EFGH", _) = states[0] else { return XCTFail("\(states)") }
        XCTAssertEqual(states[1], .paired)
    }

    func testCodeArrivesWhileTheCommandIsStillRunning() throws {
        // The code must show at once, not when the command ends.
        let states = try run("""
            echo '{"event":"code","code":"ABCD-EFGH"}'
            sleep 30
            """, cancelWhen: { if case .code = $0 { return true } else { return false } })
        XCTAssertEqual(states.count, 2)
        guard case .code = states[0] else { return XCTFail("\(states)") }
        XCTAssertEqual(states[1], .cancelled)
    }

    func testCancelTerminatesTheProcess() throws {
        let marker = dir.appendingPathComponent("survived").path
        let states = try run("""
            echo '{"event":"code","code":"ABCD-EFGH"}'
            sleep 3
            touch '\(marker)'
            """, cancelWhen: { if case .code = $0 { return true } else { return false } })
        XCTAssertEqual(states.last, .cancelled)
        Thread.sleep(forTimeInterval: 0.2)
        XCTAssertFalse(FileManager.default.fileExists(atPath: marker))
    }

    func testCancelRacingAnApprovalEndsPairedButPaused() throws {
        // Like `pair`: a stop after approval lets enrollment finish and say so.
        let states = try run("""
            paired() { echo '{"event":"paired","device_id":"d1","paused":true}'; exit 0; }
            trap paired TERM
            echo '{"event":"code","code":"ABCD-EFGH"}'
            sleep 5 & wait
            """, cancelWhen: { if case .code = $0 { return true } else { return false } })
        XCTAssertEqual(states.last, .pairedPaused)
    }

    func testUncertainIsNeitherPairedNorNothing() throws {
        let states = try run("""
            echo '{"event":"code","code":"ABCD-EFGH"}'
            echo '{"event":"uncertain"}'
            exit 4
            """)
        XCTAssertEqual(states.last, .uncertain)
    }

    func testExpired() throws {
        let states = try run("""
            echo '{"event":"code","code":"ABCD-EFGH"}'
            echo '{"event":"expired"}'
            exit 3
            """)
        XCTAssertEqual(states.last, .expired)
    }

    func testCancelledByTheHouse() throws {
        let states = try run("""
            echo '{"event":"cancelled"}'
            exit 3
            """)
        XCTAssertEqual(states, [.cancelled])
    }

    func testFailureReportsTheFirstStderrLine() throws {
        let states = try run("echo 'volume is not grantable' >&2; echo 'more' >&2; exit 1")
        XCTAssertEqual(states, [.failed("volume is not grantable")])
    }

    func testExitWithoutPairedIsAFailureEvenAtZero() throws {
        let states = try run("echo '{\"event\":\"code\",\"code\":\"ABCD-EFGH\"}'; exit 0")
        XCTAssertEqual(states.last, .failed(""))
    }

    func testPairedEventWithBadExitIsNotPaired() throws {
        let states = try run("echo '{\"event\":\"paired\",\"device_id\":\"d\"}'; echo 'could not save' >&2; exit 1")
        XCTAssertEqual(states, [.failed("could not save")])
    }

    func testMissingCompanionFails() throws {
        let run = PairingRun(path: dir.appendingPathComponent("nope").path, args: ["pair"])
        var got: [PairState] = []
        run.start { got.append($0) }
        XCTAssertEqual(got.count, 1)
        guard case .failed = got[0] else { return XCTFail("\(got)") }
    }

    func testLinesSplitAcrossWritesAreJoined() throws {
        let states = try run("""
            printf '{"event":"code",'
            sleep 0.2
            printf '"code":"ABCD-EFGH"}\\n'
            echo '{"event":"paired","device_id":"d"}'
            """)
        XCTAssertEqual(states.count, 2)
        XCTAssertEqual(states.last, .paired)
    }
}
