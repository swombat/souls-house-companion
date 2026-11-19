import Foundation

// Pairing this Mac from the menu: which disks to offer, the `pair` command to
// run, and what its JSON lines mean. The window in the app only shows this.

/// A disk the companion can offer, from `souls-house-companion volumes`.
public struct PairVolume: Equatable {
    public let key: String
    public let mountpoint: String
    /// What the owner sees next to the checkbox.
    public let label: String
    /// The Mac's main disk, ticked by default.
    public let isMain: Bool
}

/// The Mac's own disk: the Data volume, or `/` on a Mac without the split.
public func isMainDisk(_ mountpoint: String) -> Bool {
    mountpoint == "/System/Volumes/Data" || mountpoint == "/"
}

public func volumeLabel(_ mountpoint: String) -> String {
    if isMainDisk(mountpoint) { return "Main disk (your files)" }
    let last = (mountpoint as NSString).lastPathComponent
    return last.isEmpty ? mountpoint : last
}

/// Disks that can be offered: a key, and nothing denying them. Anything we
/// cannot read as a list of volumes gives nil, so the window can say so.
public func grantableVolumes(from data: Data) -> [PairVolume]? {
    guard let list = (try? JSONSerialization.jsonObject(with: data)) as? [[String: Any]] else { return nil }
    var seen = Set<String>()
    var out: [PairVolume] = []
    for entry in list {
        guard let key = entry["key"] as? String, !key.isEmpty,
              (entry["denied"] as? String ?? "").isEmpty,
              let mount = entry["mountpoint"] as? String, !mount.isEmpty,
              seen.insert(key).inserted else { continue }
        out.append(PairVolume(key: key, mountpoint: mount, label: volumeLabel(mount), isMain: isMainDisk(mount)))
    }
    // The main disk first, then the rest as the companion listed them.
    return out.filter { $0.isMain } + out.filter { !$0.isMain }
}

/// Lists the disks by asking the companion.
public func listVolumes(companion: String, run: (String, [String]) -> CLIResult) -> [PairVolume]? {
    let result = run(companion, ["volumes"])
    guard result.succeeded else { return nil }
    return grantableVolumes(from: result.output)
}

/// The name the house shows for this Mac: the Mac's own name, tidied to what
/// the house accepts (1 to 100 bytes, no control characters).
public func pairDeviceName(_ raw: String?) -> String {
    var name = ""
    for ch in (raw ?? "").unicodeScalars where !CharacterSet.controlCharacters.contains(ch) {
        if name.utf8.count + String(ch).utf8.count > 100 { break }
        name.unicodeScalars.append(ch)
    }
    name = name.trimmingCharacters(in: .whitespaces)
    return name.isEmpty ? "This Mac" : name
}

public struct PairChoice: Equatable {
    public let key: String
    public let write: Bool
    public init(key: String, write: Bool) { self.key = key; self.write = write }
}

public func pairArguments(url: String, name: String, choices: [PairChoice]) -> [String] {
    var args = ["pair", "--url", url, "--name", name]
    for c in choices { args += ["--volume", c.write ? "\(c.key)=write" : c.key] }
    return args
}

/// One line of the `pair` command's output.
public enum PairEvent: Equatable {
    case code(String, expiresAt: Date?)
    case paired
    /// Paired as a stop arrived: saved paused, nothing shared until Connect.
    case pairedPaused
    case expired
    case cancelled
    /// The house may have added this Mac, but the answer never arrived.
    case uncertain
}

public func parsePairEvent(_ line: String) -> PairEvent? {
    guard let data = line.data(using: .utf8),
          let o = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
          let event = o["event"] as? String else { return nil }
    switch event {
    case "code":
        guard let code = o["code"] as? String, !code.isEmpty else { return nil }
        return .code(code, expiresAt: parseDate(o["expires_at"]))
    case "paired": return (o["paused"] as? Bool) == true ? .pairedPaused : .paired
    case "expired": return .expired
    case "cancelled": return .cancelled
    case "uncertain": return .uncertain
    default: return nil
    }
}

/// Cuts a stream of bytes into lines as they arrive. A line is complete at
/// its newline; what is left at the end of the stream counts as a line too.
public struct LineSplitter {
    private var pending = Data()
    public init() {}

    public mutating func add(_ chunk: Data) -> [String] {
        pending.append(chunk)
        var lines: [String] = []
        while let nl = pending.firstIndex(of: 0x0A) {
            lines.append(String(decoding: pending[pending.startIndex..<nl], as: UTF8.self))
            pending.removeSubrange(pending.startIndex...nl)
        }
        // A line that never ends must not grow without bound.
        if pending.count > 1 << 16 { pending.removeAll() }
        return lines
    }

    public mutating func finish() -> [String] {
        defer { pending.removeAll() }
        return pending.isEmpty ? [] : [String(decoding: pending, as: UTF8.self)]
    }
}

/// Where a pairing stands, as the window shows it.
public enum PairState: Equatable {
    case waiting
    case code(String, expiresAt: Date?)
    case paired
    case expired
    case cancelled
    case uncertain
    case pairedPaused
    case failed(String)

    public var isFinal: Bool {
        switch self {
        case .waiting, .code: return false
        default: return true
        }
    }
}

/// One run of `pair`. Reads its JSON lines as they arrive; `cancel()` ends
/// it by terminating the process, and nothing is written when it is stopped.
/// `onState` may be called from any thread; the last call is a final state.
public final class PairingRun {
    private let path: String
    private let args: [String]
    private let process = Process()
    private let lock = NSLock()
    private var state = PairState.waiting
    private var sawPairedPaused = false
    private var sawPaired = false, sawExpired = false, sawCancelled = false, sawUncertain = false
    private var cancelRequested = false
    private var finished = false
    private var stderrText = Data()

    public init(path: String, args: [String]) { self.path = path; self.args = args }

    public func start(onState: @escaping (PairState) -> Void) {
        process.executableURL = URL(fileURLWithPath: path)
        process.arguments = args
        let out = Pipe(), err = Pipe()
        process.standardOutput = out
        process.standardError = err
        process.standardInput = FileHandle.nullDevice

        let exited = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in exited.signal() }
        do { try process.run() } catch {
            finish(.failed("The souls.house companion could not be started."), onState)
            return
        }
        let outReader = out.fileHandleForReading, errReader = err.fileHandleForReading
        let outDone = DispatchSemaphore(value: 0)
        Thread { [self] in
            var splitter = LineSplitter()
            while true {
                let chunk = outReader.availableData
                if chunk.isEmpty { break }
                for line in splitter.add(chunk) { handle(line, onState) }
            }
            for line in splitter.finish() { handle(line, onState) }
            outDone.signal()
        }.start()
        Thread { [self] in
            while true {
                let chunk = errReader.availableData
                if chunk.isEmpty { break }
                lock.lock()
                if stderrText.count < 4096 { stderrText.append(chunk.prefix(4096 - stderrText.count)) }
                lock.unlock()
            }
        }.start()
        Thread { [self] in
            exited.wait()
            // The reader ends when the pipe closes; a descendant holding it
            // open must not keep the window waiting.
            if outDone.wait(timeout: .now() + 1) == .timedOut { try? outReader.close() }
            settle(onState)
        }.start()
    }

    /// Stops the pairing. Safe to call at any time, and more than once.
    public func cancel() {
        lock.lock()
        let live = !finished && !cancelRequested
        cancelRequested = true
        lock.unlock()
        if live, process.isRunning { process.terminate() }
    }

    private func handle(_ line: String, _ onState: (PairState) -> Void) {
        guard let event = parsePairEvent(line) else { return }
        lock.lock()
        if finished { lock.unlock(); return }
        let next: PairState?
        switch event {
        case let .code(code, expiresAt): next = .code(code, expiresAt: expiresAt)
        case .paired: sawPaired = true; next = nil
        case .pairedPaused: sawPairedPaused = true; next = nil
        case .expired: sawExpired = true; next = nil
        case .cancelled: sawCancelled = true; next = nil
        case .uncertain: sawUncertain = true; next = nil
        }
        if let next { state = next }
        lock.unlock()
        if let next { onState(next) }
    }

    /// The process has ended: say how it ended, once.
    private func settle(_ onState: (PairState) -> Void) {
        lock.lock()
        let status = process.terminationStatus
        let stopped = cancelRequested
        let (paired, expired, cancelled, uncertain) = (sawPaired, sawExpired, sawCancelled, sawUncertain)
        let pairedPaused = sawPairedPaused
        let message = String(decoding: stderrText, as: UTF8.self)
            .split(whereSeparator: \.isNewline).first.map(String.init) ?? ""
        lock.unlock()
        let final: PairState
        // What the command reports wins over a cancel: once the house has
        // approved, `pair` finishes enrolling even when asked to stop.
        if pairedPaused && status == 0 { final = .pairedPaused }
        else if paired && status == 0 { final = .paired }
        else if uncertain { final = .uncertain }
        else if stopped { final = .cancelled }
        else if expired { final = .expired }
        else if cancelled { final = .cancelled }
        else { final = .failed(message) }
        finish(final, onState)
    }

    private func finish(_ final: PairState, _ onState: (PairState) -> Void) {
        lock.lock()
        if finished { lock.unlock(); return }
        finished = true
        state = final
        lock.unlock()
        onState(final)
    }
}
