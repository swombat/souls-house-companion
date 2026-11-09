import Foundation

public struct CLIResult: Equatable {
    public enum Outcome: Equatable { case exited(Int32), timedOut, failedToStart }
    public let outcome: Outcome
    public let output: Data
    public let truncated: Bool

    public var succeeded: Bool { outcome == .exited(0) && !truncated }
}

/// Runs a CLI with a deadline that does not depend on the child: output is
/// read on its own thread with a hard cap, the deadline is a semaphore wait,
/// and a late child gets TERM, then KILL to its process group. Draining the
/// pipe after exit is itself bounded, so a descendant holding stdout open
/// cannot keep the call alive.
public func runCLI(_ path: String, _ args: [String], timeout: TimeInterval, cap: Int = 1 << 20,
                   grace: TimeInterval = 1) -> CLIResult {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: path)
    process.arguments = args
    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = FileHandle.nullDevice
    process.standardInput = FileHandle.nullDevice

    let lock = NSLock()
    var buffer = Data()
    var truncated = false
    let drained = DispatchSemaphore(value: 0)
    let exited = DispatchSemaphore(value: 0)
    process.terminationHandler = { _ in exited.signal() }

    do { try process.run() } catch {
        return CLIResult(outcome: .failedToStart, output: Data(), truncated: false)
    }
    let reader = pipe.fileHandleForReading
    let readThread = Thread {
        while true {
            let chunk = reader.availableData // blocks on this thread only
            if chunk.isEmpty { break }
            lock.lock()
            let room = cap - buffer.count
            if room > 0 { buffer.append(chunk.prefix(room)) }
            if chunk.count > room { truncated = true }
            lock.unlock()
        }
        drained.signal()
    }
    readThread.start()

    var timedOut = false
    if exited.wait(timeout: .now() + timeout) == .timedOut {
        timedOut = true
        let pid = process.processIdentifier
        kill(pid, SIGTERM)
        if exited.wait(timeout: .now() + grace) == .timedOut {
            kill(-pid, SIGKILL)
            kill(pid, SIGKILL)
            _ = exited.wait(timeout: .now() + grace)
        }
    }
    // Bounded drain: an inherited stdout must not hold us.
    if drained.wait(timeout: .now() + (timedOut ? 0.1 : grace)) == .timedOut {
        try? reader.close()
    }
    lock.lock()
    defer { lock.unlock() }
    let output = buffer
    if timedOut { return CLIResult(outcome: .timedOut, output: output, truncated: truncated) }
    return CLIResult(outcome: .exited(process.terminationStatus), output: output, truncated: truncated)
}
